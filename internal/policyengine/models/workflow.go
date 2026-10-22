package models

type Workflow struct {
	Name string                 `json:"name,omitempty" yaml:"name,omitempty"`
	On   any                    `json:"on,omitempty" yaml:"on,omitempty"`
	Env  map[string]any         `json:"env,omitempty" yaml:"env,omitempty"`
	Jobs map[string]WorkflowJob `json:"jobs,omitempty" yaml:"jobs,omitempty"`
}

type WorkflowJob struct {
	If             any            `json:"if,omitempty" yaml:"if,omitempty"`
	Needs          any            `json:"needs,omitempty" yaml:"needs,omitempty"`
	RunsOn         any            `json:"runs-on,omitempty" yaml:"runs-on,omitempty"`
	Env            map[string]any `json:"env,omitempty" yaml:"env,omitempty"`
	Outputs        map[string]any `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	Steps          []WorkflowStep `json:"steps,omitempty" yaml:"steps,omitempty"`
	TimeoutMinutes int            `json:"timeout-minutes,omitempty" yaml:"timeout-minutes,omitempty"`
}

type WorkflowStep struct {
	ID               string         `json:"id,omitempty" yaml:"id,omitempty"`
	If               any            `json:"if,omitempty" yaml:"if,omitempty"`
	Name             string         `json:"name,omitempty" yaml:"name,omitempty"`
	Uses             string         `json:"uses,omitempty" yaml:"uses,omitempty"`
	Shell            string         `json:"shell,omitempty" yaml:"shell,omitempty"`
	With             map[string]any `json:"with,omitempty" yaml:"with,omitempty"`
	Env              map[string]any `json:"env,omitempty" yaml:"env,omitempty"`
	Run              string         `json:"run,omitempty" yaml:"run,omitempty"`
	WorkingDirectory string         `json:"working-directory,omitempty" yaml:"working-directory,omitempty"`
	ContinueOnError  bool           `json:"continue-on-error,omitempty" yaml:"continue-on-error,omitempty"`
	TimeoutMinutes   int            `json:"timeout-minutes,omitempty" yaml:"timeout-minutes,omitempty"`
}
