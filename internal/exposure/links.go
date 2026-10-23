package exposure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	ComponentLinkFinalizer = "exposure.socialweb.computer/componentlink"
)

type LinkPolicy struct {
	mu sync.Mutex

	handlers *bus.Bus

	sources map[string]map[string][]bus.Link
}

func NewLinkPolicy(handlers *bus.Bus) *LinkPolicy {
	policy := &LinkPolicy{handlers: handlers, sources: map[string]map[string][]bus.Link{}}
	if handlers != nil {
		handlers.SetLinks(nil)
	}
	return policy
}

func (p *LinkPolicy) Set(workspace, name string, links []bus.Link) {
	if p == nil {
		return
	}
	p.mu.Lock()
	byName := p.sources[workspace]
	if byName == nil {
		byName = map[string][]bus.Link{}
		p.sources[workspace] = byName
	}
	byName[name] = append([]bus.Link(nil), links...)
	p.publishLocked()
	p.mu.Unlock()
}

func (p *LinkPolicy) Remove(workspace, name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if byName := p.sources[workspace]; byName != nil {
		delete(byName, name)
		if len(byName) == 0 {
			delete(p.sources, workspace)
		}
	}
	p.publishLocked()
	p.mu.Unlock()
}

func (p *LinkPolicy) Links() []bus.Link {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mergedLocked()
}

func (p *LinkPolicy) publishLocked() {
	if p.handlers == nil {
		return
	}
	p.handlers.SetLinks(p.mergedLocked())
}

func (p *LinkPolicy) mergedLocked() []bus.Link {
	var out []bus.Link
	for _, byName := range p.sources {
		for _, links := range byName {
			out = append(out, links...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

type LinkOptions struct {
	Workspace  string
	Dynamic    DynamicFactory
	Registry   *component.Registry
	Policy     *LinkPolicy
	Log        *slog.Logger
	RetryAfter time.Duration
}

type LinkController struct {
	workspace string
	dynamic   DynamicFactory
	registry  *component.Registry
	policy    *LinkPolicy
	log       *slog.Logger
	retry     time.Duration
}

func NewLinkController(opts LinkOptions) (*LinkController, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("exposure: a workspace is required for the ComponentLink controller")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("exposure: a dynamic client factory is required for the ComponentLink controller")
	}
	if opts.Policy == nil {
		return nil, errors.New("exposure: a link policy is required for the ComponentLink controller")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	return &LinkController{
		workspace: workspace,
		dynamic:   opts.Dynamic,
		registry:  opts.Registry,
		policy:    opts.Policy,
		log:       log.With("component", "componentlink-controller"),
		retry:     opts.RetryAfter,
	}, nil
}

func (c *LinkController) Run(ctx context.Context) error {
	w := &watcher{
		workspace: c.workspace,
		dynamic:   c.dynamic,
		gvr:       ComponentLinkGVR,
		log:       c.log,
		retry:     c.retry,
		reconcile: c.Reconcile,
		deleted:   c.forget,
	}
	return w.Run(ctx)
}

func (c *LinkController) forget(name string) {
	c.policy.Remove(c.workspace, name)
}

func (c *LinkController) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	ri := dyn.Resource(ComponentLinkGVR)
	name := u.GetName()
	if u.GetDeletionTimestamp() != nil {
		c.forget(name)
		return ensureMetadata(ctx, ri, name, ComponentLinkFinalizer, false, nil)
	}
	if err := ensureMetadata(ctx, ri, name, ComponentLinkFinalizer, true, nil); err != nil {
		return err
	}
	var link exposurev1alpha1.ComponentLink
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &link); err != nil {
		return fmt.Errorf("exposure: decoding ComponentLink %s: %w", name, err)
	}
	generation := u.GetGeneration()
	status := link.Status
	status.ObservedGeneration = generation
	status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	compiled, reason, err := c.compileLink(name, link.Spec)
	switch {
	case err != nil:
		c.forget(name)
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, false, reason, err.Error())
	case link.Spec.Suspend:
		c.forget(name)
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, false, "Suspended", "the ComponentLink is suspended")
	default:
		c.policy.Set(c.workspace, name, []bus.Link{compiled})
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionAccepted, true, "Accepted",
			fmt.Sprintf("the bus allows %s to call %s with %s", fromString(compiled.From), compiled.To.String(), capabilitiesString(compiled.Capabilities)))

	}
	to := linkTarget(c.workspace, link.Spec.To)
	if c.targetRegistered(to) {
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionResolvedRefs, true, "TargetRegistered",
			fmt.Sprintf("the target %s is registered", to.String()))

	} else {
		conds.Set(&status.Conditions, generation, exposurev1alpha1.ComponentLinkConditionResolvedRefs, false, "TargetMissing",
			fmt.Sprintf("the target %s is not registered", to.String()))

	}
	return writeStatus(ctx, ri, name, &status)
}

func (c *LinkController) targetRegistered(to commonv1.ObjectRef) bool {
	if c.registry == nil || strings.TrimSpace(to.Kind) == "" || strings.TrimSpace(to.Name) == "" {
		return false
	}
	return c.registry.Has(to.Workspace, to.Kind, to.Name)
}

func linkTarget(workspace string, to commonv1.ObjectRef) commonv1.ObjectRef {
	out := commonv1.ObjectRef{Group: strings.TrimSpace(to.Group), Kind: strings.TrimSpace(to.Kind), Name: strings.TrimSpace(to.Name),
		Workspace: strings.TrimSpace(to.Workspace)}
	if out.Workspace == "" {
		out.Workspace = workspace
	}
	return out
}

func (c *LinkController) compileLink(name string, spec exposurev1alpha1.ComponentLinkSpec) (bus.Link, string, error) {
	to := linkTarget(c.workspace, spec.To)
	switch {
	case to.Kind == "" || to.Name == "":
		return bus.Link{}, "InvalidSpec", errors.New("the ComponentLink to ref needs a kind and a name")
	case to.Workspace != c.workspace:
		return bus.Link{}, "NotProducerWorkspace", fmt.Errorf("the ComponentLink must live in the workspace of its target %s", to.String())
	}
	from, err := compileFrom(c.workspace, spec.From)
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
	return bus.Link{Workspace: c.workspace, Name: name, From: from, To: to, Capabilities: capabilities}, "", nil
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
		ws := strings.TrimSpace(sel.Workspace)
		if ws == "" {
			ws = workspace
		}
		return bus.LinkFrom{Group: strings.TrimSpace(sel.Group), Kind: strings.TrimSpace(sel.Kind), Workspace: ws, Selector: selector}, nil
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
