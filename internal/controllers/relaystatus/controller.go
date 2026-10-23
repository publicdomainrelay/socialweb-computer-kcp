package relaystatus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

const (
	Name = "relaystatus"

	Export = "applications"

	RelayKind = "Relay"

	ConditionReady = "Ready"

	ReasonObserved = "Observed"

	ReasonWaiting = "Waiting"

	ReasonSourceUnavailable = "SourceUnavailable"

	ReasonSourceError = "SourceError"

	DefaultRefresh = 30 * time.Second
)

type Source interface {
	RelayStatus(ctx context.Context, ref string) (statusstate.RelayObservation, error)
}

var _ Source = (*statusstate.HTTPRelayHealthSource)(nil)

type Options struct {
	Source Source

	Scope func(workspace string) bool

	Refresh time.Duration
}

type Controller struct {
	source Source

	refresh time.Duration

	refs *binding.Table[struct{}]
}

func New(opts Options) *Controller {
	return &Controller{source: opts.Source, refresh: ctlruntime.Period(opts.Refresh, DefaultRefresh), refs: binding.New[struct{}]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Relay) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, RelayKind, obj.Name))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.RelayStatus](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("relaystatus: registering the RelayStatus controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RelayStatus) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.refs.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.RelayStatusStatus{ObservedGeneration: generation, Conditions: object.Status.Conditions}
	relay := object.Spec.RelayRef
	name := strings.TrimSpace(relay.Name)
	if name == "" {
		c.refs.Set(target, nil)
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonWaiting, "the RelayStatus needs a relayRef")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	c.refs.Set(target, &struct{}{}, binding.Key(relay.InWorkspace(req.Workspace), cmp.Or(strings.TrimSpace(relay.Kind), RelayKind), name))
	if c.source == nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceUnavailable, "no relay source is configured")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	observation, err := c.source.RelayStatus(ctx, refString(relay))
	if err != nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceError, "reading the relay status: "+err.Error())
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("relaystatus: reading %s: %w", refString(relay), err), c.apply(ctx, req, object, status))
	}
	status.Phase = cmp.Or(observation.Phase, v1alpha1.RelayStatusPhasePending)
	status.Cursor = max(observation.Cursor, 0)
	status.LastEventAt = observation.LastEventAt
	status.Events = max(observation.Events, 0)
	conds.Set(&status.Conditions, generation, ConditionReady, true, ReasonObserved, "the relay status is observed")
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.RefreshAfter(c.refresh), nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RelayStatus, status v1alpha1.RelayStatusStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func refString(r commonv1.ObjectRef) string {
	name := strings.TrimSpace(r.Name)
	if kind := strings.TrimSpace(r.Kind); kind != "" {
		return kind + "/" + name
	}
	return name
}
