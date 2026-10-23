package appctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	QuotaName      = "socialweb-bounded"
	QuotaNamespace = "default"
	QuotaUnlimited = -1
)

var ResourceQuotaGVR = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}

var QuotaDefaults = struct {
	Bids, ComputeVMs, ComputeContracts, NodeClaims, PolicyWorkflowRuns, Components int64
}{Bids: 10000, ComputeVMs: 64, ComputeContracts: 1024, NodeClaims: 64, PolicyWorkflowRuns: 500, Components: 16}

type quotaEntry struct {
	component string
	gvr       schema.GroupVersionResource
	field     string
	def       int64
	gate      bool
}

func (e quotaEntry) key() string { return "count/" + e.gvr.Resource + "." + e.gvr.Group }

func (e quotaEntry) name() string { return e.gvr.Resource + "." + e.gvr.Group }

func componentQuotaEntries(rootKind string) []quotaEntry {
	kinds := ComponentKindsFor(rootKind)
	out := make([]quotaEntry, 0, len(kinds))
	for _, kind := range kinds {
		ct, ok := ComponentTypeFor(kind)
		if !ok {
			continue
		}
		out = append(out, quotaEntry{"", ct.GVR, "components", QuotaDefaults.Components, false})
	}
	return out
}

func withComponentQuotas(entries []quotaEntry, rootKind string) []quotaEntry {
	out := append([]quotaEntry{}, entries...)
	return append(out, componentQuotaEntries(rootKind)...)
}

func quotaPlan(rootKind string) []quotaEntry {
	runs := []quotaEntry{
		{"", PolicyWorkflowRunGVR, "policyWorkflowRuns", QuotaDefaults.PolicyWorkflowRuns, false},
	}
	switch rootKind {
	case string(v1alpha1.ApplicationKindMarketBidder):
		return withComponentQuotas(append([]quotaEntry{
			{"", v1alpha1.BidGVR, "bids", QuotaDefaults.Bids, true},
			{"", v1alpha1.ComputeVMGVR, "computeVMs", QuotaDefaults.ComputeVMs, true},
		}, runs...), rootKind)
	case string(v1alpha1.ApplicationKindMarketRequester):
		return withComponentQuotas(append([]quotaEntry{{"", v1alpha1.ComputeContractGVR, "computeContracts", QuotaDefaults.ComputeContracts, false}}, runs...), rootKind)
	case string(v1alpha1.ApplicationKindGateway):
		return withComponentQuotas(append([]quotaEntry{{ComponentWorkspace, v1alpha1.ComputeContractGVR, "computeContracts", QuotaDefaults.ComputeContracts, false}}, runs...), rootKind)
	case string(v1alpha1.ApplicationKindSshGateway):
		return withComponentQuotas(runs, rootKind)
	case "MarketNetwork":
		return withComponentQuotas(runs, rootKind)
	}
	return nil
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

func quotaLimit(root *unstructured.Unstructured, e quotaEntry) int64 {
	m, _, _ := unstructured.NestedMap(root.Object, "spec", "quota")
	switch n := m[e.field].(type) {
	case int64:
		if n != 0 {
			return n
		}
	case float64:
		if n != 0 {
			return int64(n)
		}
	}
	return e.def
}

func (ctl *Controller) quotaWorkspace(component string) string {
	if component == "" {
		return ctl.c.workspace()
	}
	return ctl.c.workspace() + ":" + component
}

func (ctl *Controller) ensureQuotas(ctx context.Context, root *unstructured.Unstructured) {
	if root.GetDeletionTimestamp() != nil || root.GetUID() == "" {
		return
	}
	byWS := map[string]map[string]string{}
	for _, e := range quotaPlan(root.GetKind()) {
		if e.gate {
			continue
		}
		ws := ctl.quotaWorkspace(e.component)
		if byWS[ws] == nil {
			byWS[ws] = map[string]string{}
		}
		if n := quotaLimit(root, e); n > 0 {
			byWS[ws][e.key()] = fmt.Sprint(n)
		}
	}
	for ws, hard := range byWS {
		sum := sha256.Sum256([]byte(string(root.GetUID()) + fmt.Sprint(sortedPairs(hard))))
		stamp := hex.EncodeToString(sum[:8])
		if ctl.quotaApplied[ws] == stamp {
			continue
		}
		if err := ctl.applyQuota(ctx, root, ws, hard); err != nil {
			if ctx.Err() == nil && ctl.quotaFailed[ws] != err.Error() {
				ctl.quotaFailed[ws] = err.Error()
				ctl.c.log().Warn("applying the object-count quota of a workspace; the next pass retries", "workspace", ws, "err", err.Error())
			}
			continue
		}
		delete(ctl.quotaFailed, ws)
		ctl.quotaApplied[ws] = stamp
		ctl.c.log().Info("a workspace carries its object-count quota", "workspace", ws, "hard", fmt.Sprint(sortedPairs(hard)))
	}
}

func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (ctl *Controller) applyQuota(ctx context.Context, root *unstructured.Unstructured, ws string, hard map[string]string) error {
	ri, err := ctl.client(ws, ResourceQuotaGVR)
	if err != nil {
		return err
	}
	rc := ri.Namespace(QuotaNamespace)
	if len(hard) == 0 {
		err := rc.Delete(ctx, QuotaName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	meta := map[string]any{"name": QuotaName, "namespace": QuotaNamespace}
	if ws == ctl.c.workspace() {
		meta["ownerReferences"] = []metav1.OwnerReference{ownerRefOf(root)}
	}
	hardAny := map[string]any{}
	for k, v := range hard {
		hardAny[k] = v
	}
	body, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": meta, "spec": map[string]any{"hard": hardAny}})
	if err != nil {
		return err
	}
	force := true
	_, err = rc.Patch(ctx, QuotaName, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (ctl *Controller) countObjects(ctx context.Context, ws string, res schema.GroupVersionResource) (int64, bool) {
	ri, err := ctl.client(ws, res)
	if err != nil {
		return 0, false
	}
	list, err := ri.List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return 0, false
	}
	n := int64(len(list.Items))
	if r := list.GetRemainingItemCount(); r != nil {
		return n + *r, true
	}
	if list.GetContinue() != "" {
		all, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, false
		}
		return int64(len(all.Items)), true
	}
	return n, true
}

func (ctl *Controller) readQuotas(ctx context.Context) []v1alpha1.QuotaUsage {
	var out []v1alpha1.QuotaUsage
	for _, g := range rootGVRs {
		ri, err := ctl.client(ctl.c.workspace(), g)
		if err != nil {
			continue
		}
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			root := &list.Items[i]
			for _, e := range quotaPlan(root.GetKind()) {
				hard := quotaLimit(root, e)
				if hard <= 0 {
					continue
				}
				ws := ctl.quotaWorkspace(e.component)
				used, ok := ctl.countObjects(ctx, ws, e.gvr)
				if !ok {
					continue
				}
				out = append(out, v1alpha1.QuotaUsage{Workspace: ws, Resource: e.name(), Hard: hard, Used: used})
			}
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
