package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type HostProcess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostProcessSpec   `json:"spec,omitempty"`
	Status HostProcessStatus `json:"status,omitempty"`
}

type HostProcessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HostProcess `json:"items"`
}

type HostProcessRestartPolicy string

const (
	HostProcessRestartNever HostProcessRestartPolicy = "Never"

	HostProcessRestartOnFailure HostProcessRestartPolicy = "OnFailure"

	HostProcessRestartAlways HostProcessRestartPolicy = "Always"
)

type HostProcessPlacement string

const (
	HostProcessPlacementSupervisor HostProcessPlacement = "supervisor"

	HostProcessPlacementBootstrap HostProcessPlacement = "bootstrap"
)

type HostProcessSpec struct {
	Unit string `json:"unit"`

	Asset *HostProcessAsset `json:"asset,omitempty"`

	Exec string `json:"exec,omitempty"`

	Argv []string `json:"argv,omitempty"`

	Env []HostProcessEnv `json:"env,omitempty"`

	EnvFiles []string `json:"envFiles,omitempty" apigen:"userEndpoint"`

	WorkDir string `json:"workDir,omitempty"`

	Output *HostProcessOutput `json:"output,omitempty"`

	Limits *HostProcessLimits `json:"limits,omitempty"`

	RestartPolicy HostProcessRestartPolicy `json:"restartPolicy,omitempty"`

	Stop *HostProcessStop `json:"stop,omitempty"`

	After []string `json:"after,omitempty"`

	Wants []string `json:"wants,omitempty"`

	Slice string `json:"slice,omitempty"`

	PrivateHostname string `json:"privateHostname,omitempty"`

	FdStore int32 `json:"fdStore,omitempty"`

	Readiness *HostProcessReadiness `json:"readiness,omitempty"`

	RestartKey string `json:"restartKey,omitempty"`

	Placement HostProcessPlacement `json:"placement"`
}

type HostProcessAsset struct {
	Name string `json:"name"`

	Sha256 string `json:"sha256"`
}

type HostProcessEnv struct {
	Name string `json:"name"`

	Value string `json:"value"`
}

type HostProcessOutput struct {
	Stdout string `json:"stdout,omitempty"`

	Stderr string `json:"stderr,omitempty"`

	CapBytes int64 `json:"capBytes,omitempty"`
}

type HostProcessLimits struct {
	MemoryMax string `json:"memoryMax,omitempty"`

	CpuWeight int64 `json:"cpuWeight,omitempty"`

	CpuQuotaPercent int32 `json:"cpuQuotaPercent,omitempty"`

	TasksMax int64 `json:"tasksMax,omitempty"`
}

type HostProcessStop struct {
	Signal string `json:"signal,omitempty"`

	GraceSeconds int32 `json:"graceSeconds,omitempty"`
}

type HostProcessReadiness struct {
	Exec []string `json:"exec,omitempty"`

	Tcp bool `json:"tcp,omitempty"`

	HttpPath string `json:"httpPath,omitempty" apigen:"userEndpoint"`

	PgxSocketDir string `json:"pgxSocketDir,omitempty"`

	UnixSocket string `json:"unixSocket,omitempty"`
}

type HostProcessPhase string

const (
	HostProcessPhasePending HostProcessPhase = "Pending"

	HostProcessPhaseStarting HostProcessPhase = "Starting"

	HostProcessPhaseRunning HostProcessPhase = "Running"

	HostProcessPhaseStopping HostProcessPhase = "Stopping"

	HostProcessPhaseStopped HostProcessPhase = "Stopped"

	HostProcessPhaseFailed HostProcessPhase = "Failed"

	HostProcessPhaseLost HostProcessPhase = "Lost"
)

type HostProcessLostReason string

const (
	HostProcessLostHostRebooted HostProcessLostReason = "HostRebooted"

	HostProcessLostUnitGone HostProcessLostReason = "UnitGone"

	HostProcessLostUnitFailed HostProcessLostReason = "UnitFailed"

	HostProcessLostOOMKilled HostProcessLostReason = "OOMKilled"

	HostProcessLostIdentityMismatch HostProcessLostReason = "IdentityMismatch"

	HostProcessLostManagerRestarted HostProcessLostReason = "ManagerRestarted"
)

type HostProcessStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase HostProcessPhase `json:"phase,omitempty"`

	LostReason HostProcessLostReason `json:"lostReason,omitempty"`

	Message string `json:"message,omitempty"`

	Runtime *HostProcessRuntime `json:"runtime,omitempty"`

	LastResult *HostProcessResult `json:"lastResult,omitempty"`

	RestartKey string `json:"restartKey,omitempty"`

	Listeners []HostProcessListener `json:"listeners,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type HostProcessRuntime struct {
	Backend string `json:"backend"`

	Unit string `json:"unit"`

	InvocationID string `json:"invocationID,omitempty"`

	Pid int64 `json:"pid,omitempty"`

	StartTicks int64 `json:"startTicks,omitempty"`

	HostBootID string `json:"hostBootID,omitempty"`

	Manager string `json:"manager,omitempty"`

	ActiveSince string `json:"activeSince,omitempty"`
}

type HostProcessResult struct {
	Result string `json:"result,omitempty"`

	ExitCode int32 `json:"exitCode,omitempty"`

	Signal string `json:"signal,omitempty"`

	At string `json:"at,omitempty"`
}

type HostProcessListener struct {
	Name string `json:"name"`

	Address string `json:"address"`
}
