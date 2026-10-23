package pdsaccount

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	Name = "pdsaccount"

	Export = "applications"

	PDSExport = "pds"

	LinkExport = "exposures"

	Kind = "PdsAccount"

	PdsKind = "Pds"

	RemotePDSKind = "RemotePDS"

	ConditionReady = "Ready"

	ReasonObserved = "Observed"

	ReasonWaiting = "Waiting"

	ReasonSourceError = "SourceError"

	DefaultRefresh = 30 * time.Second
)

type Options struct {
	Bus *bus.Bus

	Scope func(workspace string) bool

	Refresh time.Duration
}

type Controller struct {
	bus *bus.Bus

	refresh time.Duration

	refs *binding.Table[struct{}]
}

func New(opts Options) *Controller {
	return &Controller{bus: opts.Bus, refresh: opt.Positive(opts.Refresh, DefaultRefresh),
		refs: binding.New[struct{}]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.Bus == nil {
		return nil, errors.New("pdsaccount: a bus is required")
	}
	if err := m.AddToScheme(v1alpha1.AddToScheme, pdsv1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Pds) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, PdsKind, obj.Name))
		}),
		ctlruntime.WatchesIn(PDSExport, func(_ context.Context, src ctlruntime.Request, obj *pdsv1alpha1.RemotePDS) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, RemotePDSKind, obj.Name))
		}),
		ctlruntime.WatchesIn(LinkExport, func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.ComponentLink) []ctlruntime.Request {
			to := obj.Spec.To
			return ctl.users(binding.Key(to.InWorkspace(src.Workspace), to.Kind, to.Name))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.PdsAccount](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("pdsaccount: registering the PdsAccount controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PdsAccount) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.refs.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.PdsAccountStatus{ObservedGeneration: generation, Conditions: object.Status.Conditions}
	did := strings.TrimSpace(object.Spec.DID)
	to := pdsRef(req.Workspace, object.Spec.PdsRef)
	if did == "" || strings.TrimSpace(to.Name) == "" {
		c.refs.Set(target, nil)
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonWaiting, "the PdsAccount needs a did and a pdsRef")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	c.refs.Set(target, &struct{}{}, binding.Key(to.Workspace, to.Kind, to.Name))
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace}
	account, err := c.account(ctx, self, to, did)
	if err != nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceError, "reading the account: "+err.Error())
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("pdsaccount: reading %s from %s: %w", did, to.String(), err), c.apply(ctx, req, object, status))
	}
	status.Active = account.Active
	conds.Set(&status.Conditions, generation, ConditionReady, true, ReasonObserved, "the account status is observed")
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{RequeueAfter: c.refresh}, nil
}

func (c *Controller) account(ctx context.Context, self, to commonv1.ObjectRef, did string) (pds.Account, error) {
	accounts, err := bus.Resolve[pds.Accounts](ctx, c.bus, self, to, bus.Capability{Name: pds.CapabilityAccount, Scope: []string{did}})
	if err != nil {
		return pds.Account{}, err
	}
	return accounts.Account(ctx, did)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.PdsAccount, status v1alpha1.PdsAccountStatus) error {
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

func pdsRef(workspace string, ref commonv1.ObjectRef) commonv1.ObjectRef {
	out := ref
	out.Workspace = ref.InWorkspace(workspace)
	if out.Group == "" {
		out.Group = v1alpha1.GroupName
		if strings.EqualFold(strings.TrimSpace(ref.Kind), RemotePDSKind) {
			out.Group = pdsv1alpha1.GroupName
		}
	}
	return out
}
