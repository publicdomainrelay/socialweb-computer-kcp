package component

import (
	"cmp"
	"time"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
)

func (s ComponentStatus) APIStatus() v1alpha1.ComponentStatus {
	out := v1alpha1.ComponentStatus{
		ObservedGeneration: s.ObservedGeneration,
		Phase:              v1alpha1.ComponentPhase(s.Phase),
		Ready:              s.Ready,
		Message:            s.Message,
		Restarts:           s.Restarts,
		Conditions:         append([]metav1.Condition(nil), s.Conditions...),
	}
	if !s.StartedAt.IsZero() {
		out.StartedAt = s.StartedAt.UTC().Format(time.RFC3339)
	}
	if primary, ok := s.PrimaryEndpoint(); ok {
		out.Endpoint = primary.URL
		out.Endpoint = cmp.Or(out.Endpoint, primary.Address)
	}
	if len(s.Endpoints) > 0 {
		out.Endpoints = make([]v1alpha1.Endpoint, 0, len(s.Endpoints))
		for _, endpoint := range s.Endpoints {
			out.Endpoints = append(out.Endpoints, APIEndpoint(endpoint))
		}
	}
	return out
}

func APIEndpoint(endpoint Endpoint) v1alpha1.Endpoint {
	return v1alpha1.Endpoint{
		Name:     endpoint.Name,
		URL:      endpoint.URL,
		Address:  endpoint.Address,
		Protocol: endpoint.Protocol,
	}
}

func (s ComponentStatus) PrimaryEndpoint() (Endpoint, bool) {
	return PrimaryEndpoint(s.Endpoints)
}

func PrimaryEndpoint(endpoints []Endpoint) (Endpoint, bool) {
	return contract.PrimaryEndpoint(endpoints)
}
