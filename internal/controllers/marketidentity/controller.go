package marketidentity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	vaultctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
)

const (
	Name = "marketidentity"

	Export = "marketidentities"

	PdsExport = "applications"

	RecordWatchKind = "AtprotoRecordWatch"

	VaultExport = "hostprocesses"

	VaultKind = "OpenBaoServer"

	eventBuffer = 256
)

type Options struct {
	Scope func(workspace string) bool

	Endpoints atproto.EndpointFunc

	Vault atproto.VaultFunc

	Resolver *pdsclient.Resolver

	HTTPClient *http.Client

	Bus *bus.Bus

	OAuth atproto.OAuthOptions

	PLCURL string

	RecordHTTP *http.Client

	Log *slog.Logger
}

type known struct {
	cluster string

	req ctlruntime.Request

	identity *v1alpha1.MarketIdentity
}

type Controller struct {
	accounts *atproto.Accounts

	roots *vaultctl.Roots

	log *slog.Logger

	identities *binding.Table[known]

	events chan ctlruntime.Request
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "marketidentity-controller")
	roots := vaultctl.NewRoots(opts.Bus, log)
	trust := func(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (*tls.Config, error) {
		pool, err := roots.Pool(ctx, Self(workspace, id.Name), id.Spec.CARef, nil)
		if err != nil || pool == nil {
			return nil, err
		}
		return pool.PublicClientConfig(nil), nil
	}
	accounts := atproto.New(atproto.Config{Endpoints: opts.Endpoints, Vault: opts.Vault, Resolver: opts.Resolver, HTTPClient: opts.HTTPClient,
		TLS: trust, Sessions: sessions(opts.Bus), OAuth: opts.OAuth, PLCURL: opts.PLCURL, RecordHTTP: opts.RecordHTTP, Log: log})
	return &Controller{accounts: accounts, roots: roots, log: log, identities: binding.New[known](), events: make(chan ctlruntime.Request, eventBuffer)}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	ctl.Provide(opts.Bus)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(ctl.events), ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, RecordWatchKind, obj.Name))
		}),
		ctlruntime.WatchesIn(PdsExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.Pds) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, atproto.ComponentKind, obj.Name))
		}),
		ctlruntime.WatchesIn(VaultExport, func(_ context.Context, src ctlruntime.Request, _ *v1alpha1.OpenBaoServer) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, VaultKind))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.MarketIdentity](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("marketidentity: registering the MarketIdentity controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.MarketIdentity) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		c.gone(ctx, req)
		return ctlruntime.Result{}, nil
	}
	c.remember(req, object)
	status := object.Status
	watch, err := c.associationWatch(ctx, req, object)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	atproto.Associate(object, watch, &status)
	result, reconcileErr := c.accounts.Reconcile(ctx, req.Workspace, object, status)
	refAfter, refErr := c.accounts.ApplyAssociationRef(ctx, object, &result.Status)
	if errors.Is(refErr, atproto.ErrAssociationRefStale) && refAfter > 0 && refAfter <= ctlruntime.DefaultBackoffMax {
		refErr = nil
	}
	reconcileErr = errors.Join(reconcileErr, refErr)
	if refAfter > 0 && (result.RequeueAfter == 0 || refAfter < result.RequeueAfter) {
		result.RequeueAfter = refAfter
	}
	next := object.DeepCopy()
	next.Status = result.Status
	if err := req.ApplyStatus(ctx, next); err != nil {
		return ctlruntime.Result{}, errors.Join(reconcileErr, err)
	}
	c.remember(req, next)
	if reconcileErr != nil {
		return ctlruntime.Result{}, reconcileErr
	}
	return ctlruntime.Result{RequeueAfter: result.RequeueAfter}, nil
}

func (c *Controller) CompleteBrowserAuthorization(ctx context.Context, workspace, name string, r *http.Request) error {
	entry, ok := c.identities.Get(workspace, name)
	if !ok || entry.identity == nil {
		return fmt.Errorf("marketidentity: the MarketIdentity %s in %s is not reconciled", name, workspace)
	}
	err := c.accounts.CompleteBrowserAuthorization(ctx, workspace, entry.identity, r)
	c.enqueue(ctlruntime.Request{Cluster: entry.cluster, Workspace: workspace, Name: name})
	return err
}

func (c *Controller) SignOut(ctx context.Context, workspace, name string) error {
	entry, ok := c.identities.Get(workspace, name)
	if !ok || entry.identity == nil {
		return fmt.Errorf("marketidentity: the MarketIdentity %s in %s is not reconciled", name, workspace)
	}
	var live v1alpha1.MarketIdentity
	if err := entry.req.Get(ctx, "", name, &live); err != nil {
		return fmt.Errorf("marketidentity: reading the MarketIdentity %s in %s: %w", name, workspace, err)
	}
	if err := atproto.SignOutRefusal(live.Spec.Mode); err != nil {
		return err
	}
	next := max(live.Spec.SignOutGeneration, live.Status.SignedOutGeneration) + 1
	if err := entry.req.ApplyFieldsAs(ctx, v1alpha1.FieldManagerMarketIdentitySignOut, &live,
		map[string]any{"spec": map[string]any{"signOutGeneration": next}}); err != nil {
		return fmt.Errorf("marketidentity: requesting the sign out of %s in %s: %w", name, workspace, err)
	}
	c.log.Info("sign out requested", "workspace", workspace, "identity", name, "signOutGeneration", next)
	return nil
}

func (c *Controller) ComponentChanged(workspace, kind, name string) {
	for _, r := range c.users(binding.Key(workspace, kind, name)) {
		c.enqueue(r)
	}
}

func (c *Controller) associationWatch(ctx context.Context, req ctlruntime.Request, object *v1alpha1.MarketIdentity) (*v1alpha1.AtprotoRecordWatch, error) {
	name := strings.TrimSpace(object.Spec.AssociationWatch)
	if name == "" || object.Spec.Mode != v1alpha1.MarketIdentityModeLocalRepo || object.Spec.Association == v1alpha1.AssociationSkip {
		return nil, nil
	}
	var watch v1alpha1.AtprotoRecordWatch
	err := req.Get(ctx, "", name, &watch)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("marketidentity: reading the association watch %s: %w", name, err)
	}
	return &watch, nil
}

func (c *Controller) remember(req ctlruntime.Request, object *v1alpha1.MarketIdentity) {
	identity := object.DeepCopy()
	refs := []string{binding.Key(req.Workspace, VaultKind)}
	if name := strings.TrimSpace(object.Spec.AssociationWatch); name != "" {
		refs = append(refs, binding.Key(req.Workspace, RecordWatchKind, name))
	}
	if object.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		refs = append(refs, binding.Key(req.Workspace, atproto.ComponentKind, atproto.ComponentName))
	}
	c.identities.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, &known{cluster: req.Cluster, req: req, identity: identity}, refs...)
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.identities.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
	c.accounts.Forget(req.Workspace, req.Name)
	c.roots.Drop(Self(req.Workspace, req.Name))
}

func Self(workspace, name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "MarketIdentity", Name: name, Workspace: workspace}
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.identities.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) enqueue(r ctlruntime.Request) {
	select {
	case c.events <- r:
	default:
		c.log.Warn("the MarketIdentity event queue is full; the event is dropped", "workspace", r.Workspace, "name", r.Name)
	}
}
