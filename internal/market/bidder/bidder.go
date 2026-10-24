package bidder

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	BidTTL = "24h"

	BidTTLSeconds int32 = 86400

	WonUnboundWait = time.Hour

	PublishStallThreshold = 3

	StatusMessageLimit = 512

	BidConditionReleasing = "Releasing"

	ProvisionSyncAdapter = "compute.provision"

	ReleaseSyncAdapter = "compute.release"

	MarketInboxAdapter = "bidder.market-inbox"

	DefaultRetry = 30 * time.Second

	BidSubmitRetry = 2 * time.Second

	BidSubmitRetryCap = time.Minute

	BidSubmitWindow = 10 * time.Minute

	BidSubmitTimeout = 10 * time.Second

	AwaitTimeout = 5 * time.Second

	adapterLabel = "statesync.socialweb.computer/adapter"

	releaseReason = "deleted_on_request"
)

var (
	ErrSuspended = errors.New("bidder: the bidder's participation is suspended")

	ErrEventForbidden = errors.New("bidder: not the accept author")

	ErrEventInvalid = errors.New("bidder: the market event is invalid")
)

type Config struct {
	DID string

	Workspace string

	ClaimedBy string

	ProviderHandle string

	GuestURL string

	EndpointURL string

	InboundXRPC bool

	ScopePolicy string

	GuestTLSPort int

	GuestVaultServer string

	GuestVaultMount string

	GuestLoginMount string

	StopGraceSeconds int32

	MaxConcurrentGuests int32

	Heartbeat time.Duration

	OfferingRefresh time.Duration

	BidTTL time.Duration

	Suspend bool

	Relays []string
}

type GuestVM struct {
	Name string

	Handle string

	Shape v1alpha1.ComputeVMShape

	RequesterDID string

	VaultServer string

	VaultMount string

	StopGraceSeconds int32

	Login *v1alpha1.ComputeVMGuestLogin

	Accept commonv1.StrongRef

	Owner *metav1.OwnerReference
}

type Guests interface {
	AcceptKey(uri, cid string) string

	AcceptKeyOf(vm *v1alpha1.ComputeVM) string

	Ended(vm *v1alpha1.ComputeVM) bool

	Releasing(vm *v1alpha1.ComputeVM) bool

	ReleaseRequested(vm *v1alpha1.ComputeVM) bool

	ReleaseRequestRef(vm *v1alpha1.ComputeVM) *commonv1.StrongRef

	GuestReport(vm *v1alpha1.ComputeVM) *v1alpha1.ComputeVMGuestReport

	Published(vm *v1alpha1.ComputeVM) *v1alpha1.ComputeVMPublished

	ComputeVM(in GuestVM) (*v1alpha1.ComputeVM, error)

	UserData(name, userData string) (string, map[string]any)

	LoginSecret(acceptKey string) (string, string, error)

	Audience(name string) string

	Grant(bidderDID, requesterDID, role, name string) (v1alpha1.GuestIdentityGrant, error)

	BidConfig(issuerURL, bidderDID, acceptPath, createdAt string) (string, any, error)

	Termination(vm *v1alpha1.ComputeVM, workspace, reason, observedAt string, request *commonv1.StrongRef) (v1alpha1.StateSync, error)

	EventAdapter() string

	SyncLabels(vm *v1alpha1.ComputeVM) map[string]string

	SyncVM(sync *v1alpha1.StateSync) string
}

type Deps struct {
	Intents contract.Intents

	Records *market.RepoStore

	Signer *market.Signer

	Vault vault.KV

	Guests Guests

	Tokens *participant.Tokens

	HTTP *http.Client

	Directory identity.Directory

	Policy *runs.Client

	Log *slog.Logger

	Now func() time.Time
}

type Bidder struct {
	cfg Config

	intents contract.Intents

	records *market.RepoStore

	signer *market.Signer

	vault vault.KV

	guests Guests

	outbound *participant.Outbound

	verifier participant.Verifier

	directory identity.Directory

	policy *policyEvaluator

	outbox *participant.Outbox

	log *slog.Logger

	now func() time.Time

	synced atomic.Bool

	bids *participant.Index[v1alpha1.Bid]

	vms *participant.Index[v1alpha1.ComputeVM]

	syncs *participant.Index[v1alpha1.StateSync]

	lockMu sync.Mutex

	locks map[string]*sync.Mutex

	retryMu sync.Mutex

	retryAt map[string]time.Time

	verified sync.Map

	released sync.Map

	inboxed sync.Map

	offerMu sync.Mutex

	offerWrite sync.Mutex

	offeringURI string

	badgeBound atomic.Bool

	badgeMu sync.Mutex

	wg sync.WaitGroup
}

var _ contract.Observer = (*Bidder)(nil)

func New(cfg Config, deps Deps) (*Bidder, error) {
	switch {
	case strings.TrimSpace(cfg.DID) == "":
		return nil, errors.New("bidder: the bidder names no DID")
	case deps.Intents == nil:
		return nil, errors.New("bidder: the bidder needs the market intents handle")
	case deps.Records == nil:
		return nil, errors.New("bidder: the bidder needs a market record store")
	case deps.Signer == nil:
		return nil, errors.New("bidder: the bidder needs an attestation key")
	case deps.Guests == nil:
		return nil, errors.New("bidder: the bidder needs the guest helpers")
	case strings.TrimSpace(cfg.GuestVaultServer) == "" || strings.Trim(cfg.GuestVaultMount, "/") == "":
		return nil, errors.New("bidder: the bidder names no guest vault")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	b := &Bidder{
		cfg:       cfg,
		intents:   deps.Intents,
		records:   deps.Records,
		signer:    deps.Signer,
		vault:     deps.Vault,
		guests:    deps.Guests,
		outbound:  &participant.Outbound{HTTP: deps.HTTP, Tokens: deps.Tokens, Log: log},
		verifier:  participant.Verifier{Directory: deps.Directory, Records: deps.Records},
		directory: deps.Directory,
		policy:    &policyEvaluator{runs: deps.Policy, scope: cfg.ScopePolicy, records: deps.Records},
		log:       log,
		now:       now,
		bids:      participant.NewIndex[v1alpha1.Bid](),
		vms:       participant.NewIndex[v1alpha1.ComputeVM](),
		syncs:     participant.NewIndex[v1alpha1.StateSync](),
		locks:     map[string]*sync.Mutex{},
		retryAt:   map[string]time.Time{},
	}
	outbox, err := participant.NewOutbox(deps.Intents, now, b.eventAdapter(), b.provisionAdapter(), b.releaseAdapter(), b.inboxAdapter())
	if err != nil {
		return nil, err
	}
	b.outbox = outbox
	return b, nil
}

func (b *Bidder) Observe(ctx context.Context, change contract.Change) (time.Duration, error) {
	switch change.Kind {
	case contract.KindBid:
		b.indexBid(change)
	case contract.KindComputeVM:
		if change.Gone || change.ComputeVM == nil {
			b.vms.Remove(change.Name)
		} else {
			b.vms.Put(change.Name, *change.ComputeVM)
		}
	case contract.KindStateSync:
		if change.Gone || change.StateSync == nil {
			if old, ok := b.syncs.Get(change.Name); ok {
				b.inboxed.Delete(string(old.UID) + "#" + old.Spec.IdempotencyKey)
			}
			b.syncs.Remove(change.Name)
		} else {
			b.syncs.Put(change.Name, *change.StateSync)
		}
	default:
		return 0, nil
	}
	if !b.synced.Load() {
		return 0, nil
	}
	switch change.Kind {
	case contract.KindBid:
		if change.Gone {
			return 0, nil
		}
		return b.reconcileBid(ctx, change.Name)
	case contract.KindComputeVM:
		return b.observeVM(ctx, change.Name)
	default:
		return b.observeSync(ctx, change.Name)
	}
}

func (b *Bidder) indexBid(change contract.Change) {
	if change.Gone || change.Bid == nil {
		b.bids.Remove(change.Name)
		b.verified.Delete(change.Name)
		b.retryMu.Lock()
		delete(b.retryAt, change.Name)
		b.retryMu.Unlock()
		return
	}
	incoming := *change.Bid
	if held, ok := b.bids.Get(change.Name); ok && held.UID == incoming.UID {
		if held.Generation > incoming.Generation {
			incoming.Spec = held.Spec
			incoming.Generation = held.Generation
		}
		if submitAhead(held.Status.Submit, incoming.Status.Submit) {
			incoming.Status = cloneBidStatus(incoming.Status)
			incoming.Status.Submit = held.Status.Submit
			if condition := apimeta.FindStatusCondition(held.Status.Conditions, v1alpha1.BidConditionSubmitted); condition != nil {
				apimeta.SetStatusCondition(&incoming.Status.Conditions, *condition)
			}
		}
	}
	b.bids.Put(change.Name, incoming)
}

func submitAhead(held, incoming *v1alpha1.BidSubmit) bool {
	if held == nil {
		return false
	}
	if incoming == nil {
		return true
	}
	return held.Attempts > incoming.Attempts || (held.SubmittedAt != "" && incoming.SubmittedAt == "")
}

func (b *Bidder) Synced() {
	b.synced.Store(true)
}

func (b *Bidder) observeVM(ctx context.Context, name string) (time.Duration, error) {
	vm, live := b.vms.Get(name)
	if live {
		b.report(ctx, &vm)
	}
	var related []string
	for _, bid := range b.bids.List() {
		if bid.Status.ComputeVM == name || (live && b.bidServes(&bid, &vm)) {
			related = append(related, bid.Name)
		}
	}
	return b.reconcileBids(ctx, related)
}

func (b *Bidder) bidServes(bid *v1alpha1.Bid, vm *v1alpha1.ComputeVM) bool {
	for _, owner := range vm.OwnerReferences {
		if owner.Kind == contract.KindBid && owner.Name == bid.Name {
			return true
		}
	}
	accept := bid.Status.Accept
	return accept != nil && b.guests.AcceptKey(accept.URI, accept.CID) != "" && b.guests.AcceptKey(accept.URI, accept.CID) == b.guests.AcceptKeyOf(vm)
}

func (b *Bidder) observeSync(ctx context.Context, name string) (time.Duration, error) {
	sync, ok := b.syncs.Get(name)
	if !ok {
		return 0, nil
	}
	var wait time.Duration
	var errs []error
	if b.outbox.Handles(&sync) {
		d, err := b.outbox.Reconcile(ctx, &sync)
		wait = earliest(wait, d)
		errs = append(errs, err)
	}
	if vmName := b.guests.SyncVM(&sync); vmName != "" {
		if vm, live := b.vms.Get(vmName); live {
			b.report(ctx, &vm)
		}
		var related []string
		for _, bid := range b.bids.List() {
			if bid.Status.ComputeVM == vmName {
				related = append(related, bid.Name)
			}
		}
		d, err := b.reconcileBids(ctx, related)
		wait = earliest(wait, d)
		errs = append(errs, err)
	}
	return wait, errors.Join(errs...)
}

func (b *Bidder) reconcileBids(ctx context.Context, names []string) (time.Duration, error) {
	var wait time.Duration
	var errs []error
	for _, name := range names {
		d, err := b.reconcileBid(ctx, name)
		wait = earliest(wait, d)
		errs = append(errs, err)
	}
	return wait, errors.Join(errs...)
}

func earliest(a, b time.Duration) time.Duration {
	if b > 0 && (a == 0 || b < a) {
		return b
	}
	return a
}

func (b *Bidder) lockBid(name string) func() {
	b.lockMu.Lock()
	lock := b.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		b.locks[name] = lock
	}
	b.lockMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (b *Bidder) retryWait(name string) time.Duration {
	b.retryMu.Lock()
	defer b.retryMu.Unlock()
	at, ok := b.retryAt[name]
	if !ok {
		return 0
	}
	wait := at.Sub(b.now())
	if wait <= 0 {
		delete(b.retryAt, name)
		return 0
	}
	return wait
}

func (b *Bidder) retryLater(name string) {
	b.retryMu.Lock()
	b.retryAt[name] = b.now().Add(DefaultRetry)
	b.retryMu.Unlock()
}

func (b *Bidder) DID() string {
	return b.cfg.DID
}
