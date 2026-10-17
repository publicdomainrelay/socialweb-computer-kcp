package kcpapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	FieldManager       = "socialweb-computer"
	DefaultWaitTimeout = time.Minute
	DefaultPoll        = time.Second
	defaultNamespace   = "default"
	schemaKind         = "APIResourceSchema"
)

var (
	ErrSchemaChanged  = errors.New("kcpapply: an APIResourceSchema is immutable and its live spec differs; publish a new revision name")
	ErrKubectlManaged = errors.New("kcpapply: the object was applied by kubectl and is never adopted; apply into a fresh instance")
	ErrWaitTimedOut   = errors.New("kcpapply: a wait did not hold before its deadline")
	ErrWaitFailed     = errors.New("kcpapply: a waited-on object reports a failure, so the wait cannot hold")
)

type Action string

const (
	Created    Action = "created"
	Configured Action = "configured"
	Unchanged  Action = "unchanged"
)

type Options struct {
	DryRun       bool
	WaitTimeout  time.Duration
	Poll         time.Duration
	FieldManager string
	Base         string
	Labels       map[string]string
	Log          *slog.Logger
}

func (a *Applier) fieldManager() string {
	if a.opts.FieldManager != "" {
		return a.opts.FieldManager
	}
	return FieldManager
}

type FieldChange struct {
	Field string   `json:"field"`
	From  []string `json:"from"`
	To    []string `json:"to"`
}

type Change struct {
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace,omitempty"`
	Name      string        `json:"name"`
	Action    Action        `json:"action"`
	Managers  []FieldChange `json:"managers,omitempty"`
}

type WaitReport struct {
	When      string  `json:"when"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name,omitempty"`
	Workspace string  `json:"workspace"`
	For       string  `json:"for"`
	Met       bool    `json:"met"`
	Captured  string  `json:"captured,omitempty"`
	Seconds   float64 `json:"seconds"`
}

type PhaseReport struct {
	Set       string       `json:"set"`
	Phase     string       `json:"phase"`
	Workspace string       `json:"workspace"`
	Changes   []Change     `json:"changes"`
	Waits     []WaitReport `json:"waits,omitempty"`
}

type Report struct {
	DryRun bool          `json:"dryRun"`
	Phases []PhaseReport `json:"phases"`
}

func (r Report) Changed() int {
	n := 0
	for _, p := range r.Phases {
		for _, c := range p.Changes {
			if c.Action != Unchanged {
				n++
			}
		}
	}
	return n
}

type Applier struct {
	base  *rest.Config
	opts  Options
	owner string
}

type workspaceOwner struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups,omitempty"`
}

func New(cfg *rest.Config, o Options) (*Applier, error) {
	if cfg == nil {
		return nil, errors.New("kcpapply: no rest.Config")
	}
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("kcpapply: the rest.Config host %q is no URL", cfg.Host)
	}
	if i := strings.Index(u.Path, "/clusters/"); i >= 0 {
		u.Path = u.Path[:i]
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	base := rest.CopyConfig(cfg)
	base.Host = u.String()
	if base.QPS == 0 {
		base.QPS, base.Burst = 50, 100
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = DefaultWaitTimeout
	}
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Applier{base: base, opts: o}, nil
}

func (a *Applier) ForWorkspace(path string) *rest.Config {
	c := rest.CopyConfig(a.base)
	c.Host = a.base.Host + "/clusters/" + path
	return c
}

func (a *Applier) Apply(ctx context.Context, set manifests.Set, values any) (Report, error) {
	if set.Base == "" {
		set.Base = a.opts.Base
	}
	rep := Report{DryRun: a.opts.DryRun}
	if set.BaseCluster == "" && set.Scope != manifests.ScopeKcp && set.Base != "" && set.Base != "root" {
		id, err := a.ClusterOf(ctx, set.Base)
		if err != nil {
			return rep, fmt.Errorf("kcpapply: set %s: the logical cluster of %s: %w", set.Name, set.Base, err)
		}
		set.BaseCluster = id
	}
	for _, p := range set.Phases {
		pr, err := a.applyPhase(ctx, set, p, values)
		rep.Phases = append(rep.Phases, pr)
		if err != nil {
			return rep, fmt.Errorf("kcpapply: set %s phase %s: %w", set.Name, p.Name, err)
		}
	}
	return rep, nil
}

func (a *Applier) applyPhase(ctx context.Context, set manifests.Set, p manifests.Phase, values any) (PhaseReport, error) {
	pr := PhaseReport{Set: set.Name, Phase: p.Name, Changes: []Change{}}
	ws, err := set.Workspace(p, nil, values)
	if err != nil {
		return pr, err
	}
	pr.Workspace = ws
	for _, w := range p.Before {
		wr, err := a.wait(ctx, set, p, w, values, nil, "before")
		pr.Waits = append(pr.Waits, wr)
		if err != nil {
			return pr, err
		}
	}
	r, err := set.RenderPhase(p, values)
	if err != nil {
		return pr, err
	}
	if len(r.Objects) > 0 {
		changes, err := a.ApplyObjects(ctx, ws, r.Objects)
		pr.Changes = append(pr.Changes, changes...)
		if err != nil {
			return pr, err
		}
		a.opts.Log.Info("phase applied", "set", set.Name, "phase", p.Name, "workspace", ws, "objects", len(changes))
	}
	for _, w := range p.After {
		wr, err := a.wait(ctx, set, p, w, values, r.Objects, "after")
		pr.Waits = append(pr.Waits, wr)
		if err != nil {
			return pr, err
		}
	}
	return pr, nil
}

func (a *Applier) ClusterOf(ctx context.Context, path string) (string, error) {
	i := strings.LastIndex(path, ":")
	if i < 0 {
		return path, nil
	}
	dyn, err := dynamic.NewForConfig(a.ForWorkspace(path[:i]))
	if err != nil {
		return "", err
	}
	u, err := dyn.Resource(tenancyv1alpha1.SchemeGroupVersion.WithResource("workspaces")).Get(ctx, path[i+1:], metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	id, _, _ := unstructured.NestedString(u.Object, "spec", "cluster")
	if id == "" {
		return "", fmt.Errorf("the workspace %s names no logical cluster yet", path)
	}
	return id, nil
}

func (a *Applier) ApplyObjects(ctx context.Context, workspace string, objs []manifests.Object) ([]Change, error) {
	cfg := a.ForWorkspace(workspace)
	served, err := a.servedFor(ctx, cfg, workspace, objs)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, o := range objs {
		u := o.Unstructured
		gvk := u.GroupVersionKind()
		m, ok := served[gvk]
		if !ok {
			return out, fmt.Errorf("%s: %s %s is not served in %s", o.Path, gvk, u.GetName(), workspace)
		}
		var ri dynamic.ResourceInterface = dyn.Resource(m.resource)
		ns := ""
		if m.namespaced {
			ns = u.GetNamespace()
			if ns == "" {
				ns = defaultNamespace
			}
			ri = dyn.Resource(m.resource).Namespace(ns)
		}
		c, err := a.applyOne(ctx, ri, u)
		c.Namespace = ns
		out = append(out, c)
		if err != nil {
			return out, fmt.Errorf("%s: %s %s in %s: %w", o.Path, gvk.Kind, u.GetName(), workspace, err)
		}
	}
	return out, nil
}

type servedResource struct {
	resource   schema.GroupVersionResource
	namespaced bool
}

func (a *Applier) servedFor(ctx context.Context, cfg *rest.Config, workspace string, objs []manifests.Object) (map[schema.GroupVersionKind]servedResource, error) {
	deadline := time.Now().Add(a.opts.WaitTimeout)
	for attempt := 1; ; attempt++ {
		served, err := discover(ctx, cfg, objs)
		if err != nil {
			return nil, fmt.Errorf("discovery for %s: %w", workspace, err)
		}
		var missing *manifests.Object
		for i := range objs {
			if _, ok := served[objs[i].Unstructured.GroupVersionKind()]; !ok {
				missing = &objs[i]
				break
			}
		}
		if missing == nil {
			return served, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s: %s %s is not served in %s after %s", missing.Path,
				missing.Unstructured.GroupVersionKind(), missing.Unstructured.GetName(), workspace, a.opts.WaitTimeout)
		}
		a.opts.Log.Info("waiting for every object to be served", "workspace", workspace, "attempt", attempt,
			"path", missing.Path, "kind", missing.Unstructured.GroupVersionKind().String(), "deadline", a.opts.WaitTimeout.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.opts.Poll):
		}
	}
}

func discover(ctx context.Context, cfg *rest.Config, objs []manifests.Object) (map[schema.GroupVersionKind]servedResource, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	out := map[schema.GroupVersionKind]servedResource{}
	fetched := map[schema.GroupVersion]bool{}
	for _, o := range objs {
		gv := o.Unstructured.GroupVersionKind().GroupVersion()
		if fetched[gv] {
			continue
		}
		fetched[gv] = true
		path := "/apis/" + gv.Group + "/" + gv.Version
		if gv.Group == "" {
			path = "/api/" + gv.Version
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Host+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		var list metav1.APIResourceList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			out[gv.WithKind(r.Kind)] = servedResource{resource: gv.WithResource(r.Name), namespaced: r.Namespaced}
		}
	}
	return out, nil
}

func (a *Applier) dryRun() []string {
	if a.opts.DryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func (a *Applier) applyOne(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured) (Change, error) {
	c := Change{Kind: u.GetKind(), Name: u.GetName()}
	live, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		live = nil
	case err != nil:
		return c, err
	}
	if live != nil {
		if err := refuseKubectl(live); err != nil {
			return c, err
		}
	}
	if u.GetKind() == schemaKind {
		return a.applySchema(ctx, ri, u, live, c)
	}
	if len(a.opts.Labels) > 0 {
		u = u.DeepCopy()
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range a.opts.Labels {
			labels[k] = v
		}
		u.SetLabels(labels)
	}
	if live == nil && u.GroupVersionKind().Group == tenancyv1alpha1.SchemeGroupVersion.Group && u.GetKind() == "Workspace" {
		stamped, err := a.stampOwner(ctx, u)
		if err != nil {
			return c, err
		}
		u = stamped
	}
	body, err := json.Marshal(u.Object)
	if err != nil {
		return c, err
	}
	force := true
	got, err := ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: a.fieldManager(), Force: &force, DryRun: a.dryRun()})
	if err != nil {
		return c, err
	}
	c.Managers = managerChanges(live, got)
	switch {
	case live == nil:
		c.Action = Created
	case len(c.Managers) == 0 && sameContent(live, got):
		c.Action = Unchanged
	default:
		c.Action = Configured
	}
	return c, nil
}

func (a *Applier) stampOwner(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if _, set := u.GetAnnotations()[tenancyv1alpha1.ExperimentalWorkspaceOwnerAnnotationKey]; set {
		return u, nil
	}
	if a.owner == "" {
		dyn, err := dynamic.NewForConfig(a.ForWorkspace("root"))
		if err != nil {
			return nil, err
		}
		review := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
		got, err := dyn.Resource(authenticationv1.SchemeGroupVersion.WithResource("selfsubjectreviews")).Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading the applying identity for the workspace owner: %w", err)
		}
		var r authenticationv1.SelfSubjectReview
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, &r); err != nil {
			return nil, err
		}
		b, err := json.Marshal(workspaceOwner{Username: r.Status.UserInfo.Username, Groups: r.Status.UserInfo.Groups})
		if err != nil {
			return nil, err
		}
		a.owner = string(b)
	}
	out := u.DeepCopy()
	ann := out.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[tenancyv1alpha1.ExperimentalWorkspaceOwnerAnnotationKey] = a.owner
	out.SetAnnotations(ann)
	return out, nil
}

func (a *Applier) applySchema(ctx context.Context, ri dynamic.ResourceInterface, u, live *unstructured.Unstructured, c Change) (Change, error) {
	if live == nil {
		got, err := ri.Create(ctx, u, metav1.CreateOptions{FieldManager: a.fieldManager(), DryRun: a.dryRun()})
		switch {
		case err == nil:
			c.Action, c.Managers = Created, managerChanges(nil, got)
			return c, nil
		case apierrors.IsAlreadyExists(err):
			live, err = ri.Get(ctx, u.GetName(), metav1.GetOptions{})
			if err != nil {
				return c, err
			}
		default:
			return c, err
		}
	}
	want, _, _ := unstructured.NestedMap(u.Object, "spec")
	have, _, _ := unstructured.NestedMap(live.Object, "spec")
	if at, same := derivative(want, have, "spec"); !same {
		return c, fmt.Errorf("%w: %s differs at %s", ErrSchemaChanged, u.GetName(), at)
	}
	c.Action = Unchanged
	return c, nil
}

func sameContent(a, b *unstructured.Unstructured) bool {
	strip := func(u *unstructured.Unstructured) map[string]any {
		c := u.DeepCopy()
		unstructured.RemoveNestedField(c.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(c.Object, "metadata", "generation")
		return c.Object
	}
	_, same := derivative(strip(a), strip(b), "")
	_, back := derivative(strip(b), strip(a), "")
	return same && back
}

func derivative(want, have any, at string) (string, bool) {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return at, false
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, same := derivative(w[k], h[k], at+"."+k); !same {
				return p, false
			}
		}
		return at, true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return at, false
		}
		for i := range w {
			if p, same := derivative(w[i], h[i], fmt.Sprintf("%s[%d]", at, i)); !same {
				return p, false
			}
		}
		return at, true
	}
	if wn, ok := number(want); ok {
		hn, ok := number(have)
		return at, ok && wn == hn
	}
	return at, reflect.DeepEqual(want, have)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func refuseKubectl(u *unstructured.Unstructured) error {
	for _, m := range u.GetManagedFields() {
		if strings.HasPrefix(m.Manager, "kubectl") {
			return fmt.Errorf("%w: %s %s is managed by %s", ErrKubectlManaged, u.GetKind(), u.GetName(), m.Manager)
		}
	}
	return nil
}

func owners(u *unstructured.Unstructured) map[string][]string {
	out := map[string][]string{}
	if u == nil {
		return out
	}
	for _, m := range u.GetManagedFields() {
		if m.FieldsV1 == nil {
			continue
		}
		var tree map[string]any
		if json.Unmarshal(m.FieldsV1.GetRawBytes(), &tree) != nil {
			continue
		}
		who := fmt.Sprintf("%s (%s)", m.Manager, m.Operation)
		walkFields(tree, "", func(path string) { out[path] = append(out[path], who) })
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func walkFields(tree map[string]any, at string, leaf func(string)) {
	for k, v := range tree {
		if k == "." {
			continue
		}
		path := k
		if at != "" {
			path = at + "." + k
		}
		child, _ := v.(map[string]any)
		if len(child) == 0 || (len(child) == 1 && child["."] != nil) {
			leaf(path)
			continue
		}
		walkFields(child, path, leaf)
	}
}

func managerChanges(before, after *unstructured.Unstructured) []FieldChange {
	b, a := owners(before), owners(after)
	paths := map[string]bool{}
	for p := range b {
		paths[p] = true
	}
	for p := range a {
		paths[p] = true
	}
	var out []FieldChange
	for p := range paths {
		if !reflect.DeepEqual(b[p], a[p]) {
			out = append(out, FieldChange{Field: p, From: b[p], To: a[p]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func (a *Applier) wait(ctx context.Context, set manifests.Set, p manifests.Phase, w manifests.Wait, values any, applied []manifests.Object, when string) (WaitReport, error) {
	wr := WaitReport{When: when, Kind: w.Kind, Name: w.Name, For: w.For}
	ws, err := set.Workspace(p, &w, values)
	if err != nil {
		return wr, err
	}
	wr.Workspace = ws
	kind := manifests.WaitKinds[w.Kind]
	dyn, err := dynamic.NewForConfig(a.ForWorkspace(ws))
	if err != nil {
		return wr, err
	}
	names := []string{w.Name}
	if w.Name == "" && w.Kind != "APIBinding" {
		names = nil
		for _, o := range applied {
			if o.Unstructured.GetKind() == w.Kind {
				names = append(names, o.Unstructured.GetName())
			}
		}
		if len(names) == 0 {
			return wr, fmt.Errorf("a %s %s wait names no object and the phase applied none", w.Kind, w.For)
		}
	}
	start := time.Now()
	deadline := start.Add(a.opts.WaitTimeout)
	var last error
	for {
		met, captured, err := a.check(ctx, dyn, kind.Resource, kind.Namespaced, w, names)
		last = err
		if errors.Is(err, ErrWaitFailed) && !w.BestEffort {
			wr.Seconds = time.Since(start).Seconds()
			return wr, fmt.Errorf("%s %v in %s for %s: %w", w.Kind, names, ws, w.For, err)
		}
		if met {
			wr.Met, wr.Seconds = true, time.Since(start).Seconds()
			if w.Capture != nil {
				if err := manifests.SetCaptured(values, *w.Capture, captured); err != nil {
					return wr, err
				}
				wr.Captured = captured
			}
			return wr, nil
		}
		if a.opts.DryRun || time.Now().After(deadline) {
			wr.Seconds = time.Since(start).Seconds()
			if a.opts.DryRun || w.BestEffort {
				a.opts.Log.Warn("wait not met; continuing", "kind", w.Kind, "name", w.Name, "workspace", ws, "for", w.For, "dryRun", a.opts.DryRun, "err", last)
				return wr, nil
			}
			return wr, fmt.Errorf("%w: %s %v in %s for %s after %s (last error: %v)", ErrWaitTimedOut, w.Kind, names, ws, w.For, a.opts.WaitTimeout, last)
		}
		t := time.NewTimer(a.opts.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return wr, ctx.Err()
		case <-t.C:
		}
	}
}

func (a *Applier) check(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespaced bool, w manifests.Wait, names []string) (bool, string, error) {
	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if namespaced {
		ri = dyn.Resource(gvr).Namespace(defaultNamespace)
	}
	if w.Kind == "APIBinding" {
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, "", err
		}
		if len(list.Items) == 0 {
			return false, "", errors.New("no APIBinding yet")
		}
		for i := range list.Items {
			if met, _ := w.Met(&list.Items[i]); !met {
				return false, "", fmt.Errorf("APIBinding %s not Ready", list.Items[i].GetName())
			}
		}
		return true, "", nil
	}
	captured := ""
	for _, n := range names {
		u, err := ri.Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			return false, "", err
		}
		met, value := w.Met(u)
		if !met {
			if why := failedObject(u); why != "" {
				return false, "", fmt.Errorf("%w: %s %s %s", ErrWaitFailed, w.Kind, n, why)
			}
			return false, "", fmt.Errorf("%s %s not %s", w.Kind, n, w.For)
		}
		captured = value
	}
	return true, captured, nil
}

func failedObject(u *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "message")
	observed, found, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	current := !found || observed == u.GetGeneration()
	if phase == "Failed" && current {
		return "is Failed: " + msg
	}
	return ""
}
