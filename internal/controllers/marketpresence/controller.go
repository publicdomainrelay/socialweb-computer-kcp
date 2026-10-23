package marketpresence

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	presence "github.com/johnandersen777/socialweb-computer-kcp/internal/marketpresence"
)

const (
	Name = "marketpresence"

	Export = "applications"

	BidderKind = "Bidder"

	ConditionReady = "Ready"

	ConditionBidderReady = "BidderReady"

	ConditionHeartbeat = "Heartbeat"

	DefaultRefresh = time.Minute
)

var errOtherWorkspace = errors.New("the bidderRef names another workspace and no cross-workspace Bidder reader is configured")

type Heartbeats interface {
	Observe(ctx context.Context, q presence.Query) presence.Observation
}

type Bidders interface {
	Get(ctx context.Context, workspace, name string) (*v1alpha1.Bidder, error)
}

var _ Heartbeats = (*presence.Heartbeats)(nil)

type Options struct {
	Heartbeats Heartbeats

	Bidders Bidders

	Scope func(workspace string) bool

	Refresh time.Duration

	Now func() time.Time

	Log *slog.Logger
}

type Controller struct {
	heartbeats Heartbeats

	bidders Bidders

	refresh time.Duration

	now func() time.Time

	log *slog.Logger

	refs *binding.Table[struct{}]
}

func New(opts Options) (*Controller, error) {
	if opts.Heartbeats == nil {
		return nil, errors.New("marketpresence: a heartbeat reader is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Controller{heartbeats: opts.Heartbeats, bidders: opts.Bidders, refresh: ctlruntime.Period(opts.Refresh, DefaultRefresh), now: now,
		log: log.With("component", "marketpresence-controller"), refs: binding.New[struct{}]()}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Watches(ctl.presencesOf), ctlruntime.Gone(ctl.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.MarketPresence](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("marketpresence: registering the MarketPresence controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) presencesOf(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Bidder) []ctlruntime.Request {
	derived := presence.PresenceName(obj.Name)
	out := []ctlruntime.Request{{Cluster: src.Cluster, Workspace: src.Workspace, Name: derived}}
	for _, t := range c.refs.Users(binding.Key(src.Workspace, BidderKind, obj.Name)) {
		if t.Cluster == src.Cluster && t.Name == derived {
			continue
		}
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.MarketPresence) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.refs.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
	ref := object.Spec.BidderRef
	workspace := cmp.Or(strings.TrimSpace(ref.Workspace), req.Workspace)
	kind := cmp.Or(strings.TrimSpace(ref.Kind), BidderKind)
	bidderName := strings.TrimSpace(ref.Name)
	c.refs.Set(target, &struct{}{}, binding.Key(workspace, kind, bidderName))

	gen := object.Generation
	status := object.Status
	status.Conditions = append([]metav1.Condition(nil), object.Status.Conditions...)
	status.ObservedGeneration = gen

	if !strings.EqualFold(kind, BidderKind) || bidderName == "" {
		message := "the bidderRef must name a Bidder"
		conds.Set(&status.Conditions, gen, ConditionBidderReady, false, "BidderRefInvalid", message)
		expire(&status, gen, "BidderRefInvalid", message)
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	bidder, err := c.bidder(ctx, req, workspace, bidderName)
	switch {
	case apierrors.IsNotFound(err):
		if workspace == req.Workspace && ownedBy(object, bidderName) {
			c.log.Info("a MarketPresence goes with its gone Bidder", "workspace", req.Workspace, "marketpresence", req.Name)
			return ctlruntime.Result{}, req.Delete(ctx, object)
		}
		message := "the Bidder named by the MarketPresence does not exist"
		conds.Set(&status.Conditions, gen, ConditionBidderReady, false, "BidderMissing", message)
		expire(&status, gen, "BidderMissing", message)
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	case errors.Is(err, errOtherWorkspace):
		conds.Set(&status.Conditions, gen, ConditionBidderReady, false, "BidderUnreadable", err.Error())
		expire(&status, gen, "BidderUnreadable", err.Error())
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	case err != nil:
		message := "the Bidder named by the MarketPresence could not be read"
		conds.Set(&status.Conditions, gen, ConditionBidderReady, false, "BidderUnreadable", message)
		expire(&status, gen, "BidderUnreadable", message)
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("marketpresence: reading the Bidder %s/%s: %w", workspace, bidderName, err),
			c.apply(ctx, req, object, status))
	}

	bidderReady, phase := readiness(bidder)
	if bidderReady {
		conds.Set(&status.Conditions, gen, ConditionBidderReady, true, "BidderReady", "the Bidder reports Ready")
	} else {
		message := "the Bidder is not Ready"
		if phase != "" && phase != string(v1alpha1.ComponentPhaseReady) {
			message = "the Bidder is " + phase
		}
		conds.Set(&status.Conditions, gen, ConditionBidderReady, false, "BidderNotReady", message)
	}

	offering := strings.TrimSpace(object.Spec.OfferingURI)
	did := bidderDID(bidder)
	if offering != "" {
		aturi, err := syntax.ParseATURI(offering)
		if err != nil || aturi.Collection() == "" || aturi.RecordKey() == "" {
			expire(&status, gen, "OfferingInvalid", "spec.offeringUri is not an atproto record URI")
			return ctlruntime.Result{}, c.apply(ctx, req, object, status)
		}
		if did == "" {
			did = string(aturi.Authority())
		}
	}

	now := c.now().UTC()
	ttl := opt.Positive(time.Duration(object.Spec.TtlSeconds)*time.Second, market.HeartbeatTTL)
	plc := ""
	if bidder.Spec.Network != nil {
		plc = bidder.Spec.Network.PLCDirectoryURL
	}
	observed := c.heartbeats.Observe(ctx, presence.Query{DID: did, OfferingURI: offering, PLCDirectoryURL: plc, Now: now})
	last := status.LastHeartbeatAt
	if !observed.At.IsZero() {
		if prior, err := time.Parse(time.RFC3339, last); err != nil || observed.At.After(prior) {
			last = observed.At.UTC().Format(time.RFC3339)
		}
	}
	status.LastHeartbeatAt = last

	fresh := false
	if at, err := time.Parse(time.RFC3339, last); err == nil {
		age := now.Sub(at)
		fresh = age >= -market.HeartbeatMaxSkew && age <= ttl
	}

	switch {
	case !observed.At.IsZero():
		conds.Set(&status.Conditions, gen, ConditionHeartbeat, true, "HeartbeatFresh", "the market heartbeat is readable")
	case fresh:
		conds.Set(&status.Conditions, gen, ConditionHeartbeat, true, "HeartbeatCached",
			"the market heartbeat is unreadable; the last observed heartbeat is within the TTL")
	default:
		conds.Set(&status.Conditions, gen, ConditionHeartbeat, false, observed.Reason, "no fresh market heartbeat is readable")
	}

	status.Phase = v1alpha1.MarketPresencePhaseExpired
	status.Ready = false
	switch {
	case bidderReady && fresh:
		status.Phase = v1alpha1.MarketPresencePhaseActive
		status.Ready = true
		conds.Set(&status.Conditions, gen, ConditionReady, true, "Active", "the Bidder is Ready and its heartbeat is within the TTL")
	case !fresh && last != "":
		conds.Set(&status.Conditions, gen, ConditionReady, false, "HeartbeatExpired", "the last market heartbeat is older than ttlSeconds")
	case !fresh:
		conds.Set(&status.Conditions, gen, ConditionReady, false, observed.Reason, "no fresh market heartbeat is readable")
	default:
		conds.Set(&status.Conditions, gen, ConditionReady, false, "BidderNotReady", "the Bidder is not Ready")
	}

	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	if did == "" {
		return ctlruntime.Result{}, nil
	}
	return ctlruntime.RefreshAfter(c.refresh), nil
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
	if !strings.HasPrefix(req.Name, "bidder-") {
		return
	}
	bidder, err := c.derivedBidder(ctx, req)
	if err != nil {
		c.log.Warn("finding the Bidder of a missing MarketPresence", "workspace", req.Workspace, "marketpresence", req.Name, "err", err.Error())
		return
	}
	if bidder == nil {
		return
	}
	referenced, err := c.referenced(ctx, req, bidder.Name)
	if err != nil {
		c.log.Warn("listing the MarketPresences of a Bidder", "workspace", req.Workspace, "bidder", bidder.Name, "err", err.Error())
		return
	}
	if referenced {
		return
	}
	owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: BidderKind, Name: bidder.Name, UID: bidder.UID}
	next := &v1alpha1.MarketPresence{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: v1alpha1.MarketPresenceSpec{
			BidderRef:  commonv1.ObjectRef{Workspace: req.Workspace, Kind: BidderKind, Name: bidder.Name},
			TtlSeconds: int(market.HeartbeatTTL.Seconds()),
		},
	}
	if err := req.Apply(ctx, next); err != nil {
		c.log.Warn("applying the MarketPresence of a Bidder", "workspace", req.Workspace, "bidder", bidder.Name, "err", err.Error())
		return
	}
	c.log.Info("a MarketPresence follows its Bidder", "workspace", req.Workspace, "bidder", bidder.Name, "marketpresence", req.Name)
}

func (c *Controller) derivedBidder(ctx context.Context, req ctlruntime.Request) (*v1alpha1.Bidder, error) {
	var list v1alpha1.BidderList
	if err := req.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if presence.PresenceName(item.Name) == req.Name && item.DeletionTimestamp.IsZero() {
			return item, nil
		}
	}
	return nil, nil
}

func (c *Controller) referenced(ctx context.Context, req ctlruntime.Request, bidderName string) (bool, error) {
	if len(c.refs.Users(binding.Key(req.Workspace, BidderKind, bidderName))) > 0 {
		return true, nil
	}
	var list v1alpha1.MarketPresenceList
	if err := req.List(ctx, &list); err != nil {
		return false, err
	}
	for i := range list.Items {
		ref := list.Items[i].Spec.BidderRef
		if cmp.Or(strings.TrimSpace(ref.Workspace), req.Workspace) == req.Workspace &&
			strings.EqualFold(cmp.Or(strings.TrimSpace(ref.Kind), BidderKind), BidderKind) && strings.TrimSpace(ref.Name) == bidderName {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) bidder(ctx context.Context, req ctlruntime.Request, workspace, name string) (*v1alpha1.Bidder, error) {
	if workspace != req.Workspace {
		if c.bidders == nil {
			return nil, errOtherWorkspace
		}
		return c.bidders.Get(ctx, workspace, name)
	}
	var bidder v1alpha1.Bidder
	if err := req.Get(ctx, "", name, &bidder); err != nil {
		return nil, err
	}
	return &bidder, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.MarketPresence, status v1alpha1.MarketPresenceStatus) error {
	if reflect.DeepEqual(object.Status, status) {
		return nil
	}
	next := object.DeepCopy()
	next.Status = status
	if err := req.ApplyStatus(ctx, next); err != nil {
		return err
	}
	if object.Status.Phase != status.Phase || object.Status.Ready != status.Ready {
		c.log.Info("market presence", "workspace", req.Workspace, "marketpresence", req.Name, "phase", string(status.Phase),
			"ready", status.Ready, "lastHeartbeatAt", status.LastHeartbeatAt)
	}
	return nil
}

func expire(status *v1alpha1.MarketPresenceStatus, gen int64, reason, message string) {
	status.Phase = v1alpha1.MarketPresencePhaseExpired
	status.Ready = false
	conds.Set(&status.Conditions, gen, ConditionReady, false, reason, message)
	conds.Set(&status.Conditions, gen, ConditionHeartbeat, false, reason, message)
}

func ownedBy(object *v1alpha1.MarketPresence, bidder string) bool {
	for _, owner := range object.OwnerReferences {
		if owner.Kind == BidderKind && owner.Name == bidder && strings.HasPrefix(owner.APIVersion, v1alpha1.GroupName+"/") {
			return true
		}
	}
	return false
}

func readiness(bidder *v1alpha1.Bidder) (bool, string) {
	phase := string(bidder.Status.Phase)
	if phase != "" && phase != string(v1alpha1.ComponentPhaseReady) {
		return false, phase
	}
	return bidder.Status.Ready, phase
}

func bidderDID(bidder *v1alpha1.Bidder) string {
	if bidder.Spec.Identity == nil {
		return ""
	}
	return strings.TrimSpace(bidder.Spec.Identity.Did)
}
