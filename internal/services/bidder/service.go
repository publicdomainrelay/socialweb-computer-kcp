package bidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
)

const (
	ServiceName = "Bidder"

	HealthPath = "/healthz"

	OAuthMetadataPath = "/oauth-client-metadata.json"

	DefaultCallTimeout = 20 * time.Second

	SubmitEventLookupTimeout = 3 * time.Second

	guestExposurePoll = 2 * time.Second

	guestExposureWait = 60 * time.Second
)

type Authorizer interface {
	Authorize(ctx context.Context, header, lxm, recordURI string) (string, error)
}

type ServiceAuthAuthorizer struct {
	Dir identity.Directory

	Hostname string

	ServiceIDs []string

	AudienceDIDs []string
}

func (a *ServiceAuthAuthorizer) Authorize(ctx context.Context, header, lxm, recordURI string) (string, error) {
	if a == nil || a.Dir == nil {
		return "", errors.New("bidder: the service has no identity directory")
	}
	if a.Hostname == "" {
		return "", errors.New("bidder: the service has no service hostname")
	}
	result, err := market.VerifyServiceAuth(ctx, a.Dir, market.VerifyServiceAuthOptions{
		AuthHeader:        header,
		Hostname:          a.Hostname,
		LXM:               lxm,
		ServiceIDs:        a.ServiceIDs,
		ExtraAudienceDIDs: a.AudienceDIDs,
	})
	if err != nil {
		return "", err
	}
	if err := market.AuthorizeRecord(result.IssuerDID, recordURI); err != nil {
		return "", err
	}
	return result.IssuerDID, nil
}

type Service struct {
	spec v1alpha1.BidderSpec

	deps Deps

	egress component.Egress

	log *slog.Logger

	workspace string

	handle string

	providerHandle string

	guestVaultServer string

	guestTLSPort int

	tokenRef *component.CredentialRef

	policyArgs map[string]string

	policyTimeout time.Duration

	heartbeat time.Duration

	offeringRefresh time.Duration

	bidTTL time.Duration

	pdsURL string

	pdsTokens pdsclient.TokenSource

	pdsDPoP pdsclient.DPoPSource

	marketURL string

	serviceAuthHost string

	mu sync.Mutex

	started bool

	registered bool

	pdsInternal bool

	mux *http.ServeMux

	runCtx context.Context

	cancel context.CancelFunc

	wg sync.WaitGroup

	stopOnce sync.Once

	stopErr error

	cleanupMu sync.Mutex

	endpoints []component.Endpoint

	store *kcpstore.Store

	cluster string

	did string

	records *market.RepoStore

	bids BidStore

	vms VMStore

	grants GrantStore

	vault GuestVault

	policy *PolicyEvaluator

	provider *ComputeProvider

	authorizer Authorizer

	dir identity.Directory

	attestation *market.Signer

	relay *ingress.Subscriber

	guestURL string

	loop *marketLoop

	syncController *statesync.Controller

	syncKick chan struct{}

	bidKick chan struct{}

	contracts sync.Map

	releaseApplied sync.Map

	verifiedBids sync.Map

	acceptMu sync.Mutex

	acceptLocks map[string]*sync.Mutex

	serviceAuthOnce sync.Once

	serviceAuth *ttlcache.Cache[string, string]
}

func (s *Service) lockAccept(uri string) func() {
	s.acceptMu.Lock()
	if s.acceptLocks == nil {
		s.acceptLocks = map[string]*sync.Mutex{}
	}
	lock := s.acceptLocks[uri]
	if lock == nil {
		lock = &sync.Mutex{}
		s.acceptLocks[uri] = lock
	}
	s.acceptMu.Unlock()
	lock.Lock()
	return lock.Unlock
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
	s.runCtx = runCtx
	s.cancel = cancel
	s.mu.Unlock()
	defer s.shutdown(ctx)

	if err := s.prepare(runCtx); err != nil {
		cancel()
		return err
	}
	s.startLoops(runCtx)
	<-runCtx.Done()
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	registered := s.registered
	s.mu.Unlock()
	if !started || !registered {
		return errors.New("bidder: the service is not registered")
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]component.Endpoint(nil), s.endpoints...)
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
		s.started = false
		s.registered = false
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if relay != nil {
			relay.Close()
		}
		if err := supervise.Join(ctx, s.wg.Wait); err != nil && s.stopErr == nil {
			s.stopErr = err
		}
	})
	return s.stopErr
}

func (s *Service) prepare(ctx context.Context) error {
	if s.stateDir() != "" {
		if err := os.MkdirAll(s.stateDir(), 0o700); err != nil {
			return fmt.Errorf("bidder: creating the state dir: %w", err)
		}
	}
	if err := s.resolveKCP(ctx); err != nil {
		s.log.Warn("the bidder runs without its kcp stores", "err", err)
	}
	s.dir = ingress.NewDirectory(s.network().PLCDirectoryURL)
	if err := s.resolveMarketURL(ctx); err != nil {
		s.log.Warn("the bidder market URL is not resolved yet", "err", err)
	}
	if err := s.checkServiceAuthHost(); err != nil {
		return err
	}
	if err := s.resolveIdentity(ctx); err != nil {
		s.log.Warn("the bidder identity is not resolved yet", "err", err)
	}
	if err := s.resolveRecords(ctx); err != nil {
		s.log.Warn("the bidder runs without a market record store", "err", err)
	}
	if err := s.resolveStores(); err != nil {
		s.log.Warn("the bidder runs without some kcp stores", "err", err)
	}
	if err := s.resolveVault(ctx); err != nil {
		s.log.Warn("the bidder runs without a guest vault", "err", err)
	}
	s.guestURL = s.resolveGuestURL(ctx)
	if s.did != "" && s.records != nil && s.attestation == nil && s.stateDir() != "" {
		signer, err := market.LoadOrCreateSignerFile(s.stateDir() + "/attestation.jwk")
		if err != nil {
			s.log.Warn("loading the bidder attestation key failed", "err", err)
		} else {
			s.attestation = signer
		}
	}
	if s.records != nil && s.attestation != nil && s.did != "" {
		if err := s.startStateSync(); err != nil {
			s.log.Warn("the bidder does not run the StateSync controller", "err", err)
		}
	}
	s.provider = &ComputeProvider{
		Config: ProviderConfig{
			Handle:              s.providerHandle,
			GuestTLSPort:        s.guestTLSPort,
			GuestVaultServer:    s.guestVaultServer,
			StopGraceSeconds:    s.stopGraceSeconds(),
			MaxConcurrentGuests: s.spec.MaxConcurrentGuests,
		},
		VMs:            s.vms,
		Grants:         s.grants,
		Vault:          s.vault,
		DID:            s.did,
		IssuerURL:      s.guestURL,
		Log:            s.log,
		Now:            time.Now,
		WaitForRunning: false,
	}
	s.policy = &PolicyEvaluator{Runs: s.policyRuns(), ScopePolicy: s.spec.Policy, Records: s.records}
	s.mux = s.routes()
	if err := s.register(ctx); err != nil {
		return err
	}
	s.addEndpoint(component.Endpoint{Name: "http", Protocol: component.ProtocolHTTP})
	s.addEndpoint(component.Endpoint{Name: "market", Protocol: component.ProtocolHTTP})
	if s.guestURL != "" {
		s.addEndpoint(component.Endpoint{Name: "guest", Protocol: component.ProtocolHTTPS, URL: s.guestURL})
	}
	if err := s.startRelay(ctx); err != nil {
		s.log.Warn("the bidder market relay did not start", "err", err)
	}
	s.authorizer = s.newAuthorizer()
	s.loop = &marketLoop{
		records:                 s.records,
		signer:                  s.attestation,
		did:                     s.did,
		endpointURL:             s.offeringEndpoint(),
		relays:                  append([]string(nil), s.spec.Network.Relays()...),
		offeringRefresh:         s.offeringRefresh,
		offeringRefreshDisabled: s.offeringRefresh <= 0,
		heartbeat:               s.heartbeat,
		log:                     s.log,
		now:                     time.Now,
	}
	s.log.Info("the bidder serves the market", "handler", s.listeningURL(), "did", s.did, "guest", s.guestURL,
		"offering", s.offeringEndpoint(), "serviceAuthHost", s.serviceAuthHost,
		"identityMode", strings.TrimSpace(s.spec.Mode), "inboundXRPC", s.inboundXRPC())
	s.wg.Go(func() { s.registerPDSWithRelays(ctx) })
	return nil
}

var exposureGVR = exposurev1alpha1.ExposureGVR

func (s *Service) registerPDSWithRelays(ctx context.Context) {
	if s.deps.KCP == nil || len(s.spec.Network.Relays()) == 0 {
		s.log.Warn("the bidder cannot register its PDS with the relay", "kcp", s.deps.KCP != nil, "relays", len(s.spec.Network.Relays()))
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		s.registerPDSWithRelay(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) registerPDSWithRelay(ctx context.Context) {
	client, err := s.deps.KCP.Client(s.workspace)
	if err != nil {
		s.log.Warn("the bidder cannot read its kcp client", "workspace", s.workspace, "err", err.Error())
		return
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.workspace
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		s.log.Warn("the bidder cannot build its dynamic client", "err", err.Error())
		return
	}
	u, err := dyn.Resource(exposureGVR).Get(ctx, "svc-pds-pds-local", metav1.GetOptions{})
	if err != nil {
		s.log.Warn("the bidder cannot read its PDS Exposure", "err", err.Error())
		return
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "address")
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		s.log.Warn("the bidder PDS Exposure has no URL", "url", raw)
		return
	}
	registered := false
	for _, relayURL := range s.spec.Network.Relays() {
		if xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relayURL, HTTP: &http.Client{Timeout: 10 * time.Second}}, parsed.Host) == nil {
			registered = true
		}
	}
	if registered && s.loop != nil {
		if err := s.loop.touchOffering(ctx); err != nil {
			s.log.Warn("republishing the offering after the relay crawl failed", "err", err.Error())
		}
	}
}

func (s *Service) register(ctx context.Context) error {
	if s.deps.Bus == nil {
		return errors.New("bidder: the handler registry is not available")
	}
	key := component.Key{Workspace: s.workspace, Kind: Kind, Name: s.deps.Self.Name}
	if err := s.deps.Bus.RegisterHTTP(key, s.mux); err != nil {
		return err
	}
	if err := s.deps.Bus.RegisterHTTP(key, s.mux, "market"); err != nil {
		return err
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	return nil
}

func (s *Service) resolveKCP(ctx context.Context) error {
	if s.deps.KCP == nil {
		return nil
	}
	store, cluster, err := kcpStore(s.deps.KCP, s.workspace)
	if err != nil {
		return err
	}
	s.store = store
	s.cluster = cluster
	return nil
}

func (s *Service) resolveIdentity(ctx context.Context) error {
	if s.spec.Identity != nil {
		s.did = s.spec.Identity.Did
	}
	if s.pdsURL == "" && s.spec.PdsRef != nil {
		kind := s.spec.PdsRef.Kind
		kind = cmp.Or(kind, "Pds")
		s.pdsURL = kcp.HandlerURL(s.workspace, kind, s.spec.PdsRef.Name)
		s.pdsInternal = s.pdsURL != ""
	}
	identityRef := s.identityRef()
	if s.store == nil || identityRef == nil || identityRef.Name == "" {
		return nil
	}
	identities := kcpstore.OfClusterScoped[corev1alpha1.MarketIdentity](s.store, MarketIdentityGVR)
	obj, err := identities.Get(ctx, ref.New(s.cluster, "", identityRef.Name))
	if err != nil {
		return err
	}
	s.did = cmp.Or(s.did, obj.Status.Did)
	s.pdsURL = cmp.Or(s.pdsURL, obj.Status.PdsURL)
	return nil
}

func (s *Service) resolveRecords(ctx context.Context) error {
	if s.pdsTokens == nil {
		tokens := s.credentialTokenSource()
		if tokens == nil {
			token, err := s.pdsAccessToken(ctx)
			if err != nil {
				return err
			}
			if token != "" {
				tokens = pdsclient.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
			}
		}
		s.pdsTokens = tokens
	}
	if s.did == "" {
		return errors.New("bidder: the record store needs the bidder DID")
	}
	if s.deps.Bus == nil {
		return errors.New("bidder: the record store needs the bus")
	}
	reader := market.NewHTTPReader(s.dir)
	reader.Client = s.egress.Client(RecordRequestTimeout)
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace}
	repo := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "AtprotoRepo", Name: atprotorepo.RepoName(s.did), Workspace: s.workspace}
	handlers := s.deps.Bus
	s.records = market.NewRepoStore(market.RepoStoreConfig{DID: s.did, Foreign: reader,
		Open: func(ctx context.Context) (atprotorepo.Repo, error) {
			return bus.Resolve[atprotorepo.Repo](ctx, handlers, self, repo, bus.Capability{Name: atprotorepo.CapabilityWrite})
		}})
	return nil
}

func (s *Service) credentialTokenSource() pdsclient.TokenSource {
	if s.tokenRef == nil || s.deps.Credentials == nil || strings.TrimSpace(s.pdsURL) == "" {
		return nil
	}
	client := s.egress.Client(DefaultCallTimeout)
	if s.pdsInternal && s.deps.Bus != nil {
		client = s.deps.Bus.HTTPClient()
	}
	source := &credentialPDSTokens{
		resolver: s.deps.Credentials,
		ref:      *s.tokenRef,
		pdsURL:   s.pdsURL,
		client:   client,
	}
	if writer, ok := s.deps.Credentials.(component.CredentialWriter); ok {
		source.writer = writer
	}
	return source
}

func (s *Service) pdsAccessToken(ctx context.Context) (string, error) {
	token := ""
	if s.tokenRef != nil && s.deps.Credentials != nil {
		credential, err := s.deps.Credentials.Resolve(ctx, *s.tokenRef)
		if err != nil {
			return "", err
		}
		for _, key := range []string{"accessJwt", "accessToken", "access_token", "token", "value"} {
			if value, ok := credential.Value(key); ok && value != "" {
				token = value
				break
			}
		}
	}
	return token, nil
}

func (s *Service) resolveStores() error {
	if s.store != nil {
		s.bids = NewKCPBids(s.store, s.cluster)
		s.vms = NewKCPVMs(s.store, s.cluster, s.providerHandle)
		s.grants = NewKCPGrants(s.store, s.cluster)
	}
	return nil
}

func (s *Service) resolveVault(ctx context.Context) error {
	if s.store == nil {
		return errors.New("bidder: no guest vault is available")
	}
	vault, err := kcpOpenBaoVault(s.store, s.cluster, s.guestVaultServer, vaultClientName)
	if err != nil {
		return err
	}
	s.vault = vault
	return nil
}

func (s *Service) resolveGuestURL(ctx context.Context) string {
	if s.publicNetwork() {
		deadline := time.Now().Add(guestExposureWait)
		for {
			if raw := s.guestExposureURL(ctx); raw != "" {
				s.log.Info("the bidder uses the guest control plane ingress URL", "url", raw)
				return raw
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				s.log.Warn("the bidder cannot read the guest control plane ingress URL; guest provisioning fails closed")
				return ""
			}
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(guestExposurePoll):
			}
		}
	}
	return kcp.HandlerURL(s.workspace, guestControlKind, guestControlName)
}

func (s *Service) publicNetwork() bool {
	return strings.TrimSpace(s.network().IngressProxyHost) != "" ||
		strings.TrimSpace(s.spec.Network.GuestTLSURL()) != ""
}

func (s *Service) resolveMarketURL(ctx context.Context) error {
	ref := s.spec.MarketExposureRef
	if s.marketURL != "" || ref == nil || strings.TrimSpace(ref.Name) == "" {
		return nil
	}
	client, err := s.exposureClient()
	if err != nil {
		return err
	}
	u, err := client.Resource(exposureGVR).Get(ctx, strings.TrimSpace(ref.Name), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("bidder: reading the market Exposure %s: %w", ref.Name, err)
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "address")
	s.marketURL = strings.TrimRight(strings.TrimSpace(raw), "/")
	return nil
}

func (s *Service) exposureClient() (dynamic.Interface, error) {
	if s.deps.KCP == nil {
		return nil, errors.New("bidder: the component has no kcp clients")
	}
	client, err := s.deps.KCP.Client(s.workspace)
	if err != nil {
		return nil, err
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.workspace
	return dynamic.NewForConfig(restCfg)
}

func (s *Service) guestExposureURL(ctx context.Context) string {
	name := "svc-" + strings.ToLower(guestControlKind) + "-" + guestControlName + "-ingress"
	client, err := s.exposureClient()
	if err != nil {
		return ""
	}
	u, err := client.Resource(exposureGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "address")
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if !isAbsoluteURL(raw) {
		return ""
	}
	return raw
}

func (s *Service) resolvePolicyRunner(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
	caller := commonv1.ObjectRef{Group: corev1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace}
	return bus.Resolve[policy.Runner](ctx, s.deps.Bus, caller, engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
}

func (s *Service) policyRuns() *runs.Client {
	engine := runs.EngineRef(s.spec.PolicyEngineRef, s.workspace)
	if s.deps.Bus == nil || engine.Name == "" {
		return nil
	}
	return &runs.Client{
		Resolve: s.resolvePolicyRunner,
		Owner:   policy.Owner{APIVersion: corev1alpha1.GroupVersion.String(), Kind: Kind, Name: s.deps.Self.Name, UID: string(s.deps.UID)},
		Engine:  engine,
		Role:    "bidder",
		Prefix:  "policy",
		Args:    s.policyArgs,
		Timeout: s.policyTimeout,
		Parser:  runs.Lenient,
		Now:     time.Now,
	}
}

func (s *Service) newAuthorizer() Authorizer {
	if s.serviceAuthHost == "" {
		s.serviceAuthHost = hostPortOf(s.publicMarketURL())
	}
	var audienceDIDs []string
	if s.spec.ServiceAuth != nil {
		audienceDIDs = append(audienceDIDs, s.spec.ServiceAuth.Audience...)
	}
	audienceDIDs = append(audienceDIDs, market.ServiceAudienceDIDs(s.serviceAuthHost)...)
	return &ServiceAuthAuthorizer{
		Dir:          s.dir,
		Hostname:     s.serviceAuthHost,
		ServiceIDs:   []string{market.DefaultMarketServiceID, market.DefaultComputeEventServiceID},
		AudienceDIDs: audienceDIDs,
	}
}

func (s *Service) addEndpoint(endpoint component.Endpoint) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, endpoint)
	s.mu.Unlock()
}

func (s *Service) listeningURL() string {
	return kcp.HandlerURL(s.workspace, Kind, s.deps.Self.Name)
}

func (s *Service) publicMarketURL() string {
	if url := strings.TrimSpace(s.marketURL); url != "" {
		return strings.TrimRight(url, "/")
	}
	if endpoint, ok := s.relayEndpoint(); ok {
		return strings.TrimRight(endpoint.URL, "/")
	}
	if s.guestURL != "" {
		return strings.TrimRight(s.guestURL, "/")
	}
	return ""
}

func (s *Service) publicURL() string {
	return s.inboundMarketURL()
}

func (s *Service) inboundMarketURL() string {
	if !s.inboundXRPC() {
		return ""
	}
	return s.publicMarketURL()
}

func (s *Service) submitEventURL(ctx context.Context, ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	did, fragment, found := strings.Cut(ref, "#")
	if !found || strings.TrimSpace(fragment) == "" || s.dir == nil {
		return ""
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, SubmitEventLookupTimeout)
	defer cancel()
	ident, err := s.dir.LookupDID(lookupCtx, parsed)
	if err != nil || ident == nil {
		return ""
	}
	service, ok := ident.Services[fragment]
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(service.URL), "/")
}

func (s *Service) offeringEndpoint() string {
	return s.inboundMarketURL()
}

func (s *Service) startLoops(ctx context.Context) {
	if s.bidKick == nil {
		s.bidKick = make(chan struct{}, 1)
	}
	if s.loop != nil {
		s.wg.Go(func() {
			s.loop.run(ctx)
		})
	}
	s.wg.Go(func() {
		s.bindBadgeBlueKeys(ctx)
	})
	s.wg.Go(func() {
		s.reportVMs(ctx)
	})
	if s.syncController != nil {
		controller := s.syncController
		s.wg.Go(func() {
			if err := controller.Run(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("the StateSync controller stopped", "err", err)
			}
		})
	}
	s.wg.Go(func() {
		s.reconcileBidsLoop(ctx)
	})
}

func (s *Service) kickBids() {
	if s.bidKick == nil {
		return
	}
	select {
	case s.bidKick <- struct{}{}:
	default:
	}
}

func (s *Service) bindBadgeBlueKeys(ctx context.Context) {
	if s.records == nil || s.attestation == nil || s.did == "" {
		return
	}
	key, err := s.attestation.PublicDID()
	if err != nil {
		s.log.Warn("reading the attestation key DID failed", "err", err)
		return
	}
	if _, err := market.CreateBadgeBlueKeysRecord(ctx, s.records, market.BadgeBlueKeysRecordOptions{
		DID:     s.did,
		KeyID:   key,
		Service: "bidder_attestation",
	}); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("binding the badge.blue key failed", "err", err)
		}
		return
	}
	s.log.Info("the bidder's badge.blue attestation key is bound", "did", s.did, "key", key)
}

var _ component.Service = (*Service)(nil)
