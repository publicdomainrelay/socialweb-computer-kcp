package componentlink

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "componentlink"

	Export = "exposures"
)

type Options struct {
	Policy *exposure.LinkPolicy

	Registry *component.Registry

	Scope func(workspace string) bool

	Log *slog.Logger
}

type Controller struct {
	policy *exposure.LinkPolicy

	registry *component.Registry

	log *slog.Logger

	targets *binding.Table[struct{}]

	events *ctlruntime.Queue
}

func New(opts Options) (*Controller, error) {
	if opts.Policy == nil {
		return nil, errors.New("componentlink: a link policy is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Controller{policy: opts.Policy, registry: opts.Registry, log: log.With("component", "componentlink-controller"),
		targets: binding.New[struct{}](), events: ctlruntime.NewQueue()}, nil
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	events, err := ctl.events.Run(m, Name+": components")
	if err != nil {
		return nil, fmt.Errorf("%s: forwarding component changes: %w", Name, err)
	}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Events(events), ctlruntime.Gone(ctl.gone)}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*exposurev1alpha1.ComponentLink](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("componentlink: registering the ComponentLink controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) ComponentChanged(key component.Key) {
	for _, t := range c.targets.Users(binding.Key(key.Workspace, key.Kind, key.Name)) {
		c.events.Add(ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.forget(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name})
}

func (c *Controller) forget(self binding.Target) {
	c.policy.Remove(self.Workspace, self.Name)
	c.targets.Set(self, nil)
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.ComponentLink) (ctlruntime.Result, error) {
	self := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.forget(self)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := object.Status
	status.ObservedGeneration = generation
	compiled, reason, err := compileLink(req.Workspace, req.Name, object.Spec)
	switch {
	case err != nil:
		c.policy.Remove(req.Workspace, req.Name)
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, false, reason, err.Error())
	case object.Spec.Suspend:
		c.policy.Remove(req.Workspace, req.Name)
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, false, "Suspended", "the ComponentLink is suspended")
	default:
		c.policy.Set(req.Workspace, req.Name, []bus.Link{compiled})
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, true, "Accepted",
			fmt.Sprintf("the bus allows %s to call %s with %s", fromString(compiled.From), compiled.To.String(), capabilitiesString(compiled.Capabilities)))
	}
	to := linkTarget(req.Workspace, object.Spec.To)
	if to.Kind != "" && to.Name != "" {
		c.targets.Set(self, &struct{}{}, binding.Key(to.Workspace, to.Kind, to.Name))
	} else {
		c.targets.Set(self, nil)
	}
	if c.targetRegistered(to) {
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionResolvedRefs, true, "TargetRegistered",
			fmt.Sprintf("the target %s is registered", to.String()))
	} else {
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionResolvedRefs, false, "TargetMissing",
			fmt.Sprintf("the target %s is not registered", to.String()))
	}
	next := object.DeepCopy()
	next.Status = status
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) targetRegistered(to commonv1.ObjectRef) bool {
	if c.registry == nil || to.Kind == "" || to.Name == "" {
		return false
	}
	return c.registry.Has(to.Workspace, to.Kind, to.Name)
}

func linkTarget(workspace string, to commonv1.ObjectRef) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: strings.TrimSpace(to.Group), Kind: strings.TrimSpace(to.Kind), Name: strings.TrimSpace(to.Name),
		Workspace: cmp.Or(strings.TrimSpace(to.Workspace), workspace)}
}

func compileLink(workspace, name string, spec exposurev1alpha1.ComponentLinkSpec) (bus.Link, string, error) {
	to := linkTarget(workspace, spec.To)
	switch {
	case to.Kind == "" || to.Name == "":
		return bus.Link{}, "InvalidSpec", errors.New("the ComponentLink to ref needs a kind and a name")
	case to.Workspace != workspace:
		return bus.Link{}, "NotProducerWorkspace", fmt.Errorf("the ComponentLink must live in the workspace of its target %s", to.String())
	}
	from, err := compileFrom(workspace, spec.From)
	if err != nil {
		return bus.Link{}, "InvalidSpec", err
	}
	if len(spec.Capabilities) == 0 {
		return bus.Link{}, "InvalidSpec", errors.New("the ComponentLink grants no capability")
	}
	capabilities := make([]bus.Capability, 0, len(spec.Capabilities))
	for _, capability := range spec.Capabilities {
		capabilityName := strings.ToLower(strings.TrimSpace(capability.Name))
		if capabilityName == "" {
			return bus.Link{}, "InvalidSpec", errors.New("the ComponentLink capabilities list has an empty name")
		}
		scope := make([]string, 0, len(capability.Scope))
		for _, entry := range capability.Scope {
			if entry = strings.TrimSpace(entry); entry == "" {
				return bus.Link{}, "InvalidSpec", fmt.Errorf("the capability %s has an empty scope entry", capabilityName)
			}
			scope = append(scope, entry)
		}
		capabilities = append(capabilities, bus.Capability{Name: capabilityName, Scope: scope})
	}
	return bus.Link{Workspace: workspace, Name: name, From: from, To: to, Capabilities: capabilities}, "", nil
}

func compileFrom(workspace string, from exposurev1alpha1.ComponentLinkFrom) (bus.LinkFrom, error) {
	switch {
	case from.Ref != nil && from.Selector != nil:
		return bus.LinkFrom{}, errors.New("the ComponentLink from names both a ref and a selector")
	case from.Ref != nil:
		ref := linkTarget(workspace, *from.Ref)
		if ref.Kind == "" || ref.Name == "" {
			return bus.LinkFrom{}, errors.New("the ComponentLink from ref needs a kind and a name")
		}
		return bus.LinkFrom{Ref: &ref}, nil
	case from.Selector != nil:
		sel := from.Selector
		if strings.TrimSpace(sel.Kind) == "" {
			return bus.LinkFrom{}, errors.New("the ComponentLink from selector needs a kind")
		}
		requirements := make([]metav1.LabelSelectorRequirement, 0, len(sel.MatchExpressions))
		for _, r := range sel.MatchExpressions {
			requirements = append(requirements, metav1.LabelSelectorRequirement{Key: r.Key, Operator: metav1.LabelSelectorOperator(r.Operator),
				Values: append([]string(nil), r.Values...)})
		}
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: sel.MatchLabels, MatchExpressions: requirements})
		if err != nil {
			return bus.LinkFrom{}, fmt.Errorf("the ComponentLink from selector: %w", err)
		}
		return bus.LinkFrom{Group: strings.TrimSpace(sel.Group), Kind: strings.TrimSpace(sel.Kind),
			Workspace: cmp.Or(strings.TrimSpace(sel.Workspace), workspace), Selector: selector}, nil
	default:
		return bus.LinkFrom{}, errors.New("the ComponentLink from needs a ref or a selector")
	}
}

func fromString(from bus.LinkFrom) string {
	if from.Ref != nil {
		return from.Ref.String()
	}
	return from.Workspace + ":" + from.Kind + "{" + from.Selector.String() + "}"
}

func capabilitiesString(capabilities []bus.Capability) string {
	parts := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		parts = append(parts, capability.String())
	}
	return strings.Join(parts, ", ")
}
