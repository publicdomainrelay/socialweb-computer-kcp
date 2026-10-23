package runs

import (
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
)

type Decision = policy.Verdict

type Parser struct {
	Bool func(json.RawMessage) (bool, bool)

	Strings func(json.RawMessage) []string
}

var Lenient = Parser{Bool: lenientBool, Strings: splitStrings}

var Strict = Parser{Bool: strictBool, Strings: singleString}

func Decide(run *policyv1alpha1.PolicyWorkflowRun, parser Parser) Decision {
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
	allow, ok := outputBool(run.Status.Outputs, "allow", parser.Bool)
	if !ok {
		return Decision{Reason: "the policy run produced no allow output", Run: name}
	}
	violations := outputStrings(run.Status.Outputs, "violations", parser.Strings)
	if !allow {
		reason := rejection(run.Status.Outputs)
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

func rejection(outputs map[string]json.RawMessage) string {
	var verdict executor.Verdict
	for _, raw := range outputValues(outputs, "violations") {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			raw = json.RawMessage(text)
		}
		var violations []executor.Violation
		if json.Unmarshal(raw, &violations) == nil {
			verdict.Violations = append(verdict.Violations, violations...)
		}
	}
	return executor.RejectionError(verdict)
}

func lenientBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "yes", "allow", "allowed", "success":
			return true, true
		case "false", "0", "no", "deny", "denied", "failure":
			return false, true
		default:
			return false, false
		}
	}
	var object struct {
		Allow *bool `json:"allow"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Allow != nil {
		return *object.Allow, true
	}
	return false, false
}

func strictBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func splitStrings(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		var nested []string
		if json.Unmarshal([]byte(text), &nested) == nil {
			return nested
		}
		parts := strings.Split(text, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}

func singleString(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if trimmed := strings.TrimSpace(text); trimmed != "" && trimmed != "[]" {
			return []string{trimmed}
		}
	}
	return nil
}
