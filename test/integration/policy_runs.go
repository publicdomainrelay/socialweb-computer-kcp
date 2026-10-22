//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
)

const (
	policyEngineName     = "policy-engine"
	policyEngineTimeout  = 2 * time.Minute
	policyRunTimeout     = 2 * time.Minute
	policyPollInterval   = 250 * time.Millisecond
	policyProcessTimeout = 60 * time.Second
)

var (
	policyEngineGVR   = schema.GroupVersionResource{Group: policyv1alpha1.GroupName, Version: policyv1alpha1.Version, Resource: "policyengines"}
	policyWorkflowGVR = schema.GroupVersionResource{Group: policyv1alpha1.GroupName, Version: policyv1alpha1.Version, Resource: "policyworkflows"}
	policyRunGVR      = schema.GroupVersionResource{Group: policyv1alpha1.GroupName, Version: policyv1alpha1.Version, Resource: "policyworkflowruns"}
)

type policyCluster struct {
	n         *Network
	workspace string
	dyn       dynamic.Interface
}

func newPolicyCluster(t testing.TB, n *Network) *policyCluster {
	t.Helper()
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		t.Fatalf("reading the admin kubeconfig: %v", err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		t.Fatalf("the admin kubeconfig: %v", err)
	}
	workspace := "root:" + n.bidderName(0)
	cfg.Host = strings.TrimSuffix(strings.SplitN(cfg.Host, "/clusters/", 2)[0], "/") + "/clusters/" + workspace
	cfg.Timeout = 20 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("the client of %s: %v", workspace, err)
	}
	return &policyCluster{n: n, workspace: workspace, dyn: dyn}
}

func (p *policyCluster) waitEngine(t testing.TB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastHealth := time.Now()
	var last string
	for {
		u, err := p.dyn.Resource(policyEngineGVR).Get(context.Background(), policyEngineName, metav1.GetOptions{})
		if err == nil {
			ready, _, _ := unstructured.NestedBool(u.Object, "status", "ready")
			if ready {
				return
			}
			phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
			last = "phase " + phase + ", ready false"
		} else {
			last = err.Error()
		}
		if time.Since(lastHealth) > 5*time.Second {
			lastHealth = time.Now()
			if err := p.n.health(); err != nil {
				t.Fatalf("the market network failed while waiting for the policy engine: %v", err)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the PolicyEngine %s in %s was not ready within %s (%s)", policyEngineName, p.workspace, timeout, last)
		}
		time.Sleep(policyPollInterval)
	}
}

func (p *policyCluster) create(t testing.TB, gvr schema.GroupVersionResource, kind string, object any) {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encoding %s: %v", kind, err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("encoding %s: %v", kind, err)
	}
	if _, ok := body["apiVersion"]; !ok {
		body["apiVersion"] = policyv1alpha1.GroupVersion.String()
	}
	if _, ok := body["kind"]; !ok {
		body["kind"] = kind
	}
	if _, err := p.dyn.Resource(gvr).Create(context.Background(), &unstructured.Unstructured{Object: body}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating %s %v: %v", kind, body["metadata"], err)
	}
}

func (p *policyCluster) createWorkflow(t testing.TB, workflow *policyv1alpha1.PolicyWorkflow) {
	t.Helper()
	p.create(t, policyWorkflowGVR, "PolicyWorkflow", workflow)
	p.waitWorkflow(t, workflow.Name, 30*time.Second)
}

func (p *policyCluster) waitWorkflow(t testing.TB, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		_, err := p.dyn.Resource(policyWorkflowGVR).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			return
		}
		lastErr = err
		if !apierrors.IsNotFound(err) {
			t.Fatalf("reading PolicyWorkflow %s: %v", name, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("PolicyWorkflow %s was not visible within %s: %v", name, timeout, lastErr)
		}
		time.Sleep(policyPollInterval)
	}
}

func (p *policyCluster) deleteWorkflow(t testing.TB, name string) {
	t.Helper()
	if err := p.dyn.Resource(policyWorkflowGVR).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting PolicyWorkflow %s: %v", name, err)
	}
}

func (p *policyCluster) createRun(t testing.TB, run *policyv1alpha1.PolicyWorkflowRun) {
	t.Helper()
	p.create(t, policyRunGVR, "PolicyWorkflowRun", run)
}

func (p *policyCluster) getRun(name string) (*policyv1alpha1.PolicyWorkflowRun, error) {
	u, err := p.dyn.Resource(policyRunGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	var run policyv1alpha1.PolicyWorkflowRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (p *policyCluster) waitRunPhase(t testing.TB, name string, timeout time.Duration, phases ...policyv1alpha1.PolicyWorkflowRunPhase) *policyv1alpha1.PolicyWorkflowRun {
	t.Helper()
	want := make(map[policyv1alpha1.PolicyWorkflowRunPhase]bool, len(phases))
	for _, phase := range phases {
		want[phase] = true
	}
	deadline := time.Now().Add(timeout)
	lastHealth := time.Now()
	var last *policyv1alpha1.PolicyWorkflowRun
	var lastErr error
	for {
		run, err := p.getRun(name)
		if err == nil {
			last, lastErr = run, nil
			if want[run.Status.Phase] {
				return run
			}
		} else {
			lastErr = err
		}
		if time.Since(lastHealth) > 5*time.Second {
			lastHealth = time.Now()
			if err := p.n.health(); err != nil {
				t.Fatalf("the market network failed while waiting for policy run %s: %v", name, err)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy run %s did not reach %v within %s (last %s, err %v)", name, phases, timeout, describePolicyRun(last), lastErr)
		}
		time.Sleep(policyPollInterval)
	}
}

func (p *policyCluster) waitRunGone(t testing.TB, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := p.getRun(name)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reading policy run %s: %v", name, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy run %s was not deleted within %s", name, timeout)
		}
		time.Sleep(policyPollInterval)
	}
}

func (p *policyCluster) cancelRun(t testing.TB, name string) {
	t.Helper()
	patch := []byte(`{"spec":{"cancel":true}}`)
	if _, err := p.dyn.Resource(policyRunGVR).Patch(context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("cancelling policy run %s: %v", name, err)
	}
}

func (p *policyCluster) deleteRun(t testing.TB, name string) {
	t.Helper()
	if err := p.dyn.Resource(policyRunGVR).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting policy run %s: %v", name, err)
	}
}

func describePolicyRun(run *policyv1alpha1.PolicyWorkflowRun) string {
	if run == nil {
		return "no run"
	}
	values := []string{"phase=" + string(run.Status.Phase)}
	for _, condition := range run.Status.Conditions {
		values = append(values, fmt.Sprintf("%s=%s/%s", condition.Type, condition.Status, condition.Reason))
	}
	if message := policyRunMessage(run); message != "" {
		values = append(values, "message="+message)
	}
	return strings.Join(values, " ")
}

func policyRunMessage(run *policyv1alpha1.PolicyWorkflowRun) string {
	for _, condition := range run.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Message != "" {
			return condition.Message
		}
	}
	if raw, ok := run.Status.Annotations["error"]; ok && len(raw) > 0 {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func policyRunCondition(run *policyv1alpha1.PolicyWorkflowRun, conditionType string) (metav1.Condition, bool) {
	for _, condition := range run.Status.Conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

func policyOutputs(run *policyv1alpha1.PolicyWorkflowRun, job string) (map[string]any, error) {
	raw, ok := run.Status.Outputs[job]
	if !ok {
		return nil, fmt.Errorf("policy run has no %s output", job)
	}
	var outputs map[string]any
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return nil, fmt.Errorf("decoding the %s output: %w", job, err)
	}
	return outputs, nil
}

func policyCacheKeys(run *policyv1alpha1.PolicyWorkflowRun, prefix string) []string {
	var keys []string
	for key := range run.Status.Cache {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys
}

func policyDenoPIDs(t testing.TB, n *Network) map[int]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := n.h.As(ctx, "ps", "-eo", "pid=,ppid=,args=")
	if r.Err != nil {
		t.Fatalf("listing processes: %v", r.Err)
	}
	args := map[int]string{}
	children := map[int][]int{}
	roots := map[int]bool{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		command := strings.Join(fields[2:], " ")
		args[pid] = command
		children[ppid] = append(children[ppid], pid)
		if strings.Contains(command, "internal dev-core") && strings.Contains(command, n.StateDir) {
			roots[pid] = true
		}
	}
	seen := map[int]bool{}
	queue := make([]int, 0, len(roots))
	for pid := range roots {
		queue = append(queue, pid)
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		queue = append(queue, children[pid]...)
	}
	out := map[int]bool{}
	for pid := range seen {
		command := args[pid]
		if strings.Contains(command, "deno") && strings.Contains(command, "--no-prompt") &&
			strings.Contains(command, "--no-config") && strings.Contains(command, "--allow-net") {
			out[pid] = true
		}
	}
	return out
}

func waitNewPolicyProcess(t testing.TB, n *Network, baseline map[int]bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for pid := range policyDenoPIDs(t, n) {
			if !baseline[pid] {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Deno process of this market network started within %s", timeout)
		}
		time.Sleep(policyPollInterval)
	}
}

func waitNoNewPolicyProcess(t testing.TB, n *Network, baseline map[int]bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var left []int
	for {
		left = left[:0]
		for pid := range policyDenoPIDs(t, n) {
			if !baseline[pid] {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Deno processes of this market network stayed after the run ended: %v", left)
		}
		time.Sleep(policyPollInterval)
	}
}

func policyWorkflowObject(name string, engineRef string, workflow map[string]any) *policyv1alpha1.PolicyWorkflow {
	raw, _ := json.Marshal(workflow)
	return &policyv1alpha1.PolicyWorkflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: policyv1alpha1.GroupVersion.String(), Kind: "PolicyWorkflow"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: policyv1alpha1.PolicyWorkflowSpec{
			EngineRef: engineRef,
			Workflow:  raw,
		},
	}
}

func policyRunObject(name string, workflowRef string, spec policyv1alpha1.PolicyWorkflowRunSpec) *policyv1alpha1.PolicyWorkflowRun {
	spec.EngineRef = policyEngineName
	spec.WorkflowRef = workflowRef
	return &policyv1alpha1.PolicyWorkflowRun{
		TypeMeta:   metav1.TypeMeta{APIVersion: policyv1alpha1.GroupVersion.String(), Kind: "PolicyWorkflowRun"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

func blockingPolicyWorkflow(steps int) map[string]any {
	list := make([]any, 0, steps)
	for i := 0; i < steps; i++ {
		list = append(list, map[string]any{
			"uses": "tangy/policy-open@v1",
			"id":   fmt.Sprintf("block%d", i),
			"with": map[string]any{"self-did": "${{ inputs.self-did }}"},
		})
	}
	return map[string]any{
		"name": "matrix-block",
		"on":   map[string]any{"workflow_dispatch": map[string]any{}},
		"jobs": map[string]any{
			"block": map[string]any{
				"runs-on": "self-hosted",
				"steps":   list,
			},
		},
	}
}

func echoPolicyWorkflow() map[string]any {
	return map[string]any{
		"name": "matrix-echo",
		"on":   map[string]any{"workflow_dispatch": map[string]any{}},
		"jobs": map[string]any{
			"echo": map[string]any{
				"runs-on": "self-hosted",
				"outputs": map[string]any{
					"args":        "${{ inputs.policy-args }}",
					"perspective": "${{ inputs.perspective }}",
					"self":        "${{ inputs.self-did }}",
				},
			},
		},
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func uniquePolicyName(prefix string) string {
	return strings.ToLower(prefix) + "-" + randHex(4)
}

type policyMatrixCase struct {
	Name            string
	Perspective     string
	SelfDid         string
	CounterpartyDid string
	SubjectDid      string
	RootRequester   string
	Args            map[string]string
}

func policyMatrixCases() []policyMatrixCase {
	bidder := "did:plc:itbidderpolicy00000000"
	requester := "did:plc:itrequesterpolicy0000"
	make := func(name, perspective string) policyMatrixCase {
		self, other := bidder, requester
		if perspective == "requester" {
			self, other = requester, bidder
		}
		root := self
		if perspective == "bidder" {
			root = other
		}
		return policyMatrixCase{
			Name:            name,
			Perspective:     perspective,
			SelfDid:         self,
			CounterpartyDid: other,
			SubjectDid:      other,
			RootRequester:   root,
			Args:            map[string]string{"matrixCase": name},
		}
	}
	return []policyMatrixCase{
		make("bidder-mutuals", "bidder"),
		make("bidder-only-me", "bidder"),
		make("bidder-tangled-vouch", "bidder"),
		make("bid-payload", "requester"),
		make("open", "bidder"),
		make("requester-mutuals", "requester"),
		make("requester-only-me", "requester"),
		make("requester-tangled-vouch", "requester"),
		make("under-4-cpus", "bidder"),
	}
}

func policyRecordInput(t testing.TB, did, collection, rkey, payloadCollection string) string {
	t.Helper()
	value := map[string]any{
		"$type":     collection,
		"createdAt": "2026-10-21T00:00:00.000Z",
	}
	if payloadCollection != "" {
		value["payload"] = map[string]any{
			"uri": "at://" + did + "/" + payloadCollection + "/payload-" + rkey,
			"cid": "bafyreimatrixpayload",
		}
	}
	record := map[string]any{
		"uri":   "at://" + did + "/" + collection + "/" + rkey,
		"cid":   "bafyreimatrix" + rkey,
		"value": value,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encoding the %s record: %v", collection, err)
	}
	return string(data)
}
