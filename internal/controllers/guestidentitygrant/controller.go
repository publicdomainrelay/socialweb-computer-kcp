package guestidentitygrant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	Name = "guestidentitygrant"

	Export = "computevms"
)

type Outcomes interface {
	Outcome(cluster, name string) (v1alpha1.GuestIdentityGrantStatus, bool)
	Forget(cluster, name string)
}

type Options struct {
	Outcomes Outcomes

	Events <-chan ctlruntime.Request

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	log *slog.Logger

	outcomes Outcomes
}

func New(opts Options) (*Controller, error) {
	if opts.Outcomes == nil {
		return nil, errors.New("guestidentitygrant: the grant outcomes of the OpenBaoServer controller are required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{log: log.With("component", "guestidentitygrant-controller"), outcomes: opts.Outcomes}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone)}
	if opts.Events != nil {
		options = append(options, ctlruntime.Events(opts.Events))
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.GuestIdentityGrant](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("guestidentitygrant: registering the GuestIdentityGrant controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.GuestIdentityGrant) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	outcome, ok := c.outcomes.Outcome(req.Cluster, object.Name)
	if !ok || outcome.ObservedGeneration != object.Generation {
		return ctlruntime.Result{}, nil
	}
	status := openbaoctl.MergeGrantStatus(object.Status, outcome, time.Now())
	if equality.Semantic.DeepEqual(object.Status, status) {
		return ctlruntime.Result{}, nil
	}
	next := object.DeepCopy()
	next.Status = status
	if err := req.ApplyStatus(ctx, next); err != nil {
		return ctlruntime.Result{}, err
	}
	c.log.Info("the GuestIdentityGrant status follows its OpenBaoServer", "workspace", req.Workspace, "name", req.Name,
		"applied", status.Applied, "role", status.Role)
	return ctlruntime.Result{}, nil
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.outcomes.Forget(req.Cluster, req.Name)
}
