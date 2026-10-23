package statusstate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type RelayObservation struct {
	Phase v1alpha1.RelayStatusPhase

	Cursor int64

	LastEventAt string

	Events int64
}

type RelayEndpointFunc func(ref string) (string, bool)

type HTTPRelayHealthSource struct {
	endpoint RelayEndpointFunc

	client *http.Client
}

func NewHTTPRelayHealthSource(endpoint RelayEndpointFunc, client *http.Client) *HTTPRelayHealthSource {
	return &HTTPRelayHealthSource{endpoint: endpoint, client: httpClient(client)}
}

func (s *HTTPRelayHealthSource) RelayStatus(ctx context.Context, ref string) (RelayObservation, error) {
	if s == nil {
		return RelayObservation{}, fmt.Errorf("statusstate: the relay health source is nil")
	}
	base, err := s.baseURL(ref)
	if err != nil {
		return RelayObservation{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/xrpc/_health", nil)
	if err != nil {
		return RelayObservation{}, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return RelayObservation{}, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxSourceBody))
	phase := v1alpha1.RelayStatusPhaseRunning
	if response.StatusCode < 200 || response.StatusCode > 299 {
		phase = v1alpha1.RelayStatusPhaseFailed
	}
	return RelayObservation{Phase: phase}, nil
}

func (s *HTTPRelayHealthSource) baseURL(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if s.endpoint != nil {
		if resolved, ok := s.endpoint(ref); ok {
			return sourceBaseURL(resolved, "relay")
		}
	}
	if strings.Contains(ref, "://") {
		return sourceBaseURL(ref, "relay")
	}
	return "", fmt.Errorf("statusstate: the relay ref %q has no endpoint and is not a URL", ref)
}
