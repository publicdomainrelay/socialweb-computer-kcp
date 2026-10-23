package marketpresence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	FieldManagerMarketPresence = "marketpresence-controller"

	ManagedByLabel = "socialweb.computer/managed-by"

	ConditionReady = "Ready"

	ConditionBidderReady = "BidderReady"

	ConditionHeartbeat = "Heartbeat"

	DefaultRefresh = time.Minute

	DefaultWorkspace = "root"

	DefaultKind = "Bidder"

	MaxPresenceName = 253
)

var (
	MarketPresenceGVR = v1alpha1.MarketPresenceGVR

	BidderGVR = v1alpha1.BidderGVR
)

func PresenceName(bidder string) string {
	name := "bidder-" + bidder
	if len(name) <= MaxPresenceName {
		return name
	}
	sum := sha256.Sum256([]byte(bidder))
	return "bidder-" + hex.EncodeToString(sum[:])[:32]
}

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Workspaces []string

	Dynamic DynamicFactory

	Reader market.RecordGetter

	Directory identity.Directory

	HTTPClient market.HTTPDoer

	Now func() time.Time

	Refresh time.Duration

	Log *slog.Logger
}

type Controller struct {
	workspaces []string

	dynamic DynamicFactory

	reader market.RecordGetter

	directory identity.Directory

	http market.HTTPDoer

	now func() time.Time

	refresh time.Duration

	log *slog.Logger

	mu sync.Mutex

	readers map[string]market.RecordGetter
}

func NewController(opts Options) (*Controller, error) {
	if opts.Dynamic == nil {
		return nil, fmt.Errorf("marketpresence: a dynamic client factory is required")
	}
	workspaces := make([]string, 0, len(opts.Workspaces))
	seen := map[string]bool{}
	for _, workspace := range opts.Workspaces {
		workspace = strings.TrimSpace(workspace)
		if workspace == "" {
			workspace = DefaultWorkspace
		}
		if seen[workspace] {
			continue
		}
		seen[workspace] = true
		workspaces = append(workspaces, workspace)
	}
	if len(workspaces) == 0 {
		workspaces = []string{DefaultWorkspace}
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	refresh := opts.Refresh
	if refresh <= 0 {
		refresh = DefaultRefresh
	}
	return &Controller{
		workspaces: workspaces,
		dynamic:    opts.Dynamic,
		reader:     opts.Reader,
		directory:  opts.Directory,
		http:       opts.HTTPClient,
		now:        now,
		refresh:    refresh,
		log:        log.With("component", "marketpresence-controller"),
		readers:    map[string]market.RecordGetter{},
	}, nil
}

func Register(ctx context.Context, opts Options) error {
	ctl, err := NewController(opts)
	if err != nil {
		return err
	}
	return ctl.Run(ctx)
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.pass(ctx)
	ticker := time.NewTicker(c.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.pass(ctx)
		}
	}
}

type workspaceState struct {
	name string

	dyn dynamic.Interface

	presenceListed bool

	presences []unstructured.Unstructured

	bidders []unstructured.Unstructured
}

func (c *Controller) pass(ctx context.Context) {
	states := make([]workspaceState, 0, len(c.workspaces))
	refs := map[string]bool{}
	complete := true
	for _, workspace := range c.workspaces {
		dyn, err := c.dynamic(workspace)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("the market presence dynamic client is not available", "workspace", workspace, "err", err.Error())
			}
			complete = false
			continue
		}
		list, err := dyn.Resource(MarketPresenceGVR).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			list = nil
		} else if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("listing MarketPresence objects", "workspace", workspace, "err", err.Error())
			}
			complete = false
			continue
		} else {
			for i := range list.Items {
				if key := presenceRefKey(&list.Items[i], workspace); key != "" {
					refs[key] = true
				}
			}
		}
		state := workspaceState{name: workspace, dyn: dyn}
		if list != nil {
			state.presenceListed = true
			state.presences = list.Items
		}
		bidders, err := dyn.Resource(BidderGVR).List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		case err != nil:
			if ctx.Err() == nil {
				c.log.Warn("listing Bidders to ensure their MarketPresence", "workspace", workspace, "err", err.Error())
			}
		default:
			state.bidders = bidders.Items
		}
		states = append(states, state)
	}
	for i := range states {
		state := &states[i]
		created := make([]unstructured.Unstructured, 0)
		if complete && state.presenceListed {
			for j := range state.bidders {
				if made := c.ensure(ctx, state.dyn, state.name, &state.bidders[j], refs); made != nil {
					created = append(created, *made)
				}
			}
		}
		kept := state.presences[:0]
		for j := range state.presences {
			if c.gc(ctx, state.dyn, &state.presences[j]) {
				continue
			}
			kept = append(kept, state.presences[j])
		}
		for j := range kept {
			c.reconcile(ctx, state.dyn, &kept[j])
		}
		for j := range created {
			c.reconcile(ctx, state.dyn, &created[j])
		}
	}
}

func (c *Controller) ensure(ctx context.Context, dyn dynamic.Interface, workspace string, bidder *unstructured.Unstructured, refs map[string]bool) *unstructured.Unstructured {
	if bidder == nil || bidder.GetDeletionTimestamp() != nil {
		return nil
	}
	name := strings.TrimSpace(bidder.GetName())
	if name == "" {
		return nil
	}
	key := refkey(workspace, DefaultKind, name)
	if refs[key] {
		return nil
	}
	spec := map[string]any{
		"bidderRef": map[string]any{
			"workspace": workspace,
			"kind":      DefaultKind,
			"name":      name,
		},
		"ttlSeconds": int64(market.HeartbeatTTL.Seconds()),
	}
	if relays, _, _ := unstructured.NestedStringSlice(bidder.Object, "spec", "network", "relayUrls"); len(relays) > 0 && strings.TrimSpace(relays[0]) != "" {
		spec["relayUri"] = strings.TrimSpace(relays[0])
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "MarketPresence",
		"metadata": map[string]any{
			"name":   PresenceName(name),
			"labels": map[string]any{ManagedByLabel: FieldManagerMarketPresence},
		},
		"spec": spec,
	}}
	if uid := string(bidder.GetUID()); uid != "" {
		owner := map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       DefaultKind,
			"name":       name,
			"uid":        uid,
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{owner}, "metadata", "ownerReferences")
	}
	made, err := dyn.Resource(MarketPresenceGVR).Create(ctx, u, metav1.CreateOptions{FieldManager: FieldManagerMarketPresence})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		if ctx.Err() == nil {
			c.log.Warn("creating a MarketPresence for a Bidder", "workspace", workspace, "bidder", name, "err", err.Error())
		}
		return nil
	}
	refs[key] = true
	if err != nil {
		return nil
	}
	c.log.Info("a MarketPresence follows its Bidder", "workspace", workspace, "bidder", name, "marketpresence", PresenceName(name))
	return made
}

func (c *Controller) gc(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) bool {
	if u == nil {
		return false
	}
	if u.GetLabels()[ManagedByLabel] != FieldManagerMarketPresence {
		return false
	}
	var presence v1alpha1.MarketPresence
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &presence); err != nil {
		return false
	}
	_, err := c.bidder(ctx, presence.Spec.BidderRef)
	if !apierrors.IsNotFound(err) {
		return false
	}
	if err := dyn.Resource(MarketPresenceGVR).Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		if ctx.Err() == nil {
			c.log.Warn("deleting the MarketPresence of a gone Bidder", "marketpresence", u.GetName(), "err", err.Error())
		}
		return false
	}
	c.log.Info("a MarketPresence goes with its gone Bidder", "marketpresence", u.GetName())
	return true
}

func presenceRefKey(u *unstructured.Unstructured, workspace string) string {
	if u == nil {
		return ""
	}
	bidder, _, _ := unstructured.NestedString(u.Object, "spec", "bidderRef", "workspace")
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "bidderRef", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "bidderRef", "name")
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.TrimSpace(bidder) == "" {
		bidder = workspace
	}
	if strings.TrimSpace(kind) == "" {
		kind = DefaultKind
	}
	return refkey(bidder, kind, name)
}

func refkey(workspace, kind, name string) string {
	return strings.ToLower(strings.TrimSpace(kind)) + "/" + strings.TrimSpace(workspace) + "/" + strings.TrimSpace(name)
}

func (c *Controller) reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	if u == nil || u.GetDeletionTimestamp() != nil {
		return
	}
	var presence v1alpha1.MarketPresence
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &presence); err != nil {
		c.log.Warn("decoding a MarketPresence", "name", u.GetName(), "err", err.Error())
		return
	}
	current := presence.Status
	current.Conditions = append([]metav1.Condition(nil), presence.Status.Conditions...)
	status := presence.Status
	status.Conditions = append([]metav1.Condition(nil), presence.Status.Conditions...)
	gen := presence.Generation
	now := c.now().UTC()
	status.ObservedGeneration = gen

	bidder, err := c.bidder(ctx, presence.Spec.BidderRef)
	switch {
	case apierrors.IsNotFound(err):
		setCondition(&status, gen, ConditionBidderReady, metav1.ConditionFalse, "BidderMissing", "the Bidder named by the MarketPresence does not exist")
		c.expire(&status, gen, "BidderMissing", "the Bidder named by the MarketPresence does not exist")
		c.write(ctx, dyn, presence.Name, &current, &status)
		return
	case err != nil:
		c.log.Warn("reading the Bidder of a MarketPresence", "marketpresence", presence.Name, "err", err.Error())
		setCondition(&status, gen, ConditionBidderReady, metav1.ConditionFalse, "BidderUnreadable", "the Bidder named by the MarketPresence could not be read")
		c.expire(&status, gen, "BidderUnreadable", "the Bidder named by the MarketPresence could not be read")
		c.write(ctx, dyn, presence.Name, &current, &status)
		return
	}

	bidderReady, phase := bidderReadiness(bidder)
	if bidderReady {
		setCondition(&status, gen, ConditionBidderReady, metav1.ConditionTrue, "BidderReady", "the Bidder reports Ready")
	} else {
		message := "the Bidder is not Ready"
		if phase != "" && phase != "Ready" {
			message = "the Bidder is " + phase
		}
		setCondition(&status, gen, ConditionBidderReady, metav1.ConditionFalse, "BidderNotReady", message)
	}

	offering := strings.TrimSpace(presence.Spec.OfferingURI)
	did := bidderDID(bidder)
	if offering != "" {
		aturi, err := syntax.ParseATURI(offering)
		if err != nil || aturi.Collection() == "" || aturi.RecordKey() == "" {
			c.expire(&status, gen, "OfferingInvalid", "spec.offeringUri is not an atproto record URI")
			c.write(ctx, dyn, presence.Name, &current, &status)
			return
		}
		if did == "" {
			did = string(aturi.Authority())
		}
	}

	ttl := time.Duration(presence.Spec.TtlSeconds) * time.Second
	if ttl <= 0 {
		ttl = market.HeartbeatTTL
	}

	last := status.LastHeartbeatAt
	observed, heartbeatReason := c.heartbeat(ctx, did, offering, bidder, now)
	if !observed.IsZero() {
		if prior, err := time.Parse(time.RFC3339, last); err != nil || observed.After(prior) {
			last = observed.UTC().Format(time.RFC3339)
		}
	}
	status.LastHeartbeatAt = last

	fresh := false
	if at, err := time.Parse(time.RFC3339, last); err == nil {
		age := now.Sub(at)
		fresh = age >= -market.HeartbeatMaxSkew && age <= ttl
	}

	switch {
	case !observed.IsZero():
		setCondition(&status, gen, ConditionHeartbeat, metav1.ConditionTrue, "HeartbeatFresh", "the market heartbeat is readable")
	case fresh:
		setCondition(&status, gen, ConditionHeartbeat, metav1.ConditionTrue, "HeartbeatCached", "the market heartbeat is unreadable; the last observed heartbeat is within the TTL")
	default:
		setCondition(&status, gen, ConditionHeartbeat, metav1.ConditionFalse, heartbeatReason, "no fresh market heartbeat is readable")
	}

	active := bidderReady && fresh
	switch {
	case active:
		status.Phase = v1alpha1.MarketPresencePhaseActive
		status.Ready = true
		setCondition(&status, gen, ConditionReady, metav1.ConditionTrue, "Active", "the Bidder is Ready and its heartbeat is within the TTL")
	case !fresh && last != "":
		status.Phase = v1alpha1.MarketPresencePhaseExpired
		status.Ready = false
		setCondition(&status, gen, ConditionReady, metav1.ConditionFalse, "HeartbeatExpired", "the last market heartbeat is older than ttlSeconds")
	case !fresh:
		status.Phase = v1alpha1.MarketPresencePhaseExpired
		status.Ready = false
		setCondition(&status, gen, ConditionReady, metav1.ConditionFalse, heartbeatReason, "no fresh market heartbeat is readable")
	default:
		status.Phase = v1alpha1.MarketPresencePhaseExpired
		status.Ready = false
		setCondition(&status, gen, ConditionReady, metav1.ConditionFalse, "BidderNotReady", "the Bidder is not Ready")
	}

	c.write(ctx, dyn, presence.Name, &current, &status)
}

func (c *Controller) expire(status *v1alpha1.MarketPresenceStatus, gen int64, reason, message string) {
	status.Phase = v1alpha1.MarketPresencePhaseExpired
	status.Ready = false
	setCondition(status, gen, ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(status, gen, ConditionHeartbeat, metav1.ConditionFalse, reason, message)
}

func (c *Controller) heartbeat(ctx context.Context, did, offering string, bidder *unstructured.Unstructured, now time.Time) (time.Time, string) {
	if did == "" {
		return time.Time{}, "NoDID"
	}
	reader := c.readerFor(bidder)
	if reader == nil {
		return time.Time{}, "NoReader"
	}
	rec, err := reader.GetRecord(ctx, did, market.HeartbeatNSID, market.HeartbeatRKey)
	if err != nil {
		return time.Time{}, "NoHeartbeat"
	}
	heartbeat, err := market.ParseHeartbeat(rec.Value)
	if err != nil {
		return time.Time{}, "HeartbeatInvalid"
	}
	if offering != "" && heartbeat.Offering != offering {
		return time.Time{}, "HeartbeatMismatch"
	}
	at, err := market.ParseTime(heartbeat.CreatedAt)
	if err != nil {
		return time.Time{}, "HeartbeatInvalid"
	}
	if at.After(now.Add(market.HeartbeatMaxSkew)) {
		return time.Time{}, "HeartbeatInFuture"
	}
	return at, "HeartbeatFresh"
}

func (c *Controller) readerFor(bidder *unstructured.Unstructured) market.RecordGetter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reader != nil {
		return c.reader
	}
	plc := ""
	if bidder != nil {
		plc, _, _ = unstructured.NestedString(bidder.Object, "spec", "network", "plcDirectoryUrl")
	}
	if reader, ok := c.readers[plc]; ok {
		return reader
	}
	dir := c.directory
	if dir == nil {
		dir = ingress.NewDirectory(plc)
	}
	reader := market.NewHTTPReader(dir)
	if c.http != nil {
		reader.Client = c.http
	}
	c.readers[plc] = reader
	return reader
}

func (c *Controller) bidder(ctx context.Context, ref v1alpha1.MarketPresenceBidderRef) (*unstructured.Unstructured, error) {
	workspace := strings.TrimSpace(ref.Workspace)
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	kind := strings.TrimSpace(ref.Kind)
	if kind == "" {
		kind = DefaultKind
	}
	if strings.TrimSpace(ref.Name) == "" {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: resourceForKind(kind)}, "")
	}
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return nil, err
	}
	gvr := schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: resourceForKind(kind)}
	return dyn.Resource(gvr).Get(ctx, ref.Name, metav1.GetOptions{})
}

func resourceForKind(kind string) string {
	switch strings.ToLower(kind) {
	case "bidder":
		return "bidders"
	case "marketbidder":
		return "marketbidders"
	default:
		return strings.ToLower(kind) + "s"
	}
}

func bidderReadiness(bidder *unstructured.Unstructured) (bool, string) {
	if bidder == nil {
		return false, ""
	}
	ready, _, _ := unstructured.NestedBool(bidder.Object, "status", "ready")
	phase, _, _ := unstructured.NestedString(bidder.Object, "status", "phase")
	if phase != "" && phase != "Ready" {
		return false, phase
	}
	return ready, phase
}

func bidderDID(bidder *unstructured.Unstructured) string {
	if bidder == nil {
		return ""
	}
	did, _, _ := unstructured.NestedString(bidder.Object, "spec", "identity", "did")
	return strings.TrimSpace(did)
}

func setCondition(status *v1alpha1.MarketPresenceStatus, gen int64, t string, value metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               t,
		Status:             value,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
}

func (c *Controller) write(ctx context.Context, dyn dynamic.Interface, name string, current, status *v1alpha1.MarketPresenceStatus) {
	if reflect.DeepEqual(current, status) {
		return
	}
	body, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		c.log.Warn("encoding a MarketPresence status", "marketpresence", name, "err", err.Error())
		return
	}
	u := &unstructured.Unstructured{Object: map[string]any{"status": body}}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("MarketPresence")
	u.SetName(name)
	if _, err := dyn.Resource(MarketPresenceGVR).ApplyStatus(ctx, name, u,
		metav1.ApplyOptions{FieldManager: FieldManagerMarketPresence, Force: true}); err != nil {
		if ctx.Err() == nil {
			c.log.Warn("writing a MarketPresence status", "marketpresence", name, "err", err.Error())
		}
		return
	}
	if current.Phase != status.Phase || current.Ready != status.Ready {
		c.log.Info("market presence", "marketpresence", name, "phase", string(status.Phase), "ready", status.Ready,
			"lastHeartbeatAt", status.LastHeartbeatAt)
	}
}
