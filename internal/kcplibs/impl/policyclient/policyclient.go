package policyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/outputs"
)

type Client struct {
	http *http.Client
}

var _ policy.Engine = (*Client)(nil)

func New() *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}}
}

func newWithClient(client *http.Client) *Client {
	return &Client{http: client}
}

type submitRequest struct {
	Workflow json.RawMessage `json:"workflow"`

	Inputs map[string]string `json:"inputs,omitempty"`
}

type submitResponse struct {
	Status string `json:"status"`

	Detail struct {
		ID string `json:"id"`
	} `json:"detail"`
}

type statusResponse struct {
	Status string `json:"status"`

	Detail struct {
		ExitStatus string                          `json:"exit_status"`
		Outputs    map[string]any                  `json:"outputs"`
		Cache      map[string]map[string]cacheFile `json:"cache"`
	} `json:"detail"`
}

type cacheFile struct {
	Data string `json:"data"`
}

type verdict struct {
	Allow *bool `json:"allow"`

	Violations json.RawMessage `json:"violations"`
}

func (c *Client) Submit(ctx context.Context, endpoint string, workflow []byte, inputs map[string]string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("policyclient: submitting a policy run without an engine endpoint")
	}
	if !json.Valid(workflow) {
		return "", fmt.Errorf("policyclient: the workflow is not valid JSON")
	}
	body, err := json.Marshal(submitRequest{Workflow: json.RawMessage(workflow), Inputs: inputs})
	if err != nil {
		return "", fmt.Errorf("policyclient: encoding the policy request: %w", err)
	}
	url := strings.TrimSuffix(endpoint, "/") + "/request/create"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("policyclient: submitting the policy run: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("policyclient: read the submit response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("policyclient: the policy engine returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out submitResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("policyclient: parsing the policy submit response: %w", err)
	}
	if out.Detail.ID == "" {
		return "", fmt.Errorf("policyclient: the policy engine returned no task id")
	}
	return out.Detail.ID, nil
}

func (c *Client) Status(ctx context.Context, endpoint, taskID string) (policy.Task, error) {
	url := strings.TrimSuffix(endpoint, "/") + "/request/status/" + taskID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return policy.Task{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return policy.Task{}, ctx.Err()
		}
		return policy.Task{State: policy.StateRunning}, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return policy.Task{}, fmt.Errorf("policyclient: read the status response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return policy.Task{}, fmt.Errorf("policyclient: the policy engine returned %d for task %s", resp.StatusCode, taskID)
	}
	var out statusResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return policy.Task{State: policy.StateRunning}, nil
	}
	switch out.Status {
	case "", "submitted", "in_progress":
		return policy.Task{State: policy.StateRunning}, nil
	}
	task := policy.Task{
		ExitStatus: out.Detail.ExitStatus,
		Outputs:    Outputs(out),
	}
	if out.Detail.ExitStatus == "success" {
		task.State = policy.StateSucceeded
		return task, nil
	}
	task.State = policy.StateFailed
	if out.Detail.ExitStatus != "" {
		task.Message = "the workflow exited with status " + out.Detail.ExitStatus
	} else {
		task.Message = "the policy engine did not report a success exit status"
	}
	return task, nil
}

func Outputs(response statusResponse) map[string]string {
	out := map[string]string{}
	for key, value := range response.Detail.Outputs {
		out[key] = outputs.StringifyValue(value)
	}
	keys := make([]string, 0, len(response.Detail.Cache))
	for key := range response.Detail.Cache {
		if strings.HasPrefix(key, "policy/") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	single := len(keys) == 1
	for _, key := range keys {
		file, ok := response.Detail.Cache[key]["result.json"]
		if !ok {
			continue
		}
		var verdict verdict
		if err := json.Unmarshal([]byte(file.Data), &verdict); err != nil {
			continue
		}
		prefix := ""
		if !single {
			prefix = PolicyName(key) + "/"
		}
		if verdict.Allow != nil {
			out[prefix+"allow"] = strconv.FormatBool(*verdict.Allow)
		}
		if len(verdict.Violations) > 0 && string(verdict.Violations) != "null" {
			out[prefix+"violations"] = string(verdict.Violations)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func PolicyName(cacheKey string) string {
	parts := strings.Split(cacheKey, "/")
	if len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return "policy"
}
