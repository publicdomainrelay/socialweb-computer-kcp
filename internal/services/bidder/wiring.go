package bidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestseed"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

func inputsSubscription(inputs *participant.Inputs) contract.Subscription {
	return contract.Subscription{Kinds: []string{contract.KindExposure, contract.KindMarketIdentity}, Observer: inputs}
}

func roleSubscription(bidder *core.Bidder) contract.Subscription {
	return contract.Subscription{Kinds: []string{contract.KindBid, contract.KindComputeVM, contract.KindStateSync}, Observer: bidder, Synced: bidder.Synced}
}

func (s *Service) resolveIntents(ctx context.Context) (contract.Intents, error) {
	if s.deps.Bus == nil {
		return nil, errors.New("bidder: the bus is not available")
	}
	self := s.self()
	return bus.Resolve[contract.Intents](s.deps.Caller(ctx), s.deps.Bus, self, self, bus.Capability{Name: contract.CapabilityIntents})
}

func (s *Service) awaitInputs(ctx context.Context) error {
	var wg sync.WaitGroup
	ref := s.spec.MarketExposureRef
	exposure := ""
	if ref != nil {
		exposure = strings.TrimSpace(ref.Name)
	}
	if exposure != "" {
		wg.Go(func() { s.marketURL = s.inputs.AwaitAddress(ctx, exposure, inputWait, nil) })
	}
	wg.Go(func() { s.guestURL = s.resolveGuestURL(ctx) })
	wg.Go(func() { s.resolveIdentity(ctx) })
	wg.Wait()
	if exposure != "" && s.marketURL == "" && s.inboundXRPC() {
		return fmt.Errorf("bidder: the market Exposure %s reports no address yet; the offering and the service-auth audience have one source, so the bidder does not advertise another host", exposure)
	}
	return nil
}

func (s *Service) resolveGuestURL(ctx context.Context) string {
	if !s.publicNetwork() {
		return kcp.HandlerURL(s.workspace, guestControlKind, guestControlName)
	}
	raw := s.inputs.AwaitAddress(ctx, guestExposure, guestExposureWait, participant.AbsoluteHTTPURL)
	if raw == "" {
		s.log.Warn("the bidder cannot read the guest control plane ingress URL; guest provisioning fails closed")
		return ""
	}
	s.log.Info("the bidder uses the guest control plane ingress URL", "url", raw)
	return raw
}

func (s *Service) resolveIdentity(ctx context.Context) {
	if s.spec.Identity != nil {
		s.did = s.spec.Identity.Did
	}
	ref := s.identityRef()
	if s.did != "" || ref == nil || ref.Name == "" {
		return
	}
	found, ok := s.inputs.AwaitIdentity(ctx, []string{s.workspace}, ref.Name, inputWait)
	if !ok {
		s.log.Warn("the bidder identity is not resolved yet", "marketIdentity", ref.Name)
		return
	}
	s.did = found.Status.Did
}

func (s *Service) newBidder(intents contract.Intents) (*core.Bidder, error) {
	if s.did == "" {
		return nil, errors.New("bidder: the record store needs the bidder DID")
	}
	records, err := participant.NewRepoStore(participant.RepoConfig{DID: s.did, Directory: s.dir, HTTP: s.egress.Client(RecordRequestTimeout),
		Open: s.openRepo})
	if err != nil {
		return nil, err
	}
	if s.stateDir() == "" {
		return nil, errors.New("bidder: the attestation key needs a state dir")
	}
	signer, err := market.LoadOrCreateSignerFile(s.stateDir() + "/attestation.jwk")
	if err != nil {
		return nil, err
	}
	guestNetwork := s.spec.GuestNetwork.EffectiveFor(s.spec.Policy)
	cfg := core.Config{
		DID:                 s.did,
		Workspace:           s.workspace,
		ClaimedBy:           s.deps.Self.Name,
		ProviderHandle:      s.providerHandle,
		GuestURL:            s.guestURL,
		EndpointURL:         s.inboundMarketURL(),
		InboundXRPC:         s.inboundXRPC(),
		ScopePolicy:         s.spec.Policy,
		GuestTLSPort:        portOf(s.spec.Network.GuestTLSURL()),
		GuestLocalAddress:   func() string { return s.inputs.Address(guestLocalExposure) },
		GuestDownloads:      s.guestDownloads(),
		GuestVaultServer:    s.guestVaultServer,
		GuestVaultMount:     DefaultGuestVaultMount,
		GuestLoginMount:     DefaultGuestLoginMount,
		GuestLANUDP:         guestNetwork.LANUDP,
		GuestHostUDP:        guestNetwork.HostUDP,
		MaxConcurrentGuests: s.spec.MaxConcurrentGuests,
		Heartbeat:           seconds(s.spec.HeartbeatIntervalSeconds),
		OfferingRefresh:     seconds(s.spec.OfferingRefreshSeconds),
		BidTTL:              seconds(s.spec.BidTTLSeconds),
		Suspend:             s.spec.SuspendParticipation,
		Relays:              append([]string(nil), s.spec.Network.Relays()...),
	}
	if s.spec.Kcp != nil {
		cfg.StopGraceSeconds = s.spec.Kcp.StopGraceSeconds
	}
	return core.New(cfg, core.Deps{
		Intents:     intents,
		Records:     records,
		Signer:      signer,
		Vault:       s.guestVault(),
		Guests:      guests{},
		GuestEvents: s.guestEvents(),
		Tokens:      participant.NewTokens(s.minter(), DefaultCallTimeout),
		HTTP:        s.egress.Client(DefaultCallTimeout),
		Directory:   s.dir,
		Policy:      s.policyRuns(),
		Log:         s.log,
	})
}

func (s *Service) guestDownloads() map[string]string {
	dir := ""
	if s.spec.Storage != nil {
		dir = strings.TrimSpace(s.spec.Storage.BlobsDir)
	}
	if dir == "" {
		s.log.Warn("the bidder names no guest downloads dir; a contract with a tunnel fails to provision")
		return nil
	}
	var required []string
	for _, name := range []string{guestseed.GuestBinaryOpenTunnel, guestseed.GuestBinaryAgent} {
		for _, arch := range guestseed.GuestArches {
			required = append(required, guestseed.GuestDownloadName(name, arch))
		}
	}
	pins, err := pinneddownloads.LoadPins(dir, required)
	if err != nil {
		s.log.Warn("the bidder cannot read its guest download pins; a contract with a tunnel fails to provision", "dir", dir, "err", err)
		return nil
	}
	s.log.Info("the bidder pins the guest binaries it serves", "dir", dir, "downloads", len(pins))
	return pins
}

func (s *Service) repoRef() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "AtprotoRepo", Name: atprotorepo.RepoName(s.did), Workspace: s.workspace}
}

func (s *Service) openRepo(ctx context.Context) (atprotorepo.Repo, error) {
	return bus.Resolve[atprotorepo.Repo](ctx, s.deps.Bus, s.self(), s.repoRef(), bus.Capability{Name: atprotorepo.CapabilityWrite})
}

func (s *Service) awaitRepo(ctx context.Context) {
	repo := s.repoRef()
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
		_, err := s.openRepo(ctx)
		if !errors.Is(err, atprotorepo.ErrNotBound) {
			if waited {
				s.log.Info("the bidder repo is bound; the bidder writes its offering", "repo", repo.Name)
			}
			return
		}
		if !waited {
			s.log.Info("the bidder waits for its repo binding before it writes its offering", "repo", repo.Name, "err", err)
			waited = true
		}
		select {
		case <-ctx.Done():
			return
		case <-bound:
		}
	}
}

func seconds(value *int32) time.Duration {
	if value == nil {
		return 0
	}
	return time.Duration(*value) * time.Second
}

func (s *Service) minter() xrpc.Minter {
	target, ok := s.serviceAuthSigner()
	if s.deps.Bus == nil || s.did == "" || !ok {
		return nil
	}
	handlers, self, did := s.deps.Bus, s.self(), s.did
	return participant.PDSServiceAuth{DID: did, Open: func(ctx context.Context) (pds.ServiceAuth, error) {
		return bus.Resolve[pds.ServiceAuth](ctx, handlers, self, target, bus.Capability{Name: pds.CapabilityServiceAuth, Scope: []string{did}})
	}}
}

func (s *Service) serviceAuthSigner() (commonv1.ObjectRef, bool) {
	if ref := s.spec.PdsRef; ref != nil && strings.EqualFold(strings.TrimSpace(ref.Kind), "Pds") && strings.TrimSpace(ref.Name) != "" {
		return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Pds", Name: ref.Name, Workspace: s.workspace}, true
	}
	if s.spec.Identity != nil && s.spec.Identity.MarketIdentityRef != nil && strings.TrimSpace(s.spec.Identity.MarketIdentityRef.Name) != "" {
		ref := s.spec.Identity.MarketIdentityRef
		return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "MarketIdentity", Name: ref.Name, Workspace: cmp.Or(ref.Workspace, s.workspace)}, true
	}
	return commonv1.ObjectRef{}, false
}

type guestVault struct {
	handlers *bus.Bus

	caller func(context.Context) context.Context

	self commonv1.ObjectRef

	target commonv1.ObjectRef

	scope []string
}

var _ vault.KV = guestVault{}

func (s *Service) guestVault() vault.KV {
	if s.deps.Bus == nil {
		return nil
	}
	return guestVault{handlers: s.deps.Bus, caller: s.deps.Caller, self: s.self(),
		target: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: s.guestVaultServer, Workspace: s.workspace},
		scope:  []string{DefaultGuestVaultMount + "/" + v1alpha1.GuestVaultPrefix + "/*"}}
}

func (v guestVault) open(ctx context.Context) (vault.KV, error) {
	return bus.Resolve[vault.KV](v.caller(ctx), v.handlers, v.self, v.target, bus.Capability{Name: vault.CapabilityWrite, Scope: v.scope})
}

func (v guestVault) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	kv, err := v.open(ctx)
	if err != nil {
		return nil, err
	}
	return kv.Read(ctx, mount, path, version)
}

func (v guestVault) Write(ctx context.Context, mount, path string, data map[string]any) error {
	kv, err := v.open(ctx)
	if err != nil {
		return err
	}
	return kv.Write(ctx, mount, path, data)
}

func (v guestVault) Delete(ctx context.Context, mount, path string) error {
	kv, err := v.open(ctx)
	if err != nil {
		return err
	}
	return kv.Delete(ctx, mount, path)
}

func (s *Service) resolvePolicyRunner(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
	return bus.Resolve[policy.Runner](ctx, s.deps.Bus, s.self(), engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
}

func (s *Service) policyRuns() *runs.Client {
	engine := runs.EngineRef(s.spec.PolicyEngineRef, s.workspace)
	if s.deps.Bus == nil || engine.Name == "" {
		return nil
	}
	return participant.NewPolicyRuns(participant.PolicyConfig{
		Resolve: s.resolvePolicyRunner,
		Owner:   policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: Kind, Name: s.deps.Self.Name, UID: string(s.deps.UID)},
		Engine:  engine,
		Role:    core.PerspectiveBidder,
		Prefix:  "policy",
		Args:    s.policyArgs,
		Timeout: seconds(s.spec.PolicyTimeoutSeconds),
		Parser:  runs.Lenient,
	})
}

func (s *Service) newAuthorizer() *participant.Authorizer {
	if s.serviceAuthHost == "" {
		s.serviceAuthHost = participant.HostPort(s.publicMarketURL())
	}
	var audiences []string
	if s.spec.ServiceAuth != nil {
		audiences = append(audiences, s.spec.ServiceAuth.Audience...)
	}
	audiences = append(audiences, market.ServiceAudienceDIDs(s.serviceAuthHost)...)
	return &participant.Authorizer{
		Directory:    s.dir,
		Hostname:     s.serviceAuthHost,
		ServiceIDs:   []string{market.DefaultMarketServiceID, market.DefaultComputeEventServiceID},
		AudienceDIDs: audiences,
	}
}

func (s *Service) crawl() participant.Crawl {
	return participant.Crawl{
		Relays:  append([]string(nil), s.spec.Network.Relays()...),
		Address: func() string { return s.inputs.Address(pdsExposure) },
		Crawled: func(ctx context.Context) {
			if bidder := s.bidder.Load(); bidder != nil {
				bidder.TouchOffering(ctx)
			}
		},
		Period: participant.CrawlPeriod,
		Log:    s.log,
	}
}
