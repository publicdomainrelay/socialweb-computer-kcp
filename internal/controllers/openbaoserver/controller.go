package openbaoserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	Name = "openbaoserver"

	Export = "hostprocesses"

	GrantExport = "computevms"

	GrantEventBuffer = 1024
)

type Grants interface {
	List(ctx context.Context, workspace string) ([]v1alpha1.GuestIdentityGrant, error)
}

type Options struct {
	Servers *openbaoctl.Servers

	Grants Grants

	Scope func(workspace string) bool

	Log *slog.Logger
}

type grantKey struct {
	cluster string

	name string
}

type Controller struct {
	log *slog.Logger

	servers *openbaoctl.Servers

	grants Grants

	events chan ctlruntime.Request

	mu sync.Mutex

	identity map[string]map[string]struct{}

	outcomes map[grantKey]v1alpha1.GuestIdentityGrantStatus
}

func New(opts Options) (*Controller, error) {
	if opts.Servers == nil {
		return nil, errors.New("openbaoserver: the OpenBao servers backend is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{log: log.With("component", "openbaoserver-controller"), servers: opts.Servers, grants: opts.Grants,
		events: make(chan ctlruntime.Request, GrantEventBuffer), identity: map[string]map[string]struct{}{},
		outcomes: map[grantKey]v1alpha1.GuestIdentityGrantStatus{}}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Owns[*v1alpha1.HostProcess](), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(GrantExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.GuestIdentityGrant) []ctlruntime.Request {
			return ctl.identityServers(src.Cluster)
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.OpenBaoServer](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("openbaoserver: registering the OpenBaoServer controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) GrantEvents() <-chan ctlruntime.Request { return c.events }

func (c *Controller) Outcome(cluster, name string) (v1alpha1.GuestIdentityGrantStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.outcomes[grantKey{cluster: cluster, name: name}]
	return st, ok
}

func (c *Controller) Forget(cluster, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.outcomes, grantKey{cluster: cluster, name: name})
}

func (c *Controller) identityServers(cluster string) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.identity[cluster]))
	for name := range c.identity[cluster] {
		out = append(out, ctlruntime.Request{Cluster: cluster, Name: name})
	}
	return out
}

func (c *Controller) index(cluster, name string, identity bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !identity {
		delete(c.identity[cluster], name)
		if len(c.identity[cluster]) == 0 {
			delete(c.identity, cluster)
		}
		return
	}
	if c.identity[cluster] == nil {
		c.identity[cluster] = map[string]struct{}{}
	}
	c.identity[cluster][name] = struct{}{}
}

func (c *Controller) record(cluster string, outcomes map[string]v1alpha1.GuestIdentityGrantStatus) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var changed []string
	for name, st := range outcomes {
		k := grantKey{cluster: cluster, name: name}
		if held, ok := c.outcomes[k]; ok && equality.Semantic.DeepEqual(held, st) {
			continue
		}
		c.outcomes[k] = st
		changed = append(changed, name)
	}
	return changed
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.OpenBaoServer) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	identity := openbaoctl.NeedsGrants(*object)
	c.index(req.Cluster, object.Name, identity)
	var grants []v1alpha1.GuestIdentityGrant
	if identity {
		if c.grants == nil {
			return ctlruntime.Result{}, c.apply(ctx, req, object, failed(object, "this core reads no GuestIdentityGrants, so it cannot hold spec.declared.identityOIDC"))
		}
		list, err := c.grants.List(ctx, req.Workspace)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctlruntime.Result{}, errors.Join(fmt.Errorf("openbaoserver: listing the GuestIdentityGrants of %s: %w", req.Workspace, err),
				c.apply(ctx, req, object, failed(object, fmt.Sprintf("listing the GuestIdentityGrants: %v", err))))
		}
		grants = list
	}
	pctx, cancel := context.WithTimeout(ctx, openbaoctl.PassTimeout)
	out := c.servers.Reconcile(pctx, *object, grants, children{req: req, owner: object})
	cancel()
	if err := c.apply(ctx, req, object, out.Status); err != nil {
		return ctlruntime.Result{}, errors.Join(out.Err, err)
	}
	if changed := c.record(req.Cluster, out.Grants); len(changed) > 0 {
		for _, name := range changed {
			select {
			case c.events <- ctlruntime.Request{Cluster: req.Cluster, Name: name}:
			case <-ctx.Done():
				return ctlruntime.Result{}, ctx.Err()
			}
		}
	}
	if out.Err != nil {
		return ctlruntime.Result{}, fmt.Errorf("openbaoserver: %s: %w", object.Name, out.Err)
	}
	c.log.Debug("the OpenBaoServer is reconciled", "cluster", req.Cluster, "workspace", req.Workspace, "name", req.Name,
		"phase", out.Status.Phase, "requeueAfter", out.RequeueAfter.String())
	return ctlruntime.Result{RequeueAfter: out.RequeueAfter}, nil
}

func failed(object *v1alpha1.OpenBaoServer, message string) v1alpha1.OpenBaoServerStatus {
	st := *object.Status.DeepCopy()
	st.ObservedGeneration, st.Phase, st.Message, st.Ready = object.Generation, v1alpha1.OpenBaoServerPhaseFailed, message, false
	conds.Set(&st.Conditions, object.Generation, openbaoctl.ConditionReady, false, "Failed", message)
	return st
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.OpenBaoServer, status v1alpha1.OpenBaoServerStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(ctx context.Context, req ctlruntime.Request) {
	c.index(req.Cluster, req.Name, false)
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
