package exposurelocal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const (
	Name = "exposurelocal"

	Export = "exposures"

	ServiceKind = "Service"
)

type Options struct {
	Origin string

	Handlers *bus.Bus

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	origin Origin

	handlers *bus.Bus

	registry *Registry

	refs *binding.Table[struct{}]

	events *ctlruntime.Queue

	cancel func()

	log *slog.Logger
}

func New(opts Options) (*Controller, error) {
	origin, err := ParseOrigin(opts.Origin)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctl := &Controller{origin: origin, handlers: opts.Handlers, registry: NewRegistry(), refs: binding.New[struct{}](),
		events: ctlruntime.NewQueue(), log: log.With("component", "exposurelocal-controller")}
	return ctl, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	events, err := ctl.events.Run(m, Name+": handlers")
	if err != nil {
		return nil, fmt.Errorf("exposurelocal: forwarding handler changes: %w", err)
	}
	options := []ctlruntime.Option{
		ctlruntime.Export(Export),
		ctlruntime.Events(events),
		ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.Service) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, ServiceKind, obj.Name))
		}),
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*exposurev1alpha1.Exposure](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("exposurelocal: registering the Exposure controller: %w", err)
	}
	if opts.Handlers != nil {
		ctl.cancel = opts.Handlers.Subscribe(ctl.HandlerChanged)
	}
	return ctl, nil
}

func (c *Controller) Close() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *Controller) HandlerChanged(key bus.Key) {
	c.events.Add(c.users(binding.Key(key.Workspace, key.Kind, key.Name))...)
}

func (c *Controller) Registry() *Registry { return c.registry }

func (c *Controller) Lookup(host, path string) (http.Handler, bool) {
	return c.registry.Lookup(host, path)
}

func (c *Controller) HandlerFor(host, path string) (http.Handler, bool) {
	return c.registry.Lookup(host, path)
}

func (c *Controller) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.registry.ServeHTTP(w, req)
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.remove(ownerOf(req))
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure) (ctlruntime.Result, error) {
	self := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.remove(ownerOf(req))
		c.refs.Set(self, nil)
		return ctlruntime.Result{}, nil
	}
	spec := object.Spec
	serviceName := strs.LastSegment(spec.ServiceRef)
	var refs []string
	if serviceName != "" {
		refs = append(refs, binding.Key(req.Workspace, ServiceKind, serviceName))
	}
	c.refs.Set(self, &struct{}{}, refs...)
	var svc *exposurev1alpha1.Service
	if serviceName != "" {
		var found exposurev1alpha1.Service
		switch err := req.Get(ctx, "", serviceName, &found); {
		case err == nil:
			svc = &found
		case apierrors.IsNotFound(err):
		default:
			return ctlruntime.Result{}, err
		}
	}
	switch {
	case spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "Suspended", "the Exposure is suspended")
	case spec.Mode == exposurev1alpha1.ExposureModeIngress:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "NoIngress",
			"an in-process host has no ingress; only a local Exposure is served")
	case spec.Mode != exposurev1alpha1.ExposureModeLocal:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidMode", fmt.Sprintf("unknown mode %q", spec.Mode))
	case serviceName == "":
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing", "the Exposure has no serviceRef")
	case svc == nil:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing",
			"the Service "+serviceName+" is not present in workspace "+req.Workspace)
	case svc.Spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceSuspended",
			"the Service "+serviceName+" is suspended")
	}
	target, err := targetOf(svc.Spec.TargetRef, req.Workspace)
	if err != nil {
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidTarget", err.Error())
	}
	c.refs.Set(self, &struct{}{}, append(refs, binding.Key(target.Workspace, target.Kind, target.Name))...)
	owner := ownerOf(req)
	host, base := c.origin.Host, c.origin.URL
	if c.handlers != nil && isHTTP(svc.Spec.Protocol) {
		_, ok := c.handlers.Handler(target, svc.Spec.Handler)
		if !ok {
			message := fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", handlerName(svc.Spec.Handler),
				target.Kind, target.Name, target.Workspace)
			return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "HandlerMissing", message)
		}
		if err := c.registry.Register(owner, host, spec.Path, withCaller(c.current(target, svc.Spec.Handler), target)); err != nil {
			return ctlruntime.Result{}, err
		}
		c.log.Debug("the in-process Exposure route is registered", "host", host, "path", spec.Path,
			"target", target.Kind+"/"+target.Name)
	} else {
		c.registry.Remove(owner)
	}
	next := object.DeepCopy()
	next.Status = Status(object.Generation, base, spec.Path)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) withdraw(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure,
	phase exposurev1alpha1.ExposurePhase, reason, message string) (ctlruntime.Result, error) {
	c.remove(ownerOf(req))
	next := object.DeepCopy()
	next.Status = NotReady(object.Generation, phase, reason, message)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) remove(owner string) {
	c.registry.Remove(owner)
}

func ownerOf(req ctlruntime.Request) string { return req.Workspace + "/" + req.Name }

func targetOf(ref commonv1.ObjectRef, workspace string) (bus.Key, error) {
	kind := strings.TrimSpace(ref.Kind)
	name := strings.TrimSpace(ref.Name)
	if kind == "" {
		return bus.Key{}, errors.New("the targetRef has no kind")
	}
	if name == "" {
		return bus.Key{}, errors.New("the targetRef has no name")
	}
	targetWorkspace := strings.TrimSpace(ref.Workspace)
	if targetWorkspace == "" {
		targetWorkspace = workspace
	}
	return bus.Key{Workspace: targetWorkspace, Kind: kind, Name: name}, nil
}

func handlerName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return bus.DefaultHandlerName
	}
	return value
}

func isHTTP(protocol exposurev1alpha1.ServiceProtocol) bool {
	switch exposurev1alpha1.ServiceProtocol(strings.ToLower(strings.TrimSpace(string(protocol)))) {
	case exposurev1alpha1.ServiceProtocolHTTP, exposurev1alpha1.ServiceProtocolWS:
		return true
	}
	return false
}

func (c *Controller) current(target bus.Key, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := c.handlers.Handler(target, name)
		if !ok {
			http.Error(w, "the exposed component has no handler registered", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func withCaller(handler http.Handler, target bus.Key) http.Handler {
	ref := target.Ref()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(bus.WithCaller(r.Context(), ref)))
	})
}
