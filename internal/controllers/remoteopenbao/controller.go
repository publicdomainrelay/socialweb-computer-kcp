package remoteopenbao

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/remoteopenbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	Name = "remoteopenbao"

	Export = "hostprocesses"

	DefaultCheckInterval = 30 * time.Second

	DefaultRetryAfter = 2 * time.Second

	DefaultMaxRetryAfter = 5 * time.Minute
)

type Options struct {
	Remote *openbao.Remote

	Timeout time.Duration

	CheckInterval time.Duration

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	log *slog.Logger

	prober *remoteopenbao.Prober

	remote *openbao.Remote

	checkInterval time.Duration
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{log: log.With("component", "remoteopenbao-controller"), prober: remoteopenbao.New(opts.Remote, opts.Timeout, log), remote: opts.Remote,
		checkInterval: ctlruntime.Period(opts.CheckInterval, DefaultCheckInterval)}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Backoff(DefaultRetryAfter, DefaultMaxRetryAfter)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.RemoteOpenBao](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("remoteopenbao: registering the RemoteOpenBao controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RemoteOpenBao) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		c.remote.Forget(req.Workspace, req.Name)
		return ctlruntime.Result{}, nil
	}
	status, err := c.prober.Check(ctx, req.Workspace, object)
	next := object.DeepCopy()
	next.Status = status
	if applyErr := req.ApplyStatus(ctx, next); applyErr != nil {
		return ctlruntime.Result{}, applyErr
	}
	if err != nil {
		return ctlruntime.Result{}, fmt.Errorf("remoteopenbao: %s: %w", object.Name, err)
	}
	if object.Spec.Suspend {
		return ctlruntime.Result{}, nil
	}
	c.log.Debug("the RemoteOpenBao answered", "cluster", req.Cluster, "workspace", req.Workspace, "name", req.Name,
		"address", status.Address, "fieldManager", req.FieldManager())
	return ctlruntime.RefreshAfter(c.checkInterval), nil
}
