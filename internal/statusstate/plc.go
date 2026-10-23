package statusstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
)

type PLCIdentity struct {
	Found bool

	Head string

	OpCount int64

	Tombstoned bool
}

type HTTPPLCClient struct {
	baseURL string

	client *http.Client
}

func NewHTTPPLCClient(baseURL string, client *http.Client) (*HTTPPLCClient, error) {
	parsed, err := sourceBaseURL(baseURL, "PLC")
	if err != nil {
		return nil, err
	}
	return &HTTPPLCClient{baseURL: parsed, client: httpClient(client)}, nil
}

func (c *HTTPPLCClient) Identity(ctx context.Context, did string) (PLCIdentity, error) {
	entries, err := c.auditLog(ctx, did)
	var plcErr *plc.Error
	if errors.As(err, &plcErr) && plcErr.Status == http.StatusNotFound {
		return PLCIdentity{}, nil
	}
	if err != nil {
		return PLCIdentity{}, err
	}
	return Summarize(entries), nil
}

func Summarize(entries []plc.LogEntry) PLCIdentity {
	var out PLCIdentity
	for i := range entries {
		if entries[i].Nullified {
			continue
		}
		out.OpCount++
		out.Head = entries[i].CID
		out.Tombstoned = entries[i].Operation.Type == plc.TypeTombstone
	}
	out.Found = out.OpCount > 0
	return out
}

func (c *HTTPPLCClient) auditLog(ctx context.Context, did string) ([]plc.LogEntry, error) {
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
