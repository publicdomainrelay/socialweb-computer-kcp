package atproto

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultPLCURL      = "https://plc.directory"
	defaultHTTPTimeout = 15 * time.Second
)

type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type Record struct {
	URI   string
	CID   string
	Value map[string]any
}

type Client struct {
	HTTP    *http.Client
	PLCURL  string
	Timeout time.Duration
}

func NewClient() *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
		PLCURL:  DefaultPLCURL,
		Timeout: defaultHTTPTimeout,
	}
}

func SplitURI(uri string) (repo, collection, rkey string) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(uri), "at://")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) > 0 {
		repo = parts[0]
	}
	if len(parts) > 1 {
		collection = parts[1]
	}
	if len(parts) > 2 {
		rkey = parts[2]
	}
	return repo, collection, rkey
}

func RepoFromURI(uri string) string {
	repo, _, _ := SplitURI(uri)
	return repo
}

func (c *Client) ResolvePDS(ctx context.Context, did string) (string, error) {
	did = strings.TrimSpace(did)
	if did == "" {
		return "", fmt.Errorf("did is empty")
	}
	var docURL string
	if after, ok := strings.CutPrefix(did, "did:web:"); ok {
		host := after
		host = strings.ReplaceAll(host, ":", "/")
		docURL = "https://" + host + "/did.json"
	} else {
		base := c.PLCURL
		base = cmp.Or(base, DefaultPLCURL)
		docURL = strings.TrimSuffix(base, "/") + "/" + url.PathEscape(did)
	}
	var doc struct {
		Service []struct {
			ID              string          `json:"id"`
			Type            string          `json:"type"`
			ServiceEndpoint json.RawMessage `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := c.getJSON(ctx, docURL, &doc); err != nil {
		return "", fmt.Errorf("resolve did %s: %w", did, err)
	}
	for _, service := range doc.Service {
		if service.Type != "AtprotoPersonalDataServer" && service.ID != "#atproto_pds" && !strings.HasSuffix(service.ID, "#atproto_pds") {
			continue
		}
		endpoint := decodeEndpoint(service.ServiceEndpoint)
		if endpoint != "" {
			return strings.TrimSuffix(endpoint, "/"), nil
		}
	}
	return "", fmt.Errorf("did %s has no atproto pds service", did)
}

func decodeEndpoint(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		if endpoint, ok := object["endpoint"].(string); ok {
			return endpoint
		}
	}
	return ""
}

func (c *Client) GetRecord(ctx context.Context, ref StrongRef) (map[string]any, error) {
	repo, collection, rkey := SplitURI(ref.URI)
	if repo == "" || collection == "" || rkey == "" {
		return nil, fmt.Errorf("invalid at uri %q", ref.URI)
	}
	pds, err := c.ResolvePDS(ctx, repo)
	if err != nil {
		return nil, err
	}
	endpoint := pds + "/xrpc/com.atproto.repo.getRecord"
	query := url.Values{}
	query.Set("repo", repo)
	query.Set("collection", collection)
	query.Set("rkey", rkey)
	var response struct {
		Value map[string]any `json:"value"`
	}
	if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), &response); err != nil {
		return nil, fmt.Errorf("getRecord %s: %w", ref.URI, err)
	}
	if response.Value == nil {
		response.Value = map[string]any{}
	}
	return response.Value, nil
}

func (c *Client) ListRecords(ctx context.Context, repo, collection string, limit int) ([]Record, error) {
	limit = opt.Positive(limit, 100)
	page := min(limit, 100)
	pds, err := c.ResolvePDS(ctx, repo)
	if err != nil {
		return nil, err
	}
	endpoint := pds + "/xrpc/com.atproto.repo.listRecords"
	records := make([]Record, 0, page)
	cursor := ""
	seen := map[string]bool{}
	for len(records) < limit {
		query := url.Values{}
		query.Set("repo", repo)
		query.Set("collection", collection)
		query.Set("limit", fmt.Sprintf("%d", page))
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var response struct {
			Records []struct {
				URI   string         `json:"uri"`
				CID   string         `json:"cid"`
				Value map[string]any `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), &response); err != nil {
			return nil, fmt.Errorf("listRecords %s %s: %w", repo, collection, err)
		}
		for _, record := range response.Records {
			records = append(records, Record{URI: record.URI, CID: record.CID, Value: record.Value})
			if len(records) >= limit {
				break
			}
		}
		if response.Cursor == "" || len(response.Records) == 0 || seen[response.Cursor] {
			break
		}
		seen[response.Cursor] = true
		cursor = response.Cursor
	}
	return records, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, target any) error {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: c.timeout()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	return decoder.Decode(target)
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultHTTPTimeout
}
