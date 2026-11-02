package openbaoserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	Name = "openbaoserver"

	Export = "hostprocesses"
)

type Options struct {
	Servers *openbaoctl.Servers

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	log *slog.Logger

	servers *openbaoctl.Servers
}

func New(opts Options) (*Controller, error) {
	if opts.Servers == nil {
		return nil, errors.New("openbaoserver: the OpenBao servers backend is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{log: log.With("component", "openbaoserver-controller"), servers: opts.Servers}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Owns[*v1alpha1.HostProcess](), ctlruntime.Gone(ctl.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.OpenBaoServer](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("openbaoserver: registering the OpenBaoServer controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.OpenBaoServer) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	pctx, cancel := context.WithTimeout(ctx, openbaoctl.PassTimeout)
	out := c.servers.Reconcile(pctx, *object, children{req: req, owner: object})
	cancel()
	if err := c.apply(ctx, req, object, out.Status); err != nil {
		return ctlruntime.Result{}, errors.Join(out.Err, err)
	}
	if out.Err != nil {
		return ctlruntime.Result{}, fmt.Errorf("openbaoserver: %s: %w", object.Name, out.Err)
	}
	c.log.Debug("the OpenBaoServer is reconciled", "cluster", req.Cluster, "workspace", req.Workspace, "name", req.Name,
		"phase", out.Status.Phase, "requeueAfter", out.RequeueAfter.String())
	return ctlruntime.Result{RequeueAfter: out.RequeueAfter}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.OpenBaoServer, status v1alpha1.OpenBaoServerStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	name := openbaoctl.ChildName(req.Name)
	var child v1alpha1.HostProcess
	if err := req.Get(ctx, "", name, &child); err != nil {
		return
	}
	if child.Labels[openbaoctl.OwnerLabel] != req.Name {
		return
	}
	if err := req.Delete(ctx, &child); err != nil {
		c.log.Warn("deleting the HostProcess of a gone OpenBaoServer", "workspace", req.Workspace, "openbaoserver", req.Name,
			"hostprocess", name, "err", err.Error())
		return
	}
	c.log.Info("the OpenBaoServer is gone; its HostProcess is deleted and the supervisor stops the unit; the storage stays",
		"workspace", req.Workspace, "openbaoserver", req.Name, "hostprocess", name)
}

var _ openbaoctl.Children = children{}

type children struct {
	req ctlruntime.Request

	owner *v1alpha1.OpenBaoServer
}

func (c children) Ensure(ctx context.Context, name string, spec v1alpha1.HostProcessSpec) (v1alpha1.HostProcess, error) {
	var cur v1alpha1.HostProcess
	err := c.req.Get(ctx, "", name, &cur)
	switch {
	case err == nil:
		if cur.Labels[openbaoctl.OwnerLabel] != c.owner.Name {
			return cur, fmt.Errorf("HostProcess %s exists and is not owned by OpenBaoServer %s", name, c.owner.Name)
		}
	case !apierrors.IsNotFound(err):
		return cur, err
	}
	want := &v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{openbaoctl.OwnerLabel: c.owner.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "OpenBaoServer", Name: c.owner.Name,
			UID: c.owner.UID, Controller: new(true)}}}, Spec: spec}
	if err := c.req.Apply(ctx, want); err != nil {
		return cur, err
	}
	if err != nil {
		return *want, nil
	}
	cur.Spec = spec
	return cur, nil
}

func (c children) Delete(ctx context.Context, name string) error {
	return c.req.Delete(ctx, &v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name}})
}
