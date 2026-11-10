package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AtircNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AtircNodeSpec   `json:"spec,omitempty"`
	Status AtircNodeStatus `json:"status,omitempty"`
}

type AtircNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AtircNode `json:"items"`
}

type AtircNodeSpec struct {
	Handle string `json:"handle"`

	Nick string `json:"nick,omitempty"`

	Channels []string `json:"channels,omitempty"`

	Peers []string `json:"peers,omitempty"`

	Jetstream string `json:"jetstream,omitempty" apigen:"userEndpoint"`

	IrcPort *int32 `json:"ircPort,omitempty"`

	ConfigDir string `json:"configDir,omitempty"`

	PasswordEnvFile string `json:"passwordEnvFile,omitempty" apigen:"userEndpoint"`

	NoPublish bool `json:"noPublish,omitempty"`
}

type AtircNodePhase string

const (
	AtircNodePhasePending AtircNodePhase = "Pending"

	AtircNodePhaseStarting AtircNodePhase = "Starting"

	AtircNodePhaseRunning AtircNodePhase = "Running"

	AtircNodePhaseFailed AtircNodePhase = "Failed"
)

type AtircNodeStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase AtircNodePhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Identity string `json:"identity,omitempty"`

	ConfigDir string `json:"configDir,omitempty"`

	HostProcess string `json:"hostProcess,omitempty"`

	Runtime *HostProcessRuntime `json:"runtime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
