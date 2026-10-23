package kcpinstallation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "kcpinstallation"

	Export = "applications"

	ObjectName = "host-kcp"

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
		return nil, errors.New("kcpinstallation: an authority is required")
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
			return nil, fmt.Errorf("kcpinstallation: forwarding changes: %w", err)
		}
		options = append(options, ctlruntime.Events(events))
	}
	if err := ctlruntime.For[*v1alpha1.KcpInstallation](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func Desired(ctx context.Context, a hostlifecycle.Authority) (*v1alpha1.KcpInstallation, error) {
	obj := &v1alpha1.KcpInstallation{ObjectMeta: metav1.ObjectMeta{Name: ObjectName}}
	obs, err := a.ObserveKcpInstallation(ctx, v1alpha1.KcpInstallationSpec{})
	obj.Spec.SelectedSlot, obj.Spec.FrontBackend = obs.ActiveSlot, obs.FrontBackend
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

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.KcpInstallation) (ctlruntime.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		c.gone(ctx, req)
		return ctlruntime.Result{}, nil
	}
	c.mu.Lock()
	c.known[ctlruntime.Request{Cluster: req.Cluster, Name: req.Name}] = struct{}{}
	c.mu.Unlock()
	spec := obj.Spec
	generation := obj.Generation
	obs, err := c.authority.ObserveKcpInstallation(ctx, spec)
	status := v1alpha1.KcpInstallationStatus{ObservedGeneration: generation, ActiveSlot: obs.ActiveSlot, FrontBackend: obs.FrontBackend,
		Conditions: obj.Status.Conditions}
	if err != nil {
		c.log.Warn("reading the kcp installation authority", "name", obj.Name, "err", err.Error())
		status.Message = authorityUnreadable
		for _, kind := range []string{v1alpha1.KcpInstallationConditionRolling, v1alpha1.KcpInstallationConditionDrifted, v1alpha1.KcpInstallationConditionLeaseStale} {
			conds.SetStatus(&status.Conditions, generation, kind, metav1.ConditionUnknown, "AuthorityUnreadable", authorityUnreadable)
		}
		conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionReady, metav1.ConditionFalse, "AuthorityUnreadable", authorityUnreadable)
		return ctlruntime.Result{}, errors.Join(c.apply(ctx, req, obj, status), err)
	}
	rolling := spec.SelectedSlot != "" && obs.ActiveSlot != spec.SelectedSlot
	ready := obs.ActiveSlot != "" && obs.FrontBackend != "" && !rolling && !obs.Drifted && !obs.LeaseStale
	status.Message = message(spec, obs, rolling, ready)
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionReady, conds.Status(ready), reason(ready, rolling, obs.Drifted, obs.LeaseStale), status.Message)
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionRolling, conds.Status(rolling), boolReason(rolling, "SlotMismatch", "Steady"), rollingMessage(spec, obs))
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionDrifted, conds.Status(obs.Drifted), boolReason(obs.Drifted, "SlotDrift", "NoDrift"), driftMessage(obs))
	conds.SetStatus(&status.Conditions, generation, v1alpha1.KcpInstallationConditionLeaseStale, conds.Status(obs.LeaseStale), boolReason(obs.LeaseStale, "LeaseNotRenewing", "LeaseFresh"), leaseMessage(obs))
	return ctlruntime.Result{RequeueAfter: obs.LeaseStaleIn}, c.apply(ctx, req, obj, status)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.KcpInstallation, status v1alpha1.KcpInstallationStatus) error {
	next := obj.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func message(spec v1alpha1.KcpInstallationSpec, obs hostlifecycle.KcpInstallationObservation, rolling, ready bool) string {
	switch {
	case rolling:
		return rollingMessage(spec, obs)
	case obs.Drifted:
		return driftMessage(obs)
	case obs.LeaseStale:
		return leaseMessage(obs)
	case obs.ActiveSlot == "":
		return "no kcp slot is active"
	case obs.FrontBackend == "":
		return "the front names no backend"
	case ready:
		return fmt.Sprintf("slot %s serves through the front", obs.ActiveSlot)
	default:
		return "the kcp installation is not ready"
	}
}

func reason(ready, rolling, drifted, stale bool) string {
	switch {
	case ready:
		return "Serving"
	case rolling:
		return "Rolling"
	case drifted:
		return "Drifted"
	case stale:
		return "LeaseStale"
	default:
		return "NotReady"
	}
}

func rollingMessage(spec v1alpha1.KcpInstallationSpec, obs hostlifecycle.KcpInstallationObservation) string {
	if spec.SelectedSlot == "" || obs.ActiveSlot == spec.SelectedSlot {
		return "the selected slot is active"
	}
	return fmt.Sprintf("slot %s is selected and slot %s is active", spec.SelectedSlot, obs.ActiveSlot)
}

func driftMessage(obs hostlifecycle.KcpInstallationObservation) string {
	if !obs.Drifted {
		return "the active slot matches its unit"
	}
	if obs.DriftMessage != "" {
		return obs.DriftMessage
	}
	return "the active slot process differs from its unit"
}

func leaseMessage(obs hostlifecycle.KcpInstallationObservation) string {
	if !obs.LeaseStale {
		return "the controller lease is renewing"
	}
	if obs.LeaseMessage != "" {
		return obs.LeaseMessage
	}
	return "the controller lease has not renewed"
}

func boolReason(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}
