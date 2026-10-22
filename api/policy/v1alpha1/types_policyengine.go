package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PolicyEngine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicyEngineSpec   `json:"spec,omitempty"`
	Status PolicyEngineStatus `json:"status,omitempty"`
}

type PolicyEngineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PolicyEngine `json:"items"`
}

type PolicyEngineSpec struct {
	Limits PolicyEngineLimits `json:"limits,omitempty"`

	Trust PolicyEngineTrustConfig `json:"trust,omitempty"`

	Cache PolicyEngineCacheConfig `json:"cache,omitempty"`

	BundledActionsEnabled bool `json:"bundledActionsEnabled,omitempty"`

	Suspend bool `json:"suspend,omitempty"`
}

type PolicyEngineLimits struct {
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`

	QueueDepth int32 `json:"queueDepth,omitempty"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	MaxConsoleBytes int64 `json:"maxConsoleBytes,omitempty"`

	MaxOutputBytes int64 `json:"maxOutputBytes,omitempty"`

	MaxCacheEntries int32 `json:"maxCacheEntries,omitempty"`

	MaxCacheBytes int64 `json:"maxCacheBytes,omitempty"`
}

type PolicyEngineTrustConfig struct {
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
}

type PolicyEngineCacheConfig struct {
	VerdictTTLSeconds *int64 `json:"verdictTTLSeconds,omitempty"`

	ScopeTTLSeconds *int64 `json:"scopeTTLSeconds,omitempty"`

	RecordTTLSeconds *int64 `json:"recordTTLSeconds,omitempty"`
}

type PolicyEnginePhase string

const (
	PolicyEnginePending PolicyEnginePhase = "Pending"

	PolicyEngineRunning PolicyEnginePhase = "Running"

	PolicyEngineFailed PolicyEnginePhase = "Failed"
)

type PolicyEngineStatus struct {
	Phase PolicyEnginePhase `json:"phase,omitempty"`

	Ready bool `json:"ready,omitempty"`

	ActiveRuns int32 `json:"activeRuns,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
