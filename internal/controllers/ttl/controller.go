package ttl

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ttlctl"
)

const (
	Name = "ttl"

	ExportBid = "computevms"

	ExportComputeContract = "computecontracts"

	ExportPolicyWorkflowRun = "policy"

	ReportKindsLimit = 40
)

type Options struct {
	Log *slog.Logger

	Scope func(workspace string) bool
}

type entry struct {
	kind string

	cluster string

	namespace string

	name string
}

type deletion struct {
	count int64

	last time.Time
}

type Controller struct {
	log *slog.Logger

	mu sync.Mutex

	tracked map[entry]struct{}

	deleted map[string]*deletion

	changedAt time.Time
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", "ttl-controller"), tracked: map[entry]struct{}{}, deleted: map[string]*deletion{},
		changedAt: time.Now()}
	err := errors.Join(
		register(m, c, opts, "Bid", ExportBid, func(o *v1alpha1.Bid) []metav1.Condition { return o.Status.Conditions }),
		register(m, c, opts, "ComputeContract", ExportComputeContract,
			func(o *v1alpha1.ComputeContract) []metav1.Condition { return o.Status.Conditions }),
		register(m, c, opts, "PolicyWorkflowRun", ExportPolicyWorkflowRun,
			func(o *policyv1alpha1.PolicyWorkflowRun) []metav1.Condition { return o.Status.Conditions }))
	if err != nil {
		return nil, err
	}
	return c, nil
}

func register[T ctlruntime.Object](m *ctlruntime.Manager, c *Controller, opts Options, kind, export string, conditions func(T) []metav1.Condition) error {
	r := &expiry[T]{c: c, kind: kind, conditions: conditions}
	options := []ctlruntime.Option{ctlruntime.Export(export),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj T) []ctlruntime.Request {
			return []ctlruntime.Request{{Cluster: src.Cluster, Namespace: obj.GetNamespace(), Name: obj.GetName()}}
		}),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) { c.forget(entryOf(kind, req)) })}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	return ctlruntime.For[T](m, Name+"-"+strings.ToLower(kind), r, options...)
}

func entryOf(kind string, req ctlruntime.Request) entry {
	return entry{kind: kind, cluster: req.Cluster, namespace: req.Namespace, name: req.Name}
}

type expiry[T ctlruntime.Object] struct {
	c *Controller

	kind string

	conditions func(T) []metav1.Condition
}

func (r *expiry[T]) Reconcile(ctx context.Context, req ctlruntime.Request, obj T) (ctlruntime.Result, error) {
	key := entryOf(r.kind, req)
	if !obj.GetDeletionTimestamp().IsZero() {
		r.c.forget(key)
		return ctlruntime.Result{}, nil
	}
	at, due, err := ttlctl.Expiry(obj.GetAnnotations(), r.conditions(obj))
	if err != nil {
		r.c.log.Warn("an object carries a TTL annotation that does not parse; it is left alone", "workspace", req.Workspace,
			"kind", r.kind, "name", req.Name, "ttl", obj.GetAnnotations()[ttlctl.Annotation])
		r.c.forget(key)
		return ctlruntime.Result{}, nil
	}
	if !due {
		r.c.forget(key)
		return ctlruntime.Result{}, nil
	}
	if wait := time.Until(at); wait > 0 {
		r.c.track(key)
		return ctlruntime.Result{RequeueAfter: wait}, nil
	}
	if err := req.Delete(ctx, obj); err != nil {
		return ctlruntime.Result{}, err
	}
	r.c.removed(key)
	r.c.log.Info("an object finished more than its TTL ago is deleted", "workspace", req.Workspace, "kind", r.kind, "name", req.Name,
		"ttl", obj.GetAnnotations()[ttlctl.Annotation])
	return ctlruntime.Result{}, nil
}

func (c *Controller) track(key entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tracked[key]; !ok {
		c.tracked[key] = struct{}{}
		c.changedAt = time.Now()
	}
}

func (c *Controller) forget(key entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tracked[key]; ok {
		delete(c.tracked, key)
		c.changedAt = time.Now()
	}
}

func (c *Controller) removed(key entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tracked, key)
	d := c.deleted[key.kind]
	if d == nil {
		d = &deletion{}
		c.deleted[key.kind] = d
	}
	d.count++
	d.last = time.Now()
	c.changedAt = d.last
}

func (c *Controller) Report() *v1alpha1.TTLStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &v1alpha1.TTLStatus{SampledAt: c.changedAt.UTC().Format(time.RFC3339), Tracked: int64(len(c.tracked))}
	for kind, d := range c.deleted {
		st.Deleted = append(st.Deleted, v1alpha1.TTLDeleted{Kind: kind, Count: d.count, LastAt: d.last.UTC().Format(time.RFC3339)})
		st.DeletedTotal += d.count
	}
	slices.SortFunc(st.Deleted, func(a, b v1alpha1.TTLDeleted) int { return strings.Compare(a.Kind, b.Kind) })
	if len(st.Deleted) > ReportKindsLimit {
		st.Deleted = st.Deleted[:ReportKindsLimit]
	}
	return st
}
