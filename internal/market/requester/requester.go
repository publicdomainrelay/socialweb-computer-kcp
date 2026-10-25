package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/utils/ptr"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

type IdentityMode string

const (
	IdentityModeLocal IdentityMode = "local"

	IdentityModeCallback IdentityMode = "callback"

	IdentityModeOAuth IdentityMode = "oauth"
)

const (
	DefaultVMReadyTimeout = 120 * time.Second

	DefaultHostKeysTimeout = 120 * time.Second

	DefaultReceiptTimeout = 5 * time.Minute

	DefaultReleaseTimeout = 30 * time.Second

	DefaultReleaseRetryFirst = 5 * time.Second

	DefaultReleaseRetryMax = 5 * time.Minute

	DefaultReleaseBound = 24 * time.Hour

	DefaultHTTPTimeout = 30 * time.Second

	DefaultPolicyWorkflow = "requester-tangled-vouch"

	DefaultVMNamePrefix = "compute-"

	ServiceAuthTTL = 5 * time.Minute

	MaxRecordBids = 20

	PolicyGhaLiteNSID = "computer.socialweb.temp.policy.ghalite"

	DefaultComputeDisk = "50G"

	DefaultComputeCPUs = 2

	DefaultComputeMem = "4G"

	DefaultExposureWait = 60 * time.Second

	IdentityWait = 60 * time.Second

	MarketExposureWait = 10 * time.Second

	PDSExposure = "svc-pds-pds-local"

	KindRequester = "Requester"

	ownerSessionSigningService = "market_requester"
)

type Placement struct {
	Workspace string

	Name string

	UID string

	StateDir string
}

type Config struct {
	Spec v1alpha1.RequesterSpec

	Placement

	IdentityMode IdentityMode

	LocalSigner bool

	DID string

	Handle string

	RepoDID string

	MarketDID string

	PDSURL string

	OAuthScopes []string

	OwnerSessions map[string]sshtransport.OwnerSession

	PolicyWorkflow string

	GuestAuthorizedKeys []string

	BidWindow time.Duration

	ReceiptTimeout time.Duration

	ReceiptPoll time.Duration

	SessionCap time.Duration

	SessionIdle time.Duration

	PolicyTimeout time.Duration
}

type TokenVerifier interface {
	VerifyAudience(ctx context.Context, token, audience string) (map[string]any, error)
}

type Guests interface {
	Subject(bidderDID, requesterDID, role string) (string, error)

	Verifier(issuer string, keys func(ctx context.Context) ([]byte, error)) TokenVerifier

	CheckHostKeys(keys []v1alpha1.ComputeVMHostKey) ([]v1alpha1.ComputeVMHostKey, error)
}

type Deps struct {
	Intents contractmarket.Intents

	Inputs *participant.Inputs

	OpenRepo func(ctx context.Context, did string) (atprotorepo.Repo, error)

	AwaitRepo func(ctx context.Context, did string)

	Policy runs.ResolveFunc

	Vault func(ctx context.Context, ref commonv1.SecretRef) (vault.KV, error)

	ServiceAuth func(ctx context.Context, identity commonv1.ObjectRef, did string) (pds.ServiceAuth, error)

	Guests Guests

	ContractEvents contractmarket.ContractEvents

	Directory identity.Directory

	HTTP *http.Client

	Now func() time.Time

	Log *slog.Logger
}

type Requester struct {
	cfg Config

	inboxed sync.Map

	deps Deps

	log *slog.Logger

	did string

	handle string

	repoDID string

	marketDID string

	pdsURL string

	marketURL string

	repoKey atcrypto.PrivateKey

	attestationKey atcrypto.PrivateKey

	repo *market.RepoStore

	signer *market.Signer

	identityRef commonv1.ObjectRef

	ownerServiceAuth ServiceAuthSource

	market MarketClient

	policy *PolicyEvaluator

	outbox *participant.Outbox

	contracts *participant.Index[v1alpha1.ComputeContract]

	syncs *participant.Index[v1alpha1.StateSync]

	receipts *participant.Index[market.ResolvedRecord]

	contractEvents *participant.Index[v1alpha1.ContractEvent]

	creatingInbound sync.Map

	bids *bidQueue

	mu sync.Mutex

	reconcileMu sync.Mutex

	pendingMu sync.Mutex

	ctx context.Context

	started bool

	wg sync.WaitGroup

	runners map[string]*contractRunner

	finished map[string]bool

	waiting map[string]bool

	settled map[string]bool

	running int

	perAccount map[string]int

	written map[string][]v1alpha1.ComputeContractEvent

	eventMu sync.Mutex

	eventSinks map[string]chan MarketEvent

	undelivered map[string][]MarketEvent

	bidderMu sync.Mutex

	bidders map[string]Bidder

	scopeCache *ttlcache.Cache[string, PolicyDecision]

	secretsMu sync.Mutex

	secrets map[string]*secretsState

	sealMu sync.Mutex

	heldMu sync.Mutex

	heldSessions map[string]*heldSession
}

var _ contractmarket.Observer = (*Requester)(nil)

func IdentityWorkspaces(workspace string) []string {
	workspaces := []string{workspace}
	if !strings.HasSuffix(workspace, ":app") {
		workspaces = append(workspaces, workspace+":app")
	}
	return workspaces
}

func Configure(spec v1alpha1.RequesterSpec, at Placement) (Config, error) {
	at.Workspace = cmp.Or(at.Workspace, "root")
	cfg := Config{Spec: spec, Placement: at, IdentityMode: IdentityModeLocal}
	if spec.Identity != nil {
		cfg.DID = spec.Identity.Did
		cfg.Handle = spec.Identity.Handle
	}
	cfg.RepoDID, cfg.MarketDID = cfg.DID, cfg.DID
	callback := spec.PdsRef != nil && spec.PdsRef.Name != ""
	if callback {
		cfg.IdentityMode = IdentityModeCallback
		if did := strings.TrimSpace(spec.PdsDID); did != "" {
			cfg.RepoDID, cfg.MarketDID = did, did
		}
	}
	modeText := strings.ToLower(strings.TrimSpace(spec.Mode))
	cfg.LocalSigner = modeText == "local" || modeText == "localrepo" || (modeText == "" && !callback)
	switch modeText {
	case "callback":
		cfg.IdentityMode = IdentityModeCallback
	case "oauth", "oauthbrowser", "oauthqr":
		cfg.IdentityMode = IdentityModeOAuth
	}
	if cfg.IdentityMode == IdentityModeOAuth {
		cfg.RepoDID, cfg.MarketDID = cfg.DID, cfg.DID
	}
	cfg.PolicyWorkflow = DefaultPolicyWorkflow
	if spec.OwnerSessionsFile != "" {
		if cfg.IdentityMode == IdentityModeOAuth {
			return Config{}, errors.New("requester: ownerSessionsFile and the oauth identity mode exclude each other")
		}
		owners, did, handle, err := sshtransport.LoadOwnerSessions(spec.OwnerSessionsFile)
		if err != nil {
			return Config{}, err
		}
		session := owners[did]
		if strings.TrimSpace(session.Service) == "" {
			return Config{}, fmt.Errorf("requester: the owner session of %s names no service", did)
		}
		cfg.OwnerSessions = owners
		cfg.DID, cfg.Handle, cfg.RepoDID, cfg.MarketDID = did, handle, did, did
		cfg.PolicyWorkflow = "open"
		cfg.PDSURL = session.Service
		cfg.IdentityMode = IdentityModeCallback
		cfg.LocalSigner = false
	}
	cfg.BidWindow = seconds(spec.BidWindowSeconds)
	cfg.ReceiptTimeout = seconds(spec.ReceiptTimeoutSeconds)
	cfg.ReceiptPoll = time.Duration(ptr.Deref(spec.ReceiptPollMilliseconds, 0)) * time.Millisecond
	cfg.SessionCap = seconds(spec.SessionCapSeconds)
	cfg.SessionIdle = seconds(spec.SessionIdleSeconds)
	cfg.PolicyTimeout = seconds(spec.PolicyTimeoutSeconds)
	cfg.OAuthScopes = append([]string(nil), spec.OAuthScopes...)
	if len(cfg.OAuthScopes) == 0 {
		cfg.OAuthScopes = append([]string(nil), atprotooauth.RequesterOAuthScope...)
	}
	if spec.GuestSSHKeysFile != "" {
		keys, err := loadGuestSSHKeys(spec.GuestSSHKeysFile)
		if err != nil {
			return Config{}, err
		}
		cfg.GuestAuthorizedKeys = keys
	}
	return cfg, cfg.check()
}

func seconds(value *int32) time.Duration {
	return time.Duration(ptr.Deref(value, 0)) * time.Second
}

func (c Config) check() error {
	if c.DID == "" && c.Handle == "" && c.IdentityMode != IdentityModeOAuth {
		return errors.New("requester: the component names no identity DID or handle")
	}
	return nil
}

func (c Config) Network() v1alpha1.MarketNetworkValue {
	return ptr.Deref(c.Spec.Network, v1alpha1.MarketNetworkValue{})
}

func (c Config) Relays() []string {
	return append([]string(nil), c.Spec.Network.Relays()...)
}

func (c Config) DiscoveryRelays() []string {
	return c.Spec.Network.DiscoveryRelayURLs()
}

func (c Config) SealedSecrets() bool {
	return c.Spec.SecretsFallback == v1alpha1.SecretsFallbackSealed
}

func (c Config) InboundXRPC() bool {
	if c.Spec.Comms == nil {
		return true
	}
	switch c.Spec.Comms.Mode {
	case "", v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeBoth:
		return true
	}
	return false
}

func (c Config) dataDir() string {
	if c.Spec.Storage != nil && c.Spec.Storage.DataDir != "" {
		return c.Spec.Storage.DataDir
	}
	return c.StateDir
}

func (c Config) stateFile(name string) string {
	if dir := c.dataDir(); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

func loadGuestSSHKeys(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("requester: the guest SSH keys file %s: %w", path, err)
	}
	keys := []string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys, nil
}

func New(cfg Config, deps Deps) (*Requester, error) {
	switch {
	case deps.Intents == nil:
		return nil, errors.New("requester: the market intents handle is required")
	case deps.Inputs == nil:
		return nil, errors.New("requester: the market inputs are required")
	case deps.OpenRepo == nil:
		return nil, errors.New("requester: the record store needs a repo handle")
	case deps.Guests == nil:
		return nil, errors.New("requester: the guest identity checks are required")
	case deps.Directory == nil:
		return nil, errors.New("requester: the identity directory is required")
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	q := &Requester{
		cfg:            cfg,
		deps:           deps,
		did:            cfg.DID,
		handle:         cfg.Handle,
		repoDID:        cfg.RepoDID,
		marketDID:      cfg.MarketDID,
		pdsURL:         cfg.PDSURL,
		contracts:      participant.NewIndex[v1alpha1.ComputeContract](),
		syncs:          participant.NewIndex[v1alpha1.StateSync](),
		receipts:       participant.NewIndex[market.ResolvedRecord](),
		contractEvents: participant.NewIndex[v1alpha1.ContractEvent](),
		bids:           newBidQueue(),
		runners:        map[string]*contractRunner{},
		finished:       map[string]bool{},
		waiting:        map[string]bool{},
		settled:        map[string]bool{},
		perAccount:     map[string]int{},
		written:        map[string][]v1alpha1.ComputeContractEvent{},
		eventSinks:     map[string]chan MarketEvent{},
		undelivered:    map[string][]MarketEvent{},
		bidders:        map[string]Bidder{},
		scopeCache:     ttlcache.New[string, PolicyDecision](ttlcache.Options{TTL: 30 * time.Second}),
		secrets:        map[string]*secretsState{},
		heldSessions:   map[string]*heldSession{},
	}
	q.log = deps.Log.With("component", "requester", "did", q.did)
	return q, nil
}

func (q *Requester) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("requester: Start needs the run context")
	}
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return errors.New("requester: the role is already started")
	}
	q.started = true
	q.ctx = ctx
	q.mu.Unlock()
	if dir := q.cfg.dataDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("requester: state dir: %w", err)
		}
	}
	if name := q.marketExposure(); name != "" && q.marketURL == "" {
		if raw := q.deps.Inputs.AwaitAddress(ctx, name, MarketExposureWait, nil); raw != "" {
			q.marketURL = raw
		} else if err := ctx.Err(); err != nil {
			return err
		} else {
			q.log.Warn("the requester market URL is not resolved yet", "exposure", name)
		}
	}
	if q.cfg.IdentityMode == IdentityModeOAuth {
		if err := q.resolveOAuthIdentity(ctx); err != nil {
			return err
		}
		q.log = q.deps.Log.With("component", "requester", "did", q.did)
	}
	if err := q.resolveIdentity(ctx); err != nil {
		return err
	}
	if err := q.loadKeys(); err != nil {
		return err
	}
	if len(q.cfg.OwnerSessions) > 0 {
		if err := q.authenticateOwnerSession(ctx); err != nil {
			return err
		}
	}
	q.signer = market.NewSigner(q.attestationKey, q.marketDID)
	repo, err := participant.NewRepoStore(participant.RepoConfig{DID: q.repoDID, Directory: q.deps.Directory, HTTP: q.deps.HTTP, Open: q.openRepo})
	if err != nil {
		return fmt.Errorf("requester: the record store: %w", err)
	}
	q.repo = repo
	if q.deps.AwaitRepo != nil {
		q.deps.AwaitRepo(ctx, q.repoDID)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if len(q.cfg.OwnerSessions) > 0 || q.cfg.IdentityMode == IdentityModeOAuth {
		if err := q.bindSignerKey(ctx); err != nil {
			return err
		}
	}
	var serviceAuth ServiceAuthSource
	switch {
	case q.cfg.IdentityMode == IdentityModeOAuth:
		serviceAuth = q.oauthServiceAuth()
	case !q.cfg.LocalSigner && q.ownerServiceAuth != nil:
		serviceAuth = q.ownerServiceAuth
	}
	q.log.Info("requester_service_auth", "identityMode", q.cfg.IdentityMode, "localSigner", q.cfg.LocalSigner,
		"signing", serviceAuthName(serviceAuth))
	q.market = &HTTPMarket{
		Directory:   q.deps.Directory,
		Reader:      q.repo,
		RepoDID:     q.repoDID,
		Signer:      q.signer,
		ServiceKey:  q.repoKey,
		ServiceAuth: serviceAuth,
		RelayURLs:   q.cfg.DiscoveryRelays(),
		BidderDIDs:  append([]string(nil), q.cfg.Spec.BidderDIDs...),
		OperatorDID: strings.TrimSpace(q.cfg.Spec.OperatorDID),
		HTTPClient:  q.deps.HTTP,
		Timeout:     q.cfg.BidWindow,
		Log:         q.log,
	}
	outbox, err := participant.NewOutbox(q.deps.Intents, q.deps.Now,
		&marketInbox{q: q}, inboxAck{name: BidInboxAdapter}, inboxAck{name: InboxAdapter})
	if err != nil {
		return err
	}
	q.outbox = outbox
	q.policy = &PolicyEvaluator{Runs: q.policyRuns(), Workflow: PolicyWorkflowName(q.cfg.PolicyWorkflow), DID: q.marketDID}
	return nil
}

func (q *Requester) Shutdown(ctx context.Context) {
	_ = supervise.Join(ctx, q.wg.Wait)
	q.releaseAllHeldSessions("shutdown")
}

func (q *Requester) resolveIdentity(ctx context.Context) error {
	if q.pdsURL != "" && q.did != "" {
		return nil
	}
	if q.cfg.Spec.Identity == nil {
		return nil
	}
	ref := q.cfg.Spec.Identity.MarketIdentityRef
	if ref == nil || ref.Name == "" {
		return nil
	}
	found, ok := q.deps.Inputs.AwaitIdentity(ctx, IdentityWorkspaces(q.cfg.Workspace), ref.Name, IdentityWait)
	if !ok {
		return fmt.Errorf("requester: reading its MarketIdentity %s: not found", ref.Name)
	}
	if did := found.Status.Did; did != "" {
		q.did = did
		q.repoDID = cmp.Or(q.repoDID, did)
		q.marketDID = cmp.Or(q.marketDID, did)
	}
	if found.Status.PdsURL != "" {
		q.pdsURL = found.Status.PdsURL
	}
	q.handle = cmp.Or(q.handle, found.Status.Handle)
	return nil
}

func (q *Requester) loadKeys() error {
	if q.repoKey == nil {
		if keyPath := q.cfg.stateFile("repo-key.json"); keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: repo key: %w", err)
			}
			q.repoKey = signer.PrivateKey
		}
	}
	if q.attestationKey == nil {
		if keyPath := q.cfg.stateFile("attestation-key.json"); keyPath != "" {
			signer, err := market.LoadOrCreateSignerFile(keyPath)
			if err != nil {
				return fmt.Errorf("requester: attestation key: %w", err)
			}
			q.attestationKey = signer.PrivateKey
		}
	}
	if q.attestationKey == nil {
		if q.repoKey == nil {
			return errors.New("requester: no attestation or repo key")
		}
		q.attestationKey = q.repoKey
	}
	return nil
}

func (q *Requester) policyRuns() *runs.Client {
	return participant.NewPolicyRuns(participant.PolicyConfig{
		Resolve: q.deps.Policy,
		Owner:   policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: KindRequester, Name: q.cfg.Name, UID: q.cfg.UID},
		Engine:  runs.EngineRef(q.cfg.Spec.PolicyEngineRef, q.cfg.Workspace),
		Role:    "requester",
		Prefix:  "requester-policy",
		Timeout: q.cfg.PolicyTimeout,
		Parser:  runs.Strict,
	})
}

func (q *Requester) openRepo(ctx context.Context) (atprotorepo.Repo, error) {
	return q.deps.OpenRepo(ctx, q.repoDID)
}

func (q *Requester) marketExposure() string {
	if ref := q.cfg.Spec.MarketExposureRef; ref != nil {
		return strings.TrimSpace(ref.Name)
	}
	return ""
}

func (q *Requester) marketAddress() string {
	if name := q.marketExposure(); name != "" {
		if raw := q.deps.Inputs.Address(name); raw != "" {
			return raw
		}
	}
	return strings.TrimRight(strings.TrimSpace(q.marketURL), "/")
}

func (q *Requester) DID() string {
	return q.did
}

func (q *Requester) RepoDID() string {
	return q.repoDID
}

func (q *Requester) Handle() string {
	return q.handle
}

func (q *Requester) Mode() IdentityMode {
	return q.cfg.IdentityMode
}

func (q *Requester) Records() *market.RepoStore {
	return q.repo
}

func (q *Requester) now() time.Time {
	return q.deps.Now()
}

func (q *Requester) nowISO() string {
	return market.FormatTime(q.now())
}

func (q *Requester) runContext() context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ctx
}

func (q *Requester) stopping() bool {
	return q.runContext().Err() != nil
}

func (q *Requester) callbackDID() string {
	return cmp.Or(q.did, q.marketDID)
}

func (q *Requester) submitBidRef() string {
	return q.callbackRef(market.DefaultMarketServiceID)
}

func (q *Requester) submitEventRef() string {
	return q.callbackRef(market.DefaultComputeEventServiceID)
}

func (q *Requester) callbackRef(fragment string) string {
	if !q.cfg.InboundXRPC() {
		return ""
	}
	if address := q.marketAddress(); address != "" {
		return address
	}
	if name := q.marketExposure(); name != "" {
		if address := q.deps.Inputs.AwaitAddress(q.runContext(), name, MarketExposureWait, nil); address != "" {
			return strings.TrimRight(address, "/")
		}
	}
	if q.cfg.IdentityMode == IdentityModeOAuth || q.callbackDID() == "" {
		return ""
	}
	return q.callbackDID() + "#" + fragment
}

func (q *Requester) guestCAPEM() string {
	return strings.TrimSpace(q.cfg.Network().CABundle)
}

func (q *Requester) guestKeyDir() string {
	return q.cfg.stateFile("guest-keys")
}
