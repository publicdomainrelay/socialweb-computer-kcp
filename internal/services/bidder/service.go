package bidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
)

const (
	Kind = "Bidder"

	ServiceName = "Bidder"

	HealthPath = "/healthz"

	OAuthMetadataPath = "/oauth-client-metadata.json"

	DefaultCallTimeout = 20 * time.Second

	RecordReplyLimit = 8 << 20

	RecordRequestTimeout = 30 * time.Second

	DefaultGuestLoginMount = computevm.GuestLoginDefaultMount

	DefaultGuestVaultServer = computevm.VaultServer

	DefaultGuestVaultMount = computevm.VaultMount

	guestControlKind = "GuestControlPlane"

	guestControlName = "guest-kcp"

	guestExposure = "svc-guestcontrolplane-guest-kcp-ingress"

	pdsExposure = "svc-pds-pds-local"

	guestExposureWait = 60 * time.Second

	inputWait = 30 * time.Second
)

type Deps struct {
	component.Instance

	Egress component.Egress

	OAuthCallbacks *oauthcallback.Registry
}

type Service struct {
	spec v1alpha1.BidderSpec

	deps Deps

	egress component.Egress

	trust *component.Trust

	log *slog.Logger

	workspace string

	handle string

	providerHandle string

	guestVaultServer string

	policyArgs map[string]string

	serviceAuthHost string

	marketURL string

	guestURL string

	did string

	dir identity.Directory

	inputs *participant.Inputs

	authorizer atomic.Pointer[participant.Authorizer]

	relay *ingress.Subscriber

	bidder atomic.Pointer[core.Bidder]

	mux *http.ServeMux

	mu sync.Mutex

	started bool

	readiness component.ReadySignal

	endpoints []component.Endpoint

	cancel context.CancelFunc

	wg sync.WaitGroup

	stopOnce sync.Once

	stopErr error
}

var _ component.Service = (*Service)(nil)

func New(ctx context.Context, spec v1alpha1.BidderSpec, deps Deps) (*Service, error) {
	policyArgs, err := participant.ParsePolicyArgs(spec.PolicyArgs)
	if err != nil {
		return nil, err
	}
	handle := ""
	if spec.Identity != nil {
		handle = spec.Identity.Handle
	}
	providerHandle := handle
	if spec.Kcp != nil && spec.Kcp.Handle != "" {
		providerHandle = spec.Kcp.Handle
		handle = cmp.Or(handle, spec.Kcp.Handle)
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	base := deps.Egress
	if base.Log == nil {
		base.Log = deps.Log
	}
	egress := base
	trust := component.NewTrust(spec.TLS.CA(), log)
	if spec.Network != nil {
		if hosts := participant.NetworkHosts(*spec.Network, egress.Hosts); hosts != nil {
			egress.Hosts = hosts
		}
	}
	if trust.Enabled() {
		egress.TLS = trust.Client(nil)
		log.Info("the bidder trusts the published network CA")
	}
	s := &Service{
		spec:             spec,
		deps:             deps,
		egress:           egress,
		trust:            trust,
		workspace:        cmp.Or(deps.Self.Workspace, "root"),
		handle:           handle,
		providerHandle:   providerHandle,
		guestVaultServer: v1alpha1.VaultServer(spec.VaultRef, DefaultGuestVaultServer),
		policyArgs:       policyArgs,
	}
	if spec.ServiceAuth != nil {
		s.serviceAuthHost = strings.TrimSpace(spec.ServiceAuth.Hostname)
	}
	switch {
	case s.handle == "":
		return nil, errors.New("bidder: the component names no handle")
	case s.guestVaultServer == "":
		return nil, errors.New("bidder: the provider names no guest vault server")
	}
	s.log = log.With("component", "bidder", "handle", s.handle)
	return s, nil
}

func (s *Service) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		cancel()
		return errors.New("bidder: the service is already started")
	}
	s.started = true
	s.cancel = cancel
	s.mu.Unlock()
	defer s.halt(ctx)
	stopTrust, err := s.trust.Follow(runCtx, s.deps.Instance)
	if err != nil {
		cancel()
		return err
	}
	defer stopTrust()
	if err := s.prepare(runCtx); err != nil {
		cancel()
		return err
	}
	s.readiness.Signal()
	<-runCtx.Done()
	return nil
}

func (s *Service) Ready() component.Readiness {
	return &s.readiness
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]component.Endpoint(nil), s.endpoints...)
}

func (s *Service) Stop(ctx context.Context) error {
	return s.stopWith(ctx)
}

func (s *Service) halt(ctx context.Context) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), component.DefaultStopTimeout)
	defer cancel()
	_ = s.stopWith(stopCtx)
}

func (s *Service) stopWith(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		cancel, relay := s.cancel, s.relay
		s.started = false
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if relay != nil {
			relay.Close()
		}
		s.stopErr = supervise.Join(ctx, s.wg.Wait)
	})
	return s.stopErr
}

func (s *Service) prepare(ctx context.Context) error {
	if dir := s.stateDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("bidder: creating the state dir: %w", err)
		}
	}
	s.dir = ingress.NewDirectory(s.network().PLCDirectoryURL)
	intents, err := s.resolveIntents(ctx)
	if err != nil {
		return err
	}
	s.inputs = participant.NewInputs()
	if _, err := intents.Subscribe(ctx, inputsSubscription(s.inputs)); err != nil {
		return fmt.Errorf("bidder: subscribing to the market inputs: %w", err)
	}
	s.mux = s.routes()
	if err := s.register(); err != nil {
		return err
	}
	if err := s.awaitInputs(ctx); err != nil {
		return err
	}
	if err := s.checkServiceAuthHost(); err != nil {
		return err
	}
	if err := s.startRelay(ctx); err != nil {
		s.log.Warn("the bidder market relay did not start", "err", err)
	}
	s.authorizer.Store(s.newAuthorizer())
	bidder, err := s.newBidder(intents)
	if err != nil {
		s.log.Warn("the bidder does not serve the market", "err", err)
	}
	s.addEndpoint(component.Endpoint{Name: "http", Protocol: component.ProtocolHTTP})
	s.addEndpoint(component.Endpoint{Name: "market", Protocol: component.ProtocolHTTP})
	if s.guestURL != "" {
		s.addEndpoint(component.Endpoint{Name: "guest", Protocol: component.ProtocolHTTPS, URL: s.guestURL})
	}
	if endpoint, ok := s.relayEndpoint(); ok {
		s.addEndpoint(endpoint)
	}
	if bidder != nil {
		if _, err := intents.Subscribe(ctx, roleSubscription(bidder)); err != nil {
			return fmt.Errorf("bidder: subscribing to the market objects: %w", err)
		}
		s.bidder.Store(bidder)
		s.wg.Go(func() { bidder.Run(ctx) })
	}
	s.wg.Go(func() { s.crawl().Run(ctx) })
	s.log.Info("the bidder serves the market", "did", s.did, "guest", s.guestURL, "offering", s.inboundMarketURL(),
		"serviceAuthHost", s.serviceAuthHost, "identityMode", strings.TrimSpace(s.spec.Mode), "inboundXRPC", s.inboundXRPC())
	return nil
}

func (s *Service) register() error {
	if s.deps.Bus == nil {
		return errors.New("bidder: the handler registry is not available")
	}
	key := component.Key{Workspace: s.workspace, Kind: Kind, Name: s.deps.Self.Name}
	if err := s.deps.Bus.RegisterHTTP(key, s.mux); err != nil {
		return err
	}
	return s.deps.Bus.RegisterHTTP(key, s.mux, "market")
}

func (s *Service) addEndpoint(endpoint component.Endpoint) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, endpoint)
	s.mu.Unlock()
}

func (s *Service) self() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace}
}

func (s *Service) stateDir() string {
	var dir string
	if s.spec.Storage != nil {
		dir = s.spec.Storage.DataDir
	}
	return cmp.Or(dir, s.deps.StateDir)
}

func (s *Service) network() v1alpha1.MarketNetworkValue {
	if s.spec.Network == nil {
		return v1alpha1.MarketNetworkValue{}
	}
	return *s.spec.Network
}

func (s *Service) identityRef() *commonv1.ObjectRef {
	if s.spec.Identity != nil {
		return s.spec.Identity.MarketIdentityRef
	}
	return s.spec.PdsRef
}

func (s *Service) publicNetwork() bool {
	return strings.TrimSpace(s.network().IngressProxyHost) != "" || strings.TrimSpace(s.spec.Network.GuestTLSURL()) != ""
}

func (s *Service) publicMarketURL() string {
	if raw := strings.TrimSpace(s.marketURL); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	if endpoint, ok := s.relayEndpoint(); ok {
		return strings.TrimRight(endpoint.URL, "/")
	}
	return ""
}

func (s *Service) inboundMarketURL() string {
	if !s.inboundXRPC() {
		return ""
	}
	return s.publicMarketURL()
}

func (s *Service) inboundXRPC() bool {
	switch strings.TrimSpace(s.spec.Mode) {
	case string(v1alpha1.MarketIdentityModeOAuthQr), string(v1alpha1.MarketIdentityModeOAuthBrowser), string(v1alpha1.MarketIdentityModeOAuthDelegated):
		return false
	}
	if s.spec.Comms == nil {
		return true
	}
	switch s.spec.Comms.Mode {
	case "", v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeBoth:
		return true
	}
	return false
}

func (s *Service) checkServiceAuthHost() error {
	if s.serviceAuthHost == "" || s.marketURL == "" {
		return nil
	}
	override := s.serviceAuthHost
	if !strings.Contains(override, "://") {
		override = "//" + override
	}
	got := strings.ToLower(participant.HostPort(override))
	want := strings.ToLower(participant.HostPort(s.marketURL))
	if got == "" || want == "" || got != want {
		return fmt.Errorf("bidder: serviceAuth.hostname %q must match the market URL host %q; the offering and the verifier audience have one source", s.serviceAuthHost, want)
	}
	return nil
}

func ingressConnectHost(n v1alpha1.MarketNetworkValue) string {
	_, port, err := net.SplitHostPort(n.IngressProxyHost)
	if err != nil || port == "" {
		return ""
	}
	for _, host := range n.Hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return ""
}

func portOf(raw string) int {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return 0
	}
	port := 0
	for _, r := range u.Port() {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}
