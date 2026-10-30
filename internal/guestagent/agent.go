package guestagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

type Agent struct {
	cfg Config

	log *slog.Logger

	identity *age.X25519Identity

	certificate webrtc.Certificate

	fingerprint string

	api *webrtc.API

	mux ice.UDPMux

	tokens *tokenSource

	poster *poster

	ctx context.Context

	wg sync.WaitGroup

	mu sync.Mutex

	sessions map[string]*session

	closed bool

	cursor int64

	seen map[string]bool

	requested bool

	applied map[string]bool
}

func New(cfg Config, log *slog.Logger) (*Agent, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("guestagent: generating the age identity: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("guestagent: generating the DTLS key: %w", err)
	}
	certificate, err := webrtc.GenerateCertificate(key)
	if err != nil {
		return nil, fmt.Errorf("guestagent: generating the DTLS certificate: %w", err)
	}
	fingerprint, err := Fingerprint(*certificate)
	if err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{LoggerFactory: pionLogs{log: log}}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	mux, err := forwardedPort(&settings, cfg.Bidder.WebRTC, log)
	if err != nil {
		return nil, err
	}
	tokens := newTokenSource(cfg.LoginHelper, cfg.Bidder.Audience, log)
	return &Agent{
		cfg:         cfg,
		log:         log,
		identity:    identity,
		certificate: *certificate,
		fingerprint: fingerprint,
		api:         webrtc.NewAPI(webrtc.WithSettingEngine(settings)),
		mux:         mux,
		tokens:      tokens,
		poster: &poster{
			url:    cfg.Bidder.Base + EventsPath,
			accept: cfg.Accept,
			tokens: tokens,
			client: &http.Client{Transport: &http.Transport{Proxy: nil}},
			log:    log,
		},
		sessions: map[string]*session{},
		seen:     map[string]bool{},
		applied:  map[string]bool{},
	}, nil
}

func forwardedPort(settings *webrtc.SettingEngine, w *WebRTCPort, log *slog.Logger) (ice.UDPMux, error) {
	if w == nil {
		return nil, nil
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: w.UDPPort})
	if err != nil {
		log.Warn("the forwarded webrtc UDP port does not bind; host candidates take ephemeral ports", "port", w.UDPPort, "err", err.Error())
		return nil, nil
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn, Logger: pionLogs{log: log}.NewLogger("udpmux")})
	settings.SetICEUDPMux(mux)
	rule := webrtc.ICEAddressRewriteRule{External: w.HostAddresses, AsCandidateType: webrtc.ICECandidateTypeHost, Mode: webrtc.ICEAddressRewriteReplace}
	if err := settings.SetICEAddressRewriteRules(rule); err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("guestagent: the host candidate rewrite to the bidder host's LAN addresses: %w", err)
	}
	return mux, nil
}

func Fingerprint(certificate webrtc.Certificate) (string, error) {
	fingerprints, err := certificate.GetFingerprints()
	if err != nil {
		return "", fmt.Errorf("guestagent: the DTLS certificate fingerprint: %w", err)
	}
	for _, f := range fingerprints {
		if f.Algorithm == "sha-256" {
			return f.Algorithm + " " + strings.ToUpper(f.Value), nil
		}
	}
	return "", fmt.Errorf("guestagent: the DTLS certificate has no sha-256 fingerprint")
}

func (a *Agent) Fingerprint() string { return a.fingerprint }

func (a *Agent) Run(ctx context.Context, ready func()) error {
	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: the state dir: %v", ErrConfig, err)
	}
	a.ctx = ctx
	a.loadCursor()
	tags := make([]string, 0, len(a.cfg.Tunnels))
	for _, t := range a.cfg.Tunnels {
		tags = append(tags, t.Tag)
	}
	a.log.Info("the guest agent starts", "tags", tags, "sealedSecrets", a.cfg.Secrets != nil, "agentUnit", a.cfg.AgentUnit, "base", a.cfg.Bidder.Base, "fingerprint", a.fingerprint, "cursor", a.cursor,
		"forwardedUDPPort", a.mux != nil)
	for _, t := range a.cfg.Tunnels {
		a.wg.Go(func() { a.announce(ctx, t.Tag) })
	}
	if a.cfg.Secrets != nil {
		a.wg.Go(func() { a.runSecrets(ctx) })
	}
	if ready != nil {
		ready()
	}
	a.subscribe(ctx)
	a.mu.Lock()
	a.closed = true
	open := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		open = append(open, s)
	}
	a.mu.Unlock()
	for _, s := range open {
		s.close("the agent is stopping")
	}
	a.wg.Wait()
	if a.mux != nil {
		_ = a.mux.Close()
	}
	a.log.Info("the guest agent stops")
	return nil
}
