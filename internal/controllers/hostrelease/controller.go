package hostrelease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "hostrelease"

	Export = "applications"

	ObjectName = "host-release"

	ConditionReady = "Ready"

	ConditionCurrent = "Current"

	ConditionSelfCheck = "SelfCheck"

	ConditionGC = "GC"

	authorityUnreadable = "the host authority could not be read; the controller retries"
)

type Options struct {
	Log *slog.Logger

	Authority hostlifecycle.Authority

	Scope func(workspace string) bool

	Changed <-chan struct{}
}

type Controller struct {
	log *slog.Logger

	authority hostlifecycle.Authority

	mu sync.Mutex

	known map[ctlruntime.Request]struct{}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Authority == nil {
		return nil, errors.New("hostrelease: an authority is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", Name+"-controller"), authority: opts.Authority, known: map[ctlruntime.Request]struct{}{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(c.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changed != nil {
		events, err := ctlruntime.Forward(m, Name+": changes", opts.Changed, c.forwarded)
		if err != nil {
			return nil, fmt.Errorf("hostrelease: forwarding changes: %w", err)
		}
		options = append(options, ctlruntime.Events(events))
	}
	if err := ctlruntime.For[*v1alpha1.HostRelease](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func Desired(ctx context.Context, a hostlifecycle.Authority) (*v1alpha1.HostRelease, error) {
	obj := &v1alpha1.HostRelease{ObjectMeta: metav1.ObjectMeta{Name: ObjectName}}
	obs, err := a.ObserveHostRelease(ctx, v1alpha1.HostReleaseSpec{})
	obj.Spec.Version = obs.Current
	return obj, err
}

func (c *Controller) forwarded(struct{}) []ctlruntime.Request {
	return c.requests()
}

func (c *Controller) requests() []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.known))
	for r := range c.known {
		out = append(out, r)
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, ctlruntime.Request{Cluster: req.Cluster, Name: req.Name})
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.HostRelease) (ctlruntime.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		c.gone(ctx, req)
		return ctlruntime.Result{}, nil
	}
	c.mu.Lock()
	c.known[ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}] = struct{}{}
	c.mu.Unlock()
	spec := obj.Spec
	generation := obj.Generation
	obs, err := c.authority.ObserveHostRelease(ctx, spec)
	status := v1alpha1.HostReleaseStatus{ObservedGeneration: generation, Current: obs.Current, Previous: obs.Previous,
		SelfCheck: obs.SelfCheck, GC: obs.GC, Conditions: obj.Status.Conditions}
	if err != nil {
		c.log.Warn("reading the host release authority", "name", obj.Name, "err", err.Error())
		for _, kind := range []string{ConditionCurrent, ConditionSelfCheck, ConditionGC} {
			conds.SetStatus(&status.Conditions, generation, kind, metav1.ConditionUnknown, "AuthorityUnreadable", authorityUnreadable)
		}
		conds.SetStatus(&status.Conditions, generation, ConditionReady, metav1.ConditionFalse, "AuthorityUnreadable", authorityUnreadable)
		return ctlruntime.Result{}, errors.Join(c.apply(ctx, req, obj, status), err)
	}
	matches := spec.Version == "" || obs.Current == spec.Version
	ready := obs.Current != "" && matches
	msg := message(obs, ready, matches)
	conds.SetStatus(&status.Conditions, generation, ConditionReady, conds.Status(ready), boolReason(ready, "Current", "NotCurrent"), msg)
	conds.SetStatus(&status.Conditions, generation, ConditionCurrent, conds.Status(ready), boolReason(ready, "MatchesSpec", "DiffersFromSpec"), msg)
	value, reason, selfMessage := selfCheckCondition(obs.SelfCheck)
	conds.SetStatus(&status.Conditions, generation, ConditionSelfCheck, value, reason, selfMessage)
	gcValue, gcReason, gcMessage := gcCondition(obs.GC)
	conds.SetStatus(&status.Conditions, generation, ConditionGC, gcValue, gcReason, gcMessage)
	return ctlruntime.Result{}, c.apply(ctx, req, obj, status)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.HostRelease, status v1alpha1.HostReleaseStatus) error {
	next := obj.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func message(obs hostlifecycle.HostReleaseObservation, ready, matches bool) string {
	switch {
	case obs.Current == "":
		return "the current release is not known"
	case !matches:
		return fmt.Sprintf("the requested version is not current; the current version is %s", obs.Current)
	case ready:
		return fmt.Sprintf("version %s is current", obs.Current)
	default:
		return "the host release is not ready"
	}
}

func selfCheckCondition(sc *v1alpha1.HostReleaseSelfCheck) (metav1.ConditionStatus, string, string) {
	if sc == nil {
		return metav1.ConditionUnknown, "NoSelfCheck", "no self-check report has been recorded"
	}
	switch strings.ToLower(strings.TrimSpace(sc.Phase)) {
	case "ok", "pass", "passed", "ready", "succeeded", "success", "healthy":
		return metav1.ConditionTrue, "SelfCheckPassed", "the self-check passed"
	case "", "pending", "running", "queued":
		return metav1.ConditionUnknown, "SelfCheckPending", "the self-check has not finished"
	default:
		return metav1.ConditionFalse, "SelfCheckFailed", "the self-check reported phase " + sc.Phase
	}
}

func gcCondition(gc *v1alpha1.HostReleaseGC) (metav1.ConditionStatus, string, string) {
	if gc == nil {
		return metav1.ConditionUnknown, "NoGC", "no GC report has been recorded"
	}
	return metav1.ConditionTrue, "Collected", fmt.Sprintf("GC kept %d and removed %d", gc.Kept, len(gc.Removed))
}

func boolReason(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}
