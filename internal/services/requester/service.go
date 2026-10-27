package requester

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	role "github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
)

const Kind = role.KindRequester

const requestTimeout = role.DefaultHTTPTimeout

type Deps struct {
	component.Instance

	Egress component.Egress

	OAuthCallbacks *oauthcallback.Registry

	OpenIngress func(handler http.Handler, options tunnel.ServiceOptions) (tunnel.Service, error)
}

type Service struct {
	spec v1alpha1.RequesterSpec

	deps Deps

	log *slog.Logger

	cfg role.Config

	client *http.Client

	trust *component.Trust

	directory identity.Directory

	mu sync.Mutex

	started bool

	ready bool

	readiness component.ReadySignal

	cancel context.CancelFunc

	done chan struct{}

	core *role.Requester

	ingress tunnel.Service

	wg sync.WaitGroup
}

var (
	_ component.Service  = (*Service)(nil)
	_ sshcontract.Runner = (*Service)(nil)
)

func New(ctx context.Context, spec v1alpha1.RequesterSpec, deps Deps) (*Service, error) {
	log := cmp.Or(deps.Log, slog.New(slog.DiscardHandler))
	s := &Service{spec: spec, deps: deps, log: log}
	placement := role.Placement{Workspace: deps.Self.Workspace, Name: deps.Self.Name, UID: string(deps.UID), StateDir: deps.StateDir}
	cfg, err := role.Configure(spec, placement)
	if err != nil {
		return nil, err
	}
	s.cfg = cfg
	s.trust = component.NewTrust(spec.TLS.CA(), log)
	s.client = egressFor(spec, deps, s.trust).Client(requestTimeout)
	s.directory = directoryFor(cfg.Network().PLCDirectoryURL, int(ptr.Deref(spec.DirectoryKeys, 0)), time.Duration(ptr.Deref(spec.DirectoryTTLSeconds, 0))*time.Second)
	s.log = log.With("component", "requester", "did", cfg.DID)
	return s, nil
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("requester: the service is already started")
	}
	s.started = true
	s.ready = false
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	stopTrust, err := s.trust.Follow(runCtx, s.deps.Instance)
	if err != nil {
		cancel()
		s.finish(ctx, nil)
		return err
	}
	defer stopTrust()
	if err := s.register(); err != nil {
		cancel()
		s.finish(ctx, nil)
		return err
	}
	s.log.Info("requester_registered", "workspace", s.cfg.Workspace, "name", s.deps.Self.Name)
	if s.cfg.WebRTCIngress() {
		if _, err := s.openIngress(); err != nil {
			s.log.Warn("requester_webrtc_ingress_off", "err", err.Error())
		} else {
			s.log.Info("requester_webrtc_ingress", "id", tunnel.ID(s.cfg.RepoDID))
		}
	}
	core, err := s.setup(runCtx)
	if err != nil {
		cancel()
		s.finish(ctx, core)
		return err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	if core.Records() == nil {
		s.readiness.Fail(errors.New("requester: the repo is not open"))
	} else {
		s.readiness.Signal()
	}
	s.log.Info("requester_started", "workspace", s.cfg.Workspace, "name", s.deps.Self.Name, "repo", core.RepoDID(), "mode", core.Mode())
	<-runCtx.Done()
	s.finish(ctx, core)
	return nil
}

func (s *Service) register() error {
	if s.deps.Bus == nil {
		return errors.New("requester: the handler registry is not available")
	}
	return s.deps.Bus.RegisterHTTP(component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.deps.Self.Name}, s.handler())
}

func (s *Service) openIngress() (tunnel.Service, error) {
	if s.deps.OpenIngress == nil {
		return nil, errors.New("requester: no WebRTC ingress factory is configured")
	}
	key := component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.deps.Self.Name}
	mux := s.handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(component.WithCaller(r.Context(), key)))
	})
	service, err := s.deps.OpenIngress(handler, s.cfg.WebRTCIngressOptions())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ingress = service
	s.mu.Unlock()
	return service, nil
}

func (s *Service) finish(ctx context.Context, core *role.Requester) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), component.DefaultStopTimeout)
	defer cancel()
	_ = supervise.Join(stopCtx, s.wg.Wait)
	if core != nil {
		core.Shutdown(stopCtx)
	}
	s.mu.Lock()
	s.started = false
	s.ready = false
	s.core = nil
	ingress := s.ingress
	s.ingress = nil
	done := s.done
	s.mu.Unlock()
	if ingress != nil {
		_ = ingress.Close()
	}
	close(done)
}

func (s *Service) role() *role.Requester {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.core
}

func (s *Service) healthy() *role.Requester {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.core == nil || s.core.Records() == nil {
		return nil
	}
	return s.core
}

func (s *Service) Ready() component.Readiness {
	return &s.readiness
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return nil
	}
	endpoints := []component.Endpoint{{Name: "http", Protocol: component.ProtocolHTTP}}
	if s.cfg.WebRTCIngress() {
		if id := tunnel.ID(s.cfg.RepoDID); id != "" {
			endpoints = append(endpoints, component.Endpoint{Name: "webrtc", Protocol: "webrtc", Address: tunnel.Ref(id)})
		}
	}
	return endpoints
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) RunCommand(ctx context.Context, account sshcontract.Account, command string, env map[string]string, io sshcontract.CommandIO) error {
	core := s.role()
	if core == nil {
		return errors.New("requester: the service is not ready to run commands")
	}
	return core.RunCommand(ctx, account, command, env, io)
}
