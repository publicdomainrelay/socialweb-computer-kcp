package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DenoPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DenoPodSpec   `json:"spec,omitempty"`
	Status DenoPodStatus `json:"status,omitempty"`
}

type DenoPodList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DenoPod `json:"items"`
}

type DenoPodSpec struct {
	DenoPodTemplate `json:",inline"`

	RestartPolicy DenoRestartPolicy `json:"restartPolicy,omitempty"`

	ReadinessProbe *ExecProbe `json:"readinessProbe,omitempty"`

	LivenessProbe *ExecProbe `json:"livenessProbe,omitempty"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`

	Projected []ProjectedCredential `json:"projected,omitempty"`

	Resources *DenoPodResources `json:"resources,omitempty"`
}

type DenoPodResources struct {
	Memory string `json:"memory,omitempty"`
}

type ProjectedCredential struct {
	Name string `json:"name"`

	ServiceAccountToken *ProjectedServiceAccountToken `json:"serviceAccountToken,omitempty"`

	KcpCA *ProjectedKcpCA `json:"kcpCA,omitempty"`

	OpenBaoClient *ProjectedOpenBaoClient `json:"openBaoClient,omitempty"`

	File *ProjectedFile `json:"file,omitempty"`
}

type ProjectedServiceAccountToken struct {
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`

	Workspace string `json:"workspace,omitempty"`

	ExpirationSeconds int64 `json:"expirationSeconds,omitempty"`
}

type ProjectedKcpCA struct{}

type ProjectedOpenBaoClient struct {
	Server string `json:"server"`

	Client string `json:"client"`

	Workspace string `json:"workspace,omitempty"`
}

type ProjectedFile struct {
	Path string `json:"path"`
}

type DenoPodProjected struct {
	Name string `json:"name"`

	Env string `json:"env"`

	Path string `json:"path"`

	UpdatedAt string `json:"updatedAt,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`

	Message string `json:"message,omitempty"`
}

const ProjectedEnv = "KCP_PROJECTED"

const ProjectedEnvPrefix = "KCP_PROJECTED_"

type DenoPodPhase string

const (
	DenoPodPending DenoPodPhase = "Pending"

	DenoPodRunning DenoPodPhase = "Running"

	DenoPodSucceeded DenoPodPhase = "Succeeded"

	DenoPodFailed DenoPodPhase = "Failed"
)

type DenoPodStatus struct {
	Phase DenoPodPhase `json:"phase,omitempty"`

	RunID string `json:"runID,omitempty"`

	RunGeneration int64 `json:"runGeneration,omitempty"`

	Restarts int32 `json:"restarts,omitempty"`

	RestartReasons map[string]int32 `json:"restartReasons,omitempty"`

	StartTime *metav1.Time `json:"startTime,omitempty"`

	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	ExitCode *int32 `json:"exitCode,omitempty"`

	Message string `json:"message,omitempty"`

	Outputs map[string]string `json:"outputs,omitempty"`

	Ready bool `json:"ready,omitempty"`

	Address string `json:"address,omitempty"`

	Listeners []DenoPodListener `json:"listeners,omitempty"`

	Projected []DenoPodProjected `json:"projected,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type DenoPodListener struct {
	Name string `json:"name"`

	Kind string `json:"kind"`

	Address string `json:"address"`
}

const AnnotationEdge = "deno.computer/edge"

const AnnotationRestart = "deno.computer/restart"

const ReasonRestartRequested = "RestartRequested"

const ReasonSpecChanged = "SpecChanged"

const ReasonFixedPortBehindTheEdge = "FixedPortBehindTheEdge"

const FinalizerDenoPod = "denopod.deno.computer/run"
