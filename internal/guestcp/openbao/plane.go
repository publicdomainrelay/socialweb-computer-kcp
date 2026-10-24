package openbao

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultVaultClient = "guest-kcp"

	DefaultTimeout = 10 * time.Second

	DefaultIssuerRetry = 30 * time.Second

	hostKeyProbe = time.Minute
)

type Config struct {
	Handle string

	DID string

	Mount string

	VaultClient string

	Timeout time.Duration

	GrantSync time.Duration

	IssuerRetry time.Duration

	HostKeyAttempts int
}

type Deps struct {
	Inputs guestcp.Source

	Guests guestcp.Guests

	Grants guestcp.Grants

	RBAC atprotorepo.RecordReader

	FallbackIssuer func() string

	Publish guestcp.Publish

	Clock host.Clock

	Log *slog.Logger
}

type Plane struct {
	cfg Config

	deps Deps

	log *slog.Logger

	clock host.Clock

	mu sync.RWMutex

	inputs guestcp.Inputs

	probing map[string]bool

	retrying bool

	kick chan struct{}

	issuerKick chan struct{}

	identity *guestidentity.Client

	verifier *guestidentity.Verifier

	guest *guestidentity.Guest

	syncer *guestidentity.Syncer

	known map[string]string

	cancel context.CancelFunc

	unwatch func()

	wg sync.WaitGroup

	once sync.Once
}

var _ guestcp.Plane = (*Plane)(nil)

func New(cfg Config, deps Deps) (*Plane, error) {
	switch {
	case strings.TrimSpace(cfg.Handle) == "":
		return nil, errors.New("guestcp: the guest control plane names no bidder handle")
	case deps.Inputs == nil:
		return nil, errors.New("guestcp: the guest control plane has no input source")
	case deps.Guests == nil:
		return nil, errors.New("guestcp: the guest control plane has no guest report writer")
	case deps.Grants == nil:
		return nil, errors.New("guestcp: the guest control plane has no grant writer")
	case deps.RBAC == nil:
		return nil, errors.New("guestcp: the guest control plane has no RBAC record reader")
	}
	cfg.VaultClient = cmp.Or(strings.TrimSpace(cfg.VaultClient), DefaultVaultClient)
	cfg.Timeout = opt.Positive(cfg.Timeout, DefaultTimeout)
	cfg.IssuerRetry = opt.Positive(cfg.IssuerRetry, DefaultIssuerRetry)
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Plane{
		cfg: cfg, deps: deps, log: log, clock: clock,
		probing: map[string]bool{}, known: map[string]string{},
		kick: make(chan struct{}, 1), issuerKick: make(chan struct{}, 1),
	}, nil
}

func (p *Plane) Start(ctx context.Context) error {
	initial, err := p.deps.Inputs.Inputs(ctx)
	if err != nil {
		return fmt.Errorf("guestcp: reading the guest control plane inputs: %w", err)
	}
	p.update(initial)
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	identity, err := guestidentity.NewClient(guestidentity.Options{Mount: p.cfg.Mount, Endpoint: p.endpoint, Timeout: p.cfg.Timeout})
	if err != nil {
		p.Close()
		return err
	}
	p.identity = identity
	issuer := func() string { return guestidentity.OIDCIssuer(p.IssuerURL()) }
	p.verifier = guestidentity.NewVerifier(guestidentity.IdentityKeys(identity), issuer)
	syncer, err := guestidentity.NewSyncer(runCtx, guestidentity.SyncerOptions{
		Store:  grantStore{p: p},
		RBAC:   &guestidentity.RepoRBAC{Reader: p.deps.RBAC, Log: p.log},
		Issuer: issuer,
		Log:    p.log,
		Resync: p.cfg.GrantSync,
	})
	if err != nil {
		p.Close()
		return err
	}
	p.syncer = syncer
	guest, err := guestidentity.NewGuest(runCtx, guestidentity.GuestOptions{
		Handle:          p.cfg.Handle,
		Identity:        identity,
		Verifier:        p.verifier,
		VMs:             vmStore{p: p},
		Log:             p.log,
		Now:             p.clock.Now,
		HostKeyAttempts: p.cfg.HostKeyAttempts,
		Publish:         p.publish,
	})
	if err != nil {
		p.Close()
		return err
	}
	p.guest = guest
	p.unwatch = p.deps.Inputs.Watch(p.update)
	p.wg.Go(func() { p.reconcileLoop(runCtx) })
	p.wg.Go(func() { p.issuerLoop(runCtx) })
	return nil
}

func (p *Plane) Close() {
	p.once.Do(func() {
		if p.unwatch != nil {
			p.unwatch()
		}
		if p.cancel != nil {
			p.cancel()
		}
		if p.guest != nil {
			p.guest.Close()
		}
		if p.syncer != nil {
			p.syncer.Close()
		}
		p.wg.Wait()
	})
}

func (p *Plane) Ready() error {
	if p == nil || p.guest == nil {
		return errors.New("guestcp: the guest identity proxy is not configured")
	}
	return nil
}

func (p *Plane) DID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cmp.Or(strings.TrimSpace(p.cfg.DID), strings.TrimSpace(p.inputs.DID))
}

func (p *Plane) IssuerURL() string {
	p.mu.RLock()
	raw := exposureIssuer(p.inputs.Issuer)
	p.mu.RUnlock()
	if raw == "" && p.deps.FallbackIssuer != nil {
		raw = strings.TrimSpace(p.deps.FallbackIssuer())
	}
	return strings.TrimRight(raw, "/")
}

func (p *Plane) OIDCIssuer() string {
	raw := p.IssuerURL()
	if raw == "" {
		return ""
	}
	return guestidentity.OIDCIssuer(raw)
}

func (p *Plane) Recheck() {
	nudge(p.issuerKick)
}

func (p *Plane) LoginStep(ctx context.Context, step string, input map[string]any) guestcp.Reply {
	return guestcp.Reply(p.guest.LoginStep(ctx, step, input))
}

func (p *Plane) OIDCToken(ctx context.Context, role, vaultToken string) guestcp.Reply {
	return guestcp.Reply(p.guest.OIDCToken(ctx, role, vaultToken))
}

func (p *Plane) Discovery(ctx context.Context) guestcp.Reply {
	return guestcp.Reply(p.guest.Discovery(ctx))
}

func (p *Plane) Keys(ctx context.Context) guestcp.Reply {
	return guestcp.Reply(p.guest.Keys(ctx))
}

func (p *Plane) Events(ctx context.Context, authorization string, request guestcp.EventsRequest) guestcp.Reply {
	reply := p.guest.Events(ctx, authorization, guestidentity.EventsRequest(request))
	return guestcp.Reply(p.withGuestLogin(reply))
}

func (p *Plane) publish(ctx context.Context, event guestidentity.GuestEvent) error {
	if p.deps.Publish == nil {
		return guestcp.ErrNoPublisher
	}
	record, err := json.Marshal(event.Record)
	if err != nil {
		return err
	}
	return p.deps.Publish(ctx, guestcp.GuestEvent{ComputeVM: event.ComputeVM, Collection: event.Collection, Tag: event.Tag, Kind: event.Kind, Record: record})
}

func (p *Plane) Authenticate(ctx context.Context, authorization string) (string, error) {
	name, err := p.guest.Authenticate(ctx, authorization)
	switch {
	case errors.Is(err, guestidentity.ErrUnauthenticated):
		return "", fmt.Errorf("%w: %v", guestcp.ErrUnauthenticated, err)
	case errors.Is(err, guestidentity.ErrUnknownGuest):
		return "", fmt.Errorf("%w: %v", guestcp.ErrUnknownGuest, err)
	}
	return name, err
}

func (p *Plane) update(in guestcp.Inputs) {
	p.mu.Lock()
	p.inputs = in
	p.mu.Unlock()
	nudge(p.kick)
	nudge(p.issuerKick)
}

func (p *Plane) current() guestcp.Inputs {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inputs
}

func (p *Plane) vm(name string) *v1alpha1.ComputeVM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.inputs.ComputeVMs {
		if p.inputs.ComputeVMs[i].Name == name {
			return p.inputs.ComputeVMs[i].DeepCopy()
		}
	}
	return nil
}

func (p *Plane) grants() []v1alpha1.GuestIdentityGrant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]v1alpha1.GuestIdentityGrant, 0, len(p.inputs.Grants))
	for i := range p.inputs.Grants {
		out = append(out, *p.inputs.Grants[i].DeepCopy())
	}
	return out
}

func (p *Plane) endpoint(context.Context) (guestidentity.Endpoint, error) {
	p.mu.RLock()
	status := p.inputs.OpenBao
	p.mu.RUnlock()
	if status == nil {
		return guestidentity.Endpoint{}, errors.New("guestcp: the OpenBaoServer reports no status yet")
	}
	return guestidentity.EndpointFromServer(*status, p.cfg.VaultClient)
}

func exposureIssuer(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return raw
}

func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
