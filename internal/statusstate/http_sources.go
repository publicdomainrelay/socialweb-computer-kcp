package statusstate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const maxSourceBody = 8 << 20

type HTTPPLCClient struct {
	baseURL string

	client *http.Client
}

func NewHTTPPLCClient(baseURL string, handlers *component.HandlerRegistry) (*HTTPPLCClient, error) {
	parsed, err := sourceBaseURL(baseURL, "PLC")
	if err != nil {
		return nil, err
	}
	client := http.DefaultClient
	if handlers != nil {
		client = handlers.HTTPClient()
	}
	return &HTTPPLCClient{baseURL: parsed, client: client}, nil
}

func (c *HTTPPLCClient) AuditLog(ctx context.Context, did string) ([]plc.LogEntry, error) {
	if c == nil {
		return nil, fmt.Errorf("statusstate: the HTTP PLC client is nil")
	}
	did = strings.TrimSpace(did)
	if did == "" {
		return nil, fmt.Errorf("statusstate: the PLC audit source needs a did")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+url.PathEscape(did)+"/log/audit", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBody))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, plcStatusError(response.StatusCode, body)
	}
	var entries []plc.LogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("statusstate: the PLC audit log is not JSON: %w", err)
	}
	return entries, nil
}

func plcStatusError(status int, body []byte) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = fmt.Sprintf("HTTP %d", status)
	}
	return &plc.Error{Status: status, Message: message}
}

type RelayEndpointFunc func(ref string) (string, bool)

type HTTPRelayHealthSource struct {
	Endpoint RelayEndpointFunc

	Client *http.Client
}

func NewHTTPRelayHealthSource(endpoint RelayEndpointFunc, handlers *component.HandlerRegistry) *HTTPRelayHealthSource {
	client := http.DefaultClient
	if handlers != nil {
		client = handlers.HTTPClient()
	}
	return &HTTPRelayHealthSource{Endpoint: endpoint, Client: client}
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
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
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
	if s.Endpoint != nil {
		if resolved, ok := s.Endpoint(ref); ok {
			return sourceBaseURL(resolved, "relay")
		}
	}
	if strings.Contains(ref, "://") {
		return sourceBaseURL(ref, "relay")
	}
	return "", fmt.Errorf("statusstate: the relay ref %q has no endpoint and is not a URL", ref)
}

func sourceBaseURL(raw, kind string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("statusstate: the %s base URL is invalid: %w", kind, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("statusstate: the %s base URL needs http or https", kind)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("statusstate: the %s base URL has no host", kind)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("statusstate: the %s base URL must not have a query or fragment", kind)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}
