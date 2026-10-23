package policyengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const cancelTimeout = 5 * time.Second

var ErrNotRunning = errors.New("policyengine: the policy engine is not running")

type Runner struct {
	log *slog.Logger

	mu sync.Mutex

	bindings map[string]binding

	waiters map[string]map[*waiter]struct{}
}

type binding struct {
	req ctlruntime.Request

	engine string
}

type waiter struct {
	done chan *policyv1alpha1.PolicyWorkflowRun
}

type handle struct {
	runner *Runner

	binding binding

	caller commonv1.ObjectRef

	scope []string
}

func newRunner(log *slog.Logger) *Runner {
	return &Runner{log: log, bindings: map[string]binding{}, waiters: map[string]map[*waiter]struct{}{}}
}

func (r *Runner) bind(req ctlruntime.Request, engine string) {
	b := binding{req: req, engine: engine}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range bindingKeys(req, engine) {
		r.bindings[key] = b
	}
}

func (r *Runner) unbind(req ctlruntime.Request, engine string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range bindingKeys(req, engine) {
		delete(r.bindings, key)
	}
}

func bindingKeys(req ctlruntime.Request, engine string) []string {
	keys := []string{Key(req.Cluster, engine)}
	if req.Workspace != "" && req.Workspace != req.Cluster {
		keys = append(keys, Key(req.Workspace, engine))
	}
	return keys
}

func (r *Runner) open(ctx context.Context, to commonv1.ObjectRef, scope []string) (policy.Runner, error) {
	r.mu.Lock()
	b, ok := r.bindings[Key(strings.TrimSpace(to.Workspace), strings.TrimSpace(to.Name))]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotRunning, to)
	}
	caller, _ := bus.CallerFrom(ctx)
	return &handle{runner: r, binding: b, caller: caller.Ref, scope: scope}, nil
}

func (r *Runner) observe(cluster string, run *policyv1alpha1.PolicyWorkflowRun) {
	if run == nil || !policy.Terminal(run.Status.Phase) {
		return
	}
	key := Key(cluster, run.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	for w := range r.waiters[key] {
		select {
		case w.done <- run.DeepCopy():
		default:
		}
	}
}

func (r *Runner) register(key string) *waiter {
	w := &waiter{done: make(chan *policyv1alpha1.PolicyWorkflowRun, 1)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiters[key] == nil {
		r.waiters[key] = map[*waiter]struct{}{}
	}
	r.waiters[key][w] = struct{}{}
	return w
}

func (r *Runner) release(key string, w *waiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waiters[key], w)
	if len(r.waiters[key]) == 0 {
		delete(r.waiters, key)
	}
}

func (h *handle) Run(ctx context.Context, in policy.RunRequest) (string, error) {
	workflow := strings.TrimSpace(in.Workflow)
	if ref := strings.TrimSpace(in.Spec.WorkflowRef); ref != "" {
		if workflow == "" {
			workflow = ref
		} else if workflow != ref {
			return "", fmt.Errorf("policyengine: the run names workflow %s but references %s", workflow, ref)
		}
	}
	if workflow == "" {
		return "", errors.New("policyengine: the run names no workflow")
	}
	if !scopeAllows(h.scope, workflow) {
		return "", fmt.Errorf("%w: %s may not run %s on %s", bus.ErrLinkDenied, h.caller, workflow, h.binding.engine)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", errors.New("policyengine: the run has no name")
	}
	run := &policyv1alpha1.PolicyWorkflowRun{
		TypeMeta:   metav1.TypeMeta{APIVersion: policyv1alpha1.GroupVersion.String(), Kind: "PolicyWorkflowRun"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       *in.Spec.DeepCopy(),
	}
	run.Spec.EngineRef = h.binding.engine
	if owner, ok := h.ownerRef(in.Owner); ok {
		run.OwnerReferences = []metav1.OwnerReference{owner}
	}
	if err := h.binding.req.Apply(ctx, run); err != nil {
		return "", err
	}
	return name, nil
}

func (h *handle) Status(ctx context.Context, runID string) (policyv1alpha1.PolicyWorkflowRunStatus, error) {
	run, err := h.get(ctx, runID)
	if err != nil {
		return policyv1alpha1.PolicyWorkflowRunStatus{}, err
	}
	return run.Status, nil
}

func (h *handle) Wait(ctx context.Context, runID string) (policyv1alpha1.PolicyWorkflowRunStatus, error) {
	key := Key(h.binding.req.Cluster, runID)
	w := h.runner.register(key)
	defer h.runner.release(key, w)
	run, err := h.get(ctx, runID)
	switch {
	case err == nil && policy.Terminal(run.Status.Phase):
		return run.Status, nil
	case err != nil && !apierrors.IsNotFound(err):
		if ctx.Err() != nil {
			return policyv1alpha1.PolicyWorkflowRunStatus{}, ctx.Err()
		}
		h.runner.log.Debug("reading a policy run before waiting", "run", runID, "err", err.Error())
	}
	select {
	case done := <-w.done:
		if !h.owns(done) {
			return policyv1alpha1.PolicyWorkflowRunStatus{}, fmt.Errorf("policyengine: the run %s is not the caller's", runID)
		}
		return done.Status, nil
	case <-ctx.Done():
		h.cancel(ctx, runID)
		return policyv1alpha1.PolicyWorkflowRunStatus{}, ctx.Err()
	}
}

func (h *handle) get(ctx context.Context, runID string) (*policyv1alpha1.PolicyWorkflowRun, error) {
	run := &policyv1alpha1.PolicyWorkflowRun{}
	if err := h.binding.req.Get(ctx, "", runID, run); err != nil {
		return nil, err
	}
	if !h.owns(run) {
		return nil, fmt.Errorf("policyengine: the run %s is not the caller's", runID)
	}
	return run, nil
}

func (h *handle) cancel(ctx context.Context, runID string) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	run, err := h.get(cancelCtx, runID)
	if err != nil || policy.Terminal(run.Status.Phase) || run.Spec.Cancel {
		return
	}
	run.Spec.Cancel = true
	if err := h.binding.req.Apply(cancelCtx, run); err != nil {
		h.runner.log.Warn("cancelling an abandoned policy run failed", "run", runID, "err", err.Error())
	}
}

func (h *handle) owns(run *policyv1alpha1.PolicyWorkflowRun) bool {
	if run == nil || run.Spec.EngineRef != h.binding.engine {
		return false
	}
	if len(run.OwnerReferences) == 0 {
		return true
	}
	return slices.ContainsFunc(run.OwnerReferences, func(owner metav1.OwnerReference) bool {
		return strings.EqualFold(owner.Kind, h.caller.Kind) && owner.Name == h.caller.Name
	})
}

func (h *handle) ownerRef(owner policy.Owner) (metav1.OwnerReference, bool) {
	if owner.UID == "" || owner.APIVersion == "" || owner.Name == "" || owner.Kind == "" {
		return metav1.OwnerReference{}, false
	}
	if !strings.EqualFold(owner.Kind, h.caller.Kind) || owner.Name != h.caller.Name {
		return metav1.OwnerReference{}, false
	}
	workspace := strings.TrimSpace(h.caller.Workspace)
	if workspace != h.binding.req.Workspace && workspace != h.binding.req.Cluster {
		return metav1.OwnerReference{}, false
	}
	return metav1.OwnerReference{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: types.UID(owner.UID)}, true
}

func scopeAllows(scope []string, workflow string) bool {
	for _, entry := range scope {
		entry = strings.TrimSpace(entry)
		if entry == bus.ScopeAll || entry == workflow {
			return true
		}
		if prefix, ok := strings.CutSuffix(entry, bus.ScopeAll); ok && strings.HasPrefix(workflow, prefix) {
			return true
		}
	}
	return false
}
