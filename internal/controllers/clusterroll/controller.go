package clusterroll

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
)

const (
	Name = "clusterroll"

	Export = "applications"

	ConditionReady = "Ready"

	ConditionRolling = "Rolling"
)

type Options struct {
	Rolls local.Rolls

	Changes <-chan struct{}

	Scope func(workspace string) bool
}

type Controller struct {
	rolls local.Rolls

	mu sync.Mutex

	known map[ctlruntime.Request]bool

	events chan ctlruntime.Request
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Rolls == nil {
		return nil, errors.New("clusterroll: a kcp slot reader is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := &Controller{rolls: opts.Rolls, known: map[ctlruntime.Request]bool{}}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if opts.Changes != nil {
		ctl.events = make(chan ctlruntime.Request, 16)
		options = append(options, ctlruntime.Events(ctl.events))
	}
	if err := ctlruntime.For[*v1alpha1.ClusterRoll](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("clusterroll: registering the ClusterRoll controller: %w", err)
	}
	if opts.Changes != nil {
		go ctl.forward(opts.Changes)
	}
	return ctl, nil
}

type outcome struct {
	phase v1alpha1.ClusterRollPhase

	started time.Time

	completed time.Time

	ready bool

	rolling bool

	reason string

	message string
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.ClusterRoll) (ctlruntime.Result, error) {
	self := ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.forget(self)
		return ctlruntime.Result{}, nil
	}
	c.remember(self)
	o, rollErr := c.observe(strings.TrimSpace(object.Spec.ClusterRef.Name))
	gen := object.Generation
	status := *object.Status.DeepCopy()
	status.ObservedGeneration = gen
	status.Phase = o.phase
	status.StartedAt = formatTime(o.started)
	status.CompletedAt = formatTime(o.completed)
	conds.Set(&status.Conditions, gen, ConditionReady, o.ready, o.reason, o.message)
	conds.Set(&status.Conditions, gen, ConditionRolling, o.rolling, o.reason, o.message)
	return ctlruntime.Result{}, errors.Join(rollErr, c.apply(ctx, req, object, status))
}

func (c *Controller) observe(cluster string) (outcome, error) {
	pending := func(reason, message string) outcome {
		return outcome{phase: v1alpha1.ClusterRollPhasePending, reason: reason, message: message}
	}
	if cluster == "" {
		return pending("InvalidSpec", "the ClusterRoll names no cluster"), nil
	}
	slots, err := c.rolls.Slots()
	if err != nil {
		return pending("RollStateUnavailable", err.Error()),
			fmt.Errorf("clusterroll: reading the kcp roll state for cluster %s: %w", cluster, err)
	}
	active := strings.TrimSpace(slots.Active)
	if active == "" {
		return pending("AwaitingRoll", fmt.Sprintf("the kcpserver records no active slot for cluster %s", cluster)), nil
	}
	record, ok := slots.Slots[active]
	if !ok {
		return outcome{phase: v1alpha1.ClusterRollPhaseFailed, reason: "RollStateInconsistent",
			message: fmt.Sprintf("the kcpserver records active slot %q for cluster %s but no record for it", active, cluster)}, nil
	}
	started := record.StartedAt
	if started.IsZero() {
		started = slots.Updated
	}
	if record.ReadyAt.IsZero() {
		return outcome{phase: v1alpha1.ClusterRollPhaseRunning, started: started, rolling: true, reason: "RollInProgress",
			message: fmt.Sprintf("the kcpserver is bringing slot %s of cluster %s up on port %d", active, cluster, record.Port)}, nil
	}
	return outcome{phase: v1alpha1.ClusterRollPhaseCompleted, started: started, completed: record.ReadyAt, ready: true,
		reason: "RollCompleted", message: fmt.Sprintf("the kcpserver records slot %s of cluster %s ready on port %d", active, cluster, record.Port)}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.ClusterRoll, status v1alpha1.ClusterRollStatus) error {
	if reflect.DeepEqual(object.Status, status) {
		return nil
	}
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.forget(ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name})
}

func (c *Controller) remember(r ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known[r] = true
}

func (c *Controller) forget(r ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, r)
}

func (c *Controller) all() []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.known))
	for r := range c.known {
		out = append(out, r)
	}
	return out
}

func (c *Controller) forward(changes <-chan struct{}) {
	defer close(c.events)
	for range changes {
		for _, r := range c.all() {
			c.events <- r
		}
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
