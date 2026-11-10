package webrtc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/channelconn"
)

const (
	MeshMaxCiphertext = 16384

	MeshConnectTimeout = 2 * time.Minute

	MeshMaxSessions = 64

	meshMaxPlaintext = 1 << 16
)

type MeshConfig struct {
	StateDir string

	Handler http.Handler

	Log *slog.Logger

	UDPPort int
}

type Mesh struct {
	identity *age.X25519Identity

	certificate webrtc.Certificate

	fingerprint string

	api *webrtc.API

	handler http.Handler

	log *slog.Logger

	ctx context.Context

	mu sync.Mutex

	sessions int
}

type meshOffer struct {
	SDP string `json:"sdp"`

	ReplyTo string `json:"replyTo"`
}

type meshAnswer struct {
	SDP string `json:"sdp"`
}

func NewMesh(ctx context.Context, cfg MeshConfig) (*Mesh, error) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.StateDir == "" || cfg.Handler == nil {
		return nil, errors.New("webrtc mesh: a state dir and a handler are required")
	}
	dir := filepath.Join(cfg.StateDir, "mesh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	identity, err := meshIdentity(filepath.Join(dir, "age.key"))
	if err != nil {
		return nil, err
	}
	certificate, err := meshCertificate(filepath.Join(dir, "dtls.pem"))
	if err != nil {
		return nil, err
	}
	fingerprint, err := meshFingerprint(certificate)
	if err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	tuneSettings(&settings)
	if cfg.UDPPort > 0 {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.UDPPort})
		if err != nil {
			log.Warn("webrtc_mesh_udp_port_unbound", "port", cfg.UDPPort, "err", err.Error())
		} else {
			settings.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}))
		}
	}
	return &Mesh{identity: identity, certificate: certificate, fingerprint: fingerprint, handler: cfg.Handler, log: log, ctx: ctx,
		api: webrtc.NewAPI(webrtc.WithSettingEngine(settings))}, nil
}

func (m *Mesh) Recipient() string { return m.identity.Recipient().String() }

func (m *Mesh) Fingerprint() string { return m.fingerprint }

func (m *Mesh) Answer(ctx context.Context, ciphertext string) (string, error) {
	if ciphertext == "" || len(ciphertext) > MeshMaxCiphertext {
		return "", fmt.Errorf("webrtc mesh: the offer ciphertext is empty or over %d bytes", MeshMaxCiphertext)
	}
	plaintext, err := meshOpen(m.identity, ciphertext)
	if err != nil {
		return "", errors.New("webrtc mesh: the offer is not sealed to this peer")
	}
	var offer meshOffer
	if err := json.Unmarshal(plaintext, &offer); err != nil || offer.SDP == "" {
		return "", errors.New("webrtc mesh: the offer is not {sdp, replyTo}")
	}
	replyTo, err := age.ParseX25519Recipient(strings.TrimSpace(offer.ReplyTo))
	if err != nil {
		return "", errors.New("webrtc mesh: the offer names no age X25519 replyTo")
	}
	if !m.reserve() {
		return "", fmt.Errorf("webrtc mesh: %d sessions are open", MeshMaxSessions)
	}
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{STUNServer}}},
		Certificates: []webrtc.Certificate{m.certificate}})
	if err != nil {
		m.release()
		return "", err
	}
	m.watch(pc)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("webrtc mesh: the offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(GatherTimeout):
	case <-ctx.Done():
		_ = pc.Close()
		return "", ctx.Err()
	}
	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return "", errors.New("webrtc mesh: no local description")
	}
	body, err := json.Marshal(meshAnswer{SDP: local.SDP})
	if err != nil {
		_ = pc.Close()
		return "", err
	}
	sealed, err := meshSeal(replyTo, body)
	if err != nil || len(sealed) > MeshMaxCiphertext {
		_ = pc.Close()
		return "", fmt.Errorf("webrtc mesh: sealing the answer (%d bytes): %v", len(sealed), err)
	}
	return sealed, nil
}

func (m *Mesh) reserve() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions >= MeshMaxSessions {
		return false
	}
	m.sessions++
	return true
}

func (m *Mesh) release() {
	m.mu.Lock()
	m.sessions--
	m.mu.Unlock()
}

func (m *Mesh) watch(pc *webrtc.PeerConnection) {
	var once sync.Once
	closed := make(chan struct{})
	done := func() { once.Do(func() { close(closed); m.release() }) }
	connected := make(chan struct{})
	var connectedOnce sync.Once
	timer := time.AfterFunc(MeshConnectTimeout, func() {
		select {
		case <-connected:
		default:
			_ = pc.Close()
		}
	})
	stop := context.AfterFunc(m.ctx, func() { _ = pc.Close() })
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected); timer.Stop() })
			m.log.Info("webrtc_mesh_connected")
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			_ = pc.Close()
		case webrtc.PeerConnectionStateClosed:
			timer.Stop()
			stop()
			done()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() { m.serve(dc) })
	})
}

func (m *Mesh) serve(dc *webrtc.DataChannel) {
	conn := channelconn.New(dc.Send, dc.Close)
	dc.OnMessage(func(message webrtc.DataChannelMessage) { conn.Push(message.Data) })
	dc.OnClose(func() { conn.CloseWithError(io.EOF) })
	server := &http.Server{Handler: m.handler, ReadHeaderTimeout: RequestDeadline, IdleTimeout: MeshConnectTimeout}
	go func() {
		_ = server.Serve(&oneConn{conn: conn, done: make(chan struct{})})
		_ = conn.Close()
	}()
}

type oneConn struct {
	conn net.Conn

	once sync.Once

	done chan struct{}

	taken bool

	mu sync.Mutex
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.taken {
		l.taken = true
		l.mu.Unlock()
		return &closeNotify{Conn: l.conn, closed: func() { l.Close() }}, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotify struct {
	net.Conn

	closed func()
}

func (c *closeNotify) Close() error {
	err := c.Conn.Close()
	c.closed()
	return err
}

func meshIdentity(path string) (*age.X25519Identity, error) {
	if data, err := os.ReadFile(path); err == nil {
		return age.ParseX25519Identity(strings.TrimSpace(string(data)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	return identity, os.WriteFile(path, []byte(identity.String()+"\n"), 0o600)
}

func meshCertificate(path string) (webrtc.Certificate, error) {
	if data, err := os.ReadFile(path); err == nil {
		cert, err := webrtc.CertificateFromPEM(string(data))
		if err != nil {
			return webrtc.Certificate{}, err
		}
		if time.Until(cert.Expires()) > 24*time.Hour {
			return *cert, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return webrtc.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return webrtc.Certificate{}, err
	}
	cert, err := webrtc.GenerateCertificate(key)
	if err != nil {
		return webrtc.Certificate{}, err
	}
	pem, err := cert.PEM()
	if err != nil {
		return webrtc.Certificate{}, err
	}
	return *cert, os.WriteFile(path, []byte(pem), 0o600)
}

func meshFingerprint(certificate webrtc.Certificate) (string, error) {
	fingerprints, err := certificate.GetFingerprints()
	if err != nil {
		return "", err
	}
	for _, f := range fingerprints {
		if f.Algorithm == "sha-256" {
			return f.Algorithm + " " + strings.ToUpper(f.Value), nil
		}
	}
	return "", errors.New("webrtc mesh: the DTLS certificate has no sha-256 fingerprint")
}

func meshSeal(recipient *age.X25519Recipient, plaintext []byte) (string, error) {
	var out bytes.Buffer
	armored := armor.NewWriter(&out)
	w, err := age.Encrypt(armored, recipient)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(plaintext); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := armored.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func meshOpen(identity *age.X25519Identity, ciphertext string) ([]byte, error) {
	r, err := age.Decrypt(armor.NewReader(strings.NewReader(ciphertext)), identity)
	if err != nil {
		return nil, err
	}
	plaintext, err := io.ReadAll(io.LimitReader(r, meshMaxPlaintext+1))
	if err != nil {
		return nil, err
	}
	if len(plaintext) > meshMaxPlaintext {
		return nil, fmt.Errorf("webrtc mesh: the sealed body is over %d bytes", meshMaxPlaintext)
	}
	return plaintext, nil
}
