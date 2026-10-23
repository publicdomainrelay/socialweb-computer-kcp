package policyrun

import (
	"encoding/json"
	"fmt"
	"strings"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Decision struct {
	Allow bool

	Violations []string

	Reason string

	Run string
}

func Terminal(phase policyv1alpha1.PolicyWorkflowRunPhase) bool {
	switch phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded, policyv1alpha1.PolicyWorkflowRunFailed, policyv1alpha1.PolicyWorkflowRunCancelled:
		return true
	default:
		return false
	}
}

func Decide(run *policyv1alpha1.PolicyWorkflowRun, boolOf func(json.RawMessage) (bool, bool), stringsOf func(json.RawMessage) []string) Decision {
	if run == nil {
		return Decision{Reason: "the policy run returned no status"}
	}
	name := run.Name
	switch run.Status.Phase {
	case policyv1alpha1.PolicyWorkflowRunSucceeded:
	case policyv1alpha1.PolicyWorkflowRunFailed:
		return Decision{Reason: failure(run), Run: name}
	case policyv1alpha1.PolicyWorkflowRunCancelled:
		return Decision{Reason: "the policy run was cancelled", Run: name}
	default:
		return Decision{Reason: fmt.Sprintf("the policy run ended in phase %q", run.Status.Phase), Run: name}
	}
	if run.Status.ExitStatus != "" && run.Status.ExitStatus != policyv1alpha1.PolicyExitStatusSuccess {
		return Decision{Reason: "the policy workflow failed", Run: name}
	}
	allow, ok := outputBool(run.Status.Outputs, "allow", boolOf)
	if !ok {
		return Decision{Reason: "the policy run produced no allow output", Run: name}
	}
	violations := outputStrings(run.Status.Outputs, "violations", stringsOf)
	if !allow {
		reason := "policy rejected"
		if len(violations) > 0 {
			reason += ": " + strings.Join(violations, ", ")
		}
		return Decision{Violations: violations, Reason: reason, Run: name}
	}
	return Decision{Allow: true, Violations: violations, Run: name}
}

func failure(run *policyv1alpha1.PolicyWorkflowRun) string {
	for _, condition := range run.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Message != "" {
			return condition.Message
		}
	}
	if raw, ok := run.Status.Annotations["error"]; ok && len(raw) > 0 {
		if message := annotationMessage(raw); message != "" {
			return message
		}
		return strings.TrimSpace(string(raw))
	}
	return "the policy run failed"
}

func annotationMessage(raw json.RawMessage) string {
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	items := []map[string]any{}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Items) > 0 {
		items = envelope.Items
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	for _, item := range items {
		for _, key := range []string{"message", "title", "msg"} {
			if text, ok := item[key].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

func outputBool(outputs map[string]json.RawMessage, key string, boolOf func(json.RawMessage) (bool, bool)) (bool, bool) {
	values := outputValues(outputs, key)
	found := false
	allow := true
	for _, raw := range values {
		value, ok := boolOf(raw)
		if !ok {
			continue
		}
		found = true
		if !value {
			allow = false
		}
	}
	if !found {
		return false, false
	}
	return allow, true
}

func outputStrings(outputs map[string]json.RawMessage, key string, stringsOf func(json.RawMessage) []string) []string {
	var out []string
	for _, raw := range outputValues(outputs, key) {
		out = append(out, stringsOf(raw)...)
	}
	return out
}

func outputValues(outputs map[string]json.RawMessage, key string) []json.RawMessage {
	values := make([]json.RawMessage, 0, len(outputs))
	if raw, ok := outputs[key]; ok && len(raw) > 0 {
		values = append(values, raw)
	}
	for name, raw := range outputs {
		if name == key || len(raw) == 0 {
			continue
		}
		var job map[string]json.RawMessage
		if json.Unmarshal(raw, &job) != nil {
			continue
		}
		if value, ok := job[key]; ok && len(value) > 0 {
			values = append(values, value)
		}
	}
	return values
}
