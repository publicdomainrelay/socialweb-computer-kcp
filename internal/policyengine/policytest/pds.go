package policytest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
)

const (
	PLCURL = "https://plc.policytest.invalid"
	PDSURL = "https://pds.policytest.invalid"
)

type stored struct {
	cid   string
	value map[string]any
}

type World struct {
	mu       sync.Mutex
	records  map[string]stored
	down     bool
	failRepo map[string]bool
}

func NewWorld() *World {
	return &World{records: map[string]stored{}, failRepo: map[string]bool{}}
}

func URI(repo, collection, rkey string) string {
	return "at://" + repo + "/" + collection + "/" + rkey
}

func CIDOf(value map[string]any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return "bafypolicytest" + hex.EncodeToString(sum[:12])
}

func (w *World) Put(uri string, value map[string]any) string {
	cid := CIDOf(value)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records[uri] = stored{cid: cid, value: value}
	return cid
}

func (w *World) Delete(uri string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.records, uri)
}

func (w *World) SetDown(down bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.down = down
}

func (w *World) FailRepo(did string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failRepo[did] = true
}

func (w *World) Get(uri string) (string, map[string]any, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	record, ok := w.records[uri]
	return record.cid, record.value, ok
}

func (w *World) Client() *atproto.Client {
	return &atproto.Client{HTTP: &http.Client{Transport: w}, PLCURL: PLCURL}
}

func (w *World) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	down := w.down
	w.mu.Unlock()
	if down {
		return respond(req, http.StatusServiceUnavailable, map[string]any{"error": "Unavailable", "message": "the fake PDS is down"}), nil
	}
	target := req.URL.Scheme + "://" + req.URL.Host
	switch {
	case target == PLCURL:
		return respond(req, http.StatusOK, map[string]any{"service": []any{map[string]any{
			"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": PDSURL}}}), nil
	case target == PDSURL && req.URL.Path == "/xrpc/com.atproto.repo.getRecord":
		return w.getRecord(req), nil
	case target == PDSURL && req.URL.Path == "/xrpc/com.atproto.repo.listRecords":
		return w.listRecords(req), nil
	}
	return respond(req, http.StatusNotFound, map[string]any{"error": "NotFound"}), nil
}

func (w *World) getRecord(req *http.Request) *http.Response {
	query := req.URL.Query()
	repo := query.Get("repo")
	uri := URI(repo, query.Get("collection"), query.Get("rkey"))
	w.mu.Lock()
	failed := w.failRepo[repo]
	record, ok := w.records[uri]
	w.mu.Unlock()
	if failed {
		return respond(req, http.StatusInternalServerError, map[string]any{"error": "InternalServerError"})
	}
	if !ok {
		return respond(req, http.StatusBadRequest, map[string]any{"error": "RecordNotFound"})
	}
	return respond(req, http.StatusOK, map[string]any{"uri": uri, "cid": record.cid, "value": record.value})
}

func (w *World) listRecords(req *http.Request) *http.Response {
	query := req.URL.Query()
	prefix := URI(query.Get("repo"), query.Get("collection"), "")
	w.mu.Lock()
	out := []any{}
	keys := make([]string, 0, len(w.records))
	for uri := range w.records {
		if strings.HasPrefix(uri, prefix) {
			keys = append(keys, uri)
		}
	}
	sort.Strings(keys)
	for _, uri := range keys {
		out = append(out, map[string]any{"uri": uri, "cid": w.records[uri].cid, "value": w.records[uri].value})
	}
	w.mu.Unlock()
	return respond(req, http.StatusOK, map[string]any{"records": out})
}

func respond(req *http.Request, code int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
		Request:    req,
	}
}

func (w *World) GetRecord(ctx context.Context, repo, collection, rkey string) (market.ResolvedRecord, error) {
	uri := URI(repo, collection, rkey)
	if err := w.reachable(ctx, repo); err != nil {
		return market.ResolvedRecord{}, err
	}
	cid, value, ok := w.Get(uri)
	if !ok {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %s", market.ErrNotFound, uri)
	}
	return market.ResolvedRecord{URI: uri, CID: cid, Value: value}, nil
}

func (w *World) ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (market.ListPage, error) {
	if err := w.reachable(ctx, repo); err != nil {
		return market.ListPage{}, err
	}
	prefix := URI(repo, collection, "")
	w.mu.Lock()
	defer w.mu.Unlock()
	page := market.ListPage{}
	for uri, record := range w.records {
		if strings.HasPrefix(uri, prefix) {
			page.Records = append(page.Records, market.ListedRecord{URI: uri, CID: record.cid, Value: record.value})
		}
	}
	sort.Slice(page.Records, func(i, j int) bool { return page.Records[i].URI < page.Records[j].URI })
	return page, nil
}

func (w *World) Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error) {
	repo, collection, rkey := atproto.SplitURI(ref.URI)
	record, err := w.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	if ref.CID != "" && ref.CID != record.CID {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %s has cid %s, want %s", market.ErrInvalidRef, ref.URI, record.CID, ref.CID)
	}
	return record, nil
}

func (w *World) reachable(ctx context.Context, repo string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down || w.failRepo[repo] {
		return fmt.Errorf("the fake PDS for %s is down", url.PathEscape(repo))
	}
	return nil
}
