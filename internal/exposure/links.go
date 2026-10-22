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

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const (
	ComponentLinkFinalizer = "exposure.socialweb.computer/componentlink"

	ConditionTargetRegistered = "TargetRegistered"
)

type LinkPolicy struct {
	mu sync.Mutex

	handlers *component.HandlerRegistry

	sources map[string]map[string][]component.AccessLink
}

func NewLinkPolicy(handlers *component.HandlerRegistry) *LinkPolicy {
	policy := &LinkPolicy{handlers: handlers, sources: map[string]map[string][]component.AccessLink{}}
	if handlers != nil {
		handlers.SetLinks(nil)
	}
	return policy
}

func (p *LinkPolicy) Set(workspace, name string, links []component.AccessLink) {
	if p == nil {
		return
	}
	p.mu.Lock()
	byName := p.sources[workspace]
	if byName == nil {
		byName = map[string][]component.AccessLink{}
		p.sources[workspace] = byName
	}
	byName[name] = cloneAccessLinks(links)
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

func (p *LinkPolicy) Links() []component.AccessLink {
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

func (p *LinkPolicy) mergedLocked() []component.AccessLink {
	var out []component.AccessLink
	for _, byName := range p.sources {
		for _, links := range byName {
			out = append(out, links...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.FromWorkspace != b.FromWorkspace {
			return a.FromWorkspace < b.FromWorkspace
		}
		if a.FromKind != b.FromKind {
			return a.FromKind < b.FromKind
		}
		if a.FromName != b.FromName {
			return a.FromName < b.FromName
		}
		if a.ToWorkspace != b.ToWorkspace {
			return a.ToWorkspace < b.ToWorkspace
		}
		if a.ToKind != b.ToKind {
			return a.ToKind < b.ToKind
		}
		if a.ToName != b.ToName {
			return a.ToName < b.ToName
		}
		return strings.Join(a.Handlers, ",") < strings.Join(b.Handlers, ",")
	})
	return out
}

func cloneAccessLinks(links []component.AccessLink) []component.AccessLink {
	if links == nil {
		return nil
	}
	out := make([]component.AccessLink, len(links))
	for i, link := range links {
		out[i] = link
		out[i].Handlers = append([]string(nil), link.Handlers...)
	}
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
	if err := validateLinkSpec(link.Spec); err != nil {
		c.forget(name)
		status.Phase = exposurev1alpha1.ComponentLinkFailed
		status.Ready = false
		status.Message = err.Error()
		setCondition(&status.Conditions, generation, ConditionTargetRegistered, false, "InvalidSpec", err.Error())
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidSpec", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	if link.Spec.Suspend {
		c.forget(name)
		status.Phase = exposurev1alpha1.ComponentLinkPending
		status.Ready = false
		status.Message = "the ComponentLink is suspended"
		setCondition(&status.Conditions, generation, ConditionTargetRegistered, false, "Suspended", status.Message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "Suspended", status.Message)
		return writeStatus(ctx, ri, name, &status)
	}
	to := link.Spec.To
	if !c.targetRegistered(to) {
		c.forget(name)
		status.Phase = exposurev1alpha1.ComponentLinkPending
		status.Ready = false
		status.Message = fmt.Sprintf("the target %s/%s in workspace %s is not registered", to.Kind, to.Name, to.Workspace)
		setCondition(&status.Conditions, generation, ConditionTargetRegistered, false, "TargetMissing", status.Message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "TargetMissing", status.Message)
		return writeStatus(ctx, ri, name, &status)
	}
	compiled := compileLink(link.Spec)
	c.policy.Set(c.workspace, name, []component.AccessLink{compiled})
	status.Phase = exposurev1alpha1.ComponentLinkReady
	status.Ready = true
	status.Message = "the ComponentLink is compiled into the handler registry"
	setCondition(&status.Conditions, generation, ConditionTargetRegistered, true, "TargetRegistered",
		fmt.Sprintf("the target %s/%s in workspace %s is registered", to.Kind, to.Name, to.Workspace))
	setCondition(&status.Conditions, generation, ConditionReady, true, "Ready", status.Message)
	return writeStatus(ctx, ri, name, &status)
}

func (c *LinkController) targetRegistered(to exposurev1alpha1.ComponentLinkEndpoint) bool {
	if c.registry == nil {
		return false
	}
	return c.registry.Has(strings.TrimSpace(to.Workspace), strings.TrimSpace(to.Kind), strings.TrimSpace(to.Name))
}

func validateLinkSpec(spec exposurev1alpha1.ComponentLinkSpec) error {
	if strings.TrimSpace(spec.From.Workspace) == "" {
		return errors.New("the ComponentLink from endpoint has no workspace")
	}
	if strings.TrimSpace(spec.To.Workspace) == "" {
		return errors.New("the ComponentLink to endpoint has no workspace")
	}
	for _, handler := range spec.Handlers {
		if strings.TrimSpace(handler) == "" {
			return errors.New("the ComponentLink handlers list has an empty name")
		}
	}
	return nil
}

func compileLink(spec exposurev1alpha1.ComponentLinkSpec) component.AccessLink {
	handlers := make([]string, 0, len(spec.Handlers))
	seen := map[string]bool{}
	for _, handler := range spec.Handlers {
		normalized := strings.ToLower(strings.TrimSpace(handler))
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		handlers = append(handlers, normalized)
	}
	return component.AccessLink{
		FromWorkspace: strings.TrimSpace(spec.From.Workspace),
		FromKind:      strings.TrimSpace(spec.From.Kind),
		FromName:      strings.TrimSpace(spec.From.Name),
		ToWorkspace:   strings.TrimSpace(spec.To.Workspace),
		ToKind:        strings.TrimSpace(spec.To.Kind),
		ToName:        strings.TrimSpace(spec.To.Name),
		Handlers:      handlers,
	}
}
