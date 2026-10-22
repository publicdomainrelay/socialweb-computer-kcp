package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const (
	ContextKeyCache   = "cache"
	ContextKeyConfig  = "config"
	ContextKeySecrets = "secrets"
	ContextKeyVars    = "vars"
)

type Request struct {
	Inputs   map[string]any  `json:"inputs,omitempty"`
	Workflow json.RawMessage `json:"workflow,omitempty"`
	Context  map[string]any  `json:"context,omitempty"`
	Stack    map[string]any  `json:"stack,omitempty"`
}

func (r *Request) Validate() error {
	if r == nil {
		return errors.New("workflow is required")
	}
	workflow := bytes.TrimSpace(r.Workflow)
	if len(workflow) == 0 || bytes.Equal(workflow, []byte("null")) {
		return errors.New("workflow is required")
	}
	var value string
	if err := json.Unmarshal(workflow, &value); err == nil {
		value = strings.TrimSpace(value)
		if value == "" || value == "null" {
			return errors.New("workflow is required")
		}
	}
	return nil
}

type InputValidationError struct {
	Msg   string   `json:"msg"`
	Loc   []string `json:"loc"`
	Type  string   `json:"type"`
	URL   string   `json:"url,omitempty"`
	Input string   `json:"input,omitempty"`
}
