package models

type Outputs map[string]any

type Result struct {
	RunID         string          `json:"run_id,omitempty"`
	Status        ExecutionStatus `json:"status,omitempty"`
	ExitStatus    ExitStatus      `json:"exit_status,omitempty"`
	Outputs       Outputs         `json:"outputs,omitempty"`
	Cache         Cache           `json:"cache,omitempty"`
	Annotations   Annotations     `json:"annotations,omitempty"`
	ConsoleOutput string          `json:"console_output,omitempty"`
}
