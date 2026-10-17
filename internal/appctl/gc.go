package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
)

const (
	GCSampleEvery  = 30 * time.Second
	GCStuckAfter   = 10 * time.Minute
	GCResample     = 5 * time.Minute
	GCExamples     = 5
	GCListCap      = 40
	TTLAnnotation  = "socialweb.computer/ttl-after-finished"
	ClusterLabel   = "socialweb.computer/cluster"
	NodeClaimNote  = "socialweb.computer/nodeclaim"
	providerWS     = "socialweb-provider"
	denoProviderWS = "deno-provider"

	accountWorkspaceType = "account"

	GCWorkspaceCap = 200
)

func gvr(group, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: resource}
}

type gcKind struct {
	Kind string
	GVR  schema.GroupVersionResource
}

var gcKinds = []gcKind{
	{"Bid", gvr(v1alpha1.GroupName, "bids")},
	{"ComputeVM", gvr(v1alpha1.GroupName, "computevms")},
	{"GuestIdentityGrant", gvr(v1alpha1.GroupName, "guestidentitygrants")},
	{"HostProcess", HostProcessGVR},
	{"OpenBaoServer", OpenBaoServerGVR},
	{"Postgres", gvr(v1alpha1.GroupName, "postgreses")},
	{"MarketIdentity", MarketIdentityGVR},
	{"ComputeContract", ComputeContractGVR},
	{"NodeClaim", gvr(v1alpha1.GroupName, "nodeclaims")},
	{"DenoPod", DenoPodGVR},
	{"PolicyEngine", gvr("deno.computer", "policyengines")},
	{"DenoJob", gvr("deno.computer", "denojobs")},
	{"DenoRun", gvr("deno.computer", "denoruns")},
	{"RunTrigger", gvr("deno.computer", "runtriggers")},
	{"PolicyWorkflowPod", gvr("deno.computer", "policyworkflowpods")},
	{"PolicyWorkflowRun", gvr("deno.computer", "policyworkflowruns")},
}

var rootGVRs = []schema.GroupVersionResource{MarketNetworkGVR, MarketBidderGVR, MarketRequesterGVR, GatewayGVR, SshGatewayGVR}

var FinalizerControllers = map[string]string{
	Finalizer:                                       "appctl: the application controller of the core",
	"computevm.socialweb.computer/release":          "computevm: the ComputeVM controller of the core",
	"socialweb.computer/postgres":                   "postgresctl: the Postgres controller of the core",
	"socialweb.computer/cluster-member":             "clusterprovider: the cluster provider of the gateway",
	"cluster.socialweb.computer/teardown":           "clusterprovider: the cluster provider of the gateway",
	"node.socialweb.computer/teardown":              "clusterprovider: the cluster provider of the gateway",
	"nodeclaim.socialweb.computer/release":          "socialweb-computer-k8s web app: the node-market claim mapper",
	"atprotoidentity.socialweb.computer/revocation": "socialweb-computer-k8s web app: the revocation sweep",
	"denopod.deno.computer/run":                     "denokcp provider: the deno provider of the core",
	"denorun.deno.computer/run":                     "denokcp provider: the deno provider of the core",
	"policyengine.deno.computer/run":                "denokcp provider: the deno provider of the core",
	"policyworkflowrun.deno.computer/run":           "denokcp provider: the deno provider of the core",
	"openbao.deno.computer/namespace":               "denokcp provider: the deno provider of the core",
}

func finalizerController(f string) string {
	if c, ok := FinalizerControllers[f]; ok {
		return c
	}
	return "unknown: no controller is registered for this finalizer"
}

func (ctl *Controller) gcWorkspaces(ctx context.Context) []string {
	root := ctl.c.workspace()
	out := []string{root}
	ri, err := ctl.client(root, WorkspaceGVR)
	if err != nil {
		return out
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, w := range list.Items {
		name := w.GetName()
		if _, app := w.GetLabels()[AppWorkspaceLabel]; app || name == providerWS || name == denoProviderWS || w.GetDeletionTimestamp() != nil {
			continue
		}
		if phase, _, _ := unstructured.NestedString(w.Object, "status", "phase"); phase != "Ready" {
			continue
		}
		out = append(out, root+":"+name)
		if typeName, _, _ := unstructured.NestedString(w.Object, "spec", "type", "name"); typeName == accountWorkspaceType && len(out) < GCWorkspaceCap {
			sub, err := ctl.client(root+":"+name, WorkspaceGVR)
			if err != nil {
				continue
			}
			homes, err := sub.List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, h := range homes.Items {
				if h.GetDeletionTimestamp() == nil && len(out) < GCWorkspaceCap {
					out = append(out, root+":"+name+":"+h.GetName())
				}
			}
		}
	}
	return out
}

func (ctl *Controller) rootNames(ctx context.Context) map[string]bool {
	names := map[string]bool{}
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
			names[ownerOf(&list.Items[i])] = true
		}
	}
	return names
}

func ownedByLink(u *unstructured.Unstructured, roots map[string]bool) bool {
	if len(u.GetOwnerReferences()) > 0 {
		return true
	}
	if _, ok := u.GetAnnotations()[TTLAnnotation]; ok {
		return true
	}
	if _, ok := u.GetAnnotations()[NodeClaimNote]; ok {
		return true
	}
	labels := u.GetLabels()
	if _, ok := labels[ClusterLabel]; ok {
		return true
	}
	if o, ok := labels[OwnerLabel]; ok {
		return roots[o]
	}
	return false
}

func exemptFromOwnership(kind string, u *unstructured.Unstructured) bool {
	if kind == "HostProcess" && strings.HasPrefix(u.GetName(), hostprocess.MirrorPrefix) {
		return true
	}
	return false
}

func addExample(ex []string, v string) []string {
	if len(ex) < GCExamples {
		return append(ex, v)
	}
	return ex
}

func (ctl *Controller) sampleGC(ctx context.Context) {
	t := time.NewTimer(GCSampleEvery)
	defer t.Stop()
	for {
		if st := ctl.scanGC(ctx); st != nil {
			prev := ctl.gc.Load()
			if prev != nil && time.Since(ctl.gcAt) < GCResample {
				a, b := *prev, *st
				a.SampledAt, b.SampledAt = "", ""
				ja, _ := json.Marshal(a)
				jb, _ := json.Marshal(b)
				if string(ja) == string(jb) {
					st.SampledAt = prev.SampledAt
				} else {
					ctl.gcAt = time.Now()
				}
			} else {
				ctl.gcAt = time.Now()
			}
			ctl.gc.Store(st)
		}
		if q := ctl.readQuotas(ctx); len(q) > 0 {
			ctl.quotas.Store(&q)
		}
		if ctl.c.TTLReport != nil {
			if st := ctl.c.TTLReport(); st != nil {
				ctl.ttl.Store(st)
			}
		}
		t.Reset(GCSampleEvery)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (ctl *Controller) scanGC(ctx context.Context) *v1alpha1.GCStatus {
	roots := ctl.rootNames(ctx)
	var total int64
	orphans := map[string]*v1alpha1.GCKindCount{}
	stuck := map[string]*v1alpha1.GCStuck{}
	var orphanTotal, stuckTotal int64
	for _, ws := range ctl.gcWorkspaces(ctx) {
		for _, k := range gcKinds {
			ri, err := ctl.client(ws, k.GVR)
			if err != nil {
				continue
			}
			list, err := ri.List(ctx, metav1.ListOptions{})
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			for i := range list.Items {
				u := &list.Items[i]
				total++
				ref := ws + "/" + u.GetName()
				if dt := u.GetDeletionTimestamp(); dt != nil {
					if time.Since(dt.Time) > GCStuckAfter {
						for _, f := range u.GetFinalizers() {
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
				if exemptFromOwnership(k.Kind, u) || ownedByLink(u, roots) {
					continue
				}
				c := orphans[k.Kind]
				if c == nil {
					c = &v1alpha1.GCKindCount{Kind: k.Kind}
					orphans[k.Kind] = c
				}
				c.Count++
				c.Examples = addExample(c.Examples, ref)
				orphanTotal++
			}
		}
	}
	st := &v1alpha1.GCStatus{SampledAt: time.Now().UTC().Format(time.RFC3339), Objects: total, OrphanTotal: orphanTotal, StuckTotal: stuckTotal}
	for _, c := range orphans {
		st.Orphans = append(st.Orphans, *c)
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

func ownerRefOf(root *unstructured.Unstructured) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: root.GetAPIVersion(), Kind: root.GetKind(), Name: root.GetName(), UID: root.GetUID()}
}

func adoptable(rootKind string, kind string, u *unstructured.Unstructured, owner string) bool {
	if len(u.GetOwnerReferences()) > 0 || exemptFromOwnership(kind, u) {
		return false
	}
	if u.GetLabels()[OwnerLabel] == owner {
		return true
	}
	switch kind {
	case "MarketIdentity":
		return true
	case "Bid":
		return rootKind == string(v1alpha1.ApplicationKindMarketBidder)
	case "ComputeContract":
		_, linked := u.GetAnnotations()[NodeClaimNote]
		return rootKind == string(v1alpha1.ApplicationKindMarketRequester) && !linked
	}
	return false
}

var adoptKinds = append(slices.Clone(gcKinds), gcKind{"Workspace", WorkspaceGVR})

func (ctl *Controller) adoptChildren(ctx context.Context, root *unstructured.Unstructured) {
	if root.GetDeletionTimestamp() != nil || root.GetUID() == "" {
		return
	}
	ws := ctl.c.workspace()
	owner := ownerOf(root)
	for _, k := range adoptKinds {
		ri, err := ctl.client(ws, k.GVR)
		if err != nil {
			continue
		}
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			u := &list.Items[i]
			if u.GetDeletionTimestamp() != nil || !adoptable(root.GetKind(), k.Kind, u, owner) {
				continue
			}
			body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(u.GetUID()), "ownerReferences": []metav1.OwnerReference{ownerRefOf(root)}}})
			if err != nil {
				continue
			}
			var rc dynamic.ResourceInterface = ri
			if ns := u.GetNamespace(); ns != "" {
				rc = ri.Namespace(ns)
			}
			_, err = rc.Patch(ctx, u.GetName(), types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				ctl.c.log().Warn("adopting a child under its root object", "root", owner, "kind", k.Kind, "name", u.GetName(), "err", err.Error())
				continue
			}
			if err == nil {
				ctl.c.log().Info("a child hangs from its root object", "root", owner, "kind", k.Kind, "name", u.GetName())
			}
		}
	}
}

func gcSummary(st *v1alpha1.GCStatus) string {
	if st == nil {
		return "no sample yet"
	}
	return fmt.Sprintf("%d objects, %d orphans, %d stuck terminations", st.Objects, st.OrphanTotal, st.StuckTotal)
}
