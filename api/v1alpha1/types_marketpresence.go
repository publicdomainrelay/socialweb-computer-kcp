package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MarketPresence struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarketPresenceSpec   `json:"spec,omitempty"`
	Status MarketPresenceStatus `json:"status,omitempty"`
}

type MarketPresenceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MarketPresence `json:"items"`
}

type MarketPresenceBidderRef struct {
	Workspace string `json:"workspace"`

	Kind string `json:"kind"`

	Name string `json:"name"`
}

type MarketPresencePhase string

const (
	MarketPresencePhaseActive MarketPresencePhase = "Active"

	MarketPresencePhaseExpired MarketPresencePhase = "Expired"
)

type MarketPresenceSpec struct {
	BidderRef MarketPresenceBidderRef `json:"bidderRef"`

	OfferingUri string `json:"offeringUri,omitempty"`

	RelayUri string `json:"relayUri,omitempty"`

	TtlSeconds int `json:"ttlSeconds,omitempty"`
}

type MarketPresenceStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase MarketPresencePhase `json:"phase,omitempty"`

	Ready bool `json:"ready"`

	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
