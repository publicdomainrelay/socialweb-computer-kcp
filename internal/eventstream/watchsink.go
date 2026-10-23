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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
)

const (
	maxWatchedRecords = 512

	maxDeletedRecords = 256

	flushTimeout = 10 * time.Second
)

type watchSink struct {
	ctl *Controller

	name string

	flush *debounce.Debounce

	mu sync.Mutex

	records map[string]v1alpha1.WatchedRecord

	deletes map[string]struct{}

	cursors map[string]int64
}

func (c *Controller) Sink(name string) events.Sink {
	name = strings.TrimSpace(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sinks[name]; s != nil {
		return s
	}
	s := &watchSink{ctl: c, name: name, records: map[string]v1alpha1.WatchedRecord{}, deletes: map[string]struct{}{}, cursors: map[string]int64{}}
	s.flush = debounce.New(c.opts.FlushDelay, s.flushNow)
	c.sinks[name] = s
	return s
}

func (s *watchSink) Deliver(ctx context.Context, event events.Event) error {
	repo, err := syntax.ParseDID(event.Repo)
	if err != nil {
		return fmt.Errorf("eventstream: the event repo %q is not a DID: %w", event.Repo, err)
	}
	var matches []resolvedMatch
	if slices.ContainsFunc(event.Ops, func(op events.Op) bool { return op.Action == "create" || op.Action == "update" }) {
		matches = s.ctl.watchMatches(ctx, s.name)
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
					s.ctl.log.Warn("a watched record op has an invalid cid", "uri", uri, "cid", op.CID)
					continue
				}
				expected = &parsed
			}
			path := pdsclient.RepoPath{Repo: repo, Collection: syntax.NSID(collection), Rkey: syntax.RecordKey(rkey)}
			record, err := s.ctl.getWatchedRecord(ctx, repo, path, expected)
			if err != nil {
				s.ctl.log.Warn("fetching a watched record", "uri", uri, "err", err.Error())
				continue
			}
			if record.URI.String() != uri {
				s.ctl.log.Warn("a watched record came back under another URI", "uri", uri, "got", record.URI.String())
				continue
			}
			if expected != nil && record.CID != *expected {
				s.ctl.log.Warn("a watched record came back with another CID", "uri", uri, "want", op.CID, "got", record.CID.String())
				continue
			}
			value := record.Value
			if !json.Valid(value) {
				value = json.RawMessage(`{}`)
			}
			if len(matches) > 0 && !matchesRecord(value, matches) {
				continue
			}
			records[uri] = v1alpha1.WatchedRecord{Did: event.Repo, URI: uri, Cid: record.CID.String(), Value: value, VerifiedAt: now}
		case "delete":
			deletes[uri] = struct{}{}
		}
	}
	s.mu.Lock()
	for uri, record := range records {
		s.records[uri] = record
		delete(s.deletes, uri)
	}
	for uri := range deletes {
		delete(s.records, uri)
		s.deletes[uri] = struct{}{}
	}
	if event.Source != "" {
		s.cursors[event.Source] = event.Seq
	}
	s.mu.Unlock()
	s.flush.Mark()
	return nil
}

func (s *watchSink) close() {
	s.flush.Stop()
	s.flushNow()
}

func (s *watchSink) flushNow() {
	s.mu.Lock()
	records, deletes, cursors := s.records, s.deletes, s.cursors
	if len(records) == 0 && len(deletes) == 0 && len(cursors) == 0 {
		s.mu.Unlock()
		return
	}
	s.records, s.deletes, s.cursors = map[string]v1alpha1.WatchedRecord{}, map[string]struct{}{}, map[string]int64{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := s.ctl.writeWatchStatus(ctx, s.name, records, deletes, cursors); err != nil {
		s.ctl.log.Warn("writing AtprotoRecordWatch status", "name", s.name, "err", err.Error())
		s.mu.Lock()
		for uri, record := range records {
			if _, ok := s.records[uri]; !ok {
				s.records[uri] = record
			}
		}
		for uri := range deletes {
			s.deletes[uri] = struct{}{}
		}
		for source, cursor := range cursors {
			if prior, ok := s.cursors[source]; !ok || cursor > prior {
				s.cursors[source] = cursor
			}
		}
		s.mu.Unlock()
		s.flush.Mark()
	}
}

func (c *Controller) watchMatches(ctx context.Context, name string) []resolvedMatch {
	dyn, err := c.opts.Dynamic(c.opts.Workspace)
	if err != nil {
		c.log.Warn("reading the AtprotoRecordWatch for its match", "name", name, "err", err.Error())
		return nil
	}
	object, err := dyn.Resource(RecordWatchGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.log.Warn("reading the AtprotoRecordWatch for its match", "name", name, "err", err.Error())
		}
		return nil
	}
	spec, _, err := decodeRecordWatch(object)
	if err != nil {
		c.log.Warn("decoding the AtprotoRecordWatch for its match", "name", name, "err", err.Error())
		return nil
	}
	matches, err := c.resolveMatches(ctx, dyn, spec)
	if err != nil {
		c.log.Warn("resolving the AtprotoRecordWatch match", "name", name, "err", err.Error())
		return nil
	}
	return matches
}

func (c *Controller) getWatchedRecord(ctx context.Context, did syntax.DID, path pdsclient.RepoPath, expected *cid.Cid) (pdsclient.Record, error) {
	pds, err := c.authorPDS(ctx, did)
	if err != nil {
		return pdsclient.Record{}, err
	}
	record, err := pds.GetRecord(ctx, path, expected)
	if err == nil || !pdsEndpointStale(err) {
		return record, err
	}
	c.forgetAuthorPDS(ctx, did)
	resolved, resolveErr := c.authorPDS(ctx, did)
	if resolveErr != nil {
		return pdsclient.Record{}, err
	}
	return resolved.GetRecord(ctx, path, expected)
}

func (c *Controller) authorPDS(ctx context.Context, did syntax.DID) (pdsclient.PDS, error) {
	if pds, ok := c.pdsCache.Get(did); ok && pds != nil {
		return pds, nil
	}
	ident, err := c.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("eventstream: resolving %s: %w", did, err)
	}
	endpoint := strings.TrimSpace(ident.PDSEndpoint())
	if endpoint == "" {
		return nil, fmt.Errorf("eventstream: %s declares no atproto_pds service", did)
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, DID: did, HTTPClient: c.http, Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	c.pdsCache.Set(did, pds)
	return pds, nil
}

func (c *Controller) forgetAuthorPDS(ctx context.Context, did syntax.DID) {
	c.pdsCache.Delete(did)
	_ = c.directory.Purge(ctx, did.AtIdentifier())
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

func (c *Controller) writeWatchStatus(ctx context.Context, name string, records map[string]v1alpha1.WatchedRecord, deletes map[string]struct{}, cursors map[string]int64) error {
	dyn, err := c.opts.Dynamic(c.opts.Workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(RecordWatchGVR)
	for range 5 {
		current, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.GetDeletionTimestamp() != nil {
			return nil
		}
		statusMap, _, _ := unstructured.NestedMap(current.Object, "status")
		var watch v1alpha1.AtprotoRecordWatchStatus
		if len(statusMap) > 0 {
			raw, err := json.Marshal(statusMap)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &watch); err != nil {
				return err
			}
		}
		for uri, record := range records {
			i := slices.IndexFunc(watch.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri })
			if i >= 0 {
				watch.Records[i] = record
			} else {
				watch.Records = append(watch.Records, record)
			}
		}
		for uri := range deletes {
			watch.Records = slices.DeleteFunc(watch.Records, func(r v1alpha1.WatchedRecord) bool { return r.URI == uri })
			if !slices.Contains(watch.Deleted, uri) {
				watch.Deleted = append(watch.Deleted, uri)
			}
		}
		if len(watch.Deleted) > maxDeletedRecords {
			watch.Deleted = watch.Deleted[len(watch.Deleted)-maxDeletedRecords:]
		}
		for source, cursor := range cursors {
			i := slices.IndexFunc(watch.Cursors, func(c v1alpha1.AtprotoRecordWatchCursor) bool { return c.Source == source })
			if i >= 0 {
				watch.Cursors[i].Cursor = cursor
			} else {
				watch.Cursors = append(watch.Cursors, v1alpha1.AtprotoRecordWatchCursor{Source: source, Cursor: cursor})
			}
		}
		watch.Records = boundWatchedRecords(watch.Records)
		object, err := toUnstructured(watch)
		if err != nil {
			return err
		}
		current.Object["status"] = object
		if _, err := ri.UpdateStatus(ctx, current, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("eventstream: the status of AtprotoRecordWatch %s stayed in conflict", name)
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
