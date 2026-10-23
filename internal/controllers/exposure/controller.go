package exposure

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const (
	Name = "exposure"

	Export = "exposures"

	ConditionHandlerRegistered = "HandlerRegistered"

	ConditionReady = "Ready"

	ConditionIngressProvider = "IngressProvider"

	ServiceKind = "Service"

	IngressProxyKind = "IngressProxy"

	EventBuffer = 1024
)

type Options struct {
	Publisher *exposure.Publisher

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	publisher *exposure.Publisher

	log *slog.Logger

	refs *binding.Table[struct{}]

	events chan ctlruntime.Request
}

func New(opts Options) (*Controller, error) {
	if opts.Publisher == nil {
		return nil, errors.New("exposure: a publisher is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{publisher: opts.Publisher, log: log.With("component", "exposure-controller"), refs: binding.New[struct{}](),
		events: make(chan ctlruntime.Request, EventBuffer)}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(ctl.events), ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.Service) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, ServiceKind, obj.Name))
		}),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.ComponentLink) []ctlruntime.Request {
			to := obj.Spec.To
			return ctl.users(binding.Key(to.InWorkspace(src.Workspace), to.Kind, to.Name))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*exposurev1alpha1.Exposure](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("exposure: registering the Exposure controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Close() {
	c.publisher.Close()
}

func (c *Controller) ComponentChanged(key component.Key) {
	seen := map[string]bool{}
	for _, ref := range []string{binding.Key(key.Workspace, key.Kind, key.Name), binding.Key("", key.Kind, key.Name)} {
		for _, r := range c.users(ref) {
			id := r.Cluster + "/" + r.Name
			if seen[id] {
				continue
			}
			seen[id] = true
			select {
			case c.events <- r:
			default:
				go func() { c.events <- r }()
			}
		}
	}
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.publisher.Withdraw(req.Workspace, req.Name)
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure) (ctlruntime.Result, error) {
	self := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.publisher.Withdraw(req.Workspace, req.Name)
		c.refs.Set(self, nil)
		return ctlruntime.Result{}, nil
	}
	spec := object.Spec
	serviceName := strs.LastSegment(spec.ServiceRef)
	var refs []string
	if serviceName != "" {
		refs = append(refs, binding.Key(req.Workspace, ServiceKind, serviceName))
	}
	if spec.Mode == exposurev1alpha1.ExposureModeIngress {
		ingress := cmp.Or(strings.TrimSpace(spec.IngressRef), exposure.DefaultIngressName)
		refs = append(refs, binding.Key(req.Workspace, IngressProxyKind, ingress), binding.Key("", IngressProxyKind, ingress))
	}
	var svc *exposurev1alpha1.Service
	if serviceName != "" {
		var found exposurev1alpha1.Service
		if err := req.Get(ctx, "", serviceName, &found); err == nil {
			svc = &found
		} else if !apierrors.IsNotFound(err) {
			c.refs.Set(self, &struct{}{}, refs...)
			return ctlruntime.Result{}, err
		}
	}
	var target component.Key
	var targetErr error
	if svc != nil {
		if target, targetErr = exposure.Target(svc.Spec.TargetRef, req.Workspace); targetErr == nil {
			refs = append(refs, binding.Key(target.Workspace, target.Kind, target.Name))
		}
	}
	c.refs.Set(self, &struct{}{}, refs...)
	switch {
	case spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "Suspended", "the Exposure is suspended")
	case spec.Mode != exposurev1alpha1.ExposureModeLocal && spec.Mode != exposurev1alpha1.ExposureModeIngress:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidMode", fmt.Sprintf("unknown mode %q", spec.Mode))
	case serviceName == "":
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing", "the Exposure has no serviceRef")
	case svc == nil:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing",
			"the Service "+serviceName+" is not present in workspace "+req.Workspace)
	case svc.Spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceSuspended", "the Service "+serviceName+" is suspended")
	case targetErr != nil:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidTarget", targetErr.Error())
	}
	_, found, err := c.publisher.Handler(ctx, target, svc.Spec.Protocol, svc.Spec.Handler)
	switch {
	case errors.Is(err, bus.ErrLinkDenied):
		res, applyErr := c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "LinkDenied", err.Error())
		return res, errors.Join(err, applyErr)
	case errors.Is(err, exposure.ErrUnknownProtocol):
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidProtocol", err.Error())
	case err != nil:
		res, applyErr := c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "HandlerUnreachable", err.Error())
		return res, errors.Join(err, applyErr)
	case !found:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "HandlerMissing",
			fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", exposure.HandlerName(svc.Spec.Handler), target.Kind, target.Name, target.Workspace))
	}
	published, err := c.publisher.Publish(ctx, exposure.Binding{Workspace: req.Workspace, Name: req.Name, Spec: spec, Service: svc.Spec,
		ServiceGeneration: svc.Generation, Target: target})
	generation := object.Generation
	status := object.Status
	if err != nil {
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		reason := "Pending"
		var retry error
		switch {
		case errors.Is(err, exposure.ErrStableHostMismatch):
			reason = "StableHostMismatch"
			status.Phase = exposurev1alpha1.ExposureFailed
		case errors.Is(err, exposure.ErrIngressNotReady):
			reason = "IngressNotReady"
		default:
			retry = err
		}
		conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", "the target handler is registered")
		conds.Set(&status.Conditions, generation, ConditionReady, false, reason, err.Error())
		return ctlruntime.Result{}, errors.Join(retry, apply(ctx, req, object, status))
	}
	status.Phase = exposurev1alpha1.ExposureReady
	status.Ready = true
	status.Address = published.Address
	status.Endpoint = published.Endpoint
	conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", "the target handler is registered")
	message := "the Exposure is bound"
	if published.Provider != "" {
		conds.Set(&status.Conditions, generation, ConditionIngressProvider, true, "ProviderReady", published.Provider)
		message = "the Exposure is bound by the " + published.Provider + " ingress provider"
	}
	conds.Set(&status.Conditions, generation, ConditionReady, true, "Ready", message)
	return ctlruntime.Result{}, apply(ctx, req, object, status)
}

func (c *Controller) withdraw(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure, phase exposurev1alpha1.ExposurePhase,
	reason, message string) (ctlruntime.Result, error) {
	c.publisher.Withdraw(req.Workspace, req.Name)
	generation := object.Generation
	status := object.Status
	status.Phase = phase
	status.Ready = false
	status.Address = ""
	status.Endpoint = ""
	conds.Set(&status.Conditions, generation, ConditionHandlerRegistered, false, reason, message)
	conds.Set(&status.Conditions, generation, ConditionReady, false, reason, message)
	return ctlruntime.Result{}, apply(ctx, req, object, status)
}

func apply(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure, status exposurev1alpha1.ExposureStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}
