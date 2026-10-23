package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
)

const (
	maxWatchedRecords = 512

	maxDeletedRecords = 256
)

var ErrMatchPending = errors.New("eventstream: the record watch match is not resolved")

type MatchesFunc func() ([]Match, bool)

type Pending struct {
	Records map[string]v1alpha1.WatchedRecord

	Deletes map[string]struct{}

	Cursors map[string]int64
}

func newPending() Pending {
	return Pending{Records: map[string]v1alpha1.WatchedRecord{}, Deletes: map[string]struct{}{}, Cursors: map[string]int64{}}
}

type Sink struct {
	w *Watches

	name string

	matches MatchesFunc

	flush *debounce.Debounce

	mu sync.Mutex

	pending Pending
}

var _ events.Sink = (*Sink)(nil)

func (w *Watches) NewSink(name string, matches MatchesFunc, flushed func()) *Sink {
	s := &Sink{w: w, name: strings.TrimSpace(name), matches: matches, pending: newPending()}
	s.flush = debounce.New(w.flushDelay, flushed)
	return s
}

func (s *Sink) Deliver(ctx context.Context, event events.Event) error {
	repo, err := syntax.ParseDID(event.Repo)
	if err != nil {
		return fmt.Errorf("eventstream: the event repo %q is not a DID: %w", event.Repo, err)
	}
	var matches []Match
	if slices.ContainsFunc(event.Ops, func(op events.Op) bool { return op.Action == "create" || op.Action == "update" }) {
		var ok bool
		if matches, ok = s.matches(); !ok {
			return ErrMatchPending
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	records := map[string]v1alpha1.WatchedRecord{}
	deletes := map[string]struct{}{}
	for _, op := range event.Ops {
		collection, rkey, ok := splitPath(op.Path)
		if !ok {
			continue
		}
		uri := "at://" + event.Repo + "/" + collection + "/" + rkey
		switch strings.ToLower(op.Action) {
		case "create", "update":
			var expected *cid.Cid
			if op.CID != "" {
				parsed, err := cid.Decode(op.CID)
				if err != nil {
					s.w.log.Warn("a watched record op has an invalid cid", "uri", uri, "cid", op.CID)
					continue
				}
				expected = &parsed
			}
			path := pdsclient.RepoPath{Repo: repo, Collection: syntax.NSID(collection), Rkey: syntax.RecordKey(rkey)}
			record, err := s.w.getWatchedRecord(ctx, repo, path, expected)
			if err != nil {
				s.w.log.Warn("fetching a watched record", "uri", uri, "err", err.Error())
				continue
			}
			if record.URI.String() != uri {
				s.w.log.Warn("a watched record came back under another URI", "uri", uri, "got", record.URI.String())
				continue
			}
			if expected != nil && record.CID != *expected {
				s.w.log.Warn("a watched record came back with another CID", "uri", uri, "want", op.CID, "got", record.CID.String())
				continue
			}
			value := record.Value
			if !json.Valid(value) {
				value = json.RawMessage(`{}`)
			}
			if len(matches) > 0 && !MatchesRecord(value, matches) {
				continue
			}
			records[uri] = v1alpha1.WatchedRecord{Did: event.Repo, URI: uri, Cid: record.CID.String(), Value: value, VerifiedAt: now}
		case "delete":
			deletes[uri] = struct{}{}
		}
	}
	s.mu.Lock()
	for uri, record := range records {
		s.pending.Records[uri] = record
		delete(s.pending.Deletes, uri)
	}
	for uri := range deletes {
		delete(s.pending.Records, uri)
		s.pending.Deletes[uri] = struct{}{}
	}
	if event.Source != "" {
		s.pending.Cursors[event.Source] = event.Seq
	}
	s.mu.Unlock()
	s.flush.Mark()
	return nil
}

func (s *Sink) Take() Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = newPending()
	return out
}

func (s *Sink) Close() {
	s.flush.Stop()
}

func MergeStatus(status v1alpha1.AtprotoRecordWatchStatus, p Pending) v1alpha1.AtprotoRecordWatchStatus {
	status.Records = slices.Clone(status.Records)
	status.Deleted = slices.Clone(status.Deleted)
	status.Cursors = slices.Clone(status.Cursors)
	for uri, record := range p.Records {
		i := slices.IndexFunc(status.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri })
		if i >= 0 {
			status.Records[i] = record
		} else {
			status.Records = append(status.Records, record)
		}
	}
	for uri := range p.Deletes {
		status.Records = slices.DeleteFunc(status.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri })
		if !slices.Contains(status.Deleted, uri) {
			status.Deleted = append(status.Deleted, uri)
		}
	}
	if len(status.Deleted) > maxDeletedRecords {
		status.Deleted = status.Deleted[len(status.Deleted)-maxDeletedRecords:]
	}
	for source, cursor := range p.Cursors {
		i := slices.IndexFunc(status.Cursors, func(c v1alpha1.AtprotoRecordWatchCursor) bool { return c.Source == source })
		if i >= 0 {
			status.Cursors[i].Cursor = cursor
		} else {
			status.Cursors = append(status.Cursors, v1alpha1.AtprotoRecordWatchCursor{Source: source, Cursor: cursor})
		}
	}
	status.Records = boundWatchedRecords(status.Records)
	return status
}

func (w *Watches) getWatchedRecord(ctx context.Context, did syntax.DID, path pdsclient.RepoPath, expected *cid.Cid) (pdsclient.Record, error) {
	pds, err := w.authorPDS(ctx, did)
	if err != nil {
		return pdsclient.Record{}, err
	}
	record, err := pds.GetRecord(ctx, path, expected)
	if err == nil || !pdsEndpointStale(err) {
		return record, err
	}
	w.forgetAuthorPDS(ctx, did)
	resolved, resolveErr := w.authorPDS(ctx, did)
	if resolveErr != nil {
		return pdsclient.Record{}, err
	}
	return resolved.GetRecord(ctx, path, expected)
}

func (w *Watches) authorPDS(ctx context.Context, did syntax.DID) (pdsclient.PDS, error) {
	if pds, ok := w.pdsCache.Get(did); ok && pds != nil {
		return pds, nil
	}
	ident, err := w.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("eventstream: resolving %s: %w", did, err)
	}
	endpoint := strings.TrimSpace(ident.PDSEndpoint())
	if endpoint == "" {
		return nil, fmt.Errorf("eventstream: %s declares no atproto_pds service", did)
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, DID: did, HTTPClient: w.http, Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	w.pdsCache.Set(did, pds)
	return pds, nil
}

func (w *Watches) forgetAuthorPDS(ctx context.Context, did syntax.DID) {
	w.pdsCache.Delete(did)
	_ = w.directory.Purge(ctx, did.AtIdentifier())
}

func pdsEndpointStale(err error) bool {
	var xrpc *pdsclient.XRPCError
	if !errors.As(err, &xrpc) {
		return false
	}
	switch xrpc.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

func boundWatchedRecords(records []v1alpha1.WatchedRecord) []v1alpha1.WatchedRecord {
	if len(records) <= maxWatchedRecords {
		return records
	}
	ordered := append([]v1alpha1.WatchedRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].VerifiedAt > ordered[j].VerifiedAt })
	return ordered[:maxWatchedRecords]
}

func toUnstructured(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	object := map[string]any{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func splitPath(path string) (string, string, bool) {
	collection, rkey, ok := strings.Cut(path, "/")
	if !ok || collection == "" || rkey == "" {
		return "", "", false
	}
	return collection, rkey, true
}
