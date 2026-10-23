package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "service"

	Export = "exposures"

	ConditionHandlerRegistered = "HandlerRegistered"

	ConditionReady = "Ready"
)

type Options struct {
	Handlers *component.Handlers

	Registry *component.Registry

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	handlers *component.Handlers

	registry *component.Registry

	log *slog.Logger

	targets *binding.Table[struct{}]

	events *ctlruntime.Queue
}

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{handlers: opts.Handlers, registry: opts.Registry, log: log.With("component", "service-controller"),
		targets: binding.New[struct{}](), events: ctlruntime.NewQueue()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	events, err := ctl.events.Run(m, Name+": components")
	if err != nil {
		return nil, fmt.Errorf("%s: forwarding component changes: %w", Name, err)
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(events), ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.ComponentLink) []ctlruntime.Request {
			to := obj.Spec.To
			return ctl.users(binding.Key(to.InWorkspace(src.Workspace), to.Kind, to.Name))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*exposurev1alpha1.Service](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("service: registering the Service controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) ComponentChanged(key component.Key) {
	c.events.Add(c.users(binding.Key(key.Workspace, key.Kind, key.Name))...)
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.targets.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.targets.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Service) (ctlruntime.Result, error) {
	self := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.targets.Set(self, nil)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := object.Status
	status.Phase = exposurev1alpha1.ServicePending
	status.Ready = false
	status.Endpoints = nil
	if object.Spec.Suspend {
		c.targets.Set(self, nil)
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "Suspended", "the Service is suspended")
		conds.Set(&status.Conditions, generation, ConditionReady, false, "Suspended", "the Service is suspended")
		return ctlruntime.Result{}, apply(ctx, req, object, status)
	}
	target, err := exposure.Target(object.Spec.TargetRef, req.Workspace)
	if err != nil {
		c.targets.Set(self, nil)
		status.Phase = exposurev1alpha1.ServiceFailed
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidTarget", err.Error())
		conds.Set(&status.Conditions, generation, ConditionReady, false, "InvalidTarget", err.Error())
		return ctlruntime.Result{}, apply(ctx, req, object, status)
	}
	c.targets.Set(self, &struct{}{}, binding.Key(target.Workspace, target.Kind, target.Name))
	_, found, err := exposure.FindHandler(ctx, c.handlers, target, object.Spec.Protocol, object.Spec.Handler)
	switch {
	case errors.Is(err, bus.ErrLinkDenied):
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "LinkDenied", err.Error())
		conds.Set(&status.Conditions, generation, ConditionReady, false, "LinkDenied", err.Error())
		return ctlruntime.Result{}, errors.Join(err, apply(ctx, req, object, status))
	case errors.Is(err, exposure.ErrUnknownProtocol):
		status.Phase = exposurev1alpha1.ServiceFailed
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidProtocol", err.Error())
		conds.Set(&status.Conditions, generation, ConditionReady, false, "InvalidProtocol", err.Error())
		return ctlruntime.Result{}, apply(ctx, req, object, status)
	case err != nil:
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "HandlerUnreachable", err.Error())
		conds.Set(&status.Conditions, generation, ConditionReady, false, "HandlerUnreachable", err.Error())
		return ctlruntime.Result{}, errors.Join(err, apply(ctx, req, object, status))
	case !found:
		message := fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", exposure.HandlerName(object.Spec.Handler),
			target.Kind, target.Name, target.Workspace)
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, "HandlerMissing", message)
		conds.Set(&status.Conditions, generation, ConditionReady, false, "HandlerMissing", message)
		return ctlruntime.Result{}, apply(ctx, req, object, status)
	}
	status.Phase = exposurev1alpha1.ServiceReady
	status.Ready = true
	status.Endpoints = exposure.Endpoints(c.registry, target)
	message := fmt.Sprintf("the handler %q is registered for %s/%s", exposure.HandlerName(object.Spec.Handler), target.Kind, target.Name)
	conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", message)
	conds.Set(&status.Conditions, generation, ConditionReady, true, "Ready", message)
	return ctlruntime.Result{}, apply(ctx, req, object, status)
}

func apply(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Service, status exposurev1alpha1.ServiceStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}
