package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	denov1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/denoruntime"
)

const profileSeparator = "@"

func ProfilePhases(set manifests.Set, profile string, components bool) (manifests.Set, error) {
	out := set
	out.Phases = nil
	for _, p := range set.Phases {
		_, only, scoped := strings.Cut(p.Name, profileSeparator)
		if scoped != components || (scoped && only != profile) {
			continue
		}
		out.Phases = append(out.Phases, p)
	}
	if components && len(out.Phases) == 0 {
		return manifests.Set{}, fmt.Errorf("appctl: set %s has no component for profile %q", set.Name, profile)
	}
	return out, nil
}

func (ctl *Controller) applyProfile(ctx context.Context, setName, profile, owner string, components bool, values any) ([]string, error) {
	ix, err := manifests.Load()
	if err != nil {
		return nil, err
	}
	set, ok := ix.Set(setName)
	if !ok {
		return nil, fmt.Errorf("appctl: deploy/sets.yaml names no %s set", setName)
	}
	set, err = ProfilePhases(set, profile, components)
	if err != nil {
		return nil, err
	}
	ap, err := kcpapply.New(ctl.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, FieldManager: FieldManager, Base: ctl.c.workspace(),
		Labels: map[string]string{OwnerLabel: owner}, Log: ctl.c.log()})
	if err != nil {
		return nil, err
	}
	rep, err := ap.Apply(ctx, set, values)
	if err != nil {
		return nil, err
	}
	ctl.recordPlaced(owner, components, rep)
	var pods []string
	for _, p := range rep.Phases {
		for _, c := range p.Changes {
			if c.Kind == "DenoPod" {
				pods = append(pods, c.Name)
			}
		}
	}
	return pods, nil
}

type placedRef struct {
	Workspace string
	Kind      string
	Namespace string
	Name      string
}

var placedKinds = map[string]schema.GroupVersionResource{"DenoPod": DenoPodGVR, "HostProcess": HostProcessGVR}

func (ctl *Controller) recordPlaced(owner string, components bool, rep kcpapply.Report) {
	var refs []placedRef
	for _, p := range rep.Phases {
		for _, c := range p.Changes {
			if _, ok := placedKinds[c.Kind]; ok {
				refs = append(refs, placedRef{Workspace: p.Workspace, Kind: c.Kind, Namespace: c.Namespace, Name: c.Name})
			}
		}
	}
	if ctl.placed[owner] == nil {
		ctl.placed[owner] = map[bool][]placedRef{}
	}
	ctl.placed[owner][components] = refs
}

func (ctl *Controller) forgetPlaced(owner string) { delete(ctl.placed, owner) }

func (ctl *Controller) lostComponent(ctx context.Context, owner string) string {
	type group struct{ ws, kind, ns string }
	want := map[group][]string{}
	for _, refs := range ctl.placed[owner] {
		for _, r := range refs {
			g := group{r.Workspace, r.Kind, r.Namespace}
			want[g] = append(want[g], r.Name)
		}
	}
	for g, names := range want {
		ri, err := ctl.client(g.ws, placedKinds[g.kind])
		if err != nil {
			return ""
		}
		var rc dynamic.ResourceInterface = ri
		if g.ns != "" {
			rc = ri.Namespace(g.ns)
		}
		list, err := rc.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
		if err != nil {
			return ""
		}
		have := map[string]bool{}
		for _, it := range list.Items {
			have[it.GetName()] = true
		}
		for _, n := range names {
			if !have[n] {
				ctl.c.log().Warn("a component of a root object is gone; applying its components again", "owner", owner, "kind", g.kind,
					"workspace", g.ws, "name", n)
				return g.kind + " " + g.ws + "/" + n
			}
		}
	}
	return ""
}

func (ctl *Controller) ownedPods(ctx context.Context, ws, owner string) ([]unstructured.Unstructured, error) {
	ri, err := ctl.client(ws, DenoPodGVR)
	if err != nil {
		return nil, err
	}
	list, err := ri.Namespace("default").List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (ctl *Controller) deletePods(ctx context.Context, ws, owner string, keep []string) (int, error) {
	pods, err := ctl.ownedPods(ctx, ws, owner)
	if err != nil {
		return 0, err
	}
	ri, err := ctl.client(ws, DenoPodGVR)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, p := range pods {
		if slices.Contains(keep, p.GetName()) {
			continue
		}
		left++
		if p.GetDeletionTimestamp() != nil {
			continue
		}
		if err := ri.Namespace("default").Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return left, err
		}
		ctl.c.log().Info("deleted a component the profile omits", "owner", owner, "denopod", p.GetName(), "workspace", ws)
	}
	return left, nil
}

type podView struct {
	Name       string
	Workspace  string
	Phase      string
	Ready      bool
	RunID      string
	Address    string
	Finalizers []string
	Listeners  []v1alpha1.ApplicationListener
}

func (v podView) stopsItsProcess() bool {
	return slices.Contains(v.Finalizers, denov1alpha1.FinalizerDenoPod)
}

func podViewOf(ws string, u *unstructured.Unstructured) podView {
	v := podView{Name: u.GetName(), Workspace: ws, Finalizers: u.GetFinalizers()}
	v.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	v.Ready, _, _ = unstructured.NestedBool(u.Object, "status", "ready")
	v.RunID, _, _ = unstructured.NestedString(u.Object, "status", "runID")
	v.Address, _, _ = unstructured.NestedString(u.Object, "status", "address")
	ls, _, _ := unstructured.NestedSlice(u.Object, "status", "listeners")
	for _, l := range ls {
		m, ok := l.(map[string]any)
		if !ok {
			continue
		}
		v.Listeners = append(v.Listeners, v1alpha1.ApplicationListener{Component: v.Name, Name: fmt.Sprint(m["name"]),
			Kind: fmt.Sprint(m["kind"]), Address: fmt.Sprint(m["address"])})
	}
	return v
}

func (ctl *Controller) pod(ctx context.Context, ws, name string) (podView, bool, error) {
	ri, err := ctl.client(ws, DenoPodGVR)
	if err != nil {
		return podView{}, false, err
	}
	u, err := ri.Namespace("default").Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return podView{Name: name, Workspace: ws}, false, nil
	}
	if err != nil {
		return podView{}, false, err
	}
	return podViewOf(ws, u), true, nil
}

func (v podView) component() v1alpha1.ApplicationComponent {
	return v1alpha1.ApplicationComponent{Kind: "DenoPod", Name: v.Name, Workspace: v.Workspace, Phase: v.Phase, Ready: v.Ready}
}

func (ctl *Controller) runStatus(runID string) map[string]any {
	if runID == "" {
		return nil
	}
	state := ctl.c.Layout.StateDir
	if rec, err := ctl.c.Record(); err == nil && rec.SharedKcp != "" {
		state = rec.SharedKcp
	}
	b, err := os.ReadFile(filepath.Join(denoruntime.RunsDir(state), runID, "status.json"))
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
