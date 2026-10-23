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
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
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

type Deps struct {
	component.Instance

	KCP component.KCPClients

	Credentials component.CredentialResolver

	Egress component.Egress
}

type Service struct {
	spec v1alpha1.RequesterSpec

	deps Deps

	log *slog.Logger

	httpClient *http.Client

	identityMode IdentityMode

	localSigner bool

	did string

	handle string

	repoDID string

	marketDID string

	pdsURL string

	marketURL string

	pdsAccessTokenRef *component.CredentialRef

	oauthClientID string

	oauthRedirectURI string

	oauthScopes []string

	oauthSessionData *oauthclient.SessionData

	ownerSessions map[string]sshtransport.OwnerSession

	policyWorkflow string

	guestAuthorizedKeys []string

	repoKey atcrypto.PrivateKey

	attestationKey atcrypto.PrivateKey

	contractPoll time.Duration

	bidWindow time.Duration

	receiptTimeout time.Duration

	receiptPoll time.Duration

	sessionCap time.Duration

	sessionIdle time.Duration

	directoryTTL time.Duration

	policyTimeout time.Duration

	directory identity.Directory

	repo *market.RepoStore

	signer *market.Signer

	oauth *OAuthSession

	ownerServiceAuth ServiceAuthSource

	ownerTokens *pdsAccountTokens

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

func New(spec v1alpha1.RequesterSpec, deps Deps) (*Service, error) {
	base := deps.Log
	if base == nil {
		base = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		spec:          spec,
		deps:          deps,
		log:           base,
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
	}
	if err := s.derive(); err != nil {
		return nil, err
	}
	s.log = base.With("component", "requester", "did", s.did)
	return s, nil
}

func (s *Service) baseLog() *slog.Logger {
	if s.deps.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Log
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
	s.log.Info("requester_started", "handler", s.logicalURL(), "repo", s.repoDID, "mode", s.identityMode)

	<-runCtx.Done()
	s.shutdown(ctx)
	close(s.done)
	return nil
}

func (s *Service) register(handler http.Handler) error {
	if s.deps.Bus == nil {
		return errors.New("requester: the handler registry is not available")
	}
	key := component.Key{Workspace: s.workspace(), Kind: Kind, Name: s.deps.Self.Name}
	return s.deps.Bus.RegisterHTTP(key, handler)
}

func (s *Service) registerPDSWithRelays(ctx context.Context) {
	if s.deps.KCP == nil || len(s.spec.Network.Relays()) == 0 {
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
	client, err := s.deps.KCP.Client(s.workspace())
	if err != nil {
		return
	}
	restCfg := rest.CopyConfig(client.RESTConfig())
	if restCfg == nil {
		return
	}
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(restCfg.Host, "/"), "/clusters/")
	restCfg.Host = baseHost + "/clusters/" + s.workspace()
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
	for _, relayURL := range s.spec.Network.Relays() {
		_ = xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relayURL, HTTP: &http.Client{Timeout: 10 * time.Second}}, parsed.Host)
	}
}

func (s *Service) logicalURL() string {
	return kcp.HandlerURL(s.workspace(), Kind, s.deps.Self.Name)
}

func (s *Service) resolveIdentity(ctx context.Context) error {
	if s.pdsURL != "" && s.did != "" {
		return nil
	}
	if s.spec.Identity == nil {
		return nil
	}
	ref := s.spec.Identity.MarketIdentityRef
	if ref == nil || ref.Name == "" || s.deps.KCP == nil {
		return nil
	}
	workspaces := []string{s.workspace()}
	if !strings.Contains(s.workspace(), ":") {
		workspaces = append(workspaces, s.workspace()+":app")
	}
	var u *unstructured.Unstructured
	for i, ws := range workspaces {
		client, err := s.deps.KCP.Client(ws)
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
		s.did = did
		s.repoDID = cmp.Or(s.repoDID, did)
		s.marketDID = cmp.Or(s.marketDID, did)
	}
	if pdsURL != "" {
		s.pdsURL = pdsURL
	}
	s.handle = cmp.Or(s.handle, handle)
	return nil
}

func (s *Service) setup(ctx context.Context) error {
	if s.stateDir() != "" {
		if err := os.MkdirAll(s.stateDir(), 0o700); err != nil {
			return fmt.Errorf("requester: state dir: %w", err)
		}
	}
	if ref := s.spec.MarketExposureRef; s.marketURL == "" && ref != nil && strings.TrimSpace(ref.Name) != "" {
		if raw, err := s.readExposureURL(ctx, strings.TrimSpace(ref.Name)); err != nil {
			if s.log != nil {
				s.log.Warn("the requester market URL is not resolved yet", "exposure", ref.Name, "err", err)
			}
		} else {
			s.marketURL = strings.TrimRight(raw, "/")
		}
	}
	s.directory = directoryFor(s.network().PLCDirectoryURL, int(ptr.Deref(s.spec.DirectoryKeys, 0)), s.directoryTTL)
	if s.identityMode == IdentityModeOAuth {
		session, err := openOAuthSession(ctx, s, s.directory)
		if err != nil {
			return err
		}
		s.oauth = session
		s.did = session.DID()
		s.repoDID = session.DID()
		s.marketDID = session.DID()
		if s.pdsURL == "" {
			s.pdsURL = session.HostURL()
		}
		s.log = s.baseLog().With("component", "requester", "did", s.did)
	}
	if err := s.resolveIdentity(ctx); err != nil {
		return err
	}
	if s.repoKey == nil {
		if keyPath := s.stateFile("repo-key.json"); keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: repo key: %w", err)
			}
			s.repoKey = signer.PrivateKey
		}
	}
	if s.attestationKey == nil {
		if keyPath := s.stateFile("attestation-key.json"); keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: attestation key: %w", err)
			}
			s.attestationKey = signer.PrivateKey
		}
	}
	if s.attestationKey == nil {
		if s.repoKey == nil {
			return errors.New("requester: no attestation or repo key")
		}
		s.attestationKey = s.repoKey
	}
	if len(s.ownerSessions) > 0 {
		if err := s.authenticateOwnerSession(ctx); err != nil {
			return err
		}
	}
	s.signer = market.NewSigner(s.attestationKey, s.marketDID)
	remote := market.NewHTTPReader(s.directory)
	remote.Client = s.httpClient
	repoStore, err := s.openRepo(remote)
	if err != nil {
		return err
	}
	s.repo = repoStore
	if len(s.ownerSessions) > 0 || s.identityMode == IdentityModeOAuth {
		if err := s.bindSignerKey(ctx); err != nil {
			return err
		}
	}
	var serviceAuth ServiceAuthSource
	switch {
	case s.identityMode == IdentityModeOAuth:
		serviceAuth = s.oauth
	case !s.localSigner && s.ownerServiceAuth != nil:
		serviceAuth = s.ownerServiceAuth
	}
	s.log.Info("requester_service_auth", "identityMode", s.identityMode, "localSigner", s.localSigner,
		"signing", serviceAuthName(serviceAuth))
	s.market = &HTTPMarket{
		Directory:   s.directory,
		Reader:      s.repo,
		RepoDID:     s.repoDID,
		Signer:      s.signer,
		ServiceKey:  s.repoKey,
		ServiceAuth: serviceAuth,
		RelayURLs:   append([]string(nil), s.spec.Network.Relays()...),
		BidderDIDs:  append([]string(nil), s.spec.BidderDIDs...),
		HTTPClient:  s.httpClient,
		Timeout:     s.bidWindow,
		Log:         s.log,
	}
	if err := s.setupKCP(); err != nil {
		return err
	}
	if err := s.startStateSync(); err != nil {
		s.log.Warn("requester_does_not_run_statesync", "err", err)
	}
	s.wg.Go(func() { s.registerPDSWithRelays(ctx) })
	if s.contracts == nil {
		return errors.New("requester: no ComputeContract store is configured")
	}
	s.policy = &PolicyEvaluator{Runs: s.policyRuns(), Workflow: PolicyWorkflowName(s.policyWorkflow), DID: s.marketDID}
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
	session, ok := s.ownerSessions[s.repoDID]
	if !ok {
		return fmt.Errorf("requester: the owner sessions file names no account %s", s.repoDID)
	}
	did, err := syntax.ParseDID(s.repoDID)
	if err != nil {
		return fmt.Errorf("requester: owner DID: %w", err)
	}
	if _, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    session.Service,
		DID:        did,
		HTTPClient: s.httpClient,
		Timeout:    DefaultHTTPTimeout,
	}); err != nil {
		return fmt.Errorf("requester: the owner PDS %s: %w", session.Service, err)
	}
	tokens := newPDSAccountTokens(session.Service, session.Identifier, session.Password, s.httpClient)
	if _, err := tokens.RefreshAccessToken(ctx); err != nil {
		return fmt.Errorf("requester: the owner session of %s: %w", s.repoDID, err)
	}
	s.ownerTokens = tokens
	s.pdsURL = session.Service
	s.ownerServiceAuth = newOwnerSessionServiceAuth(session.Service, tokens, s.httpClient)
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
		DID:       s.repoDID,
		KeyID:     key,
		Service:   ownerSessionSigningService,
		CreatedAt: s.nowISO(),
	})
	rkey := market.BadgeBlueKeysRkey(s.repoDID, key)
	if _, err := s.repo.PutRecord(ctx, s.repoDID, market.BadgeBlueKeysNSID, rkey, record); err != nil {
		return fmt.Errorf("requester: publishing the signer key binding: %w", err)
	}
	s.log.Info("requester_signer_key_bound", "did", s.repoDID, "key", key, "rkey", rkey)
	return nil
}

func (s *Service) openRepo(remote market.Foreign) (*market.RepoStore, error) {
	if s.repoDID == "" {
		return nil, errors.New("requester: the record store needs the repo DID")
	}
	if s.deps.Bus == nil {
		return nil, errors.New("requester: the record store needs the bus")
	}
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace()}
	repo := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "AtprotoRepo", Name: atprotorepo.RepoName(s.repoDID), Workspace: s.workspace()}
	handlers := s.deps.Bus
	return market.NewRepoStore(market.RepoStoreConfig{DID: s.repoDID, Foreign: remote,
		Open: func(ctx context.Context) (atprotorepo.Repo, error) {
			return bus.Resolve[atprotorepo.Repo](ctx, handlers, self, repo, bus.Capability{Name: atprotorepo.CapabilityWrite})
		}}), nil
}

func (s *Service) resolvePolicyRunner(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
	caller := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace()}
	return bus.Resolve[policy.Runner](ctx, s.deps.Bus, caller, engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
}

func (s *Service) policyRuns() *runs.Client {
	if s.deps.Bus == nil {
		return nil
	}
	return &runs.Client{
		Resolve: s.resolvePolicyRunner,
		Owner:   policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: Kind, Name: s.deps.Self.Name, UID: string(s.deps.UID)},
		Engine:  runs.EngineRef(s.spec.PolicyEngineRef, s.workspace()),
		Role:    "requester",
		Prefix:  "requester-policy",
		Timeout: s.policyTimeout,
		Parser:  runs.Strict,
		Now:     time.Now,
	}
}

func (s *Service) setupKCP() error {
	if s.contracts != nil {
		return nil
	}
	store, err := kcpStore(s.deps.KCP, s.workspace())
	if err != nil {
		return err
	}
	s.inbox = kcpstore.OfClusterScoped[v1alpha1.StateSync](store, inboxGVR)
	s.contracts = NewKCPContracts(kcpstore.OfClusterScoped[v1alpha1.ComputeContract](store, ComputeContractGVR), s.workspace(), "")
	return nil
}

func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(s.contractPoll)
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
	contracts, err := s.contracts.List(ctx)
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
				if err := s.contracts.PatchStatus(ctx, contract.Name, status); err != nil {
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
				if err := s.contracts.PatchStatus(ctx, contract.Name, status); err != nil {
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
	if maxConcurrent := int(s.spec.MaxConcurrent); s.running >= maxConcurrent {
		return false, fmt.Sprintf("waiting for admission: %d of %d contracts are running", s.running, maxConcurrent)
	}
	if perAccount := int(s.spec.PerAccount); account != "" && s.perAccount[account] >= perAccount {
		return false, fmt.Sprintf("this account has %d of %d contracts running", s.perAccount[account], perAccount)
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
	if s.inboundXRPC() {
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
	name := "svc-" + strings.ToLower(Kind) + "-" + strings.TrimSpace(s.deps.Self.Name) + "-ingress"
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
	if s.deps.KCP == nil {
		return "", errors.New("requester: no kcp clients")
	}
	client, err := s.deps.KCP.Client(s.workspace())
	if err != nil {
		return "", err
	}
	cfg := rest.CopyConfig(client.RESTConfig())
	if cfg == nil || cfg.Host == "" {
		return "", errors.New("requester: the kcp client has no rest config")
	}
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = baseHost + "/clusters/" + s.workspace()
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
	vaultRef := s.spec.VaultRef
	if vaultRef == nil || vaultRef.Name == "" {
		return nil, errors.New("requester: the contract names a secrets bundle and the component names no OpenBaoServer")
	}
	target := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: vaultRef.Name, Workspace: cmp.Or(vaultRef.Workspace, s.deps.Self.Workspace)}
	kv, err := bus.Resolve[vault.KV](s.deps.Caller(ctx), s.deps.Bus, s.deps.Self, target, bus.Capability{Name: vault.CapabilityRead, Scope: []string{ref.Mount + "/" + ref.Path}})
	if err != nil {
		return nil, fmt.Errorf("requester: the vault client: %w", err)
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
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
	did := s.did
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
	repoDID = cmp.Or(repoDID, s.repoDID)
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
	repoDID = cmp.Or(repoDID, s.repoDID)
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
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.repoDID, "handle": s.handle, "collections": []string{}})
}

func (s *Service) serverDescribe(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"did": s.repoDID, "version": "socialweb-computer"})
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
	audiences := []string{s.marketDID, s.repoDID, s.did}
	for _, raw := range []string{s.marketURL, s.network().IngressProxyHost} {
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
	for _, raw := range []string{s.network().IngressProxyHost} {
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
	provisionTimeout := max(s.receiptTimeout, submitAcceptProvisionTimeout)
	if !s.outboundXRPC() || bidder.Endpoint == "" {
		return s.waitReceiptWithin(ctx, winnerDID, acceptURI, s.receiptTimeout)
	}
	if receipt, err := s.findReceiptWithin(ctx, winnerDID, acceptURI, submitAcceptReceiptTimeout); err == nil && receipt != nil {
		return receipt, nil
	}
	select {
	case <-time.After(acceptSettleDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if copies := int(s.spec.AcceptCopies); copies > 1 {
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
	deadline := time.Now().Add(s.receiptTimeout)
	delay := s.receiptPoll
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
	self := s.marketDID
	if operator := strings.TrimSpace(s.spec.OperatorDID); operator != "" {
		self = operator
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
	bound := DefaultReleaseBound
	if raw := contract.Annotations["socialweb.computer/ttl-after-finished"]; raw != "" {
		if parsed, ok := parseDuration(raw); ok && parsed > 0 {
			bound = parsed
		}
	}
	return base.Add(bound)
}

func (s *Service) releaseRetryDelay(attempt int) time.Duration {
	delay := DefaultReleaseRetryFirst
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= DefaultReleaseRetryMax {
			return DefaultReleaseRetryMax
		}
	}
	if delay > DefaultReleaseRetryMax {
		delay = DefaultReleaseRetryMax
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
	return time.Now()
}

func (s *Service) nowISO() string {
	return market.FormatTime(s.now())
}

func (s *Service) callbackDID() string {
	if s.did != "" {
		return s.did
	}
	return s.marketDID
}

func (s *Service) submitBidRef() string {
	if !s.inboundXRPC() {
		return ""
	}
	if url := strings.TrimRight(strings.TrimSpace(s.marketURL), "/"); url != "" {
		return url
	}
	if s.callbackDID() == "" {
		return ""
	}
	return s.callbackDID() + "#" + defaultMarketServiceID
}

func (s *Service) submitEventRef() string {
	if !s.inboundXRPC() {
		return ""
	}
	if url := strings.TrimRight(strings.TrimSpace(s.marketURL), "/"); url != "" {
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
