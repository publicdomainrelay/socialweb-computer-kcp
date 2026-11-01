package spec

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/policytest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

func TestMain(m *testing.M) {
	path, from, err := policytest.DenoPath()
	if err != nil {
		os.Stderr.WriteString("policy spec: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stderr.WriteString("policy spec: deno " + path + " (" + from + ")\n")
	os.Exit(m.Run())
}

type lane string

const (
	scope lane = runs.ModeScope
	full  lane = runs.ModeFull
)

type side string

const (
	bidderSide    side = "bidder"
	requesterSide side = "requester"
)

type decision struct {
	verdict runs.Decision
	runs    []*policytest.Run
}

func (d decision) mustRun(t *testing.T, workflow string) *policytest.Run {
	t.Helper()
	if len(d.runs) == 0 {
		t.Fatalf("no policy run executed for %s; verdict %+v", workflow, d.verdict)
	}
	for _, run := range d.runs {
		if run.Workflow == workflow || run.Spec.WorkflowRef == workflow {
			if run.Status.Phase == "" || run.Status.CompletionTime == nil {
				t.Fatalf("the policy run %s for %s did not finish: %+v", run.Name, workflow, run.Status)
			}
			return run
		}
	}
	names := []string{}
	for _, run := range d.runs {
		names = append(names, run.Workflow)
	}
	t.Fatalf("no policy run of %s executed; runs: %v", workflow, names)
	return nil
}

func (d decision) mustAllow(t *testing.T) {
	t.Helper()
	if !d.verdict.Allow {
		t.Fatalf("want allow, got deny: %s %v", d.verdict.Reason, d.verdict.Violations)
	}
}

func (d decision) mustDeny(t *testing.T, reason string) {
	t.Helper()
	if d.verdict.Allow {
		t.Fatalf("want deny (%s), got allow from run %s", reason, d.verdict.Run)
	}
	text := d.verdict.Reason + " " + strings.Join(d.verdict.Violations, " ")
	if strings.TrimSpace(text) == "" {
		t.Fatalf("a deny carries no reason")
	}
	if reason != "" && !strings.Contains(text, reason) {
		t.Fatalf("want a deny with %q, got %q", reason, text)
	}
}

func (d decision) mustDenyBecause(t *testing.T, cause string) {
	t.Helper()
	d.mustDeny(t, "")
	text := d.verdict.Reason + " " + strings.Join(d.verdict.Violations, " ")
	for _, run := range d.runs {
		for _, raw := range run.Status.Annotations {
			text += " " + string(raw)
		}
		for _, condition := range run.Status.Conditions {
			text += " " + condition.Message
		}
		text += " " + run.Status.ConsoleOutput
	}
	if !strings.Contains(text, cause) {
		t.Fatalf("the deny does not carry the cause %q: %s", cause, text)
	}
}

func actionRan(t *testing.T, run *policytest.Run, policyName string, allow bool) policytest.ActionVerdict {
	t.Helper()
	for _, verdict := range run.Actions() {
		if verdict.Policy == policyName {
			if verdict.Allow != allow {
				t.Fatalf("the %s action answered allow=%t, want %t", policyName, verdict.Allow, allow)
			}
			return verdict
		}
	}
	t.Fatalf("the %s action did not run in %s; console:\n%s", policyName, run.Name, run.Status.ConsoleOutput)
	return policytest.ActionVerdict{}
}

type party struct {
	self         string
	counterparty string
}

func partiesOf(perspective side) party {
	if perspective == requesterSide {
		return party{self: policytest.Requester, counterparty: policytest.Bidder}
	}
	return party{self: policytest.Bidder, counterparty: policytest.Requester}
}

func evaluate(t *testing.T, h *policytest.Harness, workflow string, perspective side, mode lane, self, counterparty string, rfp *policytest.Ref) decision {
	t.Helper()
	input := runs.Input{Perspective: string(perspective), Mode: string(mode), SelfDid: self, CounterpartyDid: counterparty}
	if mode == full {
		input.SubjectDid = counterparty
		input.RootRequesterDid = counterparty
		if perspective == requesterSide {
			input.RootRequesterDid = self
		}
	}
	if rfp != nil {
		input.RFP = &commonv1.StrongRef{URI: rfp.URI, CID: rfp.CID}
	}
	return run(t, h, string(perspective), runs.Request{WorkflowRef: workflow, Input: input})
}

func run(t *testing.T, h *policytest.Harness, role string, req runs.Request) decision {
	t.Helper()
	mark := h.Runner.Mark()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	verdict := h.Client(role).Run(ctx, req)
	return decision{verdict: verdict, runs: h.Runner.Since(mark)}
}
