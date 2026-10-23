package bidder

import (
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
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
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

type AuthorizerFunc func(ctx context.Context, header, lxm, recordURI string) (string, error)

func (f AuthorizerFunc) Authorize(ctx context.Context, header, lxm, recordURI string) (string, error) {
	return f(ctx, header, lxm, recordURI)
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
	cfg Config

	log *slog.Logger

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

	records RecordStore

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

	serviceAuthMu sync.Mutex

	serviceAuth map[string]serviceAuthToken
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

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.cfg = s.cfg.withDeps(deps)
	if deps.Log != nil {
		s.log = deps.Log.With("component", "bidder", "handle", s.cfg.Handle)
	}
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
	defer s.shutdown()

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
	if ctx == nil {
		ctx = context.Background()
	}
	return s.shutdownWith(ctx)
}

func (s *Service) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), component.DefaultStopTimeout)
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
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			if s.stopErr == nil {
				s.stopErr = ctx.Err()
			}
		}
	})
	return s.stopErr
}

func (s *Service) prepare(ctx context.Context) error {
	if s.cfg.StateDir != "" {
		if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
			return fmt.Errorf("bidder: creating the state dir: %w", err)
		}
	}
	if s.cfg.Records == nil || s.cfg.Bids == nil || s.cfg.VMs == nil || s.cfg.Grants == nil {
		if err := s.resolveKCP(ctx); err != nil {
			s.log.Warn("the bidder runs without its kcp stores", "err", err)
		}
	}
	s.dir = ingress.NewDirectory(s.cfg.Network.PlcDirectoryUrl)
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
	s.records = s.cfg.Records
	if s.did != "" && s.records != nil && s.attestation == nil && s.cfg.StateDir != "" {
		signer, err := market.LoadOrCreateSignerFile(s.cfg.StateDir + "/attestation.jwk")
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
		Config:         s.cfg.Provider,
		VMs:            s.vms,
		Grants:         s.grants,
		Vault:          s.vault,
		DID:            s.did,
		IssuerURL:      s.guestURL,
		Log:            s.log,
		Now:            s.cfg.Now,
		WaitForRunning: false,
	}
	if s.provider.Config.Handle == "" {
		s.provider.Config.Handle = s.cfg.Handle
	}
	s.policy = &PolicyEvaluator{
		Runs:        s.policyRuns(),
		Cluster:     s.cluster,
		EngineRef:   s.engineRef(),
		ScopePolicy: s.cfg.Policy,
		Args:        s.cfg.PolicyArgs,
		Timeout:     s.cfg.PolicyTimeout,
		Poll:        s.cfg.PolicyPoll,
		Records:     s.records,
		Log:         s.log,
		Now:         s.cfg.Now,
	}
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
		relays:                  append([]string(nil), s.cfg.Network.RelayUrls...),
		offeringRefresh:         s.cfg.OfferingRefresh,
		offeringRefreshDisabled: s.cfg.OfferingRefreshDisabled,
		heartbeat:               s.cfg.Heartbeat,
		log:                     s.log,
		now:                     s.cfg.Now,
	}
	s.log.Info("the bidder serves the market", "handler", s.listeningURL(), "did", s.did, "guest", s.guestURL,
		"offering", s.offeringEndpoint(), "serviceAuthHost", s.cfg.ServiceAuth.Hostname,
		"identityMode", s.cfg.IdentityMode, "inboundXRPC", s.cfg.inboundXRPC())
	go s.registerPDSWithRelays(ctx)
	return nil
}

var exposureGVR = exposurev1alpha1.ExposureGVR

func (s *Service) registerPDSWithRelays(ctx context.Context) {
	if s.cfg.Deps.KCP == nil || len(s.cfg.Network.RelayUrls) == 0 {
		s.log.Warn("the bidder cannot register its PDS with the relay", "kcp", s.cfg.Deps.KCP != nil, "relays", len(s.cfg.Network.RelayUrls))
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
	client, err := s.cfg.Deps.KCP.Client(s.cfg.Workspace)
	if err != nil {
		s.log.Warn("the bidder cannot read its kcp client", "workspace", s.cfg.Workspace, "err", err.Error())
		return
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.cfg.Workspace
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
	raw, _, _ := unstructured.NestedString(u.Object, "status", "url")
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		s.log.Warn("the bidder PDS Exposure has no URL", "url", raw)
		return
	}
	body := strings.NewReader(fmt.Sprintf(`{"hostname":%q}`, parsed.Host))
	registered := false
	for _, relayURL := range s.cfg.Network.RelayUrls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(relayURL, "/")+"/xrpc/com.atproto.sync.requestCrawl", body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				registered = true
			}
			resp.Body.Close()
		}
		body = strings.NewReader(fmt.Sprintf(`{"hostname":%q}`, parsed.Host))
	}
	if registered && s.loop != nil {
		if err := s.loop.touchOffering(ctx); err != nil {
			s.log.Warn("republishing the offering after the relay crawl failed", "err", err.Error())
		}
	}
}

func (s *Service) register(ctx context.Context) error {
	if s.cfg.Deps.Handlers == nil {
		return errors.New("bidder: the handler registry is not available")
	}
	key := component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.cfg.ComponentName}
	if err := s.cfg.Deps.Handlers.RegisterHTTP(key, s.mux); err != nil {
		return err
	}
	if err := s.cfg.Deps.Handlers.RegisterHTTP(key, s.mux, "market"); err != nil {
		return err
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	return nil
}

func (s *Service) resolveKCP(ctx context.Context) error {
	if s.cfg.Deps.KCP == nil {
		return nil
	}
	store, cluster, err := kcpStore(s.cfg.Deps, s.cfg.Workspace)
	if err != nil {
		return err
	}
	s.store = store
	s.cluster = cluster
	return nil
}

func (s *Service) resolveIdentity(ctx context.Context) error {
	s.did = s.cfg.DID
	if s.cfg.PDSURL == "" && s.cfg.PDSRef != nil {
		kind := s.cfg.PDSRef.Kind
		if kind == "" {
			kind = "Pds"
		}
		s.cfg.PDSURL = handlerURL(s.cfg.Workspace, kind, s.cfg.PDSRef.Name)
		s.pdsInternal = s.cfg.PDSURL != ""
	}
	if s.store == nil || s.cfg.IdentityRef == nil || s.cfg.IdentityRef.Name == "" {
		return nil
	}
	identities := kcpstore.OfClusterScoped[corev1alpha1.MarketIdentity](s.store, MarketIdentityGVR)
	obj, err := identities.Get(ctx, ref.New(s.cluster, "", s.cfg.IdentityRef.Name))
	if err != nil {
		return err
	}
	if s.did == "" {
		s.did = obj.Status.Did
	}
	if s.cfg.PDSURL == "" {
		s.cfg.PDSURL = obj.Status.PdsUrl
	}
	if s.cfg.Handle == "" {
		s.cfg.Handle = obj.Status.Handle
	}
	return nil
}

func (s *Service) resolveRecords(ctx context.Context) error {
	if s.cfg.Records != nil {
		return nil
	}
	pds := s.cfg.PDS
	if pds == nil {
		tokens := s.cfg.PDSTokens
		if tokens == nil {
			tokens = s.credentialTokenSource()
		}
		if tokens == nil {
			token, err := s.pdsAccessToken(ctx)
			if err != nil {
				return err
			}
			tokens = pdsclient.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
		}
		s.cfg.PDSTokens = tokens
		resolver := s.cfg.PDSResolver
		if resolver == nil {
			resolver = &pdsclient.Resolver{
				Handlers:   s.cfg.Deps.Handlers,
				HTTPClient: externalHTTPClient(s.cfg.Deps, RecordRequestTimeout),
				TLS:        s.cfg.Deps.TLS,
				Tokens:     tokens,
				DPoP:       s.cfg.PDSDPoP,
				Timeout:    RecordRequestTimeout,
			}
		}
		ref, err := s.pdsRef()
		if err != nil {
			return err
		}
		pds, err = resolver.Resolve(ref)
		if err != nil {
			return err
		}
	}
	store, err := NewPDSStore(PDSOptions{PDS: pds, DID: s.did})
	if err != nil {
		return err
	}
	reader := market.NewHTTPReader(s.dir)
	reader.Client = externalHTTPClient(s.cfg.Deps, RecordRequestTimeout)
	if client, ok := reader.Client.(*http.Client); ok {
		if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
			s.log.Info("the bidder's record reader trusts its roots", "rootCAs", transport.TLSClientConfig.RootCAs != nil,
				"bundle", s.cfg.Network.CaBundleRef)
		}
	}
	s.cfg.Records = NewRoutedStore(s.did, store, reader)
	return nil
}

func (s *Service) credentialTokenSource() pdsclient.TokenSource {
	if s.cfg.PDSAccessTokenRef == nil || s.cfg.Deps.Credentials == nil || strings.TrimSpace(s.cfg.PDSURL) == "" {
		return nil
	}
	client := externalHTTPClient(s.cfg.Deps, DefaultCallTimeout)
	if s.pdsInternal && s.cfg.Deps.Handlers != nil {
		client = s.cfg.Deps.Handlers.HTTPClient()
	}
	source := &credentialPDSTokens{
		resolver: s.cfg.Deps.Credentials,
		ref:      *s.cfg.PDSAccessTokenRef,
		pdsURL:   s.cfg.PDSURL,
		client:   client,
	}
	if writer, ok := s.cfg.Deps.Credentials.(component.CredentialWriter); ok {
		source.writer = writer
	}
	return source
}

func (s *Service) pdsAccessToken(ctx context.Context) (string, error) {
	token := s.cfg.PDSAccessToken
	if token == "" && s.cfg.PDSAccessTokenRef != nil && s.cfg.Deps.Credentials != nil {
		credential, err := s.cfg.Deps.Credentials.Resolve(ctx, *s.cfg.PDSAccessTokenRef)
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

func (s *Service) pdsRef() (pdsclient.Ref, error) {
	if s.cfg.RemotePDS != nil {
		return pdsclient.CRRef{RemotePDS: s.cfg.RemotePDS}, nil
	}
	if s.cfg.PDSRef != nil {
		kind := s.cfg.PDSRef.Kind
		if kind == "" {
			kind = "Pds"
		}
		return pdsclient.LocalRef{Workspace: s.cfg.Workspace, Kind: kind, Name: s.cfg.PDSRef.Name}, nil
	}
	if s.cfg.PDSURL != "" {
		if s.pdsInternal {
			return pdsclient.LocalRef{Endpoint: s.cfg.PDSURL}, nil
		}
		return pdsclient.CRRef{RemotePDS: &pdsv1alpha1.RemotePDS{
			ObjectMeta: metav1.ObjectMeta{Name: "pds"},
			Spec:       pdsv1alpha1.RemotePDSSpec{URL: s.cfg.PDSURL, DID: s.did},
		}}, nil
	}
	return nil, errors.New("bidder: no PDS endpoint is resolved")
}

func (s *Service) resolveStores() error {
	if s.store != nil {
		if s.cfg.Bids == nil {
			s.cfg.Bids = NewKCPBids(s.store, s.cluster)
		}
		if s.cfg.VMs == nil {
			s.cfg.VMs = NewKCPVMs(s.store, s.cluster, s.cfg.Provider.Handle)
		}
		if s.cfg.Grants == nil {
			s.cfg.Grants = NewKCPGrants(s.store, s.cluster)
		}
	}
	s.bids = s.cfg.Bids
	s.vms = s.cfg.VMs
	s.grants = s.cfg.Grants
	return nil
}

func (s *Service) resolveVault(ctx context.Context) error {
	if s.cfg.Vault != nil {
		s.vault = s.cfg.Vault
		return nil
	}
	if s.store != nil {
		clientName := s.cfg.VaultClient
		if clientName == "" {
			clientName = "bidder"
		}
		vault, err := kcpOpenBaoVault(s.store, s.cluster, s.cfg.Provider.GuestVaultServer, clientName)
		if err != nil {
			return err
		}
		s.vault = vault
		return nil
	}
	if s.cfg.Deps.Vault != nil {
		client, err := s.cfg.Deps.Vault.Client(ctx, s.cfg.Provider.GuestVaultServer)
		if err == nil {
			if guest, ok := client.(GuestVault); ok {
				s.vault = guest
				return nil
			}
		}
	}
	return errors.New("bidder: no guest vault is available")
}

func (s *Service) resolveGuestURL(ctx context.Context) string {
	if raw := strings.TrimSpace(s.cfg.GuestURL); raw != "" {
		if !isAbsoluteURL(raw) {
			s.log.Warn("the bidder guest URL is not an absolute URL", "url", raw)
			return ""
		}
		return strings.TrimRight(raw, "/")
	}
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
	if s.cfg.GuestControlRef == nil {
		return ""
	}
	kind := s.cfg.GuestControlRef.Kind
	if kind == "" {
		kind = "GuestControlPlane"
	}
	return handlerURL(s.cfg.Workspace, kind, s.cfg.GuestControlRef.Name)
}

func (s *Service) publicNetwork() bool {
	return strings.TrimSpace(s.cfg.Network.IngressProxyHost) != "" ||
		strings.TrimSpace(s.cfg.Network.GuestTlsUrl) != ""
}

func (s *Service) exposureClient() (dynamic.Interface, error) {
	if s.cfg.Deps.KCP == nil {
		return nil, errors.New("bidder: the component has no kcp clients")
	}
	client, err := s.cfg.Deps.KCP.Client(s.cfg.Workspace)
	if err != nil {
		return nil, err
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.cfg.Workspace
	return dynamic.NewForConfig(restCfg)
}

func (s *Service) guestExposureURL(ctx context.Context) string {
	ref := s.cfg.GuestControlRef
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return ""
	}
	kind := ref.Kind
	if kind == "" {
		kind = "GuestControlPlane"
	}
	name := "svc-" + strings.ToLower(kind) + "-" + strings.TrimSpace(ref.Name) + "-ingress"
	client, err := s.exposureClient()
	if err != nil {
		return ""
	}
	u, err := client.Resource(exposureGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "url")
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if !isAbsoluteURL(raw) {
		return ""
	}
	return raw
}

func (s *Service) policyRuns() PolicyRunClient {
	if s.cfg.PolicyRuns != nil {
		return s.cfg.PolicyRuns
	}
	if s.store == nil || s.cfg.PolicyEngineRef == nil || s.cfg.PolicyEngineRef.Name == "" {
		return nil
	}
	return NewKCPPolicyRuns(s.store, s.cluster)
}

func (s *Service) engineRef() string {
	if s.cfg.PolicyEngineRef == nil {
		return ""
	}
	return s.cfg.PolicyEngineRef.Name
}

func (s *Service) newAuthorizer() Authorizer {
	if s.cfg.Authorize != nil {
		return AuthorizerFunc(s.cfg.Authorize)
	}
	if s.cfg.ServiceAuth.Hostname == "" {
		s.cfg.ServiceAuth.Hostname = hostPortOf(s.publicMarketURL())
	}
	audienceDIDs := append([]string(nil), s.cfg.ServiceAuth.AudienceDIDs...)
	audienceDIDs = append(audienceDIDs, bidderAudienceDIDs(s.cfg.ServiceAuth.Hostname)...)
	return &ServiceAuthAuthorizer{
		Dir:          s.dir,
		Hostname:     s.cfg.ServiceAuth.Hostname,
		ServiceIDs:   []string{market.DefaultMarketServiceID, market.DefaultComputeEventServiceID},
		AudienceDIDs: audienceDIDs,
	}
}

func bidderAudienceDIDs(host string) []string {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	candidates := []string{host, strings.ToLower(host)}
	if name, _, err := net.SplitHostPort(host); err == nil && name != "" {
		candidates = append(candidates, name, strings.ToLower(name))
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		did := market.ServiceDIDForHost(candidate)
		if seen[did] {
			continue
		}
		seen[did] = true
		unique = append(unique, did)
	}
	return unique
}

func (s *Service) addEndpoint(endpoint component.Endpoint) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, endpoint)
	s.mu.Unlock()
}

func (s *Service) listeningURL() string {
	return handlerURL(s.cfg.Workspace, Kind, s.cfg.ComponentName)
}

func (s *Service) publicMarketURL() string {
	if url := strings.TrimSpace(s.cfg.MarketURL); url != "" {
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
	if !s.cfg.inboundXRPC() {
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
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.loop.run(ctx)
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.bindBadgeBlueKeys(ctx)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.reportVMs(ctx)
	}()
	if s.syncController != nil {
		controller := s.syncController
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := controller.Run(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("the StateSync controller stopped", "err", err)
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.reconcileBidsLoop(ctx)
	}()
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
