package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type HTTPReader struct {
	Directory identity.Directory
	Client    HTTPDoer
}

func NewHTTPReader(dir identity.Directory) *HTTPReader {
	return &HTTPReader{Directory: dir, Client: http.DefaultClient}
}

func (h *HTTPReader) doer() HTTPDoer {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

func (h *HTTPReader) PDSEndpoint(ctx context.Context, did string) (string, error) {
	if h.Directory == nil {
		return "", fmt.Errorf("market: no identity directory")
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return "", fmt.Errorf("market: %w", err)
	}
	ident, err := h.Directory.LookupDID(ctx, parsed)
	if err != nil {
		return "", fmt.Errorf("market: resolve %s: %w", did, err)
	}
	endpoint := ident.PDSEndpoint()
	if endpoint == "" {
		return "", fmt.Errorf("market: no PDS endpoint for %s", did)
	}
	return strings.TrimRight(endpoint, "/"), nil
}

func (h *HTTPReader) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := h.doer()
	var lastErr error
	for attempt := range 5 {
		res, err := client.Do(req)
		if err == nil {
			if res.StatusCode < 500 {
				return res, nil
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			lastErr = fmt.Errorf("market: GET %s: HTTP %d", rawURL, res.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func (h *HTTPReader) GetRecord(ctx context.Context, repo, collection, rkey string) (ResolvedRecord, error) {
	pds, err := h.PDSEndpoint(ctx, repo)
	if err != nil {
		return ResolvedRecord{}, err
	}
	u := pds + "/xrpc/com.atproto.repo.getRecord"
	values := url.Values{}
	values.Set("repo", repo)
	values.Set("collection", collection)
	values.Set("rkey", rkey)
	res, err := h.get(ctx, u+"?"+values.Encode())
	if err != nil {
		return ResolvedRecord{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, res.Body)
		return ResolvedRecord{}, fmt.Errorf("%w: %s/%s/%s: HTTP %d", ErrNotFound, repo, collection, rkey, res.StatusCode)
	}
	var body struct {
		URI   string         `json:"uri"`
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return ResolvedRecord{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if body.Value == nil {
		return ResolvedRecord{}, fmt.Errorf("%w: no record value", ErrNotFound)
	}
	if body.URI == "" {
		body.URI = "at://" + repo + "/" + collection + "/" + rkey
	}
	return ResolvedRecord{URI: body.URI, CID: body.CID, Value: body.Value}, nil
}

func (h *HTTPReader) ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (ListPage, error) {
	pds, err := h.PDSEndpoint(ctx, repo)
	if err != nil {
		return ListPage{}, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	u := pds + "/xrpc/com.atproto.repo.listRecords"
	values := url.Values{}
	values.Set("repo", repo)
	values.Set("collection", collection)
	values.Set("limit", fmt.Sprintf("%d", limit))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	res, err := h.get(ctx, u+"?"+values.Encode())
	if err != nil {
		return ListPage{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, res.Body)
		return ListPage{}, fmt.Errorf("%w: list %s/%s: HTTP %d", ErrNotFound, repo, collection, res.StatusCode)
	}
	var body struct {
		Records []struct {
			URI   string         `json:"uri"`
			CID   string         `json:"cid"`
			Value map[string]any `json:"value"`
		} `json:"records"`
		Cursor string `json:"cursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return ListPage{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	page := ListPage{Cursor: body.Cursor, Records: make([]ListedRecord, 0, len(body.Records))}
	for _, rec := range body.Records {
		page.Records = append(page.Records, ListedRecord{URI: rec.URI, CID: rec.CID, Value: rec.Value})
	}
	return page, nil
}

func (h *HTTPReader) ListAll(ctx context.Context, repo, collection string, limit int) ([]ListedRecord, error) {
	pageLimit := limit
	if pageLimit <= 0 || pageLimit > 100 {
		pageLimit = 100
	}
	out := []ListedRecord{}
	cursor := ""
	for {
		page, err := h.ListRecords(ctx, repo, collection, pageLimit, cursor)
		if err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, page.Records...)
		if page.Cursor == "" {
			return out, nil
		}
		cursor = page.Cursor
	}
}

func (h *HTTPReader) Scan(ctx context.Context, repo, collection string, visit func(ListedRecord) (bool, error)) (bool, error) {
	cursor := ""
	for {
		page, err := h.ListRecords(ctx, repo, collection, 100, cursor)
		if err != nil {
			return false, err
		}
		for _, rec := range page.Records {
			stop, err := visit(rec)
			if err != nil {
				return false, err
			}
			if stop {
				return true, nil
			}
		}
		if len(page.Records) == 0 || page.Cursor == "" {
			return false, nil
		}
		cursor = page.Cursor
	}
}

func (h *HTTPReader) Find(ctx context.Context, repo, collection string, match func(ListedRecord) bool) (*ListedRecord, error) {
	var found *ListedRecord
	_, err := h.Scan(ctx, repo, collection, func(rec ListedRecord) (bool, error) {
		if !match(rec) {
			return false, nil
		}
		copied := rec
		found = &copied
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

type recordResolver struct {
	remote RecordGetter
	ownDID string
	own    RecordGetter
}

func NewRecordResolver(remote RecordGetter, ownDID string, own RecordGetter) RecordResolver {
	return &recordResolver{remote: remote, ownDID: ownDID, own: own}
}

func (r *recordResolver) Resolve(ctx context.Context, ref RecordRef) (ResolvedRecord, error) {
	aturi, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return ResolvedRecord{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	repo := string(aturi.Authority())
	collection := string(aturi.Collection())
	rkey := string(aturi.RecordKey())
	if repo == "" || collection == "" || rkey == "" {
		return ResolvedRecord{}, fmt.Errorf("%w: %q is not a record AT URI", ErrInvalidRef, ref.URI)
	}
	if r.own != nil && repo == r.ownDID {
		rec, err := r.own.GetRecord(ctx, repo, collection, rkey)
		if err != nil {
			return ResolvedRecord{}, fmt.Errorf("getRecord failed: %s is not in the own repo: %w", ref.URI, err)
		}
		if rec.CID == "" {
			rec.CID = ref.CID
		}
		return rec, nil
	}
	if r.remote == nil {
		return ResolvedRecord{}, fmt.Errorf("%w: no remote record getter", ErrNotFound)
	}
	rec, err := r.remote.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return ResolvedRecord{}, err
	}
	if rec.CID == "" {
		rec.CID = ref.CID
	}
	return rec, nil
}

func (h *HTTPReader) Resolve(ctx context.Context, ref RecordRef) (ResolvedRecord, error) {
	aturi, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return ResolvedRecord{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	repo := string(aturi.Authority())
	collection := string(aturi.Collection())
	rkey := string(aturi.RecordKey())
	if repo == "" || collection == "" || rkey == "" {
		return ResolvedRecord{}, fmt.Errorf("%w: %q is not a record AT URI", ErrInvalidRef, ref.URI)
	}
	rec, err := h.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return ResolvedRecord{}, err
	}
	if rec.CID == "" {
		rec.CID = ref.CID
	}
	return rec, nil
}
