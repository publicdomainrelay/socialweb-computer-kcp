package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/diskusage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
)

const (
	GCSampleEvery       = 30 * time.Second
	StateDirSampleEvery = 3 * time.Minute
	GCStuckAfter        = 10 * time.Minute
	GCExamples          = 5
	GCListCap           = 40
	GCWorkspaceCap      = 200
	ClusterLabel        = "socialweb.computer/cluster"
	AppWorkspaceLabel   = "apps.socialweb.computer/application"
	providerWS          = "socialweb-provider"
	accountWorkspace    = "account"

	QuotaName      = "socialweb-bounded"
	QuotaNamespace = "default"
)

var ResourceQuotaGVR = kcpgvr.ResourceQuotas

var QuotaDefaults = struct {
	Bids, ComputeVMs, ComputeContracts, NodeClaims, PolicyWorkflowRuns, Components int64
}{Bids: 10000, ComputeVMs: 64, ComputeContracts: 1024, NodeClaims: 64, PolicyWorkflowRuns: 500, Components: 16}

var FinalizerControllers = map[string]string{
	Finalizer:                                       "app: the composition controller of the root",
	"computevm.socialweb.computer/release":          "computevm: the ComputeVM controller of the core",
	"socialweb.computer/postgres":                   "postgresctl: the Postgres controller of the core",
	"socialweb.computer/cluster-member":             "clusterprovider: the cluster provider of the gateway",
	"cluster.socialweb.computer/teardown":           "clusterprovider: the cluster provider of the gateway",
	"node.socialweb.computer/teardown":              "clusterprovider: the cluster provider of the gateway",
	"nodeclaim.socialweb.computer/release":          "socialweb-computer-k8s web app: the node-market claim mapper",
	"atprotoidentity.socialweb.computer/revocation": "socialweb-computer-k8s web app: the revocation sweep",
}

func (c *Composer) ApplyInfra(ctx context.Context, r Root, setName, profile string, values any, include ...string) error {
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(setName)
	if !ok {
		return fmt.Errorf("compose: deploy/sets.yaml names no %s set", setName)
	}
	out := set
	out.Phases = nil
	for _, p := range set.Phases {
		base, only, scoped := strings.Cut(p.Name, "@")
		if !scoped {
			out.Phases = append(out.Phases, p)
			continue
		}
		if only == profile && slices.Contains(include, base) {
			out.Phases = append(out.Phases, p)
		}
	}
	return c.applySet(ctx, r, out, values)
}

func (c *Composer) ApplySetPhases(ctx context.Context, r Root, setName string, values any, phases ...string) error {
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(setName)
	if !ok {
		return fmt.Errorf("compose: deploy/sets.yaml names no %s set", setName)
	}
	var keep []manifests.Phase
	for _, p := range set.Phases {
		if slices.Contains(phases, p.Name) {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return fmt.Errorf("compose: the %s set names none of the phases %v", setName, phases)
	}
	set.Phases = keep
	return c.applySet(ctx, r, set, values)
}

func (c *Composer) applySet(ctx context.Context, r Root, set manifests.Set, values any) error {
	ap, err := kcpapply.New(c.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, FieldManager: r.Manager(), Base: c.Workspace(),
		Labels: map[string]string{OwnerLabel: r.Owner(), RootKindLabel: r.Kind, RootClusterLabel: r.Cluster}, Log: c.log})
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, set, values)
	return err
}

func ApplySet(ctx context.Context, c *Composer) error {
	ap, err := kcpapply.New(c.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, Base: c.Workspace(), Log: c.log})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(ManifestSet)
	if !ok {
		return fmt.Errorf("compose: deploy/sets.yaml names no %s set", ManifestSet)
	}
	values, err := manifests.DecodeValues(set.Values, nil)
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, set, values)
	return err
}

type adoptKind struct {
	Kind string
	GVR  schema.GroupVersionResource
}

var adoptKinds = []adoptKind{
	{KindBid, v1alpha1.BidGVR},
	{KindComputeContract, ComputeContractGVR},
	{KindStateSync, v1alpha1.StateSyncGVR},
	{KindMarketPresence, v1alpha1.MarketPresenceGVR},
	{KindMarketIdentity, MarketIdentityGVR},
	{KindOpenBaoServer, OpenBaoServerGVR},
	{KindHostProcess, HostProcessGVR},
	{"AtprotoRecord", AtprotoRecordGVR},
	{"AtprotoRecordWatch", RecordWatchGVR},
	{"Workspace", WorkspaceGVR},
}

func adoptable(rootKind, kind string, u *Generic, owner string) bool {
	if len(u.OwnerReferences) > 0 || u.DeletionTimestamp != nil {
		return false
	}
	if kind == KindHostProcess && strings.HasPrefix(u.Name, hostprocess.MirrorPrefix) {
		return false
	}
	if u.Labels[OwnerLabel] == owner {
		return true
	}
	switch kind {
	case KindBid, KindStateSync, KindMarketPresence:
		return rootKind == string(v1alpha1.ApplicationKindMarketBidder) || (kind != KindBid && rootKind == string(v1alpha1.ApplicationKindMarketRequester))
	case KindComputeContract:
		linked := specField(u, "nodeClaimRef")
		return rootKind == string(v1alpha1.ApplicationKindMarketRequester) && !linked
	}
	return false
}

func (c *Composer) Adopt(ctx context.Context, r Root) {
	if r.Deleting || r.UID == "" {
		return
	}
	for _, k := range adoptKinds {
		ri, err := c.Client(r.Workspace, k.GVR)
		if err != nil {
			continue
		}
		items, err := kube.List[Generic](ctx, ri, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range items {
			u := &items[i]
			if !adoptable(r.Kind, k.Kind, u, r.Owner()) {
				continue
			}
			patch := NewDoc(u.APIVersion, u.Kind, u.Name)
			meta := DocMeta(patch)
			if u.Namespace != "" {
				meta["namespace"] = u.Namespace
			}
			if u.UID != "" {
				meta["uid"] = string(u.UID)
			}
			meta["ownerReferences"] = []metav1.OwnerReference{r.OwnerRef()}
			if err := c.applyAs(ctx, r, r.AdoptManager(), r.Workspace, k.GVR, patch); err != nil {
				if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					c.log.Warn("adopting an intent object under its root", "root", r.Owner(), "kind", k.Kind, "name", u.Name, "err", err.Error())
				}
				continue
			}
			c.log.Info("an intent object hangs from its root", "root", r.Owner(), "kind", k.Kind, "name", u.Name)
		}
	}
}

type quotaEntry struct {
	component string
	gvr       schema.GroupVersionResource
	field     string
	def       int64
	gate      bool
}

func (e quotaEntry) key() string { return "count/" + e.gvr.Resource + "." + e.gvr.Group }

func (e quotaEntry) name() string { return e.gvr.Resource + "." + e.gvr.Group }

func quotaPlan(rootKind string) []quotaEntry {
	runs := []quotaEntry{{"", PolicyWorkflowRunGVR, "policyWorkflowRuns", QuotaDefaults.PolicyWorkflowRuns, false}}
	var entries []quotaEntry
	switch rootKind {
	case string(v1alpha1.ApplicationKindMarketBidder):
		entries = append([]quotaEntry{
			{"", v1alpha1.BidGVR, "bids", QuotaDefaults.Bids, true},
			{"", v1alpha1.ComputeVMGVR, "computeVMs", QuotaDefaults.ComputeVMs, true},
		}, runs...)
	case string(v1alpha1.ApplicationKindMarketRequester):
		entries = append([]quotaEntry{{"", ComputeContractGVR, "computeContracts", QuotaDefaults.ComputeContracts, false}}, runs...)
	case string(v1alpha1.ApplicationKindGateway):
		entries = append([]quotaEntry{{ComponentWorkspace, ComputeContractGVR, "computeContracts", QuotaDefaults.ComputeContracts, false}}, runs...)
	case string(v1alpha1.ApplicationKindSshGateway), "MarketNetwork":
		entries = runs
	default:
		return nil
	}
	for _, kind := range ComponentKindsFor(rootKind) {
		if ct, ok := ComponentTypeFor(kind); ok {
			entries = append(entries, quotaEntry{"", ct.GVR, "components", QuotaDefaults.Components, false})
		}
	}
	return entries
}

func limitOf(configured int32, def int64) int64 {
	if configured != 0 {
		return int64(configured)
	}
	return def
}

func BidderQuotaArgs(q *v1alpha1.ApplicationQuota) []string {
	var bids, vms int32
	if q != nil {
		bids, vms = q.Bids, q.ComputeVMs
	}
	var out []string
	if n := limitOf(bids, QuotaDefaults.Bids); n > 0 {
		out = append(out, "--bid-quota", strconv.FormatInt(n, 10))
	}
	if n := limitOf(vms, QuotaDefaults.ComputeVMs); n > 0 {
		out = append(out, "--computevm-quota", strconv.FormatInt(n, 10))
	}
	return out
}

func quotaLimit(q *v1alpha1.ApplicationQuota, e quotaEntry) int64 {
	if q == nil {
		return e.def
	}
	var n int32
	switch e.field {
	case "bids":
		n = q.Bids
	case "computeVMs":
		n = q.ComputeVMs
	case "computeContracts":
		n = q.ComputeContracts
	case "policyWorkflowRuns":
		n = q.PolicyWorkflowRuns
	}
	return limitOf(n, e.def)
}

func (c *Composer) quotaWorkspace(r Root, component string) string {
	if component == "" {
		return r.Workspace
	}
	return r.Workspace + ":" + component
}

func (c *Composer) ApplyQuotas(ctx context.Context, r Root, q *v1alpha1.ApplicationQuota) {
	if r.Deleting || r.UID == "" {
		return
	}
	byWS := map[string]map[string]string{}
	for _, e := range quotaPlan(r.Kind) {
		if e.gate {
			continue
		}
		ws := c.quotaWorkspace(r, e.component)
		if byWS[ws] == nil {
			byWS[ws] = map[string]string{}
		}
		if n := quotaLimit(q, e); n > 0 {
			byWS[ws][e.key()] = fmt.Sprint(n)
		}
	}
	for ws, hard := range byWS {
		if err := c.applyQuota(ctx, r, ws, hard); err != nil && ctx.Err() == nil {
			c.log.Warn("applying the object-count quota of a workspace", "workspace", ws, "err", err.Error())
		}
	}
	c.samples.recordQuotas(r, q)
}

func (c *Composer) applyQuota(ctx context.Context, r Root, ws string, hard map[string]string) error {
	ri, err := c.Client(ws, ResourceQuotaGVR)
	if err != nil {
		return err
	}
	if len(hard) == 0 {
		if err := ri.In(QuotaNamespace).Delete(ctx, QuotaName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	hardAny := map[string]any{}
	for k, v := range hard {
		hardAny[k] = v
	}
	u := NewDoc("v1", "ResourceQuota", QuotaName)
	u["spec"] = map[string]any{"hard": hardAny}
	meta := DocMeta(u)
	meta["namespace"] = QuotaNamespace
	if ws == r.Workspace {
		meta["ownerReferences"] = []metav1.OwnerReference{r.OwnerRef()}
	}
	return c.applyAs(ctx, r, r.Manager(), ws, ResourceQuotaGVR, u)
}

type samples struct {
	mu sync.Mutex

	stateDir *v1alpha1.StateDirUsage

	stateDirAt time.Time

	gc *v1alpha1.GCStatus

	gcAt time.Time

	quotas map[string]rootQuota

	quotaUsage []v1alpha1.QuotaUsage
}

type rootQuota struct {
	root Root

	quota *v1alpha1.ApplicationQuota
}

func (s *samples) recordQuotas(r Root, q *v1alpha1.ApplicationQuota) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quotas == nil {
		s.quotas = map[string]rootQuota{}
	}
	s.quotas[r.Owner()] = rootQuota{root: r, quota: q}
}

func (s *samples) into(ctx context.Context, c *Composer, m map[string]any, declared map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if dir := c.cfg.Layout.StateDir; dir != "" && now.Sub(s.stateDirAt) > StateDirSampleEvery {
		s.stateDirAt = now
		if u, err := diskusage.Of(dir); err == nil {
			s.stateDir = &v1alpha1.StateDirUsage{Path: dir, Bytes: u.Bytes, Files: u.Files, SampledAt: now.UTC().Format(time.RFC3339)}
		}
	}
	if now.Sub(s.gcAt) > GCSampleEvery {
		s.gcAt = now
		if st := c.scanGC(ctx); st != nil {
			s.gc = st
		}
		s.quotaUsage = c.readQuotas(ctx, s.quotas)
	}
	put := func(key string, v any) {
		if !declared[key] {
			return
		}
		var out any
		if b, err := json.Marshal(v); err == nil && json.Unmarshal(b, &out) == nil {
			m[key] = out
		}
	}
	if s.stateDir != nil {
		put("stateDir", s.stateDir)
	}
	if s.gc != nil {
		put("gc", s.gc)
	}
	if len(s.quotaUsage) > 0 {
		put("quotas", s.quotaUsage)
	}
	if c.cfg.TTLReport != nil {
		if st := c.cfg.TTLReport(); st != nil {
			put("ttl", st)
		}
	}
	if c.cfg.StoreUsage != nil {
		if u := c.cfg.StoreUsage(); u != nil {
			put("store", u)
		}
	}
}

func (c *Composer) countObjects(ctx context.Context, ws string, res schema.GroupVersionResource) (int64, bool) {
	ri, err := c.Client(ws, res)
	if err != nil {
		return 0, false
	}
	items, meta, err := kube.ListPage[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{Limit: 1})
	if err != nil {
		return 0, false
	}
	n := int64(len(items))
	if rem := meta.RemainingItemCount; rem != nil {
		return n + *rem, true
	}
	if meta.Continue != "" {
		all, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{})
		if err != nil {
			return 0, false
		}
		return int64(len(all)), true
	}
	return n, true
}

func (c *Composer) readQuotas(ctx context.Context, roots map[string]rootQuota) []v1alpha1.QuotaUsage {
	var out []v1alpha1.QuotaUsage
	for _, rq := range roots {
		for _, e := range quotaPlan(rq.root.Kind) {
			hard := quotaLimit(rq.quota, e)
			if hard <= 0 {
				continue
			}
			ws := c.quotaWorkspace(rq.root, e.component)
			used, ok := c.countObjects(ctx, ws, e.gvr)
			if !ok {
				continue
			}
			out = append(out, v1alpha1.QuotaUsage{Workspace: ws, Resource: e.name(), Hard: hard, Used: used})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Resource < out[j].Resource
	})
	if len(out) > GCListCap {
		out = out[:GCListCap]
	}
	return out
}

var gcKinds = []adoptKind{
	{KindBid, v1alpha1.BidGVR},
	{KindComputeVM, v1alpha1.ComputeVMGVR},
	{"GuestIdentityGrant", v1alpha1.GuestIdentityGrantGVR},
	{KindHostProcess, HostProcessGVR},
	{KindOpenBaoServer, OpenBaoServerGVR},
	{"Postgres", v1alpha1.PostgresGVR},
	{KindMarketIdentity, MarketIdentityGVR},
	{KindComputeContract, ComputeContractGVR},
	{"NodeClaim", v1alpha1.NodeClaimGVR},
	{KindStateSync, v1alpha1.StateSyncGVR},
	{KindMarketPresence, v1alpha1.MarketPresenceGVR},
}

func gcAllKinds() []adoptKind {
	out := slices.Clone(gcKinds)
	for _, t := range ComponentTypes {
		out = append(out, adoptKind{t.Kind, t.GVR})
	}
	for _, t := range PolicyTypes {
		if t.Kind != ComponentKindPolicyEngine {
			out = append(out, adoptKind{t.Kind, t.GVR})
		}
	}
	return out
}

func (c *Composer) gcWorkspaces(ctx context.Context) []string {
	root := c.Workspace()
	out := []string{root}
	ri, err := c.Client(root, WorkspaceGVR)
	if err != nil {
		return out
	}
	list, err := kube.List[tenancyv1alpha1.Workspace](ctx, ri, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, w := range list {
		name := w.Name
		if _, app := w.Labels[AppWorkspaceLabel]; app || name == providerWS || w.DeletionTimestamp != nil {
			continue
		}
		if w.Status.Phase != "Ready" {
			continue
		}
		out = append(out, root+":"+name)
		if w.Spec.Type != nil && w.Spec.Type.Name == accountWorkspace && len(out) < GCWorkspaceCap {
			sub, err := c.Client(root+":"+name, WorkspaceGVR)
			if err != nil {
				continue
			}
			homes, err := kube.List[metav1.PartialObjectMetadata](ctx, sub, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, h := range homes {
				if h.DeletionTimestamp == nil && len(out) < GCWorkspaceCap {
					out = append(out, root+":"+name+":"+h.Name)
				}
			}
		}
	}
	return out
}

func specField(u *Generic, field string) bool {
	_, found := u.Spec[field]
	return found
}

func ownedByLink(u *Generic) bool {
	if len(u.OwnerReferences) > 0 {
		return true
	}
	if specField(u, "ttlSecondsAfterFinished") || specField(u, "nodeClaimRef") {
		return true
	}
	labels := u.Labels
	if _, ok := labels[ClusterLabel]; ok {
		return true
	}
	_, ok := labels[OwnerLabel]
	return ok
}

func addExample(ex []string, v string) []string {
	if len(ex) < GCExamples {
		return append(ex, v)
	}
	return ex
}

func finalizerController(f string) string {
	if c, ok := FinalizerControllers[f]; ok {
		return c
	}
	return "unknown: no controller is registered for this finalizer"
}

func (c *Composer) scanGC(ctx context.Context) *v1alpha1.GCStatus {
	var total, orphanTotal, stuckTotal int64
	orphans := map[string]*v1alpha1.GCKindCount{}
	stuck := map[string]*v1alpha1.GCStuck{}
	kinds := gcAllKinds()
	for _, ws := range c.gcWorkspaces(ctx) {
		for _, k := range kinds {
			ri, err := c.Client(ws, k.GVR)
			if err != nil {
				continue
			}
			items, err := kube.List[Generic](ctx, ri, metav1.ListOptions{})
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			for i := range items {
				u := &items[i]
				total++
				ref := ws + "/" + u.Name
				if dt := u.DeletionTimestamp; dt != nil {
					if time.Since(dt.Time) > GCStuckAfter {
						for _, f := range u.Finalizers {
							key := k.Kind + "|" + f
							s := stuck[key]
							if s == nil {
								s = &v1alpha1.GCStuck{Kind: k.Kind, Finalizer: f, Controller: finalizerController(f)}
								stuck[key] = s
							}
							s.Count++
							s.Examples = addExample(s.Examples, ref)
							stuckTotal++
						}
					}
					continue
				}
				if (k.Kind == KindHostProcess && strings.HasPrefix(u.Name, hostprocess.MirrorPrefix)) || ownedByLink(u) {
					continue
				}
				cnt := orphans[k.Kind]
				if cnt == nil {
					cnt = &v1alpha1.GCKindCount{Kind: k.Kind}
					orphans[k.Kind] = cnt
				}
				cnt.Count++
				cnt.Examples = addExample(cnt.Examples, ref)
				orphanTotal++
			}
		}
	}
	st := &v1alpha1.GCStatus{SampledAt: time.Now().UTC().Format(time.RFC3339), Objects: total, OrphanTotal: orphanTotal, StuckTotal: stuckTotal}
	for _, cnt := range orphans {
		st.Orphans = append(st.Orphans, *cnt)
	}
	sort.Slice(st.Orphans, func(i, j int) bool { return st.Orphans[i].Kind < st.Orphans[j].Kind })
	if len(st.Orphans) > GCListCap {
		st.Orphans = st.Orphans[:GCListCap]
	}
	for _, s := range stuck {
		st.Terminating = append(st.Terminating, *s)
	}
	sort.Slice(st.Terminating, func(i, j int) bool {
		if st.Terminating[i].Kind != st.Terminating[j].Kind {
			return st.Terminating[i].Kind < st.Terminating[j].Kind
		}
		return st.Terminating[i].Finalizer < st.Terminating[j].Finalizer
	})
	if len(st.Terminating) > GCListCap {
		st.Terminating = st.Terminating[:GCListCap]
	}
	return st
}

func componentRank(kind string) int {
	switch kind {
	case ComponentKindService, ComponentKindExposure:
		return 0
	case ComponentKindGatewayAPI, ComponentKindSshServer, ComponentKindBidder:
		return 1
	case ComponentKindRequester, ComponentKindGuestControlPlane, ComponentKindPolicyEngine:
		return 2
	case ComponentKindIngressProxy, ComponentKindDispatcherTLS:
		return 3
	case ComponentKindPlc, ComponentKindRelay:
		return 4
	case ComponentKindPds:
		return 5
	}
	return 6
}

func (c *Composer) StopComponents(ctx context.Context, r Root, ws string) (int, error) {
	ws = workspaceOr(ws, r.Workspace)
	views, err := c.ListComponents(ctx, ws, r.Owner())
	if err != nil {
		return 0, err
	}
	if len(views) == 0 {
		return 0, c.DeleteLinks(ctx, r, ws)
	}
	lowest := componentRank(views[0].Kind)
	for _, v := range views {
		lowest = min(lowest, componentRank(v.Kind))
	}
	for _, v := range views {
		if componentRank(v.Kind) == lowest {
			if err := c.DeleteComponent(ctx, ws, v.Kind, v.Name); err != nil {
				return len(views), err
			}
		}
	}
	return len(views), nil
}

func (c *Composer) ReleaseRecords(ctx context.Context, recordsWs string, pdsUp bool) (int, error) {
	ri, err := c.Client(recordsWs, AtprotoRecordGVR)
	if err != nil {
		return 0, err
	}
	items, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || (err == nil && len(items) == 0) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for i := range items {
		o := &items[i]
		switch {
		case o.DeletionTimestamp == nil:
			err = ri.Delete(ctx, o.Name, metav1.DeleteOptions{})
		case !pdsUp:
			_, err = kube.Patch[metav1.PartialObjectMetadata](ctx, ri, o.Name, types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return len(items), err
		}
	}
	return len(items), nil
}

func Stamp(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}
