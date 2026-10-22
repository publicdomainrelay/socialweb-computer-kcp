package status

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
	PhaseCancelled Phase = "Cancelled"
)

const (
	ConditionReady     = "Ready"
	ConditionComplete  = "Complete"
	ConditionFailed    = "Failed"
	ConditionSuspended = "Suspended"
	ConditionCancelled = "Cancelled"
	ConditionRestarted = "Restarted"
)

type Status struct {
	Phase         Phase              `json:"phase,omitempty"`
	RunID         string             `json:"runID,omitempty"`
	ExitStatus    models.ExitStatus  `json:"exitStatus,omitempty"`
	Outputs       models.Outputs     `json:"outputs,omitempty"`
	Cache         models.Cache       `json:"cache,omitempty"`
	ConsoleOutput string             `json:"consoleOutput,omitempty"`
	Annotations   models.Annotations `json:"annotations,omitempty"`
	Restarts      int32              `json:"restarts,omitempty"`
	Deadline      *metav1.Time       `json:"deadline,omitempty"`
	StartedAt     *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt   *metav1.Time       `json:"completedAt,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

func (p Phase) Terminal() bool {
	switch p {
	case PhaseSucceeded, PhaseFailed, PhaseCancelled:
		return true
	default:
		return false
	}
}
