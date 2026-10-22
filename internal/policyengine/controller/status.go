package controller

import (
	"bytes"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

func CacheFromAPI(value policyv1alpha1.PolicyCache) models.Cache {
	if value == nil {
		return models.Cache{}
	}
	out := make(models.Cache, len(value))
	for key, entry := range value {
		files := make(models.CacheEntry, len(entry))
		for path, file := range entry {
			encoding := models.CacheEncodingText
			if file.Encoding == policyv1alpha1.PolicyCacheEncodingBase64 {
				encoding = models.CacheEncodingBase64
			}
			files[path] = models.CacheFile{Data: file.Data, Encoding: encoding}
		}
		out[key] = files
	}
	return out
}

func CacheToAPI(value models.Cache) policyv1alpha1.PolicyCache {
	if value == nil {
		return nil
	}
	out := make(policyv1alpha1.PolicyCache, len(value))
	for key, entry := range value {
		files := make(policyv1alpha1.PolicyCacheEntry, len(entry))
		for path, file := range entry {
			encoding := policyv1alpha1.PolicyCacheEncodingText
			if file.Encoding == models.CacheEncodingBase64 {
				encoding = policyv1alpha1.PolicyCacheEncodingBase64
			}
			files[path] = policyv1alpha1.PolicyCacheFile{Data: file.Data, Encoding: encoding}
		}
		out[key] = files
	}
	return out
}

func RunStatusFromOutcome(outcome Outcome) policyv1alpha1.PolicyWorkflowRunStatus {
	value := policyv1alpha1.PolicyWorkflowRunStatus{
		Phase:          runPhase(outcome.Status.Phase),
		RunID:          outcome.Status.RunID,
		ExitStatus:     exitStatus(outcome.Status.ExitStatus),
		Cache:          CacheToAPI(outcome.Status.Cache),
		ConsoleOutput:  outcome.Status.ConsoleOutput,
		Restarts:       outcome.Status.Restarts,
		StartTime:      outcome.Status.StartedAt,
		CompletionTime: outcome.Status.CompletedAt,
		Conditions:     append([]metav1.Condition(nil), outcome.Status.Conditions...),
	}
	if len(outcome.Status.Outputs) > 0 {
		value.Outputs = make(map[string]json.RawMessage, len(outcome.Status.Outputs))
		for key, item := range outcome.Status.Outputs {
			if data, err := json.Marshal(item); err == nil {
				value.Outputs[key] = data
			}
		}
	}
	if len(outcome.Status.Annotations) > 0 {
		value.Annotations = annotationsToAPI(outcome.Status.Annotations)
	}
	return value
}

func annotationsToAPI(value models.Annotations) map[string]json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(value))
	for key, item := range value {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 {
			continue
		}
		switch trimmed[0] {
		case '[':
			wrapped, err := json.Marshal(map[string]json.RawMessage{"items": json.RawMessage(trimmed)})
			if err != nil {
				continue
			}
			out[key] = wrapped
		case '{':
			out[key] = append(json.RawMessage(nil), trimmed...)
		default:
			wrapped, err := json.Marshal(map[string]json.RawMessage{"value": json.RawMessage(trimmed)})
			if err != nil {
				continue
			}
			out[key] = wrapped
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runPhase(value policyengine.Phase) policyv1alpha1.PolicyWorkflowRunPhase {
	switch value {
	case policyengine.PhaseSucceeded:
		return policyv1alpha1.PolicyWorkflowRunSucceeded
	case policyengine.PhaseFailed:
		return policyv1alpha1.PolicyWorkflowRunFailed
	case policyengine.PhaseCancelled:
		return policyv1alpha1.PolicyWorkflowRunCancelled
	case policyengine.PhaseRunning:
		return policyv1alpha1.PolicyWorkflowRunRunning
	default:
		return policyv1alpha1.PolicyWorkflowRunPending
	}
}

func exitStatus(value models.ExitStatus) policyv1alpha1.PolicyExitStatus {
	if value == models.ExitSuccess {
		return policyv1alpha1.PolicyExitStatusSuccess
	}
	if value == models.ExitFailure {
		return policyv1alpha1.PolicyExitStatusFailure
	}
	return ""
}
