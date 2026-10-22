//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
)

const (
	matrixRFPCollection    = "com.publicdomainrelay.temp.market.rfp"
	matrixBidCollection    = "com.publicdomainrelay.temp.market.bid"
	matrixAcceptCollection = "com.publicdomainrelay.temp.market.accept"
)

func policyTestNetwork(t testing.TB) *Network {
	t.Helper()
	return SharedNetwork(t, NetworkOptions{Bidders: 1, UpTimeout: 5 * time.Minute})
}

func matrixRunInputs(t testing.TB, c policyMatrixCase) map[string]string {
	t.Helper()
	rfpAuthor, bidAuthor := c.CounterpartyDid, c.SelfDid
	if c.Perspective == "requester" {
		rfpAuthor, bidAuthor = c.SelfDid, c.CounterpartyDid
	}
	return map[string]string{
		"mode":               "full",
		"self-did":           c.SelfDid,
		"perspective":        c.Perspective,
		"subject-did":        c.SubjectDid,
		"counterparty-did":   c.CounterpartyDid,
		"root-requester-did": c.RootRequester,
		"rfp":                policyRecordInput(t, rfpAuthor, matrixRFPCollection, "rfp-"+c.Name, ""),
		"bid":                policyRecordInput(t, bidAuthor, matrixBidCollection, "bid-"+c.Name, ""),
		"accept":             policyRecordInput(t, c.SelfDid, matrixAcceptCollection, "accept-"+c.Name, ""),
	}
}

func matrixRunSpec(c policyMatrixCase) policyv1alpha1.PolicyWorkflowRunSpec {
	return policyv1alpha1.PolicyWorkflowRunSpec{
		Perspective:             c.Perspective,
		SelfDid:                 c.SelfDid,
		SubjectDid:              c.SubjectDid,
		CounterpartyDid:         c.CounterpartyDid,
		RootRequesterDid:        c.RootRequester,
		Args:                    c.Args,
		Inputs:                  nil,
		ActiveDeadlineSeconds:   int64Ptr(120),
		TTLSecondsAfterFinished: int64Ptr(600),
	}
}

func assertSucceeded(t testing.TB, run *policyv1alpha1.PolicyWorkflowRun, job string) map[string]any {
	t.Helper()
	if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunSucceeded {
		t.Fatalf("policy run %s ended in phase %s, want Succeeded (%s)", run.Name, run.Status.Phase, describePolicyRun(run))
	}
	if run.Status.ExitStatus != policyv1alpha1.PolicyExitStatusSuccess {
		t.Fatalf("policy run %s has exit status %q, want success", run.Name, run.Status.ExitStatus)
	}
	condition, ok := policyRunCondition(run, "Complete")
	if !ok || condition.Status != metav1.ConditionTrue {
		t.Fatalf("policy run %s has no Complete=True condition (%s)", run.Name, describePolicyRun(run))
	}
	outputs, err := policyOutputs(run, job)
	if err != nil {
		t.Fatalf("policy run %s: %v (outputs %v)", run.Name, err, run.Status.Outputs)
	}
	if _, ok := outputs["allow"]; !ok {
		t.Fatalf("policy run %s has no allow output (outputs %v)", run.Name, outputs)
	}
	allow := fmt.Sprint(outputs["allow"])
	if allow != "true" && allow != "false" {
		t.Fatalf("policy run %s has allow %q, want true or false", run.Name, allow)
	}
	raw, ok := outputs["violations"]
	if !ok {
		t.Fatalf("policy run %s has no violations output (outputs %v)", run.Name, outputs)
	}
	var violations []any
	if err := json.Unmarshal([]byte(fmt.Sprint(raw)), &violations); err != nil {
		t.Fatalf("policy run %s has violations %q that are not a JSON array: %v", run.Name, fmt.Sprint(raw), err)
	}
	if strings.TrimSpace(run.Status.ConsoleOutput) == "" {
		t.Fatalf("policy run %s captured no console output", run.Name)
	}
	return outputs
}

func TestPolicyWorkflowRunsBundledMatrix(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)
	for _, c := range policyMatrixCases() {
		t.Run(c.Name, func(t *testing.T) {
			name := uniquePolicyName("it-" + c.Name)
			spec := matrixRunSpec(c)
			spec.Inputs = matrixRunInputs(t, c)
			p.createRun(t, policyRunObject(name, c.Name, spec))
			run := p.waitRunPhase(t, name, policyRunTimeout,
				policyv1alpha1.PolicyWorkflowRunSucceeded,
				policyv1alpha1.PolicyWorkflowRunFailed,
				policyv1alpha1.PolicyWorkflowRunCancelled)
			assertSucceeded(t, run, "evaluate")
			if !strings.Contains(run.Status.ConsoleOutput, `"policy":"`+c.Name+`"`) {
				t.Fatalf("the console output of %s names no %s verdict: %q", name, c.Name, run.Status.ConsoleOutput)
			}
			keys := policyCacheKeys(run, "policy/"+c.Name+"/")
			if len(keys) == 0 {
				t.Fatalf("policy run %s persisted no policy/%s cache entry (cache keys %v)", name, c.Name, cacheKeyList(run))
			}
			for _, key := range keys {
				entry := run.Status.Cache[key]
				if _, ok := entry["result.json"]; !ok {
					t.Fatalf("policy run %s cache entry %s has no result.json", name, key)
				}
			}
			t.Logf("%s: phase %s, allow %v, cache %v", c.Name, run.Status.Phase, mustOutput(run, "evaluate", "allow"), keys)
		})
	}
}

func TestPolicyWorkflowRunCacheRoundTrip(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)
	c := policyMatrixCase{
		Name:            "open",
		Perspective:     "bidder",
		SelfDid:         "did:plc:itcachebidder0000000000",
		CounterpartyDid: "did:plc:itcacherequester000000",
		SubjectDid:      "did:plc:itcacherequester000000",
		RootRequester:   "did:plc:itcacherequester000000",
		Args:            map[string]string{"cacheRoundTrip": uniquePolicyName("marker")},
	}
	spec := matrixRunSpec(c)
	spec.Inputs = matrixRunInputs(t, c)
	firstName := uniquePolicyName("it-cache-first")
	p.createRun(t, policyRunObject(firstName, c.Name, spec))
	first := p.waitRunPhase(t, firstName, policyRunTimeout,
		policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed)
	assertSucceeded(t, first, "evaluate")
	keys := policyCacheKeys(first, "policy/"+c.Name+"/")
	if len(keys) == 0 {
		t.Fatalf("the first run persisted no policy/open cache entry (cache keys %v)", cacheKeyList(first))
	}
	secondName := uniquePolicyName("it-cache-second")
	secondSpec := spec
	secondSpec.Cache = first.Status.Cache
	p.createRun(t, policyRunObject(secondName, c.Name, secondSpec))
	second := p.waitRunPhase(t, secondName, policyRunTimeout,
		policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed)
	assertSucceeded(t, second, "evaluate")
	if !strings.Contains(second.Status.ConsoleOutput, "policy cache hit") {
		t.Fatalf("the second run did not reuse the seeded cache: %q", second.Status.ConsoleOutput)
	}
	for _, key := range keys {
		before, ok := first.Status.Cache[key]
		if !ok {
			t.Fatalf("the first run lost its cache entry %s", key)
		}
		after, ok := second.Status.Cache[key]
		if !ok {
			t.Fatalf("the second run did not round-trip the cache entry %s (cache keys %v)", key, cacheKeyList(second))
		}
		if before["result.json"].Data != after["result.json"].Data {
			t.Fatalf("cache entry %s changed across the round trip: %q then %q", key, before["result.json"].Data, after["result.json"].Data)
		}
	}
}

func TestPolicyWorkflowRunInputsArgsPerspective(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)
	workflowName := uniquePolicyName("it-echo-workflow")
	p.createWorkflow(t, policyWorkflowObject(workflowName, policyEngineName, echoPolicyWorkflow()))
	selfDid := "did:plc:itechobidder000000000000"
	spec := policyv1alpha1.PolicyWorkflowRunSpec{
		Perspective:             "requester",
		SelfDid:                 selfDid,
		SubjectDid:              "did:plc:itechocounterparty0000",
		CounterpartyDid:         "did:plc:itechocounterparty0000",
		RootRequesterDid:        selfDid,
		Args:                    map[string]string{"alpha": "one", "beta": "two"},
		ActiveDeadlineSeconds:   int64Ptr(60),
		TTLSecondsAfterFinished: int64Ptr(600),
	}
	name := uniquePolicyName("it-echo-run")
	p.createRun(t, policyRunObject(name, workflowName, spec))
	run := p.waitRunPhase(t, name, policyRunTimeout,
		policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed)
	if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunSucceeded {
		t.Fatalf("the echo run ended in phase %s (%s)", run.Status.Phase, describePolicyRun(run))
	}
	outputs, err := policyOutputs(run, "echo")
	if err != nil {
		t.Fatalf("%v (outputs %v)", err, run.Status.Outputs)
	}
	if got := fmt.Sprint(outputs["args"]); got != `{"alpha":"one","beta":"two"}` {
		t.Fatalf("args reached the workflow as %q", got)
	}
	if got := fmt.Sprint(outputs["perspective"]); got != "requester" {
		t.Fatalf("perspective reached the workflow as %q", got)
	}
	if got := fmt.Sprint(outputs["self"]); got != selfDid {
		t.Fatalf("self-did reached the workflow as %q, want %s", got, selfDid)
	}
}

func TestPolicyWorkflowRunCancellationAndDeadline(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)

	t.Run("spec-cancel", func(t *testing.T) {
		baseline := policyDenoPIDs(t, n)
		name := uniquePolicyName("it-cancel")
		raw, err := json.Marshal(blockingPolicyWorkflow(800))
		if err != nil {
			t.Fatal(err)
		}
		spec := policyv1alpha1.PolicyWorkflowRunSpec{
			SelfDid:                 "did:plc:itcancel0000000000000000",
			Inputs:                  map[string]string{"self-did": "did:plc:itcancel0000000000000000"},
			Workflow:                raw,
			ActiveDeadlineSeconds:   int64Ptr(600),
			TTLSecondsAfterFinished: int64Ptr(600),
		}
		p.createRun(t, policyRunObject(name, "", spec))
		p.waitRunPhase(t, name, policyRunTimeout, policyv1alpha1.PolicyWorkflowRunRunning)
		waitNewPolicyProcess(t, n, baseline, policyProcessTimeout)
		p.cancelRun(t, name)
		run := p.waitRunPhase(t, name, policyRunTimeout,
			policyv1alpha1.PolicyWorkflowRunCancelled,
			policyv1alpha1.PolicyWorkflowRunSucceeded,
			policyv1alpha1.PolicyWorkflowRunFailed)
		if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunCancelled {
			t.Fatalf("the run ended in phase %s after spec.cancel (%s)", run.Status.Phase, describePolicyRun(run))
		}
		condition, ok := policyRunCondition(run, "Cancelled")
		if !ok || condition.Status != metav1.ConditionTrue {
			t.Fatalf("the cancelled run has no Cancelled=True condition (%s)", describePolicyRun(run))
		}
		waitNoNewPolicyProcess(t, n, baseline, policyProcessTimeout)
	})

	t.Run("deadline", func(t *testing.T) {
		baseline := policyDenoPIDs(t, n)
		name := uniquePolicyName("it-deadline")
		raw, err := json.Marshal(blockingPolicyWorkflow(800))
		if err != nil {
			t.Fatal(err)
		}
		spec := policyv1alpha1.PolicyWorkflowRunSpec{
			SelfDid:                 "did:plc:itdeadline00000000000000",
			Inputs:                  map[string]string{"self-did": "did:plc:itdeadline00000000000000"},
			Workflow:                raw,
			ActiveDeadlineSeconds:   int64Ptr(1),
			TTLSecondsAfterFinished: int64Ptr(600),
		}
		p.createRun(t, policyRunObject(name, "", spec))
		run := p.waitRunPhase(t, name, policyRunTimeout,
			policyv1alpha1.PolicyWorkflowRunFailed,
			policyv1alpha1.PolicyWorkflowRunSucceeded)
		if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunFailed {
			t.Fatalf("the run ended in phase %s, want Failed at its deadline (%s)", run.Status.Phase, describePolicyRun(run))
		}
		condition, ok := policyRunCondition(run, "Failed")
		if !ok || condition.Status != metav1.ConditionTrue || condition.Reason != "DeadlineExceeded" {
			t.Fatalf("the run has no Failed/DeadlineExceeded condition (%s)", describePolicyRun(run))
		}
		waitNoNewPolicyProcess(t, n, baseline, policyProcessTimeout)
	})

	t.Run("deletion", func(t *testing.T) {
		baseline := policyDenoPIDs(t, n)
		name := uniquePolicyName("it-delete")
		raw, err := json.Marshal(blockingPolicyWorkflow(800))
		if err != nil {
			t.Fatal(err)
		}
		spec := policyv1alpha1.PolicyWorkflowRunSpec{
			SelfDid:                 "did:plc:itdelete0000000000000000",
			Inputs:                  map[string]string{"self-did": "did:plc:itdelete0000000000000000"},
			Workflow:                raw,
			ActiveDeadlineSeconds:   int64Ptr(600),
			TTLSecondsAfterFinished: int64Ptr(600),
		}
		p.createRun(t, policyRunObject(name, "", spec))
		p.waitRunPhase(t, name, policyRunTimeout, policyv1alpha1.PolicyWorkflowRunRunning)
		waitNewPolicyProcess(t, n, baseline, policyProcessTimeout)
		p.deleteRun(t, name)
		p.waitRunGone(t, name, 30*time.Second)
		waitNoNewPolicyProcess(t, n, baseline, policyProcessTimeout)
	})
}

func TestPolicyWorkflowRunFailsClosedOnRecordTypes(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)
	cases := []struct {
		name       string
		recordType string
		want       string
	}{
		{"typescript", "computer.socialweb.temp.policy.typescript", "typescript policy records are disabled"},
		{"deno-worker", "com.publicdomainrelay.temp.market.policies.denoWorker", "denoWorker policy records are disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			baseline := policyDenoPIDs(t, n)
			workflowName := uniquePolicyName("it-fail-" + c.name)
			cid := "bafyfailclosed" + strings.ReplaceAll(c.name, "-", "")
			uri := "at://did:example:matrix/" + c.recordType + "/" + c.name
			workflow := policyWorkflowObject(workflowName, policyEngineName, map[string]any{})
			workflow.Spec.RecordRef = &policyv1alpha1.PolicyStrongRef{URI: uri, CID: cid}
			p.createWorkflow(t, workflow)
			t.Cleanup(func() { p.deleteWorkflow(t, workflowName) })
			name := uniquePolicyName("it-fail-run-" + c.name)
			spec := policyv1alpha1.PolicyWorkflowRunSpec{
				SelfDid: "did:plc:itfailclosed000000000000",
				Cache: policyv1alpha1.PolicyCache{
					"rec/" + uri + "/" + cid: policyv1alpha1.PolicyCacheEntry{
						"value.json": policyv1alpha1.PolicyCacheFile{
							Data:     `{"$type":"` + c.recordType + `"}`,
							Encoding: policyv1alpha1.PolicyCacheEncodingText,
						},
					},
				},
				ActiveDeadlineSeconds:   int64Ptr(60),
				TTLSecondsAfterFinished: int64Ptr(600),
			}
			p.createRun(t, policyRunObject(name, workflowName, spec))
			run := p.waitRunPhase(t, name, policyRunTimeout,
				policyv1alpha1.PolicyWorkflowRunFailed, policyv1alpha1.PolicyWorkflowRunSucceeded)
			if run.Status.Phase != policyv1alpha1.PolicyWorkflowRunFailed {
				t.Fatalf("the %s record run ended in phase %s (%s)", c.name, run.Status.Phase, describePolicyRun(run))
			}
			message := policyRunMessage(run)
			if !strings.Contains(message, c.want) {
				t.Fatalf("the %s record run failed without the refusal: %q", c.name, message)
			}
			waitNoNewPolicyProcess(t, n, baseline, 10*time.Second)
		})
	}
}

func TestPolicyWorkflowRunTTLDeletesFinishedRun(t *testing.T) {
	n := policyTestNetwork(t)
	p := newPolicyCluster(t, n)
	p.waitEngine(t, policyEngineTimeout)
	c := policyMatrixCase{
		Name:            "open",
		Perspective:     "bidder",
		SelfDid:         "did:plc:ittlbidder00000000000000",
		CounterpartyDid: "did:plc:ittlrequester0000000000",
		SubjectDid:      "did:plc:ittlrequester0000000000",
		RootRequester:   "did:plc:ittlrequester0000000000",
		Args:            map[string]string{"ttl": uniquePolicyName("marker")},
	}
	name := uniquePolicyName("it-ttl")
	spec := matrixRunSpec(c)
	spec.Inputs = matrixRunInputs(t, c)
	spec.TTLSecondsAfterFinished = int64Ptr(2)
	p.createRun(t, policyRunObject(name, c.Name, spec))
	run := p.waitRunPhase(t, name, policyRunTimeout,
		policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed)
	assertSucceeded(t, run, "evaluate")
	p.waitRunGone(t, name, 60*time.Second)
}

func cacheKeyList(run *policyv1alpha1.PolicyWorkflowRun) []string {
	keys := make([]string, 0, len(run.Status.Cache))
	for key := range run.Status.Cache {
		keys = append(keys, key)
	}
	return keys
}

func mustOutput(run *policyv1alpha1.PolicyWorkflowRun, job, key string) any {
	outputs, err := policyOutputs(run, job)
	if err != nil {
		return nil
	}
	return outputs[key]
}
