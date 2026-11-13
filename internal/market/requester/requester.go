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
	"sync/atomic"
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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/memsize"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
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

	heldReleaseAttempts = 6

	heldReleaseBackoffMax = 30 * time.Second

	transientContractPrefix = "ssh-run-"

	heldContractPrefix = "ssh-"

	heldRecordTimeout = 10 * time.Second

	signerBindVaultWait = 2 * time.Minute

	keepVMLabel = "socialweb.computer/ssh-keep-vm"

	HeldSessionPolicyTimeout = 30 * time.Second

	TransientResumeGiveUp = 2 * time.Minute

	DefaultReleaseRetryFirst = 5 * time.Second

	DefaultReleaseRetryMax = 5 * time.Minute

	DefaultReleaseBound = 24 * time.Hour

	DefaultHTTPTimeout = 30 * time.Second

	DefaultPolicyWorkflow = v1alpha1.RequesterDefaultPolicy

	SessionFullDeniesPerPick = 5

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

	CallbackWait = 5 * time.Minute

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

	AtircURL string

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

	WatchBinds func() (<-chan struct{}, func())

	Policy runs.ResolveFunc

	Vault func(ctx context.Context, ref commonv1.SecretRef) (vault.KV, error)

	VaultWrite func(ctx context.Context, ref commonv1.SecretRef) (vault.KV, error)

	ServiceAuth func(ctx context.Context, identity commonv1.ObjectRef, did string) (pds.ServiceAuth, error)

	Guests Guests

	ContractEvents contractmarket.ContractEvents

	Ingress tunnel.Service

	Directory identity.Directory

	HTTP *http.Client

	EndpointKinds []string

	Now func() time.Time

	Log *slog.Logger
}

type Requester struct {
	cfg Config

	inboxed sync.Map

	synced atomic.Bool

	startedAt atomic.Pointer[time.Time]

	firstResume atomic.Bool

	prunedForeign atomic.Int64

	prunedEnded atomic.Int64

	deps Deps

	log *slog.Logger

	did string

	handle string

	repoDID string

	marketDID string

	pdsURL string

	repoKey atcrypto.PrivateKey

	attestationKey atcrypto.PrivateKey

	repo *market.RepoStore

	signer *market.Signer

	identityRef commonv1.ObjectRef

	ownerServiceAuth ServiceAuthSource

	serviceAuth ServiceAuthSource

	atircAuth ServiceAuthSource

	market MarketClient

	policy *PolicyEvaluator

	outbox *participant.Outbox

	contracts *participant.Index[v1alpha1.ComputeContract]

	foreignContracts sync.Map

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

	releaseRevoking sync.Map

	releaseRetried sync.Map

	bundleLocks sync.Map

	atircIndexMu sync.Mutex

	atircMintRetrying sync.Map

	atircRevokesStarted sync.Map

	orphanSweep sync.Once

	runnerEnded chan struct{}

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

	secretsMu sync.Mutex

	secrets map[string]*secretsState

	sealMu sync.Mutex

	heldMu sync.Mutex

	heldSessions map[string]*heldSession

	transientMu sync.Mutex

	transient map[string]context.CancelFunc

	transientWG sync.WaitGroup

	staleSweep sync.Once

	ingressMu sync.Mutex

	ingressRef *market.StrongRef

	ingressScopeSkipped atomic.Bool
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
		if want := strings.TrimSpace(cfg.DID); want != "" {
			if own, ok := owners[want]; ok {
				did, handle = want, own.Identifier
			}
		}
		session := owners[did]
		if strings.TrimSpace(session.Service) == "" {
			return Config{}, fmt.Errorf("requester: the owner session of %s names no service", did)
		}
		cfg.OwnerSessions = owners
		cfg.DID, cfg.Handle, cfg.RepoDID, cfg.MarketDID = did, handle, did, did
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

func (c Config) WebRTCIngress() bool {
	return c.InboundXRPC() && c.Spec.Ingress != nil && c.Spec.Ingress.ProviderName() == v1alpha1.IngressProviderWebRTC
}

func (c Config) WebRTCIngressOptions() tunnel.ServiceOptions {
	opts := tunnel.ServiceOptions{STUN: c.WebRTCSTUN()}
	if c.Spec.Ingress != nil && c.Spec.Ingress.WebRTC != nil {
		webRTC := c.Spec.Ingress.WebRTC
		if seconds := ptr.Deref(webRTC.SessionTTLSeconds, 0); seconds > 0 {
			opts.SessionTTL = time.Duration(seconds) * time.Second
		}
		if sessions := ptr.Deref(webRTC.MaxSessions, 0); sessions > 0 {
			opts.MaxSessions = int(sessions)
		}
	}
	return opts
}

func (c Config) WebRTCSTUN() []string {
	if c.Spec.Ingress != nil && c.Spec.Ingress.WebRTC != nil {
		if urls := cleanedSTUN(c.Spec.Ingress.WebRTC.STUNURLs); len(urls) > 0 {
			return urls
		}
	}
	return []string{tunnel.DefaultSTUN}
}

func cleanedSTUN(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	return out
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
		secrets:        map[string]*secretsState{},
		heldSessions:   map[string]*heldSession{},
		runnerEnded:    make(chan struct{}, 1),
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
	startedAt := time.Now()
	q.startedAt.Store(&startedAt)
	q.ctx = ctx
	q.mu.Unlock()
	if dir := q.cfg.dataDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("requester: state dir: %w", err)
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
	identityMs := time.Since(startedAt).Milliseconds()
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
	repoMs := time.Since(startedAt).Milliseconds()
	if len(q.cfg.OwnerSessions) > 0 || q.cfg.IdentityMode == IdentityModeOAuth || (q.cfg.IdentityMode == IdentityModeCallback && !q.cfg.LocalSigner) {
		if err := q.bindSignerKeyAfterBinds(ctx); err != nil {
			return err
		}
	}
	var serviceAuth ServiceAuthSource
	switch {
	case q.cfg.IdentityMode == IdentityModeOAuth:
		serviceAuth = q.oauthServiceAuth()
	case !q.cfg.LocalSigner && q.ownerServiceAuth != nil:
		serviceAuth = q.ownerServiceAuth
	default:
		serviceAuth = q.ownerIdentityServiceAuth()
	}
	q.serviceAuth = serviceAuth
	atircAuth, atircSigning, atircReason := q.atircServiceAuth(serviceAuth)
	q.atircAuth = atircAuth
	q.log.Info("requester_service_auth", "identityMode", q.cfg.IdentityMode, "localSigner", q.cfg.LocalSigner,
		"signing", serviceAuthName(serviceAuth), "atircSigning", atircSigning, "atircReason", atircReason)
	q.market = &HTTPMarket{
		Directory:       q.deps.Directory,
		Reader:          q.repo,
		RepoDID:         q.repoDID,
		Signer:          q.signer,
		ServiceKey:      q.repoKey,
		ServiceAuth:     serviceAuth,
		RelayURLs:       q.cfg.DiscoveryRelays(),
		BidderDIDs:      append([]string(nil), q.cfg.Spec.BidderDIDs...),
		OperatorDID:     strings.TrimSpace(q.cfg.Spec.OperatorDID),
		Trust:           q.trustResolver(),
		Ingress:         q.ingress(),
		IngressRef:      q.ingressDescriptor,
		IngressWritable: q.ingressWritable,
		IngressSTUN:     q.cfg.WebRTCSTUN(),
		HTTPClient:      q.marketHTTP(),
		EndpointKinds:   q.deps.EndpointKinds,
		Timeout:         q.cfg.BidWindow,
		Log:             q.log,
		StartedAt:       startedAt,
	}
	outbox, err := participant.NewOutbox(q.deps.Intents, q.deps.Now,
		&marketInbox{q: q}, inboxAck{name: BidInboxAdapter}, inboxAck{name: InboxAdapter})
	if err != nil {
		return err
	}
	q.outbox = outbox
	q.policy = &PolicyEvaluator{Runs: q.policyRuns(), Workflow: PolicyWorkflowName(q.cfg.PolicyWorkflow), DID: q.marketDID}
	q.log.Info("requester_start_timing", "step", "started", "sinceStartMs", time.Since(startedAt).Milliseconds(),
		"identityMs", identityMs, "repoMs", repoMs)
	return nil
}

func (q *Requester) sinceStartMs() (int64, bool) {
	startedAt := q.startedAt.Load()
	if startedAt == nil {
		return 0, false
	}
	return time.Since(*startedAt).Milliseconds(), true
}

func (q *Requester) noteFirstResume(contract string, readMs int64) {
	since, ok := q.sinceStartMs()
	if !ok || q.firstResume.Swap(true) {
		return
	}
	q.log.Info("requester_start_timing", "step", "firstContractResumed", "contract", contract,
		"sinceStartMs", since, "terminationReadMs", readMs)
}

func (q *Requester) Shutdown(ctx context.Context) {
	q.endTransientSessions(ctx, "shutdown")
	_ = supervise.Join(ctx, q.wg.Wait)
	q.keepHeldSessions("shutdown; the next start adopts it until its recorded expiry")
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
	q.noteIdentityRef(ref.Name)
	return nil
}

func (q *Requester) noteIdentityRef(name string) {
	if q.identityRef.Name != "" {
		return
	}
	for _, ws := range IdentityWorkspaces(q.cfg.Workspace) {
		if _, has := q.deps.Inputs.Identity(ws, name); has {
			q.identityRef = commonv1.ObjectRef{Name: name, Workspace: ws}
			return
		}
	}
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
		Resolve:     q.deps.Policy,
		Owner:       policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: KindRequester, Name: q.cfg.Name, UID: q.cfg.UID},
		Engine:      runs.EngineRef(q.cfg.Spec.PolicyEngineRef, q.cfg.Workspace),
		Role:        "requester",
		Prefix:      "requester-policy",
		Timeout:     q.cfg.PolicyTimeout,
		Parser:      runs.Strict,
		Association: q.cfg.Spec.Association,
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
		return q.deps.Inputs.Address(name)
	}
	return ""
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

func (q *Requester) submitBidRef(ctx context.Context) (string, error) {
	return q.callbackRef(ctx, market.DefaultMarketServiceID)
}

func (q *Requester) submitEventRef(ctx context.Context) (string, error) {
	return q.callbackRef(ctx, market.DefaultComputeEventServiceID)
}

var errNoCallback = errors.New("requester: the market Exposure has no address, and in XRPC comms mode no bidder could reach a record without it")

func (q *Requester) callbackRef(ctx context.Context, fragment string) (string, error) {
	if !q.cfg.InboundXRPC() {
		return "", nil
	}
	if address := q.marketAddress(); address != "" {
		return address, nil
	}
	xrpcOnly := q.cfg.Spec.Comms != nil && q.cfg.Spec.Comms.Mode == v1alpha1.MarketCommsModeXRPC
	if name := q.marketExposure(); name != "" {
		deadline := time.Now().Add(CallbackWait)
		for attempt := 1; ; attempt++ {
			if address := q.deps.Inputs.AwaitAddress(ctx, name, MarketExposureWait, nil); address != "" {
				return strings.TrimRight(address, "/"), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !xrpcOnly {
				break
			}
			if !time.Now().Before(deadline) {
				return "", fmt.Errorf("%w (%s, after %s)", errNoCallback, name, CallbackWait)
			}
			q.log.Warn("requester_callback_waits", "exposure", name, "attempt", attempt)
		}
	}
	if q.cfg.IdentityMode == IdentityModeOAuth || q.callbackDID() == "" {
		if xrpcOnly {
			return "", errNoCallback
		}
		return "", nil
	}
	return q.callbackDID() + "#" + fragment, nil
}

func (q *Requester) ingress() tunnel.Service {
	if !q.cfg.WebRTCIngress() {
		return nil
	}
	return q.deps.Ingress
}

func (q *Requester) ingressDescriptor(ctx context.Context) *market.RecordRef {
	if q.ingress() == nil || q.repo == nil {
		return nil
	}
	id := tunnel.ID(q.repoDID)
	if id == "" {
		return nil
	}
	q.ingressMu.Lock()
	cached := q.ingressRef
	q.ingressMu.Unlock()
	if cached != nil {
		return &market.RecordRef{URI: cached.URI, CID: cached.CID}
	}
	if !q.ingressWritable(ctx) {
		return nil
	}
	value, err := market.EncodeRecord(market.IngressWebRTC{Kind: tunnel.KindWebRTC, Mode: market.IngressModeListen, ID: id, StunUrls: q.cfg.WebRTCSTUN(), CreatedAt: q.nowISO()})
	if err != nil {
		q.log.Warn("requester_webrtc_descriptor_failed", "err", err)
		return nil
	}
	ref, err := participant.EnsureRecordAt(ctx, q.repo, q.repoDID, market.IngressWebRTCNSID, "webrtc", value)
	if err != nil {
		q.log.Warn("requester_webrtc_descriptor_failed", "err", err)
		return nil
	}
	q.ingressMu.Lock()
	q.ingressRef = &ref
	q.ingressMu.Unlock()
	q.log.Info("requester_webrtc_descriptor", "uri", ref.URI, "cid", ref.CID)
	return &market.RecordRef{URI: ref.URI, CID: ref.CID}
}

func (q *Requester) ingressWritable(ctx context.Context) bool {
	if q.cfg.IdentityMode != IdentityModeOAuth {
		return true
	}
	granted, known, err := q.oauthGrants(ctx)
	if err != nil || !known || len(atprotooauth.MissingScopes([]string{atprotooauth.RepoIngressWebRTCCreate}, granted)) == 0 {
		q.ingressScopeSkipped.Store(false)
		return true
	}
	if !q.ingressScopeSkipped.Swap(true) {
		q.log.Info("requester_webrtc_ingress_skipped", "reason", "the oauth session was not granted "+atprotooauth.RepoIngressWebRTCCreate)
	}
	return false
}

func (q *Requester) ingressRecords(ctx context.Context) []market.RecordRef {
	ref := q.ingressDescriptor(ctx)
	if ref == nil {
		return nil
	}
	return []market.RecordRef{*ref}
}

func (q *Requester) IngressRecords(ctx context.Context) []market.StrongRef {
	refs := q.ingressRecords(ctx)
	out := make([]market.StrongRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, market.NewStrongRef(ref.URI, ref.CID))
	}
	return out
}

func (q *Requester) guestCAPEM() string {
	return strings.TrimSpace(q.cfg.Network().CABundle)
}

func (q *Requester) guestKeyDir() string {
	return q.cfg.stateFile("guest-keys")
}

func (q *Requester) bundleLock(contract string) *sync.Mutex {
	lock, _ := q.bundleLocks.LoadOrStore(contract, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func CheckVMSize(t v1alpha1.ComputeContractTemplate) error {
	if t.Cpus < 0 {
		return fmt.Errorf("cpus %d is not a vCPU count", t.Cpus)
	}
	if mib, err := memsize.ParseMiB(t.Mem); err != nil || (mib == 0 && strings.TrimSpace(t.Mem) != "") {
		return fmt.Errorf("mem %q is not a memory size such as 8G", t.Mem)
	}
	return nil
}
