package atprotorepo

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "atprotorepo"

	Export = "applications"

	Kind = "AtprotoRepo"

	LocalRepoKind = "LocalRepo"

	RemoteRepoKind = "RemoteRepo"

	ReasonBound = "Bound"

	ReasonBackendMissing = "BackendMissing"

	ReasonInvalidBackend = "InvalidBackend"

	ReasonBackendReady = "BackendReady"

	ReasonBackendNotReady = "BackendNotReady"

	ReasonBackendStale = "BackendStale"
)

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger
}

type repoBinding struct {
	self commonv1.ObjectRef

	backend commonv1.ObjectRef

	labels map[string]string
}

type Controller struct {
	bus *bus.Bus

	log *slog.Logger

	bindings *binding.Table[repoBinding]
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{bus: opts.Bus, log: log.With("component", "atprotorepo-controller"), bindings: binding.New[repoBinding]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	ctl.Provide()
	if err := ctlruntime.For[*v1alpha1.AtprotoRepo](m, Name, ctl, ctlruntime.Export(Export),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.LocalRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, LocalRepoKind, obj.Name)
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.RemoteRepo) []ctlruntime.Request {
			return ctl.users(src.Workspace, RemoteRepoKind, obj.Name)
		})); err != nil {
		return nil, fmt.Errorf("atprotorepo: registering the AtprotoRepo controller: %w", err)
	}
	return ctl, nil
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
		b, ok := c.bindings.Get(to.Workspace, to.Name)
		if !ok {
			return zero, fmt.Errorf("%w: %s", atprotorepo.ErrNotBound, to.String())
		}
		return bus.Resolve[T](bus.WithCallerLabels(ctx, b.self, b.labels), c.bus, b.self, b.backend,
			bus.Capability{Name: capability, Scope: scope})
	})
}

func (c *Controller) users(workspace, kind, name string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.bindings.Users(binding.Key(workspace, kind, name)) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) bind(req ctlruntime.Request, b *repoBinding, backend string) {
	c.bindings.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, b, backend)
}

func contractKind(kind string) bool {
	for _, res := range v1alpha1.APIResources {
		if strings.EqualFold(res.Kind, strings.TrimSpace(kind)) && slices.Contains(res.Contracts, commonv1.ContractAtprotoRepos) {
			return true
		}
	}
	return false
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRepo) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		c.bind(req, nil, "")
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.AtprotoRepoStatus{ObservedGeneration: generation, Conditions: slices.Clone(object.Status.Conditions)}
	ref := object.Spec.BackendRef
	workspace := ref.InWorkspace(req.Workspace)
	kind := strings.TrimSpace(ref.Kind)
	name := strings.TrimSpace(ref.Name)
	switch {
	case kind == "" || name == "":
		c.bind(req, nil, "")
		return ctlruntime.Result{}, c.unbound(ctx, req, object, status, ReasonInvalidBackend, "the backendRef needs a kind and a name")
	case !contractKind(kind):
		c.bind(req, nil, "")
		return ctlruntime.Result{}, c.unbound(ctx, req, object, status, ReasonInvalidBackend,
			fmt.Sprintf("the backendRef kind %s does not carry the %s contract", kind, commonv1.ContractLabelAtprotoRepos))
	case workspace != req.Workspace:
		c.bind(req, nil, "")
		return ctlruntime.Result{}, c.unbound(ctx, req, object, status, ReasonInvalidBackend,
			fmt.Sprintf("the backend lives in %s; an AtprotoRepo binds a backend in its own workspace %s", workspace, req.Workspace))
	}
	bk := binding.Key(workspace, kind, name)
	backend, backendGeneration, err := c.readBackend(ctx, req, kind, name)
	if apierrors.IsNotFound(err) {
		c.bind(req, nil, bk)
		return ctlruntime.Result{}, c.unbound(ctx, req, object, status, ReasonBackendMissing, fmt.Sprintf("the %s %s does not exist", kind, name))
	}
	if err != nil {
		return ctlruntime.Result{}, err
	}
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace}
	c.bind(req, &repoBinding{self: self, backend: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: kind, Name: name, Workspace: workspace},
		labels: maps.Clone(object.Labels)}, bk)
	status.DID = backend.DID
	status.Head = backend.Head
	status.Rev = backend.Rev
	status.Writable = backend.Writable
	status.Address = backend.Address
	setCondition(&status, v1alpha1.AtprotoRepoConditionBackendBound, true, ReasonBound, fmt.Sprintf("bound to %s %s", kind, name), generation)
	ready := meta.FindStatusCondition(backend.Conditions, commonv1.ConditionReady)
	switch {
	case backend.ObservedGeneration != backendGeneration:
		setCondition(&status, v1alpha1.AtprotoRepoConditionReady, false, ReasonBackendStale,
			fmt.Sprintf("the %s status is at generation %d, the spec at %d", kind, backend.ObservedGeneration, backendGeneration), generation)
	case ready == nil:
		setCondition(&status, v1alpha1.AtprotoRepoConditionReady, false, ReasonBackendNotReady, fmt.Sprintf("the %s reports no Ready condition", kind), generation)
	case ready.Status != metav1.ConditionTrue:
		setCondition(&status, v1alpha1.AtprotoRepoConditionReady, false, ReasonBackendNotReady, ready.Message, generation)
	default:
		setCondition(&status, v1alpha1.AtprotoRepoConditionReady, true, ReasonBackendReady, ready.Message, generation)
	}
	return ctlruntime.Result{}, c.apply(ctx, req, object, status)
}

func (c *Controller) readBackend(ctx context.Context, req ctlruntime.Request, kind, name string) (v1alpha1.AtprotoRepoBackendStatus, int64, error) {
	switch strings.ToLower(kind) {
	case strings.ToLower(LocalRepoKind):
		var obj v1alpha1.LocalRepo
		if err := req.Get(ctx, "", name, &obj); err != nil {
			return v1alpha1.AtprotoRepoBackendStatus{}, 0, err
		}
		return obj.Status, obj.Generation, nil
	case strings.ToLower(RemoteRepoKind):
		var obj v1alpha1.RemoteRepo
		if err := req.Get(ctx, "", name, &obj); err != nil {
			return v1alpha1.AtprotoRepoBackendStatus{}, 0, err
		}
		return obj.Status, obj.Generation, nil
	}
	return v1alpha1.AtprotoRepoBackendStatus{}, 0, fmt.Errorf("atprotorepo: no reader for the backend kind %s", kind)
}

func (c *Controller) unbound(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRepo, status v1alpha1.AtprotoRepoStatus,
	reason, message string) error {
	generation := object.Generation
	setCondition(&status, v1alpha1.AtprotoRepoConditionBackendBound, false, reason, message, generation)
	setCondition(&status, v1alpha1.AtprotoRepoConditionReady, false, reason, message, generation)
	return c.apply(ctx, req, object, status)
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.AtprotoRepo, status v1alpha1.AtprotoRepoStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func setCondition(status *v1alpha1.AtprotoRepoStatus, conditionType string, value bool, reason, message string, generation int64) {
	if message == "" {
		message = reason
	}
	conds.Set(&status.Conditions, generation, conditionType, value, reason, message)
}
