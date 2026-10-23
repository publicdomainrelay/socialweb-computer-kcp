package requester

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	healthPath = "/xrpc/_health"

	maxCallbackBody = 2 << 20

	submitAcceptProvisionTimeout = 3 * time.Minute

	submitAcceptReceiptTimeout = 5 * time.Second

	submitAcceptReceiptPoll = 250 * time.Millisecond

	acceptSettleDelay = 2 * time.Second

	serviceAuthTimeout = 5 * time.Second
)

type Service struct {
	cfg Config

	log *slog.Logger

	directory identity.Directory

	repo RecordStore

	ownsRepo bool

	signer *market.Signer

	oauth *OAuthSession

	ownerServiceAuth ServiceAuthSource

	ownerTokens *pdsAccountTokens

	tokenMu sync.Mutex

	credentialTokens *credentialTokens

	market MarketClient

	contracts ContractStore

	inbox *kcpstore.Resource[v1alpha1.StateSync]

	policy *PolicyEvaluator

	mu sync.Mutex

	reconcileMu sync.Mutex

	started bool

	ready bool

	stopped bool

	ctx context.Context

	cancel context.CancelFunc

	done chan struct{}

	runners map[string]*contractRunner

	finished map[string]bool

	waiting map[string]bool

	wg sync.WaitGroup

	running int

	perAccount map[string]int

	pendingBids map[string][]BidRecord

	eventMu sync.Mutex

	eventSinks map[string]chan MarketEvent

	pendingEvents map[string][]MarketEvent

	bidderMu sync.Mutex

	bidders map[string]Bidder

	scopeCache *ttlcache.Cache[string, PolicyDecision]

	secretsMu sync.Mutex

	secrets map[string]*secretsState

	heldMu sync.Mutex

	heldSessions map[string]*heldSession

	syncController *statesync.Controller
}

func New(cfg Config) (*Service, error) {
	cfg.defaults()
	if err := cfg.check(); err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		cfg:           cfg,
		log:           log.With("component", "requester", "did", cfg.DID),
		runners:       map[string]*contractRunner{},
		finished:      map[string]bool{},
		waiting:       map[string]bool{},
		perAccount:    map[string]int{},
		pendingBids:   map[string][]BidRecord{},
		eventSinks:    map[string]chan MarketEvent{},
		pendingEvents: map[string][]MarketEvent{},
		bidders:       map[string]Bidder{},
		scopeCache:    ttlcache.New[string, PolicyDecision](ttlcache.Options{TTL: 30 * time.Second}),
		secrets:       map[string]*secretsState{},
		heldSessions:  map[string]*heldSession{},
	}, nil
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("requester: the service is already started")
	}
	s.cfg = s.cfg.withDeps(deps)
	if s.cfg.Log == nil {
		s.cfg.Log = deps.Log
	}
	if s.cfg.Log != nil {
		s.log = s.cfg.Log.With("component", "requester", "did", s.cfg.DID)
	}
	s.started = true
	s.ready = false
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.ctx = runCtx
	s.cancel = cancel
	s.mu.Unlock()

	if err := s.setup(runCtx); err != nil {
		cancel()
		s.shutdown(ctx)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return err
	}

	handler := s.handler()
	if err := s.register(handler); err != nil {
		s.shutdown(ctx)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		close(s.done)
		return err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.wg.Go(func() { s.loop(runCtx) })
	if s.syncController != nil {
		controller := s.syncController
		s.wg.Go(func() {
			if err := controller.Run(runCtx); err != nil && runCtx.Err() == nil {
				s.log.Warn("requester_statesync_stopped", "err", err)
			}
		})
	}
	s.log.Info("requester_started", "handler", s.logicalURL(), "repo", s.cfg.RepoDID, "mode", s.cfg.IdentityMode)

	<-runCtx.Done()
	s.shutdown(ctx)
	close(s.done)
	return nil
}

func (s *Service) register(handler http.Handler) error {
	if s.cfg.Deps.Handlers == nil {
		return errors.New("requester: the handler registry is not available")
	}
	key := component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.cfg.ComponentName}
	return s.cfg.Deps.Handlers.RegisterHTTP(key, handler)
}

func (s *Service) registerPDSWithRelays(ctx context.Context) {
	if s.cfg.Deps.KCP == nil || len(s.cfg.Network.RelayURLs) == 0 {
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
		return
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	if restCfg == nil {
		return
	}
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.cfg.Workspace
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return
	}
	u, err := dyn.Resource(exposureGVR).Get(ctx, "svc-pds-pds-local", metav1.GetOptions{})
	if err != nil {
		return
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "address")
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return
	}
	for _, relayURL := range s.cfg.Network.RelayURLs {
		_ = xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relayURL, HTTP: &http.Client{Timeout: 10 * time.Second}}, parsed.Host)
	}
}

func (s *Service) logicalURL() string {
	return kcp.HandlerURL(s.cfg.Workspace, Kind, s.cfg.ComponentName)
}

func (s *Service) resolveIdentity(ctx context.Context) error {
	if s.cfg.PDSURL != "" && s.cfg.DID != "" {
		return nil
	}
	ref := s.cfg.Identity.MarketIdentityRef
	if ref == nil || ref.Name == "" || s.cfg.Deps.KCP == nil {
		return nil
	}
	workspaces := []string{s.cfg.Workspace}
	if !strings.Contains(s.cfg.Workspace, ":") {
		workspaces = append(workspaces, s.cfg.Workspace+":app")
	}
	var u *unstructured.Unstructured
	for i, ws := range workspaces {
		client, err := s.cfg.Deps.KCP.Client(ws)
		if err != nil {
			return fmt.Errorf("requester: kcp client for its MarketIdentity: %w", err)
		}
		cfg := client.RESTConfig()
		if cfg == nil || cfg.Host == "" {
			return errors.New("requester: the kcp client has no rest config for its MarketIdentity")
		}
		baseHost, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
		cfg = rest.CopyConfig(cfg)
		cfg.Host = baseHost + "/clusters/" + ws
		dc, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return fmt.Errorf("requester: reading its MarketIdentity: %w", err)
		}
		u, err = dc.Resource(marketIdentityGVR).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			break
		}
		if apierrors.IsNotFound(err) && i+1 < len(workspaces) {
			continue
		}
		return fmt.Errorf("requester: reading its MarketIdentity %s: %w", ref.Name, err)
	}
	if u == nil {
		return fmt.Errorf("requester: reading its MarketIdentity %s: not found", ref.Name)
	}
	did, _, _ := unstructured.NestedString(u.Object, "status", "did")
	pdsURL, _, _ := unstructured.NestedString(u.Object, "status", "pdsUrl")
	handle, _, _ := unstructured.NestedString(u.Object, "status", "handle")
	if did != "" {
		s.cfg.DID = did
		s.cfg.RepoDID = cmp.Or(s.cfg.RepoDID, did)
		s.cfg.MarketDID = cmp.Or(s.cfg.MarketDID, did)
	}
	if pdsURL != "" {
		s.cfg.PDSURL = pdsURL
	}
	s.cfg.Handle = cmp.Or(s.cfg.Handle, handle)
	return nil
}

func (s *Service) setup(ctx context.Context) error {
	if s.cfg.StateDir != "" {
		if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
			return fmt.Errorf("requester: state dir: %w", err)
		}
	}
	if ref := s.cfg.MarketExposureRef; s.cfg.MarketURL == "" && ref != nil && strings.TrimSpace(ref.Name) != "" {
		if raw, err := s.readExposureURL(ctx, strings.TrimSpace(ref.Name)); err != nil {
			if s.log != nil {
				s.log.Warn("the requester market URL is not resolved yet", "exposure", ref.Name, "err", err)
			}
		} else {
			s.cfg.MarketURL = strings.TrimRight(raw, "/")
		}
	}
	if s.cfg.Directory == nil {
		s.cfg.Directory = directoryFor(s.cfg.Network.PLCDirectoryURL, s.cfg.DirectoryKeys, s.cfg.DirectoryTTL)
	}
	s.directory = s.cfg.Directory
	if s.cfg.IdentityMode == IdentityModeOAuth {
		session, err := openOAuthSession(ctx, s.cfg, s.directory)
		if err != nil {
			return err
		}
		s.oauth = session
		s.cfg.DID = session.DID()
		s.cfg.RepoDID = session.DID()
		s.cfg.MarketDID = session.DID()
		if s.cfg.PDSURL == "" {
			s.cfg.PDSURL = session.HostURL()
		}
		if s.cfg.Log != nil {
			s.log = s.cfg.Log.With("component", "requester", "did", s.cfg.DID)
		}
	}
	if err := s.resolveIdentity(ctx); err != nil {
		return err
	}
	if s.cfg.RepoKey == nil {
		keyPath := s.cfg.RepoKeyPath
		if keyPath == "" && s.cfg.StateDir != "" {
			keyPath = filepath.Join(s.cfg.StateDir, "repo-key.json")
		}
		if keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: repo key: %w", err)
			}
			s.cfg.RepoKey = signer.PrivateKey
		}
	}
	if s.cfg.AttestationKey == nil {
		keyPath := s.cfg.AttestationKeyPath
		if keyPath == "" && s.cfg.StateDir != "" {
			keyPath = filepath.Join(s.cfg.StateDir, "attestation-key.json")
		}
		if keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: attestation key: %w", err)
			}
			s.cfg.AttestationKey = signer.PrivateKey
		}
	}
	if s.cfg.AttestationKey == nil {
		if s.cfg.RepoKey == nil {
			return errors.New("requester: no attestation or repo key")
		}
		s.cfg.AttestationKey = s.cfg.RepoKey
	}
	if len(s.cfg.OwnerSessions) > 0 {
		if err := s.authenticateOwnerSession(ctx); err != nil {
			return err
		}
	}
	s.signer = market.NewSigner(s.cfg.AttestationKey, s.cfg.MarketDID)
	remote := market.NewHTTPReader(s.directory)
	remote.Client = s.cfg.HTTPClient
	if s.cfg.Repo == nil {
		repoStore, err := s.openRepo(ctx, remote)
		if err != nil {
			return err
		}
		s.repo = repoStore
		s.ownsRepo = true
	} else {
		s.repo = s.cfg.Repo
	}
	if len(s.cfg.OwnerSessions) > 0 || s.cfg.IdentityMode == IdentityModeOAuth {
		if err := s.bindSignerKey(ctx); err != nil {
			return err
		}
	}
	if s.cfg.Market == nil {
		var serviceAuth ServiceAuthSource
		switch {
		case s.cfg.IdentityMode == IdentityModeOAuth:
			serviceAuth = s.oauth
		case !s.cfg.LocalSigner && s.ownerServiceAuth != nil:
			serviceAuth = s.ownerServiceAuth
		}
		s.log.Info("requester_service_auth", "identityMode", s.cfg.IdentityMode, "localSigner", s.cfg.LocalSigner,
			"signing", serviceAuthName(serviceAuth))
		s.market = &HTTPMarket{
			Directory:      s.directory,
			Reader:         s.repo,
			RepoDID:        s.cfg.RepoDID,
			Signer:         s.signer,
			ServiceKey:     s.cfg.RepoKey,
			ServiceAuth:    serviceAuth,
			RelayURLs:      append([]string(nil), s.cfg.Network.RelayURLs...),
			BidderDIDs:     append([]string(nil), s.cfg.BidderDIDs...),
			DenyBidderDIDs: append([]string(nil), s.cfg.DenyBidderDIDs...),
			HTTPClient:     s.cfg.HTTPClient,
			Timeout:        s.cfg.BidWindow,
			Log:            s.log,
		}
	} else {
		s.market = s.cfg.Market
	}
	if err := s.setupKCP(); err != nil {
		return err
	}
	if err := s.startStateSync(); err != nil {
		s.log.Warn("requester_does_not_run_statesync", "err", err)
	}
	s.wg.Go(func() { s.registerPDSWithRelays(ctx) })
	if s.cfg.Contracts == nil {
		return errors.New("requester: no ComputeContract store is configured")
	}
	s.contracts = s.cfg.Contracts
	s.policy = &PolicyEvaluator{Runs: s.policyRuns(), Workflow: PolicyWorkflowName(s.cfg.PolicyWorkflow), DID: s.cfg.MarketDID}
	return nil
}

const ownerSessionSigningService = "market_requester"

func serviceAuthName(source ServiceAuthSource) string {
	switch source.(type) {
	case nil:
		return "local"
	case *OAuthSession:
		return "oauth"
	case *ownerSessionServiceAuth:
		return "owner-session"
	}
	return "other"
}

func (s *Service) authenticateOwnerSession(ctx context.Context) error {
	session, ok := s.cfg.OwnerSessions[s.cfg.RepoDID]
	if !ok {
		return fmt.Errorf("requester: the owner sessions file names no account %s", s.cfg.RepoDID)
	}
	did, err := syntax.ParseDID(s.cfg.RepoDID)
	if err != nil {
		return fmt.Errorf("requester: owner DID: %w", err)
	}
	if _, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    session.Service,
		DID:        did,
		HTTPClient: s.cfg.HTTPClient,
		Timeout:    DefaultHTTPTimeout,
	}); err != nil {
		return fmt.Errorf("requester: the owner PDS %s: %w", session.Service, err)
	}
	tokens := newPDSAccountTokens(session.Service, session.Identifier, session.Password, s.cfg.HTTPClient)
	access, err := tokens.RefreshAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("requester: the owner session of %s: %w", s.cfg.RepoDID, err)
	}
	s.ownerTokens = tokens
	s.cfg.PDSAccessToken = access
	s.cfg.PDSURL = session.Service
	s.ownerServiceAuth = newOwnerSessionServiceAuth(session.Service, tokens, s.cfg.HTTPClient)
	return nil
}

type ownerSessionServiceAuth struct {
	minter xrpc.PDSMinter
}

func newOwnerSessionServiceAuth(pdsURL string, tokens pdsclient.TokenSource, client *http.Client) *ownerSessionServiceAuth {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	var refresh xrpc.TokenFunc
	if refresher, ok := tokens.(pdsclient.TokenRefresher); ok {
		refresh = refresher.RefreshAccessToken
	}
	auth := &xrpc.Bearer{Tokens: xrpc.Refreshing(tokens.AccessToken, refresh)}
	return &ownerSessionServiceAuth{minter: xrpc.PDSMinter{Client: &xrpc.Client{Host: pdsURL, HTTP: client, Auth: auth}}}
}

func (a *ownerSessionServiceAuth) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	token, err := a.minter.ServiceAuth(ctx, audience, lxm)
	if err != nil {
		return "", fmt.Errorf("requester: getServiceAuth: %w", err)
	}
	return token, nil
}

func (s *Service) bindSignerKey(ctx context.Context) error {
	if s.repo == nil || s.signer == nil {
		return errors.New("requester: the signer binding has no repo or signer")
	}
	key, err := s.signer.PublicDID()
	if err != nil {
		return fmt.Errorf("requester: the signer key: %w", err)
	}
	record := market.BuildBadgeBlueKeysRecord(market.BadgeBlueKeysRecordOptions{
		DID:       s.cfg.RepoDID,
		KeyID:     key,
		Service:   ownerSessionSigningService,
		CreatedAt: s.nowISO(),
	})
	rkey := market.BadgeBlueKeysRkey(s.cfg.RepoDID, key)
	if _, err := s.repo.PutRecord(ctx, s.cfg.RepoDID, market.BadgeBlueKeysNSID, rkey, record); err != nil {
		return fmt.Errorf("requester: publishing the signer key binding: %w", err)
	}
	s.log.Info("requester_signer_key_bound", "did", s.cfg.RepoDID, "key", key, "rkey", rkey)
	return nil
}

func (s *Service) openRepo(ctx context.Context, remote market.RecordResolver) (RecordStore, error) {
	ref, tokens, client, err := s.pdsTarget(ctx)
	if err != nil {
		return nil, err
	}
	if ref != nil {
		resolver := &pdsclient.Resolver{
			Handlers:   s.cfg.Deps.Handlers,
			HTTPClient: client,
			TLS:        s.cfg.Deps.TLS,
			Tokens:     tokens,
			Timeout:    RecordRequestTimeout,
		}
		pds, err := resolver.Resolve(ref)
		if err != nil {
			return nil, err
		}
		return NewPDSStore(PDSOptions{PDS: pds, DID: s.cfg.RepoDID, Remote: remote})
	}
	if s.cfg.RepoKey == nil {
		return nil, errors.New("requester: no repo signing key")
	}
	path := repoPath(s.cfg.StateDir)
	if s.cfg.StateDir == "" {
		return nil, errors.New("requester: the local repo needs a state dir")
	}
	return NewLocalRepo(ctx, path, s.cfg.RepoDID, s.cfg.RepoKey, remote)
}

func (s *Service) pdsTarget(ctx context.Context) (pdsclient.Ref, pdsclient.TokenSource, *http.Client, error) {
	switch s.cfg.IdentityMode {
	case IdentityModeOAuth:
		if s.oauth == nil {
			return nil, nil, nil, errors.New("requester: oauth identity mode has no session")
		}
		did, err := syntax.ParseDID(s.oauth.DID())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("requester: oauth PDS did: %w", err)
		}
		ref := pdsclient.CRRef{RemotePDS: &pdsv1alpha1.RemotePDS{
			ObjectMeta: metav1.ObjectMeta{Name: "pds"},
			Spec:       pdsv1alpha1.RemotePDSSpec{URL: s.oauth.HostURL(), DID: did.String()},
		}}
		return ref, s.oauth, s.oauth.HTTPClient(s.cfg.HTTPClient), nil
	case IdentityModeCallback:
		ref, err := s.pdsRef()
		if err != nil || ref == nil {
			return nil, nil, nil, err
		}
		if s.ownerTokens != nil {
			return ref, s.ownerTokens, s.cfg.HTTPClient, nil
		}
		if source := s.pdsCredentialTokens(); source != nil {
			return ref, source, s.cfg.HTTPClient, nil
		}
		token, err := s.pdsAccessToken(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		return ref, pdsTokenSource(token), s.cfg.HTTPClient, nil
	}
	return nil, nil, nil, nil
}

func (s *Service) pdsCredentialTokens() pdsclient.TokenSource {
	if s.cfg.PDSAccessTokenRef == nil || s.cfg.Deps.Credentials == nil {
		return nil
	}
	endpoint := strings.TrimSpace(s.cfg.PDSURL)
	if endpoint == "" && s.cfg.RemotePDS != nil {
		endpoint = strings.TrimSpace(s.cfg.RemotePDS.Spec.URL)
	}
	if endpoint == "" {
		return nil
	}
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.credentialTokens == nil {
		source := &credentialTokens{
			resolver: s.cfg.Deps.Credentials,
			ref:      *s.cfg.PDSAccessTokenRef,
			pdsURL:   endpoint,
			client:   s.cfg.HTTPClient,
		}
		if writer, ok := s.cfg.Deps.Credentials.(component.CredentialWriter); ok {
			source.writer = writer
		}
		s.credentialTokens = source
	}
	return s.credentialTokens
}

func (s *Service) pdsRef() (pdsclient.Ref, error) {
	if s.cfg.RemotePDS != nil {
		return pdsclient.CRRef{RemotePDS: s.cfg.RemotePDS}, nil
	}
	if s.cfg.PDSURL != "" {
		return pdsclient.CRRef{RemotePDS: &pdsv1alpha1.RemotePDS{
			ObjectMeta: metav1.ObjectMeta{Name: "pds"},
			Spec:       pdsv1alpha1.RemotePDSSpec{URL: s.cfg.PDSURL, DID: s.cfg.RepoDID},
		}}, nil
	}
	if s.cfg.PDSRef != nil && s.cfg.PDSRef.Name != "" {
		kind := s.cfg.PDSRef.Kind
		kind = cmp.Or(kind, "Pds")
		return pdsclient.LocalRef{Workspace: s.cfg.Workspace, Kind: kind, Name: s.cfg.PDSRef.Name}, nil
	}
	return nil, nil
}

func (s *Service) pdsAccessToken(ctx context.Context) (string, error) {
	token := s.cfg.PDSAccessToken
	if token != "" || s.cfg.PDSAccessTokenRef == nil || s.cfg.Deps.Credentials == nil {
		return token, nil
	}
	credential, err := s.cfg.Deps.Credentials.Resolve(ctx, *s.cfg.PDSAccessTokenRef)
	if err != nil {
		return "", fmt.Errorf("requester: PDS token: %w", err)
	}
	for _, key := range s.cfg.PDSAccessTokenRef.Keys {
		if value, ok := credential.Value(key); ok && value != "" {
			return value, nil
		}
	}
	return "", nil
}

func pdsTokenSource(token string) pdsclient.TokenSource {
	return pdsclient.TokenSourceFunc(func(context.Context) (string, error) {
		return token, nil
	})
}

func (s *Service) resolvePolicyRunner(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
	caller := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.cfg.ComponentName, Workspace: s.cfg.Workspace}
	return bus.Resolve[policy.Runner](ctx, s.cfg.Deps.Handlers, caller, engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
}

func (s *Service) policyRuns() *runs.Client {
	if s.cfg.Deps.Handlers == nil {
		return nil
	}
	return &runs.Client{
		Resolve: s.resolvePolicyRunner,
		Owner:   policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: Kind, Name: s.cfg.ComponentName, UID: s.cfg.ComponentUID},
		Engine:  runs.EngineRef(s.cfg.PolicyEngineRef, s.cfg.Workspace),
		Role:    "requester",
		Prefix:  "requester-policy",
		Args:    s.cfg.PolicyArgs,
		Timeout: s.cfg.PolicyTimeout,
		Parser:  runs.Strict,
		Now:     s.cfg.Now,
	}
}

func (s *Service) setupKCP() error {
	if s.cfg.Contracts != nil {
		return nil
	}
	store, err := kcpStore(s.cfg.Deps, s.cfg.Workspace)
	if err != nil {
		return err
	}
	s.inbox = kcpstore.OfClusterScoped[v1alpha1.StateSync](store, inboxGVR)
	if s.cfg.Contracts == nil {
		s.cfg.Contracts = NewKCPContracts(kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, ComputeContractGVR), s.cfg.Workspace, "")
	}
	s.contracts = s.cfg.Contracts
	return nil
}

func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.ContractPoll)
	defer ticker.Stop()
	for {
		s.reconcile(ctx)
		s.processPendingEvents(ctx)
		s.processBidMailboxes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	contracts, err := s.cfg.Contracts.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("requester_contracts_list_failed", "err", err)
		}
		return
	}
	present := make(map[string]bool, len(contracts))
	for i := range contracts {
		contract := contracts[i]
		present[contract.Name] = true
		s.mu.Lock()
		runner := s.runners[contract.Name]
		finished := s.finished[contract.Name]
		s.mu.Unlock()
		if runner != nil {
			runner.Update(&contract)
			continue
		}
		if finished {
			continue
		}
		phase := contract.Status.Phase
		phase = cmp.Or(phase, v1alpha1.ComputeContractPending)
		terminal := phase == v1alpha1.ComputeContractReleased
		if phase == v1alpha1.ComputeContractFailed {
			terminal = contract.Status.Contract == nil || contract.Status.Contract.Receipt == nil
		}
		if terminal {
			state := contract.Status.Contract
			if state != nil && state.Receipt != nil && state.Termination == nil && winnerDID(state) != "" {
				terminal = false
			}
		}
		if terminal {
			s.mu.Lock()
			s.finished[contract.Name] = true
			s.mu.Unlock()
			continue
		}
		hasReceipt := contract.Status.Contract != nil && contract.Status.Contract.Receipt != nil
		if !hasReceipt && (contract.Spec.Release || contract.DeletionTimestamp != nil) {
			if contract.DeletionTimestamp == nil {
				status := contract.Status
				status.Phase = v1alpha1.ComputeContractReleased
				status.Message = "the contract was released before it started"
				addContractConditions(&status, contract.Generation, status.Phase, status.Message, s.now(), false)
				if err := s.cfg.Contracts.PatchStatus(ctx, contract.Name, status); err != nil {
					s.log.Warn("requester_contract_status_failed", "contract", contract.Name, "err", err)
				}
			}
			s.mu.Lock()
			s.finished[contract.Name] = true
			s.mu.Unlock()
			continue
		}
		account := contract.Spec.OwnerDid
		s.mu.Lock()
		admit, reason := s.admitLocked(account)
		if !admit {
			_, waiting := s.waiting[contract.Name]
			s.waiting[contract.Name] = true
			s.mu.Unlock()
			if !waiting && phase != v1alpha1.ComputeContractReleased {
				s.log.Info("requester_contract_waiting", "contract", contract.Name, "reason", reason)
				status := contract.Status
				status.ObservedGeneration = contract.Generation
				status.Phase = v1alpha1.ComputeContractPending
				status.Message = reason
				addContractConditions(&status, contract.Generation, status.Phase, reason, s.now(), true)
				if err := s.cfg.Contracts.PatchStatus(ctx, contract.Name, status); err != nil {
					s.log.Warn("requester_contract_status_failed", "contract", contract.Name, "err", err)
				}
			}
			continue
		}
		delete(s.waiting, contract.Name)
		s.running++
		if account != "" {
			s.perAccount[account]++
		}
		started := newContractRunner(s, contract)
		if contract.Spec.Release || contract.DeletionTimestamp != nil {
			started.requestRelease()
		}
		s.runners[contract.Name] = started
		s.wg.Go(func() {
			started.Run(ctx)
			s.runnerDone(started, account)
		})
		s.mu.Unlock()
	}
	s.mu.Lock()
	for name := range s.finished {
		if !present[name] {
			delete(s.finished, name)
		}
	}
	for name := range s.waiting {
		if !present[name] {
			delete(s.waiting, name)
		}
	}
	s.mu.Unlock()
}

func (s *Service) admitLocked(account string) (bool, string) {
	if s.running >= s.cfg.MaxConcurrent {
		return false, fmt.Sprintf("waiting for admission: %d of %d contracts are running", s.running, s.cfg.MaxConcurrent)
	}
	if account != "" && s.perAccount[account] >= s.cfg.PerAccount {
		return false, fmt.Sprintf("this account has %d of %d contracts running", s.perAccount[account], s.cfg.PerAccount)
	}
	return true, ""
}

func (s *Service) runnerDone(runner *contractRunner, account string) {
	s.mu.Lock()
	current, ok := s.runners[runner.name]
	if ok && current == runner {
		delete(s.runners, runner.name)
		if s.running > 0 {
			s.running--
		}
		if account != "" {
			if s.perAccount[account] <= 1 {
				delete(s.perAccount, account)
			} else {
				s.perAccount[account]--
			}
		}
		s.finished[runner.name] = true
	}
	s.mu.Unlock()
	s.wg.Go(func() { s.reconcile(s.ctx) })
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return errors.New("requester: the service is not started")
	}
	if !s.ready {
		return errors.New("requester: the service is starting")
	}
	if s.repo == nil {
		return errors.New("requester: the repo is not open")
	}
	return nil
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
	cancel := s.cancel
	done := s.done
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

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), component.DefaultStopTimeout)
	defer cancel()
	_ = supervise.Join(ctx, s.wg.Wait)
	s.releaseAllHeldSessions("shutdown")
	if s.ownsRepo {
		if closer, ok := s.repo.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	s.mu.Lock()
	s.started = false
	s.ready = false
	s.mu.Unlock()
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, s.health)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.getRecord", s.repoGetRecord)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.listRecords", s.repoListRecords)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.describeRepo", s.repoDescribe)
	mux.HandleFunc("GET /xrpc/com.atproto.server.describeServer", s.serverDescribe)
	if s.cfg.inboundXRPC() {
		mux.HandleFunc("POST /xrpc/"+market.SubmitBidNSID, s.submitBid)
		mux.HandleFunc("POST /xrpc/"+market.SubmitEventNSID, s.submitEvent)
	}
	mux.HandleFunc("GET "+SecretsRoute+"/{contract}", s.getSecrets)
	mux.HandleFunc("GET /oauth-client-metadata.json", s.oauthClientMetadata)
	mux.HandleFunc("GET /oauth/callback/{identity}", s.oauthCallback)
	return mux
}

func (s *Service) oauthClientMetadata(w http.ResponseWriter, r *http.Request) {
	base := "https://" + r.Host
	httpjson.Write(w, http.StatusOK, map[string]any{
		"client_id":                  base + "/oauth-client-metadata.json",
		"client_name":                "socialweb-computer requester",
		"redirect_uris":              []string{base + "/oauth/callback/requester"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      atprotooauth.ScopeString(atprotooauth.RequesterOAuthScope),
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
		"dpop_bound_access_tokens":   true,
	})

}

func (s *Service) oauthCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("identity")
	handler, ok := oauthcallback.Lookup(name)
	if !ok {
		http.Error(w, "unknown identity", http.StatusNotFound)
		return
	}
	if err := handler(r.Context(), w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("signed in"))
}

func (s *Service) registerSecrets(name string, state *secretsState) {
	s.secretsMu.Lock()
	s.secrets[name] = state
	s.secretsMu.Unlock()
}

func (s *Service) unregisterSecrets(name string) {
	s.secretsMu.Lock()
	delete(s.secrets, name)
	s.secretsMu.Unlock()
}

func (s *Service) getSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("contract")
	s.secretsMu.Lock()
	state := s.secrets[name]
	s.secretsMu.Unlock()
	if state == nil {
		http.Error(w, `{"error":"NotFound"}`, http.StatusNotFound)
		return
	}
	state.serve(r.Context(), w, r)
}

var exposureGVR = exposurev1alpha1.ExposureGVR

var marketIdentityGVR = v1alpha1.MarketIdentityGVR

func (s *Service) ingressExposureURL(ctx context.Context) (string, error) {
	name := "svc-" + strings.ToLower(Kind) + "-" + strings.TrimSpace(s.cfg.ComponentName) + "-ingress"
	deadline := time.Now().Add(DefaultExposureWait)
	for {
		raw, err := s.readExposureURL(ctx, name)
		if err == nil && raw != "" {
			return strings.TrimRight(raw, "/"), nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("requester: the %s exposure: %w", name, err)
			}
			return "", fmt.Errorf("requester: the %s exposure reports no url", name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) readExposureURL(ctx context.Context, name string) (string, error) {
	if s.cfg.Deps.KCP == nil {
		return "", errors.New("requester: no kcp clients")
	}
	client, err := s.cfg.Deps.KCP.Client(s.cfg.Workspace)
	if err != nil {
		return "", err
	}
	cfg := rest.CopyConfig(client.RESTConfig())
	if cfg == nil || cfg.Host == "" {
		return "", errors.New("requester: the kcp client has no rest config")
	}
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = baseHost + "/clusters/" + s.cfg.Workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	u, err := dc.Resource(exposureGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "address")
	return strings.TrimSpace(raw), nil
}

func (s *Service) readSecretsBundle(ctx context.Context, ref commonv1.SecretRef) ([]secretEntry, error) {
	if s.cfg.Deps.Vault == nil {
		return nil, errors.New("requester: no vault clients")
	}
	if s.cfg.VaultRef == nil || s.cfg.VaultRef.Name == "" {
		return nil, errors.New("requester: the contract names a secrets bundle and the component names no OpenBaoServer")
	}
	server := v1alpha1.VaultServer(s.cfg.VaultRef, "")
	client, err := s.cfg.Deps.Vault.Client(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("requester: the vault client: %w", err)
	}
	data, err := client.ReadKV(ctx, ref.Mount, ref.Path)
	if err != nil {
		return nil, fmt.Errorf("requester: reading the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	text, _ := data["bundle"].(string)
	if text == "" {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s carries no bundle", ref.Mount, ref.Path)
	}
	entries := []secretEntry{}
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	for i := range entries {
		if entries[i].Path == "" || !strings.HasPrefix(entries[i].Path, "/") {
			return nil, fmt.Errorf("requester: the secrets bundle names the path %q", entries[i].Path)
		}
	}
	return entries, nil
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready && s.repo != nil
	did := s.cfg.DID
	s.mu.Unlock()
	if !ready {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true, "did": did})
}

func (s *Service) repoGetRecord(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	repoDID := query.Get("repo")
	repoDID = cmp.Or(repoDID, s.cfg.RepoDID)
	record, err := s.repo.GetRecord(r.Context(), repoDID, query.Get("collection"), query.Get("rkey"))
	if err != nil {
		status := http.StatusNotFound
		if !errors.Is(err, market.ErrNotFound) {
			status = http.StatusBadGateway
		}
		httpjson.Write(w, status, map[string]any{"error": "RecordNotFound", "message": err.Error()})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"uri": record.URI, "cid": record.CID, "value": record.Value})
}

func (s *Service) repoListRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	repoDID := query.Get("repo")
	repoDID = cmp.Or(repoDID, s.cfg.RepoDID)
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		fmt.Sscanf(raw, "%d", &limit)
	}
	page, err := s.repo.ListRecords(r.Context(), repoDID, query.Get("collection"), limit, query.Get("cursor"))
	if err != nil {
		httpjson.Write(w, http.StatusBadGateway, map[string]any{"error": "RepoError", "message": err.Error()})
		return
	}
	records := make([]any, 0, len(page.Records))
	for _, record := range page.Records {
		records = append(records, map[string]any{"uri": record.URI, "cid": record.CID, "value": record.Value})
	}
	body := map[string]any{"records": records}
	if page.Cursor != "" {
		body["cursor"] = page.Cursor
	}
	httpjson.Write(w, http.StatusOK, body)
}

func (s *Service) repoDescribe(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.cfg.RepoDID, "handle": s.cfg.Handle, "collections": []string{}})
}

func (s *Service) serverDescribe(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.cfg.RepoDID, "version": "socialweb-computer"})
}

func (s *Service) submitBid(w http.ResponseWriter, r *http.Request) {
	issuer, ok := s.authorizeCallback(w, r, market.SubmitBidNSID, defaultMarketServiceID)
	if !ok {
		return
	}
	var body struct {
		URI    string         `json:"uri"`
		CID    string         `json:"cid"`
		Record map[string]any `json:"record"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": err.Error()})
		return
	}
	if err := market.AuthorizeRecord(issuer, body.URI); err != nil {
		httpjson.Write(w, http.StatusForbidden, map[string]any{"error": "Forbidden", "message": err.Error()})
		return
	}
	bid, err := market.ParseBid(body.Record)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": err.Error()})
		return
	}
	if err := s.storeBidMailbox(r.Context(), issuer, body.URI, body.CID, body.Record); err != nil {
		s.log.Warn("requester_bid_enqueue_failed", "uri", body.URI, "err", err)
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"error": "Unavailable", "message": err.Error()})
		return
	}
	s.queueBid(bid.RFP.URI, BidRecord{DID: issuer, URI: body.URI, CID: body.CID, Record: body.Record, ArrivedAt: s.now()})
	s.log.Info("requester_bid_received", "bidder", issuer, "uri", body.URI, "rfp", bid.RFP.URI)
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) submitEvent(w http.ResponseWriter, r *http.Request) {
	issuer, ok := s.authorizeCallback(w, r, market.SubmitEventNSID, defaultComputeEventServiceID)
	if !ok {
		return
	}
	var body struct {
		URI    string         `json:"uri"`
		CID    string         `json:"cid"`
		Record map[string]any `json:"record"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": err.Error()})
		return
	}
	if body.URI == "" || body.CID == "" || body.Record == nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": "missing uri, cid, or record"})
		return
	}
	if err := market.AuthorizeRecord(issuer, body.URI); err != nil {
		httpjson.Write(w, http.StatusForbidden, map[string]any{"error": "Forbidden", "message": err.Error()})
		return
	}
	event, err := market.ParseEvent(body.Record)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": err.Error()})
		return
	}
	payloadNSID := market.NSIDFromURI(event.Payload.URI)
	switch payloadNSID {
	case market.ComputeEventsVMOnNetworkNSID, market.ComputeEventsVMHostKeysNSID, market.ComputeEventsVMTerminatedNSID:
	default:
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": "the payload is not a guest event"})
		return
	}
	ref := v1alpha1.ComputeContractEvent{URI: body.URI, CID: body.CID, ReceiptURI: event.Receipt.URI, ReceivedAt: s.nowISO()}
	if err := s.enqueueEvent(r.Context(), ref); err != nil {
		s.log.Warn("requester_event_enqueue_failed", "uri", body.URI, "err", err)
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"error": "Unavailable", "message": err.Error()})
		return
	}
	s.log.Info("requester_event_queued", "uri", body.URI, "receipt", event.Receipt.URI)
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) authorizeCallback(w http.ResponseWriter, r *http.Request, lxm, serviceID string) (string, bool) {
	hostname := s.callbackHostname(r)
	if hostname == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest", "message": "no callback hostname"})
		return "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), serviceAuthTimeout)
	defer cancel()
	audiences := []string{s.cfg.MarketDID, s.cfg.RepoDID, s.cfg.DID}
	for _, raw := range []string{s.cfg.MarketURL, s.cfg.CallbackHostname, s.cfg.PublicHostname, s.cfg.Network.IngressProxyHost} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		host := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		audiences = append(audiences, market.ServiceAudienceDIDs(host)...)
	}
	audiences = append(audiences, market.ServiceAudienceDIDs(hostname)...)
	result, err := market.VerifyServiceAuth(ctx, s.directory, market.VerifyServiceAuthOptions{
		AuthHeader:        r.Header.Get("Authorization"),
		Hostname:          hostname,
		LXM:               lxm,
		ServiceIDs:        []string{serviceID},
		ExtraAudienceDIDs: audiences,
	})
	if err != nil {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized", "message": err.Error()})
		return "", false
	}
	return result.IssuerDID, true
}

func (s *Service) callbackHostname(r *http.Request) string {
	if host := strings.TrimSpace(r.Host); host != "" {
		return host
	}
	for _, raw := range []string{s.cfg.CallbackHostname, s.cfg.PublicHostname, s.cfg.Network.IngressProxyHost} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
		if !strings.Contains(raw, "://") {
			return strings.Trim(raw, "[]")
		}
	}
	host := r.Host
	return host
}

func (s *Service) badgeBlueBinding() func(ctx context.Context, did, key string) (bool, error) {
	return func(ctx context.Context, did, key string) (bool, error) {
		return market.KeyBoundByBadgeBlueKey(ctx, s.repo, did, key), nil
	}
}

func (s *Service) queueBid(rfpURI string, bid BidRecord) {
	if rfpURI == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.pendingBids[rfpURI]
	if len(list) >= 100 {
		list = list[1:]
	}
	s.pendingBids[rfpURI] = append(list, bid)
}

func (s *Service) takeBids(rfpURI string) []BidRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	bids := s.pendingBids[rfpURI]
	delete(s.pendingBids, rfpURI)
	return bids
}

func (s *Service) registerEventSink(receiptURI string) (<-chan MarketEvent, []MarketEvent, func()) {
	s.eventMu.Lock()
	channel := make(chan MarketEvent, 32)
	s.eventSinks[receiptURI] = channel
	pending := s.pendingEvents[receiptURI]
	delete(s.pendingEvents, receiptURI)
	s.eventMu.Unlock()
	closeFn := func() {
		s.eventMu.Lock()
		if s.eventSinks[receiptURI] == channel {
			delete(s.eventSinks, receiptURI)
		}
		s.eventMu.Unlock()
	}
	return channel, pending, closeFn
}

func (s *Service) dispatchEvent(receiptURI string, event MarketEvent) {
	if receiptURI == "" {
		return
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if channel, ok := s.eventSinks[receiptURI]; ok {
		select {
		case channel <- event:
		default:
		}
		return
	}
	list := s.pendingEvents[receiptURI]
	if len(list) >= 64 {
		list = list[1:]
	}
	s.pendingEvents[receiptURI] = append(list, event)
}

func (s *Service) rememberBidders(bidders []Bidder) {
	s.bidderMu.Lock()
	defer s.bidderMu.Unlock()
	for _, bidder := range bidders {
		if strings.TrimSpace(bidder.DID) == "" {
			continue
		}
		if bidder.Endpoint == "" && bidder.Audience == "" {
			continue
		}
		if current, ok := s.bidders[bidder.DID]; ok && current.Endpoint != "" && bidder.Endpoint == "" {
			continue
		}
		s.bidders[bidder.DID] = bidder
	}
}

func (s *Service) bidderFor(did string) (Bidder, bool) {
	s.bidderMu.Lock()
	defer s.bidderMu.Unlock()
	bidder, ok := s.bidders[did]
	return bidder, ok
}

func (s *Service) bidderForBid(ctx context.Context, bid BidRecord) (Bidder, bool) {
	if bidder, ok := s.bidderFor(bid.DID); ok && bidder.Endpoint != "" {
		return bidder, true
	}
	raw := strings.TrimSpace(stringOf(bid.Record["submitAccept"]))
	if raw == "" {
		return Bidder{}, false
	}
	endpoint, audience, err := resolveBidderEndpoint(ctx, s.directory, raw)
	if err != nil || endpoint == "" {
		return Bidder{}, false
	}
	bidder := Bidder{DID: bid.DID, Endpoint: endpoint, Audience: audience}
	s.rememberBidders([]Bidder{bidder})
	return bidder, true
}

func (s *Service) bidderForDID(ctx context.Context, did string) (Bidder, bool) {
	if bidder, ok := s.bidderFor(did); ok && bidder.Endpoint != "" {
		return bidder, true
	}
	if discovered, err := s.market.Discover(ctx, s.now()); err == nil {
		s.rememberBidders(discovered)
		if bidder, ok := s.bidderFor(did); ok && bidder.Endpoint != "" {
			return bidder, true
		}
	}
	if s.directory == nil {
		return Bidder{}, false
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return Bidder{}, false
	}
	identityDoc, err := s.directory.LookupDID(ctx, parsed)
	if err != nil || identityDoc == nil {
		return Bidder{}, false
	}
	for fragment, service := range identityDoc.Services {
		if service.URL == "" {
			continue
		}
		if fragment != defaultMarketServiceID && !strings.Contains(fragment, "market") {
			continue
		}
		endpoint, audience, err := resolveBidderEndpoint(ctx, s.directory, service.URL)
		if err != nil || endpoint == "" {
			continue
		}
		bidder := Bidder{DID: did, Endpoint: endpoint, Audience: audience}
		s.rememberBidders([]Bidder{bidder})
		return bidder, true
	}
	return Bidder{}, false
}

func (s *Service) submitAcceptAndWait(ctx context.Context, bidder Bidder, acceptURI, acceptCID, winnerDID string) (*market.ResolvedRecord, error) {
	provisionTimeout := max(s.cfg.ReceiptTimeout, submitAcceptProvisionTimeout)
	if !s.outboundXRPC() || bidder.Endpoint == "" {
		timeout := s.cfg.ReceiptTimeout
		timeout = opt.Positive(timeout, DefaultReceiptTimeout)
		return s.waitReceiptWithin(ctx, winnerDID, acceptURI, timeout)
	}
	if receipt, err := s.findReceiptWithin(ctx, winnerDID, acceptURI, submitAcceptReceiptTimeout); err == nil && receipt != nil {
		return receipt, nil
	}
	select {
	case <-time.After(acceptSettleDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if copies := s.cfg.AcceptCopies; copies > 1 {
		copiesCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
		var wg sync.WaitGroup
		for range copies {
			wg.Go(func() {
				_, _ = s.market.SubmitAccept(copiesCtx, bidder, acceptURI, acceptCID)
			})
		}
		wg.Wait()
		cancel()
		receipts := []string{}
		receipt, receiptErr := s.waitReceiptWithin(ctx, winnerDID, acceptURI, provisionTimeout)
		errText := ""
		if receiptErr != nil {
			errText = receiptErr.Error()
		}
		if receiptErr == nil && receipt != nil {
			receipts = append(receipts, receipt.URI)
		}
		s.log.Info("submitAccept_copies", "copies", copies, "receipts", receipts, "err", errText)
		if receiptErr == nil && receipt != nil {
			return receipt, nil
		}
	}
	deadline := time.Now().Add(s.cfg.ReceiptTimeout)
	delay := s.cfg.ReceiptPoll
	delay = opt.Positive(delay, DefaultReceiptPoll)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		s.log.Warn("submitAccept_retry", "bidder", bidder.DID, "accept", acceptURI, "acceptCid", acceptCID)
		attemptCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
		_, err := s.market.SubmitAccept(attemptCtx, bidder, acceptURI, acceptCID)
		cancel()
		if err != nil {
			s.log.Warn("requester_submit_accept_failed", "bidder", bidder.DID, "err", err)
		}
		receipt, err := s.findReceiptWithin(ctx, winnerDID, acceptURI, submitAcceptReceiptTimeout)
		if err == nil && receipt != nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
	}
}

func (s *Service) findReceiptWithin(ctx context.Context, bidderDID, acceptURI string, timeout time.Duration) (*market.ResolvedRecord, error) {
	receiptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.market.FindReceipt(receiptCtx, bidderDID, acceptURI)
}

func (s *Service) waitReceiptWithin(ctx context.Context, bidderDID, acceptURI string, timeout time.Duration) (*market.ResolvedRecord, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		receipt, err := s.findReceiptWithin(ctx, bidderDID, acceptURI, submitAcceptReceiptTimeout)
		if err == nil && receipt != nil {
			return receipt, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(submitAcceptReceiptPoll):
		}
	}
}

func (s *Service) classifyBid(ctx context.Context, bid *BidRecord) error {
	if bid == nil {
		return errors.New("requester: no bid")
	}
	ref := payloadRef(bid.Record)
	if ref.URI == "" {
		return nil
	}
	resolved, err := s.repo.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	kind := market.NSIDFromURI(resolved.URI)
	switch kind {
	case market.BidsFreeNSID:
		bid.Free = true
		bid.Reason = stringOf(resolved.Value["reason"])
	case market.BidsX402NSID:
		bid.Cost = numberValue(resolved.Value["cost"])
	}
	if bid.Free {
		bid.Cost = 0
	}
	return nil
}

func (s *Service) bidAllowedWithPolicy(ctx context.Context, bid BidRecord, usePolicy bool, workflow string) bool {
	if !usePolicy {
		return true
	}
	key := bid.DID + "|" + workflow
	if decision, ok := s.scopeCache.Get(key); ok {
		return decision.Allow
	}
	if s.policy == nil || (s.policy.Workflow == "" && workflow == "") {
		return true
	}
	self := s.cfg.MarketDID
	if s.cfg.OperatorDID != "" {
		self = s.cfg.OperatorDID
	}
	input := PolicyInput{
		SelfDid:          self,
		SubjectDid:       bid.DID,
		CounterpartyDid:  bid.DID,
		RootRequesterDid: self,
	}
	if state := s.activeState(bid.Record); state != nil {
		input.RFP = state.RFP
	}
	decision := s.policy.Scope(ctx, input)
	s.scopeCache.Set(key, decision)
	if !decision.Allow {
		s.log.Info("requester_bid_policy_denied", "bidder", bid.DID, "reason", decision.Reason)
	}
	return decision.Allow
}

func (s *Service) activeState(record map[string]any) *PolicyInput {
	rfp, _ := record["rfp"].(map[string]any)
	if rfp == nil {
		return nil
	}
	return &PolicyInput{RFP: &PolicyStrongRef{URI: stringOf(rfp["uri"]), CID: stringOf(rfp["cid"])}}
}

func (s *Service) releaseDeadline(contract v1alpha1.ComputeContract) time.Time {
	base := contract.CreationTimestamp.Time
	if base.IsZero() {
		base = s.now()
	}
	bound := s.cfg.ReleaseBound
	if raw := contract.Annotations["socialweb.computer/ttl-after-finished"]; raw != "" {
		if parsed, ok := parseDuration(raw); ok && parsed > 0 {
			bound = parsed
		}
	}
	return base.Add(bound)
}

func (s *Service) releaseRetryDelay(attempt int) time.Duration {
	delay := s.cfg.ReleaseRetryFirst
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= s.cfg.ReleaseRetryMax {
			return s.cfg.ReleaseRetryMax
		}
	}
	if delay > s.cfg.ReleaseRetryMax {
		delay = s.cfg.ReleaseRetryMax
	}
	return delay
}

func parseDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
		return parsed, true
	}
	if len(raw) > 1 {
		unit := raw[len(raw)-1]
		value := raw[:len(raw)-1]
		var amount int64
		if _, err := fmt.Sscanf(value, "%d", &amount); err == nil && amount > 0 {
			switch unit {
			case 's':
				return time.Duration(amount) * time.Second, true
			case 'm':
				return time.Duration(amount) * time.Minute, true
			case 'h':
				return time.Duration(amount) * time.Hour, true
			case 'd':
				return time.Duration(amount) * 24 * time.Hour, true
			}
		}
	}
	return 0, false
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Service) nowISO() string {
	return market.FormatTime(s.now())
}

func (s *Service) callbackDID() string {
	if s.cfg.DID != "" {
		return s.cfg.DID
	}
	return s.cfg.MarketDID
}

func (s *Service) submitBidRef() string {
	if !s.cfg.inboundXRPC() {
		return ""
	}
	if url := strings.TrimRight(strings.TrimSpace(s.cfg.MarketURL), "/"); url != "" {
		return url
	}
	if s.callbackDID() == "" {
		return ""
	}
	return s.callbackDID() + "#" + defaultMarketServiceID
}

func (s *Service) submitEventRef() string {
	if !s.cfg.inboundXRPC() {
		return ""
	}
	if url := strings.TrimRight(strings.TrimSpace(s.cfg.MarketURL), "/"); url != "" {
		return url
	}
	if s.callbackDID() == "" {
		return ""
	}
	return s.callbackDID() + "#" + defaultComputeEventServiceID
}

func (s *Service) generatedVMName() string {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%s%d", DefaultVMNamePrefix, time.Now().UnixNano()%100000000)
	}
	return DefaultVMNamePrefix + hex.EncodeToString(raw)
}

func (s *Service) directoryIdentity() identity.Directory {
	return s.directory
}

func decodeBody(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxCallbackBody {
		return errors.New("body too large")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && (apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found"))
}
