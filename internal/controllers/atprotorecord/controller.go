package atprotorecord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "atprotorecord"

	Export = "marketidentities"

	RepoExport = "applications"

	LinkExport = "exposures"

	Kind = "AtprotoRecord"

	RepoKind = "AtprotoRepo"

	MarketIdentityKind = "MarketIdentity"

	RecordWatchKind = "AtprotoRecordWatch"

	LegacyOperatorRepo = "operator"

	ConditionReferencesResolved = v1alpha1.ConditionReferencesResolved

	ConditionInSync = v1alpha1.ConditionRecordInSync

	ReasonLinkDenied = "LinkDenied"

	ReasonRepoNotReady = "RepoNotReady"

	ReasonInvalidSpec = "InvalidSpec"
)

var OwnKinds = []string{MarketIdentityKind, Kind, RecordWatchKind}

type DynamicFactory func(workspace string) (dynamic.Interface, error)

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
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, dynamic: opts.Dynamic, log: log.With("component", "atprotorecord-controller"),
		repos: binding.New[commonv1.ObjectRef]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme, exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export),
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
	if object.Spec.RepoRef == nil && strings.TrimSpace(object.Spec.Repo) == LegacyOperatorRepo {
		c.repos.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
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
	if errors.Is(err, atprotorepo.ErrNotBound) || errors.Is(err, bus.ErrNoProvider) {
		return c.fail(ctx, req, object, ReasonRepoNotReady, err)
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
	ref, err := repoRef(req.Workspace, object.Spec)
	if err != nil {
		return nil
	}
	repo, err := bus.Resolve[atprotorepo.Repo](ctx, c.bus, self(req, object), ref,
		bus.Capability{Name: atprotorepo.CapabilityWrite, Scope: scopeOf(object.Spec)})
	if errors.Is(err, atprotorepo.ErrNotBound) {
		c.log.Warn("the AtprotoRecord repo is gone; the record is released without a delete", "workspace", req.Workspace,
			"name", object.Name, "repo", ref.String())
		return nil
	}
	if err != nil {
		return err
	}
	return deleteRecord(ctx, repo, strings.TrimSpace(object.Spec.Collection), strings.TrimSpace(object.Spec.Rkey))
}

func (c *Controller) client(workspace string) (dynamic.Interface, error) {
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
