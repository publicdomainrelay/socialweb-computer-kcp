package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/outputs"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

type Tasks struct {
	Engine *Engine
}

var _ policy.Engine = Tasks{}

func (t Tasks) Submit(ctx context.Context, workflow []byte, inputs map[string]string) (string, error) {
	if t.Engine == nil {
		return "", errors.New("policy engine: no in-process engine is running")
	}
	if !json.Valid(workflow) {
		return "", errors.New("policy engine: the workflow is not valid JSON")
	}
	values := make(map[string]any, len(inputs))
	for key, value := range inputs {
		values[key] = value
	}
	return t.Engine.Submit(ctx, &PolicyEngineRequest{Workflow: append([]byte(nil), workflow...), Inputs: values}, RunOptions{})
}

func (t Tasks) Status(_ context.Context, taskID string) (policy.Task, error) {
	if t.Engine == nil {
		return policy.Task{}, errors.New("policy engine: no in-process engine is running")
	}
	status, ok := t.Engine.RunStatus(taskID)
	if !ok {
		return policy.Task{}, fmt.Errorf("policy engine: task %s not found", taskID)
	}
	if !status.Phase.Terminal() {
		return policy.Task{State: policy.StateRunning}, nil
	}
	task := policy.Task{ExitStatus: string(status.ExitStatus), Outputs: outputs.Stringify(status.Outputs)}
	if status.Phase == policyengine.PhaseSucceeded {
		task.State = policy.StateSucceeded
		return task, nil
	}
	task.State = policy.StateFailed
	if task.ExitStatus != "" {
		task.Message = "the workflow exited with status " + task.ExitStatus
	} else {
		task.Message = "the policy engine did not report a success exit status"
	}
	return task, nil
}
