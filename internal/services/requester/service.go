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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	role "github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
)

const Kind = role.KindRequester

const requestTimeout = role.DefaultHTTPTimeout

type Deps struct {
	component.Instance

	Credentials component.CredentialResolver

	Egress component.Egress
}

type Service struct {
	spec v1alpha1.RequesterSpec

	deps Deps

	log *slog.Logger

	cfg role.Config

	client *http.Client

	directory identity.Directory

	sessions *participant.SessionStore

	saveSessions bool

	mu sync.Mutex

	started bool

	ready bool

	readiness component.ReadySignal

	cancel context.CancelFunc

	done chan struct{}

	core *role.Requester

	wg sync.WaitGroup
}

var (
	_ component.Service  = (*Service)(nil)
	_ sshcontract.Runner = (*Service)(nil)
)

func New(spec v1alpha1.RequesterSpec, deps Deps) (*Service, error) {
	log := cmp.Or(deps.Log, slog.New(slog.DiscardHandler))
	s := &Service{spec: spec, deps: deps, log: log}
	if ref := sessionCredential(spec); ref != nil && deps.Credentials != nil {
		store := component.CredentialStore{Resolver: deps.Credentials, Ref: *ref}
		s.sessions = &participant.SessionStore{Credentials: store, Merge: false}
		s.saveSessions = store.Writable()
	}
	placement := role.Placement{Workspace: deps.Self.Workspace, Name: deps.Self.Name, UID: string(deps.UID), StateDir: deps.StateDir}
	cfg, err := role.Configure(spec, placement, s.sessionStore(), log)
	if err != nil {
		return nil, err
	}
	s.cfg = cfg
	s.client = egressFor(spec, deps).Client(requestTimeout)
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

	core, err := s.setup(runCtx)
	if err == nil {
		err = s.deps.Bus.RegisterHTTP(component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.deps.Self.Name}, s.handler())
	}
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
	done := s.done
	s.mu.Unlock()
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
	return []component.Endpoint{{Name: "http", Protocol: component.ProtocolHTTP}}
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
