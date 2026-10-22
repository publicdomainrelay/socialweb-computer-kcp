package ttlctl

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	Annotation       = v1alpha1.AnnotationTTLAfterFinished
	ConditionDone    = "Complete"
	ConditionFailed  = "Failed"
	RetryAfter       = 5 * time.Second
	UnboundRetry     = time.Minute
	DefaultRoot      = "root"
	reportKindsLimit = 40
)

var (
	WorkspaceGVR = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
	groupApps    = v1alpha1.GroupName
)

type Target struct {
	Kind string
	GVR  schema.GroupVersionResource
}

func DefaultTargets() []Target {
	return []Target{
		{"Bid", schema.GroupVersionResource{Group: groupApps, Version: "v1alpha1", Resource: "bids"}},
		{"ComputeContract", schema.GroupVersionResource{Group: groupApps, Version: "v1alpha1", Resource: "computecontracts"}},
		{"PolicyWorkflowRun", schema.GroupVersionResource{Group: policyv1alpha1.GroupName, Version: "v1alpha1", Resource: "policyworkflowruns"}},
	}
}

type Config struct {
	Kubeconfig string
	Root       string
	Skip       []string
	Targets    []Target
	Log        *slog.Logger
	Now        func() time.Time
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "ttlctl")
}

func (c Config) root() string {
	if c.Root == "" {
		return DefaultRoot
	}
	return c.Root
}

func (c Config) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

type deletion struct {
	count int64
	last  time.Time
}

type Controller struct {
	c       Config
	restCfg *rest.Config

	mu        sync.Mutex
	timers    map[string]*time.Timer
	deleted   map[string]*deletion
	changedAt time.Time
}

func New(c Config) (*Controller, error) {
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if len(c.Targets) == 0 {
		c.Targets = DefaultTargets()
	}
	return &Controller{c: c, restCfg: restCfg, timers: map[string]*time.Timer{}, deleted: map[string]*deletion{}, changedAt: c.now()}, nil
}

func clusterConfig(base *rest.Config, ws string) *rest.Config {
	cfg := rest.CopyConfig(base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + ws
	return cfg
}

func (ct *Controller) client(ws string, gvr schema.GroupVersionResource) (dynamic.NamespaceableResourceInterface, error) {
	dc, err := dynamic.NewForConfig(clusterConfig(ct.restCfg, ws))
	if err != nil {
		return nil, err
	}
	return dc.Resource(gvr), nil
}

func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("ttlctl: %q is not a duration", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("ttlctl: %q is not a duration", s)
	}
	return d, nil
}

func FinishedAt(u *unstructured.Unstructured) (time.Time, bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var at time.Time
	found := false
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != ConditionDone && t != ConditionFailed {
			continue
		}
		if s, _ := m["status"].(string); s != "True" {
			continue
		}
		stamp, _ := m["lastTransitionTime"].(string)
		when, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			continue
		}
		if !found || when.After(at) {
			at, found = when, true
		}
	}
	return at, found
}

func keyOf(ws string, t Target, u *unstructured.Unstructured) string {
	return ws + "|" + t.GVR.Resource + "|" + u.GetNamespace() + "|" + u.GetName() + "|" + string(u.GetUID())
}

func (ct *Controller) forget(key string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if t, ok := ct.timers[key]; ok {
		t.Stop()
		delete(ct.timers, key)
		ct.changedAt = ct.c.now()
	}
}

func (ct *Controller) forgetObject(ws string, t Target, u *unstructured.Unstructured) {
	ct.forget(keyOf(ws, t, u))
}

func (ct *Controller) consider(ctx context.Context, ws string, t Target, u *unstructured.Unstructured) {
	key := keyOf(ws, t, u)
	raw, ok := u.GetAnnotations()[Annotation]
	if !ok || u.GetDeletionTimestamp() != nil {
		ct.forget(key)
		return
	}
	ttl, err := ParseTTL(raw)
	if err != nil {
		ct.c.log().Warn("an object carries a TTL annotation that does not parse; it is left alone", "workspace", ws, "kind", t.Kind,
			"name", u.GetName(), "ttl", raw)
		ct.forget(key)
		return
	}
	done, finished := FinishedAt(u)
	if !finished {
		ct.forget(key)
		return
	}
	wait := done.Add(ttl).Sub(ct.c.now())
	if wait <= 0 {
		ct.forget(key)
		ct.remove(ctx, ws, t, u)
		return
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if old, ok := ct.timers[key]; ok {
		old.Stop()
	}
	name, ns := u.GetName(), u.GetNamespace()
	ct.timers[key] = time.AfterFunc(wait, func() { ct.fire(ctx, ws, t, ns, name, key) })
	ct.changedAt = ct.c.now()
}

func (ct *Controller) fire(ctx context.Context, ws string, t Target, ns, name, key string) {
	if ctx.Err() != nil {
		return
	}
	ri, err := ct.client(ws, t.GVR)
	if err != nil {
		return
	}
	var rc dynamic.ResourceInterface = ri
	if ns != "" {
		rc = ri.Namespace(ns)
	}
	cur, err := rc.Get(ctx, name, metav1.GetOptions{})
	ct.forget(key)
	if err != nil {
		if !apierrors.IsNotFound(err) && ctx.Err() == nil {
			ct.c.log().Warn("re-reading an object whose TTL ended", "workspace", ws, "kind", t.Kind, "name", name, "err", err.Error())
		}
		return
	}
	ct.consider(ctx, ws, t, cur)
}

func (ct *Controller) remove(ctx context.Context, ws string, t Target, u *unstructured.Unstructured) {
	ri, err := ct.client(ws, t.GVR)
	if err != nil {
		return
	}
	var rc dynamic.ResourceInterface = ri
	if ns := u.GetNamespace(); ns != "" {
		rc = ri.Namespace(ns)
	}
	uid := u.GetUID()
	err = rc.Delete(ctx, u.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) && ctx.Err() == nil {
			ct.c.log().Warn("deleting an object whose TTL ended", "workspace", ws, "kind", t.Kind, "name", u.GetName(), "err", err.Error())
		}
		return
	}
	ct.mu.Lock()
	d := ct.deleted[t.Kind]
	if d == nil {
		d = &deletion{}
		ct.deleted[t.Kind] = d
	}
	d.count++
	d.last = ct.c.now()
	ct.changedAt = d.last
	ct.mu.Unlock()
	ct.c.log().Info("an object finished more than its TTL ago is deleted", "workspace", ws, "kind", t.Kind, "name", u.GetName(),
		"ttl", u.GetAnnotations()[Annotation])
}

func (ct *Controller) Report() *v1alpha1.TTLStatus {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	st := &v1alpha1.TTLStatus{SampledAt: ct.changedAt.UTC().Format(time.RFC3339), Tracked: int64(len(ct.timers))}
	for kind, d := range ct.deleted {
		st.Deleted = append(st.Deleted, v1alpha1.TTLDeleted{Kind: kind, Count: d.count, LastAt: d.last.UTC().Format(time.RFC3339)})
		st.DeletedTotal += d.count
	}
	slices.SortFunc(st.Deleted, func(a, b v1alpha1.TTLDeleted) int { return strings.Compare(a.Kind, b.Kind) })
	if len(st.Deleted) > reportKindsLimit {
		st.Deleted = st.Deleted[:reportKindsLimit]
	}
	return st
}

func (ct *Controller) follow(ctx context.Context, ws string, t Target) {
	for ctx.Err() == nil {
		ri, err := ct.client(ws, t.GVR)
		if err != nil {
			return
		}
		opened := time.Now()
		w, err := ri.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			wait := RetryAfter
			if apierrors.IsNotFound(err) {
				wait = UnboundRetry
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		for ev := range w.ResultChan() {
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if ev.Type == watch.Deleted {
				ct.forgetObject(ws, t, u)
				continue
			}
			if ev.Type == watch.Added || ev.Type == watch.Modified {
				ct.consider(ctx, ws, t, u)
			}
		}
		w.Stop()
		if time.Since(opened) < RetryAfter {
			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryAfter):
			}
		}
	}
}

func (ct *Controller) skipped(name string) bool { return slices.Contains(ct.c.Skip, name) }

func (ct *Controller) startWorkspace(ctx context.Context, ws string) context.CancelFunc {
	wctx, cancel := context.WithCancel(ctx)
	for _, t := range ct.c.Targets {
		go ct.follow(wctx, ws, t)
	}
	ct.c.log().Info("watching the objects of a workspace for TTLs", "workspace", ws, "kinds", len(ct.c.Targets))
	return cancel
}

func (ct *Controller) Run(ctx context.Context) error {
	root := ct.c.root()
	active := map[string]context.CancelFunc{root: ct.startWorkspace(ctx, root)}
	defer func() {
		for _, cancel := range active {
			cancel()
		}
	}()
	for ctx.Err() == nil {
		ri, err := ct.client(root, WorkspaceGVR)
		if err != nil {
			return err
		}
		opened := time.Now()
		w, err := ri.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(RetryAfter):
			}
			continue
		}
		for ev := range w.ResultChan() {
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok || ct.skipped(u.GetName()) {
				continue
			}
			ws := root + ":" + u.GetName()
			if ev.Type == watch.Deleted || u.GetDeletionTimestamp() != nil {
				if cancel, ok := active[ws]; ok {
					cancel()
					delete(active, ws)
				}
				continue
			}
			phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
			if phase != "Ready" {
				continue
			}
			if _, ok := active[ws]; !ok {
				active[ws] = ct.startWorkspace(ctx, ws)
			}
		}
		w.Stop()
		if time.Since(opened) < RetryAfter {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(RetryAfter):
			}
		}
	}
	return nil
}

func Supervise(ctx context.Context, ct *Controller) {
	log := ct.c.log()
	for {
		err := ct.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Warn("the TTL controller stopped; starting it again", "err", fmt.Sprint(err), "after", RetryAfter.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryAfter):
		}
	}
}
