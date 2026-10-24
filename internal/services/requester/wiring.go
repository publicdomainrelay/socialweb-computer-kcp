package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	role "github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
)

func egressFor(spec v1alpha1.RequesterSpec, deps Deps, trust *component.Trust) component.Egress {
	egress := deps.Egress
	egress.Log = cmp.Or(egress.Log, deps.Log)
	if trust.Enabled() {
		egress.TLS = trust.Client(nil)
	}
	if spec.Network == nil {
		return egress
	}
	if hosts := participant.NetworkHosts(*spec.Network, egress.Hosts); hosts != nil {
		egress.Hosts = hosts
	}
	return egress
}

func directoryFor(raw string, keys int, ttl time.Duration) identity.Directory {
	base := identity.DefaultDirectory()
	raw = strings.TrimRight(raw, "/")
	if raw == "" || raw == strings.TrimRight(identity.DefaultPLCURL, "/") {
		return base
	}
	cached, ok := base.(*identity.CacheDirectory)
	if !ok {
		return base
	}
	inner := cached.Inner
	if directory, ok := inner.(*identity.BaseDirectory); ok {
		inner = &identity.BaseDirectory{
			PLCURL:                 raw,
			PLCLimiter:             directory.PLCLimiter,
			DIDWebLimitFunc:        directory.DIDWebLimitFunc,
			HTTPClient:             directory.HTTPClient,
			PLCClient:              directory.PLCClient,
			TryAuthoritativeDNS:    directory.TryAuthoritativeDNS,
			SkipDNSDomainSuffixes:  append([]string(nil), directory.SkipDNSDomainSuffixes...),
			FallbackDNSServers:     append([]string(nil), directory.FallbackDNSServers...),
			SkipHandleVerification: directory.SkipHandleVerification,
			UserAgent:              directory.UserAgent,
		}
	}
	return identity.NewCacheDirectory(inner, keys, ttl, ttl, ttl)
}

func (s *Service) self() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.cfg.Workspace}
}

func (s *Service) setup(ctx context.Context) (*role.Requester, error) {
	if s.deps.Bus == nil {
		return nil, fmt.Errorf("requester: the bus is not available")
	}
	self := s.self()
	intents, err := bus.Resolve[contractmarket.Intents](ctx, s.deps.Bus, self, self, bus.Capability{Name: contractmarket.CapabilityIntents})
	if err != nil {
		return nil, fmt.Errorf("requester: the market intents handle: %w", err)
	}
	inputs := participant.NewInputs()
	if _, err := intents.Subscribe(ctx, contractmarket.Subscription{
		Kinds: []string{contractmarket.KindExposure, contractmarket.KindMarketIdentity}, Observer: inputs}); err != nil {
		return nil, fmt.Errorf("requester: subscribing to the exposures: %w", err)
	}
	if extra := role.IdentityWorkspaces(s.cfg.Workspace)[1:]; len(extra) > 0 && s.identityRef() {
		if _, err := intents.Subscribe(ctx, contractmarket.Subscription{
			Kinds: []string{contractmarket.KindMarketIdentity}, Workspaces: extra, Observer: inputs}); err != nil {
			return nil, fmt.Errorf("requester: subscribing to the market identity: %w", err)
		}
	}
	handlers := s.deps.Bus
	runner := func(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
		return bus.Resolve[policy.Runner](ctx, handlers, self, engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
	}
	core, err := role.New(s.cfg, role.Deps{
		Intents:     intents,
		Inputs:      inputs,
		OpenRepo:    s.openRepo,
		AwaitRepo:   s.awaitRepo,
		Policy:      runner,
		Vault:       s.secretsVault,
		ServiceAuth: s.openServiceAuth,
		Guests:      guests{},
		Directory:   s.directory,
		HTTP:        s.client,
		Now:         time.Now,
		Log:         s.deps.Log,
	})
	if err != nil {
		return nil, err
	}
	if err := core.Start(ctx); err != nil {
		return core, err
	}
	s.mu.Lock()
	s.core = core
	s.mu.Unlock()
	if _, err := intents.Subscribe(ctx, contractmarket.Subscription{
		Kinds: []string{contractmarket.KindComputeContract, contractmarket.KindStateSync}, Observer: core}); err != nil {
		return core, fmt.Errorf("requester: subscribing to the contracts: %w", err)
	}
	if relays := s.cfg.Relays(); len(relays) > 0 {
		crawl := participant.Crawl{Relays: relays, Address: func() string { return inputs.Address(role.PDSExposure) }, Period: participant.CrawlPeriod, Log: s.log}
		s.wg.Go(func() { crawl.Run(ctx) })
	}
	return core, nil
}

func (s *Service) identityRef() bool {
	identity := s.spec.Identity
	return identity != nil && identity.MarketIdentityRef != nil && identity.MarketIdentityRef.Name != ""
}

func (s *Service) repoRef(did string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "AtprotoRepo", Name: atprotorepo.RepoName(did), Workspace: s.cfg.Workspace}
}

func (s *Service) openRepo(ctx context.Context, did string) (atprotorepo.Repo, error) {
	return bus.Resolve[atprotorepo.Repo](ctx, s.deps.Bus, s.self(), s.repoRef(did), bus.Capability{Name: atprotorepo.CapabilityWrite})
}

func (s *Service) awaitRepo(ctx context.Context, did string) {
	repo := s.repoRef(did)
	bound := make(chan struct{}, 1)
	cancel := s.deps.Bus.Subscribe(func(key bus.Key) {
		if key.Workspace != repo.Workspace || key.Name != repo.Name {
			return
		}
		select {
		case bound <- struct{}{}:
		default:
		}
	})
	defer cancel()
	waited := false
	for {
		_, err := s.openRepo(ctx, did)
		if !errors.Is(err, atprotorepo.ErrNotBound) && !errors.Is(err, pds.ErrNotFound) {
			if waited {
				s.log.Info("the requester repo is bound; the requester runs its contracts and releases", "repo", repo.Name)
			}
			return
		}
		if !waited {
			s.log.Info("the requester waits for its repo binding before it runs contracts or releases", "repo", repo.Name, "err", err.Error())
			waited = true
		}
		select {
		case <-ctx.Done():
			return
		case <-bound:
		}
	}
}

func (s *Service) openServiceAuth(ctx context.Context, identity commonv1.ObjectRef, did string) (pds.ServiceAuth, error) {
	identity.Group, identity.Kind = v1alpha1.GroupName, "MarketIdentity"
	return bus.Resolve[pds.ServiceAuth](ctx, s.deps.Bus, s.self(), identity, bus.Capability{Name: pds.CapabilityServiceAuth, Scope: []string{did}})
}

func (s *Service) secretsVault(ctx context.Context, ref commonv1.SecretRef) (vault.KV, error) {
	vaultRef := s.spec.VaultRef
	if vaultRef == nil || vaultRef.Name == "" {
		return nil, fmt.Errorf("requester: the contract names a secrets bundle and the component names no OpenBaoServer")
	}
	target := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: vaultRef.Name, Workspace: cmp.Or(vaultRef.Workspace, s.deps.Self.Workspace)}
	kv, err := bus.Resolve[vault.KV](s.deps.Caller(ctx), s.deps.Bus, s.deps.Self, target, bus.Capability{Name: vault.CapabilityRead, Scope: []string{ref.Mount + "/" + ref.Path}})
	if err != nil {
		return nil, fmt.Errorf("requester: the vault client: %w", err)
	}
	return kv, nil
}

type guests struct{}

var _ role.Guests = guests{}

func (g guests) Subject(bidderDID, requesterDID, roleName string) (string, error) {
	return guestidentity.SubjectFor(bidderDID, requesterDID, roleName)
}

func (g guests) Verifier(issuer string, keys func(ctx context.Context) ([]byte, error)) role.TokenVerifier {
	return guestidentity.NewVerifier(keys, func() string { return issuer })
}

func (g guests) CheckHostKeys(keys []v1alpha1.ComputeVMHostKey) ([]v1alpha1.ComputeVMHostKey, error) {
	return computevm.CheckHostKeys(keys)
}
