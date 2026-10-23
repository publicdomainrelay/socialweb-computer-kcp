package plcidentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

const (
	Name = "plcidentity"

	Export = "applications"

	ConditionReady = "Ready"

	ReasonObserved = "Observed"

	ReasonWaiting = "Waiting"

	ReasonSourceUnavailable = "SourceUnavailable"

	ReasonSourceError = "SourceError"

	ReasonNotFound = "NotFound"

	DefaultRefresh = 30 * time.Second
)

type Directory interface {
	Identity(ctx context.Context, did string) (statusstate.PLCIdentity, error)
}

var _ Directory = (*statusstate.HTTPPLCClient)(nil)

type Options struct {
	Directory Directory

	Scope func(workspace string) bool

	Refresh time.Duration
}

type Controller struct {
	directory Directory

	refresh time.Duration
}

func New(opts Options) *Controller {
	return &Controller{directory: opts.Directory, refresh: opt.Positive(opts.Refresh, DefaultRefresh)}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.PlcIdentity](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("plcidentity: registering the PlcIdentity controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PlcIdentity) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.PlcIdentityStatus{ObservedGeneration: generation, Conditions: object.Status.Conditions}
	did := strings.TrimSpace(object.Spec.DID)
	if did == "" {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonWaiting, "the PlcIdentity needs a did")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	if c.directory == nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceUnavailable, "no PLC directory is configured")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	identity, err := c.directory.Identity(ctx, did)
	if err != nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceError, "reading the PLC log: "+err.Error())
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("plcidentity: reading the PLC log of %s: %w", did, err), c.apply(ctx, req, object, status))
	}
	if !identity.Found {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonNotFound, "the PLC directory holds no current operation for "+did)
	} else {
		status.Head = identity.Head
		status.OpCount = identity.OpCount
		status.Tombstoned = identity.Tombstoned
		conds.Set(&status.Conditions, generation, ConditionReady, true, ReasonObserved, "the PLC identity has current operations")
	}
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{RequeueAfter: c.refresh}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PlcIdentity, status v1alpha1.PlcIdentityStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}
