package remoterepo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/scoped"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "remoterepo"

	Export = "applications"

	PDSExport = "pds"

	IdentityExport = "marketidentities"

	Kind = "RemoteRepo"

	RemotePDSKind = "RemotePDS"

	ReasonReady = "Ready"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonLinkDenied = "LinkDenied"

	ReasonPDSUnavailable = "PDSUnavailable"

	ReasonCredentialsUnavailable = "CredentialsUnavailable"

	ReasonObserveFailed = "ObserveFailed"
)

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}

type Options struct {
	Bus *bus.Bus

	Network atprotorepo.RecordReader

	Log *slog.Logger
}

type backend struct {
	self commonv1.ObjectRef

	labels map[string]string

	pds commonv1.ObjectRef

	credentials commonv1.ObjectRef

	did string
}

type Controller struct {
	bus *bus.Bus

	network atprotorepo.RecordReader

	log *slog.Logger

	backends *binding.Table[backend]
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, network: opts.Network, log: log.With("component", "remoterepo-controller"),
		backends: binding.New[backend]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme, pdsv1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	ctl.Provide()
	if err := ctlruntime.For[*v1alpha1.RemoteRepo](m, Name, ctl, ctlruntime.Export(Export),
		ctlruntime.WatchesIn(PDSExport, func(_ context.Context, src ctlruntime.Request, obj *pdsv1alpha1.RemotePDS) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, RemotePDSKind, obj.Name))
		}),
		ctlruntime.WatchesIn(IdentityExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.MarketIdentity) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, "MarketIdentity", obj.Name))
		})); err != nil {
		return nil, fmt.Errorf("remoterepo: registering the RemoteRepo controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.backends.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) Provide() {
	provide[atprotorepo.Repo](c, atprotorepo.CapabilityWrite)
	provide[atprotorepo.RecordWriter](c, atprotorepo.CapabilityWrite)
	provide[atprotorepo.RecordReader](c, atprotorepo.CapabilityRead)
	provide[atprotorepo.Subscriber](c, atprotorepo.CapabilitySubscribe)
}

func provide[T any](c *Controller, capability string) {
	bus.Provide(c.bus, GroupKind, capability, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (T, error) {
		var zero T
		b, ok := c.backends.Get(to.Workspace, to.Name)
		if !ok {
			return zero, fmt.Errorf("%w: the RemoteRepo %s is not reconciled", atprotorepo.ErrNotBound, to.String())
		}
		repo, err := c.open(ctx, b, scope)
		if err != nil {
			return zero, err
		}
		out, ok := repo.(T)
		if !ok {
			return zero, fmt.Errorf("remoterepo: the repo handle is not a %T", zero)
		}
		return out, nil
	})
}

func (b backend) caller(ctx context.Context) context.Context {
	return bus.WithCallerLabels(ctx, b.self, b.labels)
}

func (c *Controller) transport(ctx context.Context, b backend) (pds.Transport, error) {
	return bus.Resolve[pds.Transport](b.caller(ctx), c.bus, b.self, b.pds, bus.Capability{Name: pds.CapabilityAccount, Scope: []string{b.did}})
}

func (c *Controller) authenticator(ctx context.Context, b backend) (pds.Authenticator, error) {
	return bus.Resolve[pds.Authenticator](b.caller(ctx), c.bus, b.self, b.credentials,
		bus.Capability{Name: pds.CapabilitySession, Scope: []string{b.did}})
}

func (c *Controller) open(ctx context.Context, b backend, scope []string) (atprotorepo.Repo, error) {
	transport, err := c.transport(ctx, b)
	if err != nil {
		return nil, err
	}
	auth, err := c.authenticator(ctx, b)
	if err != nil {
		return nil, err
	}
	repo, err := reporemote.New(reporemote.Config{DID: b.did, Transport: transport, Auth: reporemote.NewAuth(auth)})
	if err != nil {
		return nil, err
	}
	return scoped.New(scoped.Route(repo, c.network), scope), nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RemoteRepo) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.backends.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.AtprotoRepoBackendStatus{ObservedGeneration: generation, DID: strings.TrimSpace(object.Spec.DID),
		Conditions: slices.Clone(object.Status.Conditions)}
	pdsRef := object.Spec.PdsRef
	pdsRef.Group = pdsv1alpha1.GroupName
	pdsRef.Workspace = pdsRef.InWorkspace(req.Workspace)
	credentials := object.Spec.CredentialsRef.ObjectRef
	if credentials.Group == "" {
		credentials.Group = v1alpha1.GroupName
	}
	credentials.Workspace = credentials.InWorkspace(req.Workspace)
	_, didErr := syntax.ParseDID(status.DID)
	switch {
	case !strings.EqualFold(strings.TrimSpace(pdsRef.Kind), RemotePDSKind) || strings.TrimSpace(pdsRef.Name) == "":
		c.backends.Set(target, nil)
		return c.notReady(ctx, req, object, status, ReasonInvalidSpec, "the pdsRef must name a RemotePDS", nil)
	case strings.TrimSpace(credentials.Kind) == "" || strings.TrimSpace(credentials.Name) == "":
		c.backends.Set(target, nil)
		return c.notReady(ctx, req, object, status, ReasonInvalidSpec, "the credentialsRef must name a credentials provider", nil)
	case didErr != nil:
		c.backends.Set(target, nil)
		return c.notReady(ctx, req, object, status, ReasonInvalidSpec, fmt.Sprintf("the did %q is not a DID", status.DID), nil)
	}
	b := backend{self: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace},
		labels: maps.Clone(object.Labels), pds: pdsRef, credentials: credentials, did: status.DID}
	c.backends.Set(target, &b, binding.Key(pdsRef.Workspace, RemotePDSKind, pdsRef.Name),
		binding.Key(credentials.Workspace, credentials.Kind, credentials.Name))
	transport, err := c.transport(ctx, b)
	if err != nil {
		return c.resolveFailed(ctx, req, object, status, ReasonPDSUnavailable, err)
	}
	status.Address = transport.Address()
	syncer, err := bus.Resolve[pds.Sync](b.caller(ctx), c.bus, b.self, b.pds, bus.Capability{Name: pds.CapabilitySync})
	if err != nil {
		return c.resolveFailed(ctx, req, object, status, ReasonPDSUnavailable, err)
	}
	head, err := syncer.LatestCommit(ctx, status.DID)
	if err != nil {
		return c.notReady(ctx, req, object, status, ReasonObserveFailed, err.Error(), err)
	}
	status.Head, status.Rev = head.CID, head.Rev
	if _, err := c.authenticator(ctx, b); err != nil {
		return c.resolveFailed(ctx, req, object, status, ReasonCredentialsUnavailable, err)
	}
	status.Writable = true
	conds.Set(&status.Conditions, generation, commonv1.ConditionReady, true, ReasonReady,
		fmt.Sprintf("the RemotePDS %s serves %s with the credentials of %s %s", pdsRef.Name, status.DID, credentials.Kind, credentials.Name))
	return ctlruntime.Result{}, c.apply(ctx, req, object, status)
}

func (c *Controller) resolveFailed(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RemoteRepo,
	status v1alpha1.AtprotoRepoBackendStatus, reason string, err error) (ctlruntime.Result, error) {
	switch {
	case errors.Is(err, bus.ErrLinkDenied):
		return c.notReady(ctx, req, object, status, ReasonLinkDenied, err.Error(), err)
	case errors.Is(err, pds.ErrNotFound), errors.Is(err, bus.ErrNoProvider):
		return c.notReady(ctx, req, object, status, reason, err.Error(), nil)
	}
	return c.notReady(ctx, req, object, status, reason, err.Error(), err)
}

func (c *Controller) notReady(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RemoteRepo, status v1alpha1.AtprotoRepoBackendStatus,
	reason, message string, cause error) (ctlruntime.Result, error) {
	status.Writable = false
	conds.Set(&status.Conditions, object.Generation, commonv1.ConditionReady, false, reason, message)
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, errors.Join(cause, err)
	}
	return ctlruntime.Result{}, cause
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.RemoteRepo, status v1alpha1.AtprotoRepoBackendStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}
