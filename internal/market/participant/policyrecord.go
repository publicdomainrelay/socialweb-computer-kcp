package participant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const PolicyRecordType = "computer.socialweb.temp.policy.ghalite"

func PolicyRecordWorkflow(value map[string]any) (json.RawMessage, error) {
	if kind, _ := value["$type"].(string); kind != PolicyRecordType {
		return nil, fmt.Errorf("the policy record type %q is not gha-lite and is denied", kind)
	}
	raw, ok := value["workflow"]
	if !ok || raw == nil {
		return nil, errors.New("the policy record names no workflow")
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, errors.New("the policy record names an empty workflow")
		}
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(v), &parsed); err != nil || parsed == nil {
			return nil, errors.New("the policy record's workflow is not a gha-lite workflow mapping")
		}
		return json.Marshal(parsed)
	case map[string]any:
		return json.Marshal(v)
	default:
		return nil, errors.New("the policy record's workflow is not a string or object")
	}
}
