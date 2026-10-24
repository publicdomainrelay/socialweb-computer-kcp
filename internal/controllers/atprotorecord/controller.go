package atprotorecord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
)

const (
	Name = "atprotorecord"

	Export = "marketidentities"

	RepoExport = "applications"

	LinkExport = "exposures"

	Kind = "AtprotoRecord"

	RepoKind = "AtprotoRepo"

	boundQueue = 64

	MarketIdentityKind = "MarketIdentity"

	RecordWatchKind = "AtprotoRecordWatch"

	ConditionReferencesResolved = v1alpha1.ConditionReferencesResolved

	ConditionInSync = v1alpha1.ConditionRecordInSync

	ReasonLinkDenied = "LinkDenied"

	ReasonRepoNotReady = "RepoNotReady"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonSwapMismatch = "SwapMismatch"
)

var OwnKinds = []string{MarketIdentityKind, Kind, RecordWatchKind}

type DynamicFactory = kube.Factory

type Options struct {
	Bus *bus.Bus

	Dynamic DynamicFactory

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	bus *bus.Bus

	dynamic DynamicFactory

	log *slog.Logger

	repos *binding.Table[commonv1.ObjectRef]

	bound chan ctlruntime.Request
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, dynamic: opts.Dynamic, log: log.With("component", "atprotorecord-controller"),
		repos: binding.New[commonv1.ObjectRef](), bound: make(chan ctlruntime.Request, boundQueue)}
}

func (c *Controller) wakeOnBind() {
	c.bus.Subscribe(func(key bus.Key) {
		if key.Kind != RepoKind {
			return
		}
		for _, req := range c.users(key.Workspace, RepoKind, key.Name) {
			select {
			case c.bound <- req:
			default:
			}
		}
	})
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	ctl.wakeOnBind()
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(ctl.bound),
		ctlruntime.Finalizer(v1alpha1.FinalizerAtprotoRecord),
		ctlruntime.WatchesIn(RepoExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, RepoKind, obj.Name)
		}),
		ctlruntime.WatchesIn(LinkExport, func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.ComponentLink) []ctlruntime.Request {
			to := obj.Spec.To
			if !strings.EqualFold(strings.TrimSpace(to.Kind), RepoKind) {
				return nil
			}
			return ctl.users(to.InWorkspace(src.Workspace), RepoKind, to.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.MarketIdentity) []ctlruntime.Request {
			return ctl.users(src.Workspace, MarketIdentityKind, obj.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecord) []ctlruntime.Request {
			return ctl.users(src.Workspace, Kind, obj.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.AtprotoRecordWatch) []ctlruntime.Request {
			return ctl.users(src.Workspace, RecordWatchKind, obj.Name)
		}),
		ctlruntime.Gone(func(_ context.Context, req ctlruntime.Request) {
			ctl.repos.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.AtprotoRecord](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("atprotorecord: registering the AtprotoRecord controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) users(workspace, kind, name string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.repos.Users(binding.Key(workspace, kind, name)) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func repoRef(workspace string, spec v1alpha1.AtprotoRecordSpec) (commonv1.ObjectRef, error) {
	if spec.RepoRef == nil {
		return commonv1.ObjectRef{}, errors.New("the record names no repoRef")
	}
	ref := *spec.RepoRef
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), RepoKind) || strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}, fmt.Errorf("the repoRef must name an %s, not %s %s", RepoKind, ref.Kind, ref.Name)
	}
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: RepoKind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}, nil
}

func scopeOf(spec v1alpha1.AtprotoRecordSpec) []string {
	return []string{strings.TrimSpace(spec.Collection)}
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRecord) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, c.finalize(ctx, req, object, target)
	}
	ref, err := repoRef(req.Workspace, object.Spec)
	if err != nil {
		c.repos.Set(target, nil)
		return ctlruntime.Result{}, c.fail(ctx, req, object, ReasonInvalidSpec, err)
	}
	c.repos.Set(target, &ref, append([]string{binding.Key(ref.Workspace, RepoKind, ref.Name)}, referenceKeys(req.Workspace, object.Spec.Record)...)...)
	repo, err := bus.Resolve[atprotorepo.Repo](ctx, c.bus, self(req, object), ref,
		bus.Capability{Name: atprotorepo.CapabilityWrite, Scope: scopeOf(object.Spec)})
	if err != nil {
		return ctlruntime.Result{}, c.resolveFailed(ctx, req, object, err)
	}
	return ctlruntime.Result{}, c.sync(ctx, req, object, repo)
}

func self(req ctlruntime.Request, object *v1alpha1.AtprotoRecord) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace}
}

func (c *Controller) resolveFailed(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRecord, err error) error {
	if errors.Is(err, bus.ErrLinkDenied) {
		return errors.Join(err, c.fail(ctx, req, object, ReasonLinkDenied, err))
	}
	if errors.Is(err, atprotorepo.ErrNotBound) {
		c.log.Info("an AtprotoRecord waits for its repo binding", "workspace", req.Workspace, "name", object.Name, "err", err.Error())
		next := object.DeepCopy()
		next.Status = failed(object, ReasonRepoNotReady, err).Status(object)
		return req.ApplyStatus(ctx, next)
	}
	return errors.Join(err, c.fail(ctx, req, object, ReasonRepoNotReady, err))
}

func (c *Controller) sync(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRecord, repo atprotorepo.Repo) error {
	dyn, err := c.client(req.Workspace)
	if err != nil {
		return err
	}
	own := func(ctx context.Context, from v1alpha1.AtprotoValueFrom) (map[string]any, bool, error) {
		return ReadOwn(ctx, req, from)
	}
	outcome, err := Apply(ctx, repo, object, References{Own: own, Dynamic: dyn, Factory: c.dynamic, Workspace: req.Workspace})
	if outcome.Written {
		c.log.Info("an AtprotoRecord wrote its declared record", "workspace", req.Workspace, "name", object.Name, "uri", outcome.URI,
			"cid", outcome.CID, "reason", outcome.Reason)
	}
	if err != nil {
		c.log.Warn("an AtprotoRecord is not applied", "workspace", req.Workspace, "name", object.Name, "reason", outcome.Reason, "err", err.Error())
	}
	next := object.DeepCopy()
	next.Status = outcome.Status(object)
	if applyErr := req.ApplyStatus(ctx, next); applyErr != nil {
		return errors.Join(err, applyErr)
	}
	if outcome.Retry {
		return err
	}
	return nil
}

func (c *Controller) finalize(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRecord, target binding.Target) error {
	defer c.repos.Set(target, nil)
	if strings.TrimSpace(object.Status.URI) == "" && strings.TrimSpace(object.Status.Cid) == "" {
		return nil
	}
	if object.Spec.DeletionPolicy == v1alpha1.RecordDeletionRetain {
		c.log.Info("atprotorecord_retained", "workspace", req.Workspace, "name", object.Name, "uri", object.Status.URI)
		return nil
	}
	ref, err := repoRef(req.Workspace, object.Spec)
	if err != nil {
		return nil
	}
	repo, err := bus.Resolve[atprotorepo.Repo](ctx, c.bus, self(req, object), ref,
		bus.Capability{Name: atprotorepo.CapabilityWrite, Scope: scopeOf(object.Spec)})
	if errors.Is(err, atprotorepo.ErrNotBound) || (err != nil && c.repoGone(ctx, req.Workspace, ref)) {
		c.log.Warn("the AtprotoRecord repo is gone; the record is released without a delete", "workspace", req.Workspace,
			"name", object.Name, "repo", ref.String())
		return nil
	}
	if err != nil {
		return err
	}
	return deleteRecord(ctx, repo, strings.TrimSpace(object.Spec.Collection), strings.TrimSpace(object.Spec.Rkey))
}

func (c *Controller) repoGone(ctx context.Context, workspace string, ref commonv1.ObjectRef) bool {
	cl, err := c.client(ref.InWorkspace(workspace))
	if err != nil || cl == nil {
		return false
	}
	_, err = kube.Get[metav1.PartialObjectMetadata](ctx, cl.Resource(v1alpha1.AtprotoRepoGVR), ref.Name)
	return apierrors.IsNotFound(err)
}

func (c *Controller) client(workspace string) (*kube.Client, error) {
	if c.dynamic == nil {
		return nil, nil
	}
	return c.dynamic(workspace)
}

func (c *Controller) fail(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRecord, reason string, cause error) error {
	c.log.Warn("an AtprotoRecord is not applied", "workspace", req.Workspace, "name", object.Name, "reason", reason, "err", cause.Error())
	next := object.DeepCopy()
	next.Status = failed(object, reason, cause).Status(object)
	return req.ApplyStatus(ctx, next)
}
