package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

const (
	DefaultScopeTTL = 30 * time.Second

	ScopeModeInput = "mode"

	ScopeMode = "scope"

	ReasonCacheServed = "CacheServed"

	AnnotationCacheServed = "cache-served"
)

func scopeMemoKey(workflow json.RawMessage, inputs map[string]any) string {
	if stringInput(inputs, ScopeModeInput) != ScopeMode {
		return ""
	}
	key := stringInput(inputs, CacheKeyInput)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256(workflow)
	return "scope-verdict/" + hex.EncodeToString(sum[:8]) + "/" + key
}

func (e *Engine) scopeTTL(allow bool) time.Duration {
	ttl := opt.Positive(e.config.ScopeTTL, DefaultScopeTTL)
	if allow {
		return ttl
	}
	return opt.Positive(e.config.NegativeScopeTTL, ttl)
}

func (e *Engine) servedScope(key string) (Outcome, bool) {
	if key == "" {
		return Outcome{}, false
	}
	data, ok := cache.ReadVerdict(e.scopeVerdicts, key)
	if !ok {
		return Outcome{}, false
	}
	outputs := models.Outputs{}
	if err := json.Unmarshal(data, &outputs); err != nil {
		e.scopeVerdicts.Delete(key)
		return Outcome{}, false
	}
	now := metav1.Now()
	status := policyengine.Status{
		Phase:       policyengine.PhaseSucceeded,
		RunID:       uuid.NewString(),
		ExitStatus:  models.ExitSuccess,
		Outputs:     outputs,
		Annotations: models.Annotations{AnnotationCacheServed: key},
		StartedAt:   &now,
		CompletedAt: &now,
	}
	e.setCondition(&status, policyengine.ConditionComplete, metav1.ConditionTrue, ReasonCacheServed,
		"the scope verdict was served from the policy engine cache")
	e.log("info", "policy run served from the scope verdict cache", map[string]any{"runID": status.RunID, "key": key})
	return Outcome{
		Status: status,
		Result: models.Result{RunID: status.RunID, Status: models.ExecutionComplete, ExitStatus: models.ExitSuccess, Outputs: outputs,
			Annotations: status.Annotations},
	}, true
}

func (e *Engine) rememberScope(key string, outcome Outcome) {
	if key == "" || outcome.Status.Phase != policyengine.PhaseSucceeded || outcome.Status.ExitStatus != models.ExitSuccess {
		return
	}
	data, err := json.Marshal(outcome.Status.Outputs)
	if err != nil {
		return
	}
	cache.WriteVerdict(e.scopeVerdicts, key, data, e.scopeTTL(outputsAllow(outcome.Status.Outputs)))
}

func outputsAllow(outputs models.Outputs) bool {
	found, allow := false, true
	var visit func(value map[string]any)
	visit = func(value map[string]any) {
		for name, item := range value {
			if nested, ok := item.(map[string]any); ok {
				visit(nested)
				continue
			}
			if name != "allow" {
				continue
			}
			found = true
			if parsed, ok := boolOutput(item); !ok || !parsed {
				allow = false
			}
		}
	}
	visit(outputs)
	return found && allow
}

func boolOutput(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(v)
		return parsed, err == nil
	}
	return false, false
}
