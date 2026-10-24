package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	guestplane "github.com/johnandersen777/socialweb-computer-kcp/internal/guestcp/openbao"
	guestrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/guestcp/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

type Service struct {
	spec v1alpha1.GuestControlPlaneSpec

	deps Deps

	log *slog.Logger

	trust *component.Trust

	stateDir string

	grantSync time.Duration

	issuerRetry time.Duration

	downloads *pinneddownloads.Set

	mu sync.Mutex

	started bool

	registered bool

	readiness component.ReadySignal

	mux *http.ServeMux

	cancel context.CancelFunc

	wg sync.WaitGroup

	stopOnce sync.Once

	stopErr error

	cleanupMu sync.Mutex

	plane *guestplane.Plane

	relay *guestrelay.Relay

	queue guestcp.Queue

	unwatchQueue func()
}

func (s *Service) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		cancel()
		return errors.New("guestcontrolplane: the service is already started")
	}
	s.started = true
	s.cancel = cancel
	s.mu.Unlock()
	defer s.shutdown(ctx)

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
	s.mu.Lock()
	plane := s.plane
	s.mu.Unlock()
	if plane.Ready() != nil {
		s.readiness.Fail(errors.New("guestcontrolplane: the guest identity proxy is not configured"))
	} else {
		s.readiness.Signal()
	}
	<-runCtx.Done()
	return nil
}

func (s *Service) Ready() component.Readiness {
	return &s.readiness
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	registered := s.registered
	plane := s.plane
	s.mu.Unlock()
	if !registered {
		return nil
	}
	out := []component.Endpoint{{Name: "http", Protocol: component.ProtocolHTTP}}
	if raw := s.relayURL(); raw != "" {
		protocol := component.ProtocolHTTPS
		if strings.HasPrefix(raw, "http://") {
			protocol = component.ProtocolHTTP
		}
		out = append(out, component.Endpoint{Name: "relay", Protocol: protocol, URL: raw})
	}
	if plane != nil {
		if issuer := plane.OIDCIssuer(); issuer != "" {
			out = append(out, component.Endpoint{Name: "issuer", Protocol: component.ProtocolHTTPS, URL: issuer})
		}
	}
	return out
}

func (s *Service) Stop(ctx context.Context) error {
	return s.shutdownWith(ctx)
}

func (s *Service) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), component.DefaultStopTimeout)
	defer cancel()
	return s.shutdownWith(ctx)
}

func (s *Service) shutdownWith(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.cleanupMu.Lock()
		defer s.cleanupMu.Unlock()
		s.mu.Lock()
		cancel := s.cancel
		relay := s.relay
		plane := s.plane
		unwatchQueue := s.unwatchQueue
		s.started = false
		s.registered = false
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if unwatchQueue != nil {
			unwatchQueue()
		}
		if relay != nil {
			relay.Close()
		}
		if plane != nil {
			plane.Close()
		}
		if err := supervise.Join(ctx, s.wg.Wait); err != nil && s.stopErr == nil {
			s.stopErr = err
		}
	})
	return s.stopErr
}

func (s *Service) prepare(ctx context.Context) error {
	if s.stateDir != "" {
		if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
			return fmt.Errorf("guestcontrolplane: creating the state dir: %w", err)
		}
	}
	if s.deps.Bus == nil {
		return errors.New("guestcontrolplane: the handler registry is not available")
	}
	inputs, guests, grants, err := s.handles(ctx)
	if err != nil {
		return err
	}
	plane, err := guestplane.New(guestplane.Config{
		Handle:          s.handle(),
		DID:             s.did(),
		Mount:           s.spec.GuestLoginMount,
		Timeout:         DefaultOpenBaoTimeout,
		GrantSync:       s.grantSync,
		IssuerRetry:     s.issuerRetry,
		HostKeyAttempts: int(ptr.Deref(s.spec.HostKeyAttempts, 0)),
	}, guestplane.Deps{
		Inputs: inputs,
		Guests: guests,
		Grants: grants,
		RBAC: reporemote.NewNetwork(reporemote.NetworkConfig{Directory: ingress.NewDirectory(s.network().PLCDirectoryURL),
			HTTP: networkHTTPClient(s.deps.Hosts, s.network(), s.trust, DefaultOpenBaoTimeout)}),
		FallbackIssuer: s.relayURL,
		Publish:        s.publishGuestEvent,
		Log:            s.log,
	})
	if err != nil {
		return err
	}
	if err := plane.Start(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.plane = plane
	s.mu.Unlock()
	if err := s.openQueue(ctx, inputs); err != nil {
		s.log.Warn("the guest event queue is not open; guest events are refused", "err", err)
	}
	s.mux = s.routes()
	if err := s.register(); err != nil {
		return err
	}
	if err := s.startRelay(ctx, plane); err != nil {
		s.log.Warn("the guest relay did not start", "err", err)
	}
	s.log.Info("the guest control plane serves guests", "handler", s.logicalURL(), "handle", s.handle(),
		"issuer", plane.IssuerURL(), "mount", s.spec.GuestLoginMount, "downloads", len(s.downloads.List()))
	return nil
}

func (s *Service) handles(ctx context.Context) (guestcp.Source, guestcp.Guests, guestcp.Grants, error) {
	self := s.self()
	ctx = bus.WithCallerLabels(ctx, self, s.deps.Labels)
	inputs, err := bus.Resolve[guestcp.Source](ctx, s.deps.Bus, self, self, bus.Capability{Name: guestcp.CapabilityInputs})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestcontrolplane: resolving the guest inputs: %w", err)
	}
	guests, err := bus.Resolve[guestcp.Guests](ctx, s.deps.Bus, self, self, bus.Capability{Name: guestcp.CapabilityGuests})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestcontrolplane: resolving the guest report writer: %w", err)
	}
	grants, err := bus.Resolve[guestcp.Grants](ctx, s.deps.Bus, self, self, bus.Capability{Name: guestcp.CapabilityGrants})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestcontrolplane: resolving the grant writer: %w", err)
	}
	return inputs, guests, grants, nil
}

func (s *Service) startRelay(ctx context.Context, plane *guestplane.Plane) error {
	if !ptr.Deref(s.spec.RelayEnabled, false) {
		return nil
	}
	target, err := s.pdsRef()
	if err != nil {
		return err
	}
	keyFile := ""
	if s.stateDir != "" {
		keyFile = s.stateDir + "/guest-relay.jwk"
	}
	relay, err := guestrelay.New(guestrelay.Config{
		Label:            "guest-" + s.spec.Role,
		KeyFile:          keyFile,
		IngressProxyHost: s.relayHost(),
		TLS:              s.relayTLS(),
	}, guestrelay.Deps{
		Handler:     s.mux,
		Subscribe:   s.subscribeDirect(ctx),
		ServiceAuth: busServiceAuth{bus: s.deps.Bus, self: s.self(), labels: s.deps.Labels, pds: target},
		DID:         plane.DID,
		Log:         s.log,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.relay = relay
	s.mu.Unlock()
	s.wg.Go(func() {
		if err := relay.Run(ctx, plane.Recheck); err != nil && ctx.Err() == nil {
			s.log.Warn("the guest relay did not start", "err", err)
		}
	})
	return nil
}

func (s *Service) relayURL() string {
	s.mu.Lock()
	relay := s.relay
	s.mu.Unlock()
	if relay == nil {
		return ""
	}
	return relay.URL()
}

func (s *Service) register() error {
	key := component.Key{Workspace: s.workspace(), Kind: Kind, Name: s.deps.Self.Name}
	if err := s.deps.Bus.RegisterHTTP(key, s.mux); err != nil {
		return err
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	return nil
}

func (s *Service) logicalURL() string {
	return kcp.HandlerURL(s.workspace(), Kind, s.deps.Self.Name)
}

var _ component.Service = (*Service)(nil)
