package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	Name = "guestcontrolplane"

	Kind = "GuestControlPlane"

	Export = "applications"

	ComputeVMExport = "computevms"

	IdentityExport = "marketidentities"

	VaultExport = "hostprocesses"

	ExposureExport = "exposures"

	DefaultVaultServer = "bidder"

	FieldManager = v1alpha1.FieldManagerComputeVMMarketGuest
)

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}

type Store interface {
	Get(ctx context.Context, workspace, name string, obj client.Object) error

	List(ctx context.Context, workspace string, list client.ObjectList) error

	Apply(ctx context.Context, workspace, fieldManager string, obj kube.Doc, status bool) error

	Delete(ctx context.Context, workspace string, obj client.Object) error
}

type Options struct {
	Bus *bus.Bus

	Store Store

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	store Store

	log *slog.Logger

	mu sync.Mutex

	feeds map[string]*feed

	planes map[string]map[string]bool
}

func New(opts Options) (*Controller, error) {
	if opts.Store == nil {
		return nil, errors.New("guestcontrolplane: the controller needs a workspace object store")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, store: opts.Store, log: log.With("component", "guestcontrolplane-controller"),
		feeds: map[string]*feed{}, planes: map[string]map[string]bool{}}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	ctl.Provide()
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.FieldManager(FieldManager), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(ComputeVMExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.ComputeVM) []ctlruntime.Request {
			return ctl.planesIn(src.Cluster)
		}),
		ctlruntime.WatchesIn(ComputeVMExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.GuestIdentityGrant) []ctlruntime.Request {
			return ctl.planesIn(src.Cluster)
		}),
		ctlruntime.WatchesIn(IdentityExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.MarketIdentity) []ctlruntime.Request {
			return ctl.planesIn(src.Cluster)
		}),
		ctlruntime.WatchesIn(VaultExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.OpenBaoServer) []ctlruntime.Request {
			return ctl.planesIn(src.Cluster)
		}),
		ctlruntime.WatchesIn(ExposureExport, func(_ context.Context, src ctlruntime.Request, _ *exposurev1alpha1.Exposure) []ctlruntime.Request {
			return ctl.planesIn(src.Cluster)
		}),
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.GuestControlPlane](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("guestcontrolplane: registering the GuestControlPlane input controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.GuestControlPlane) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		c.forget(req)
		return ctlruntime.Result{}, nil
	}
	c.remember(req)
	in, err := c.inputs(ctx, req.Workspace, object)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if c.feedFor(req.Workspace, object.Name).publish(in) {
		c.log.Info("the guest control plane inputs changed", "workspace", req.Workspace, "name", object.Name,
			"computevms", len(in.ComputeVMs), "grants", len(in.Grants), "did", in.DID, "issuer", in.Issuer, "openbao", in.OpenBao != nil)
	}
	return ctlruntime.Result{}, nil
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.forget(req)
}

func (c *Controller) inputs(ctx context.Context, workspace string, object *v1alpha1.GuestControlPlane) (guestcp.Inputs, error) {
	var in guestcp.Inputs
	if ref := identityRef(object.Spec); ref != nil && strings.TrimSpace(ref.Name) != "" {
		var identity v1alpha1.MarketIdentity
		switch err := c.store.Get(ctx, workspace, strings.TrimSpace(ref.Name), &identity); {
		case err == nil:
			in.DID = strings.TrimSpace(identity.Status.Did)
		case !apierrors.IsNotFound(err):
			return guestcp.Inputs{}, fmt.Errorf("reading the MarketIdentity %s: %w", ref.Name, err)
		}
	}
	var exposure exposurev1alpha1.Exposure
	switch err := c.store.Get(ctx, workspace, ExposureName(object.Name), &exposure); {
	case err == nil:
		in.Issuer = strings.TrimSpace(exposure.Status.Address)
	case !apierrors.IsNotFound(err):
		return guestcp.Inputs{}, fmt.Errorf("reading the Exposure %s: %w", ExposureName(object.Name), err)
	}
	if server := v1alpha1.VaultServer(object.Spec.VaultRef, DefaultVaultServer); !strings.HasPrefix(server, v1alpha1.KindRemoteOpenBao+"/") {
		var openbao v1alpha1.OpenBaoServer
		switch err := c.store.Get(ctx, workspace, server, &openbao); {
		case err == nil:
			in.OpenBao = openbao.Status.DeepCopy()
		case !apierrors.IsNotFound(err):
			return guestcp.Inputs{}, fmt.Errorf("reading the OpenBaoServer %s: %w", server, err)
		}
	}
	var vms v1alpha1.ComputeVMList
	if err := c.store.List(ctx, workspace, &vms); err != nil {
		return guestcp.Inputs{}, fmt.Errorf("listing the ComputeVMs: %w", err)
	}
	in.ComputeVMs = vms.Items
	var grants v1alpha1.GuestIdentityGrantList
	if err := c.store.List(ctx, workspace, &grants); err != nil {
		return guestcp.Inputs{}, fmt.Errorf("listing the GuestIdentityGrants: %w", err)
	}
	in.Grants = grants.Items
	return in, nil
}

func ExposureName(name string) string {
	return "svc-guestcontrolplane-" + strings.TrimSpace(name) + "-ingress"
}

func identityRef(spec v1alpha1.GuestControlPlaneSpec) *commonv1.ObjectRef {
	if spec.Identity != nil {
		return spec.Identity.MarketIdentityRef
	}
	return spec.PdsRef
}

func (c *Controller) remember(req ctlruntime.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := c.planes[req.Cluster]
	if names == nil {
		names = map[string]bool{}
		c.planes[req.Cluster] = names
	}
	names[req.Name] = true
}

func (c *Controller) forget(req ctlruntime.Request) {
	c.mu.Lock()
	if names := c.planes[req.Cluster]; names != nil {
		delete(names, req.Name)
		if len(names) == 0 {
			delete(c.planes, req.Cluster)
		}
	}
	key := feedKey(req.Workspace, req.Name)
	f := c.feeds[key]
	delete(c.feeds, key)
	c.mu.Unlock()
	if f != nil {
		f.close()
	}
}

func (c *Controller) planesIn(cluster string) []ctlruntime.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.planes[cluster]))
	for name := range c.planes[cluster] {
		out = append(out, ctlruntime.Request{Cluster: cluster, Name: name})
	}
	return out
}

func (c *Controller) feedFor(workspace, name string) *feed {
	key := feedKey(workspace, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.feeds[key]
	if f == nil {
		f = newFeed()
		c.feeds[key] = f
	}
	return f
}

func feedKey(workspace, name string) string {
	return strings.TrimSpace(workspace) + "/" + strings.TrimSpace(name)
}
