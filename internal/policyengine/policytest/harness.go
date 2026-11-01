package policytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/registry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
	sandboxdeno "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/deno"
)

const (
	PolicyGhaLiteNSID = executor.PolicyGhaLiteNSID
	EngineName        = "policy-engine"
	DefaultTimeout    = 60 * time.Second
)

type Harness struct {
	World      *World
	Resolver   *trust.Resolver
	Sandbox    *Sandbox
	Engine     *controller.Engine
	Reconciler *controller.RunReconciler
	Runner     *Runner
	Timeout    time.Duration

	mu         sync.Mutex
	resolveErr error
	ctx        context.Context
}

type Options struct {
	TrustTTL time.Duration
}

func New(t testing.TB) *Harness {
	t.Helper()
	return NewWith(t, Options{})
}

func NewWith(t testing.TB, opts Options) *Harness {
	t.Helper()
	path, _, err := DenoPath()
	if err != nil {
		t.Fatalf("policy spec tests need the policy engine's deno: %v", err)
	}
	world := NewWorld()
	resolver := trust.NewResolver(trust.ResolverOptions{Client: world.Client(), TTL: opts.TrustTTL})
	box := &Sandbox{inner: sandboxdeno.New(path)}
	engine := controller.NewEngine(controller.Config{Sandbox: box, Host: resolver, Limits: controller.Limits{MaxConcurrentRuns: 16}})
	policies := registry.GhaLiteOnly(executor.NewGhaLite(executor.GhaLiteOptions{Sandbox: box}))
	records := controller.NewRecordResolver(func(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error) {
		return resolver.Resolve(ctx, atproto.StrongRef{URI: ref.URI, CID: ref.CID})
	}, policies)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &Harness{
		World:      world,
		Resolver:   resolver,
		Sandbox:    box,
		Engine:     engine,
		Reconciler: &controller.RunReconciler{Engine: engine, Records: records, Host: resolver},
		Timeout:    DefaultTimeout,
		ctx:        ctx,
	}
	h.Runner = &Runner{h: h, runs: map[string]*Run{}}
	return h
}

func (h *Harness) SetHost(host trust.Host) {
	h.Reconciler.Host = host
}

func (h *Harness) SetWorkflows(source controller.WorkflowSource) {
	h.Reconciler.Workflows = source
}

func (h *Harness) FailResolve(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolveErr = err
}

func (h *Harness) resolve(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
	h.mu.Lock()
	err := h.resolveErr
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if engine.Name != EngineName {
		return nil, fmt.Errorf("no PolicyEngine %q", engine.Name)
	}
	return h.Runner, nil
}

func (h *Harness) Client(role string) *runs.Client {
	return &runs.Client{
		Resolve: h.resolve,
		Owner:   policy.Owner{APIVersion: "policytest/v1", Kind: role, Name: "policytest-" + role, UID: "policytest-uid"},
		Engine:  commonv1.ObjectRef{Group: policyv1alpha1.GroupName, Kind: "PolicyEngine", Name: EngineName},
		Role:    role,
		Prefix:  "policytest-" + role,
		Timeout: h.Timeout,
		Parser:  runs.Strict,
		Now:     time.Now,
	}
}

func (h *Harness) ForgetTrust() {
	h.Resolver.Invalidate()
}

type Run struct {
	Name     string
	Workflow string
	Spec     policyv1alpha1.PolicyWorkflowRunSpec
	Status   policyv1alpha1.PolicyWorkflowRunStatus

	done   chan struct{}
	cancel context.CancelFunc
}

type ActionVerdict struct {
	Policy    string `json:"policy"`
	Allow     bool   `json:"allow"`
	FromCache bool   `json:"fromCache"`
	Mode      string `json:"mode"`
}

func (r *Run) Actions() []ActionVerdict {
	var out []ActionVerdict
	for line := range strings.SplitSeq(r.Status.ConsoleOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"policy":`) {
			continue
		}
		var verdict ActionVerdict
		if json.Unmarshal([]byte(line), &verdict) == nil {
			out = append(out, verdict)
		}
	}
	return out
}

func (r *Run) Input(name string) string {
	return r.Spec.Inputs[name]
}

func (r *Run) Output(job, name string) (string, bool) {
	raw, ok := r.Status.Outputs[job]
	if !ok {
		return "", false
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return "", false
	}
	value, ok := values[name]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%v", value), true
}

type Runner struct {
	h     *Harness
	mu    sync.Mutex
	runs  map[string]*Run
	order []string
}

var _ policy.Runner = (*Runner)(nil)

func (r *Runner) Run(ctx context.Context, req policy.RunRequest) (string, error) {
	if req.Name == "" {
		return "", errors.New("the policy run has no name")
	}
	runCtx, cancel := context.WithCancel(r.h.ctx)
	entry := &Run{Name: req.Name, Workflow: req.Workflow, Spec: *req.Spec.DeepCopy(), done: make(chan struct{}), cancel: cancel}
	r.mu.Lock()
	if _, exists := r.runs[req.Name]; exists {
		r.mu.Unlock()
		cancel()
		return "", fmt.Errorf("the policy run %s already exists", req.Name)
	}
	r.runs[req.Name] = entry
	r.order = append(r.order, req.Name)
	r.mu.Unlock()
	object := &policyv1alpha1.PolicyWorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: req.Name, UID: types.UID("policytest-" + req.Name)}, Spec: *req.Spec.DeepCopy()}
	object.Status = controller.StartingStatus(object, time.Now())
	go func() {
		defer cancel()
		outcome, err := r.h.Reconciler.ReconcilePolicyWorkflowRun(runCtx, object)
		if err != nil {
			outcome = controller.Outcome{}
		}
		status := controller.FinalStatus(object, outcome, time.Now())
		r.mu.Lock()
		entry.Status = status
		r.mu.Unlock()
		close(entry.done)
	}()
	return req.Name, nil
}

func (r *Runner) Status(ctx context.Context, name string) (policyv1alpha1.PolicyWorkflowRunStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[name]
	if !ok {
		return policyv1alpha1.PolicyWorkflowRunStatus{}, fmt.Errorf("the policy run %s is not found", name)
	}
	return entry.Status, nil
}

func (r *Runner) Wait(ctx context.Context, name string) (policyv1alpha1.PolicyWorkflowRunStatus, error) {
	r.mu.Lock()
	entry, ok := r.runs[name]
	r.mu.Unlock()
	if !ok {
		return policyv1alpha1.PolicyWorkflowRunStatus{}, fmt.Errorf("the policy run %s is not found", name)
	}
	select {
	case <-entry.done:
		return r.Status(ctx, name)
	case <-ctx.Done():
		return policyv1alpha1.PolicyWorkflowRunStatus{}, ctx.Err()
	}
}

func (r *Runner) CancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.runs {
		entry.cancel()
	}
}

func (r *Runner) Runs() []*Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Run, 0, len(r.order))
	for _, name := range r.order {
		entry := *r.runs[name]
		out = append(out, &entry)
	}
	return out
}

func (r *Runner) Get(name string) (*Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[name]
	if !ok {
		return nil, false
	}
	copied := *entry
	return &copied, true
}

func (r *Runner) Since(mark int) []*Run {
	all := r.Runs()
	if mark >= len(all) {
		return nil
	}
	return all[mark:]
}

func (r *Runner) Mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.order)
}
