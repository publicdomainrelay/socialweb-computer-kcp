package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

type eventStream struct {
	ctl  *Controller
	es   *eventsv1alpha1.EventStream
	dyn  dynamic.Interface
	once sync.Mutex

	mu          sync.Mutex
	started     bool
	cursor      int64
	lag         int64
	lastEventAt *metav1.Time
	phase       eventsv1alpha1.EventStreamPhase
	ready       bool
	lagging     bool
	failed      bool
	message     string

	records      map[string]v1alpha1.WatchedRecord
	deletes      map[string]struct{}
	watchCursors map[string]int64
	watchDirty   bool

	hasStatus  bool
	lastStatus eventsv1alpha1.EventStreamStatus
}

func newEventStream(ctl *Controller, es *eventsv1alpha1.EventStream, dyn dynamic.Interface) *eventStream {
	return &eventStream{
		ctl:          ctl,
		es:           es,
		dyn:          dyn,
		phase:        eventsv1alpha1.EventStreamPhasePending,
		records:      map[string]v1alpha1.WatchedRecord{},
		deletes:      map[string]struct{}{},
		watchCursors: map[string]int64{},
	}
}

func (s *eventStream) run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		s.flushLoop(runCtx)
	}()
	defer func() { <-flushed }()

	if s.es.Spec.Suspend {
		s.set(eventsv1alpha1.EventStreamPhaseSuspended, false, false, false, "suspended")
		s.publish(ctx)
		return
	}
	s.set(eventsv1alpha1.EventStreamPhasePending, false, false, false, "starting")
	s.publish(ctx)

	delay := s.ctl.opts.ReconnectDelay
	for runCtx.Err() == nil {
		pds, relayBase, source, err := s.resolveSource(runCtx)
		if err != nil {
			s.set(eventsv1alpha1.EventStreamPhaseFailed, false, false, true, err.Error())
			s.publish(ctx)
			if !sleep(runCtx, delay) {
				return
			}
			delay = nextDelay(delay, s.ctl.opts.MaxReconnectWait)
			continue
		}
		var stream repoStream
		if relayBase != "" {
			stream, err = openRelayStream(runCtx, relayBase, s.nextCursor(), s.ctl.opts.Resolver.Handlers)
		} else {
			stream, err = pds.SubscribeRepos(runCtx, s.nextCursor())
		}
		if err != nil {
			s.set(eventsv1alpha1.EventStreamPhaseFailed, false, false, true, err.Error())
			s.publish(ctx)
			if !sleep(runCtx, delay) {
				return
			}
			delay = nextDelay(delay, s.ctl.opts.MaxReconnectWait)
			continue
		}
		delay = s.ctl.opts.ReconnectDelay
		s.mu.Lock()
		s.phase = eventsv1alpha1.EventStreamPhaseRunning
		s.ready = true
		s.failed = false
		s.message = ""
		s.mu.Unlock()
		s.publish(ctx)
		for {
			event, err := stream.Next(runCtx)
			if err == nil {
				s.handle(runCtx, pds, source, event)
				continue
			}
			_ = stream.Close()
			if runCtx.Err() != nil {
				return
			}
			s.set(eventsv1alpha1.EventStreamPhaseFailed, false, false, true, err.Error())
			s.publish(ctx)
			break
		}
		if !sleep(runCtx, delay) {
			return
		}
		delay = nextDelay(delay, s.ctl.opts.MaxReconnectWait)
	}
}

func (s *eventStream) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(s.ctl.opts.StatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.publish(ctx)
		}
	}
}

func (s *eventStream) resolveSource(ctx context.Context) (pdsclient.PDS, string, string, error) {
	source := s.es.Spec.Source
	switch eventsv1alpha1.EventStreamSourceKind(strings.ToLower(strings.TrimSpace(string(source.Kind)))) {
	case eventsv1alpha1.EventStreamSourcePDS:
		ref := source.PDSRef
		if ref == nil {
			return nil, "", "", errors.New("eventstream: source.kind pds needs pdsRef")
		}
		if strings.TrimSpace(ref.Name) == "" {
			return nil, "", "", errors.New("eventstream: pdsRef needs a name")
		}
		if strings.TrimSpace(ref.Kind) == "" {
			return nil, "", "", errors.New("eventstream: pdsRef needs a kind")
		}
		kind := strings.ToLower(strings.TrimSpace(ref.Kind))
		if kind == "remotepds" || kind == "remote" {
			workspace := strings.TrimSpace(ref.Workspace)
			if workspace == "" {
				workspace = s.ctl.opts.Workspace
			}
			dyn, err := s.ctl.opts.Dynamic(workspace)
			if err != nil {
				return nil, "", "", err
			}
			object, err := dyn.Resource(RemotePDSGVR).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return nil, "", "", fmt.Errorf("eventstream: reading RemotePDS %s: %w", ref.Name, err)
			}
			var remote pdsv1alpha1.RemotePDS
			raw, err := json.Marshal(object.Object)
			if err != nil {
				return nil, "", "", err
			}
			if err := json.Unmarshal(raw, &remote); err != nil {
				return nil, "", "", fmt.Errorf("eventstream: decoding RemotePDS %s: %w", ref.Name, err)
			}
			pds, err := s.ctl.opts.Resolver.Resolve(pdsclient.CRRef{RemotePDS: &remote})
			if err != nil {
				return nil, "", "", err
			}
			return pds, "", "pds/" + ref.Name, nil
		}
		pds, err := s.ctl.opts.Resolver.Resolve(pdsclient.LocalRef{Workspace: ref.Workspace, Kind: ref.Kind, Name: ref.Name})
		if err != nil {
			return nil, "", "", err
		}
		return pds, "", "pds/" + ref.Name, nil
	case eventsv1alpha1.EventStreamSourceRelay:
		ref := source.RelayRef
		if ref == nil {
			return nil, "", "", errors.New("eventstream: source.kind relay needs relayRef")
		}
		base, err := relayBaseURL(*ref)
		if err != nil {
			return nil, "", "", err
		}
		return nil, base, "relay/" + strings.TrimSpace(ref.Name), nil
	default:
		return nil, "", "", fmt.Errorf("eventstream: unknown source kind %q", source.Kind)
	}
}

func (s *eventStream) nextCursor() *int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		value := s.cursor
		return &value
	}
	if s.es.Spec.Cursor != nil {
		value := *s.es.Spec.Cursor
		return &value
	}
	if s.es.Status.Cursor > 0 {
		value := s.es.Status.Cursor
		return &value
	}
	return nil
}

func (s *eventStream) handle(ctx context.Context, pds pdsclient.PDS, source string, event pdsclient.RepoEvent) {
	when := event.Time
	if when.IsZero() {
		when = time.Now().UTC()
	}
	stamp := metav1.NewTime(when)
	ops := filterOps(event.Ops, s.es.Spec.Source.Collections)
	if !allowedDID(event.Repo.String(), s.es.Spec.Source.DIDs) {
		ops = nil
	}
	s.mu.Lock()
	if s.started && event.Seq > s.cursor+1 {
		s.lag = event.Seq - s.cursor - 1
	} else {
		s.lag = 0
	}
	s.started = true
	s.cursor = event.Seq
	s.lastEventAt = &stamp
	s.lagging = s.lag > 0
	s.mu.Unlock()
	if len(ops) == 0 {
		return
	}
	switch s.es.Spec.Sink.Kind {
	case eventsv1alpha1.EventStreamSinkHandler:
		s.deliverHandler(ctx, source, event, when, ops)
	case eventsv1alpha1.EventStreamSinkRecordWatch:
		if pds == nil {
			s.set(eventsv1alpha1.EventStreamPhaseFailed, s.isReady(), s.isLagging(), true,
				"the atprotoRecordWatch sink needs a pds source to read records")
			return
		}
		s.bufferRecords(ctx, pds, source, event, ops)
	default:
		s.set(eventsv1alpha1.EventStreamPhaseFailed, s.isReady(), s.isLagging(), true,
			fmt.Sprintf("unknown sink kind %q", s.es.Spec.Sink.Kind))
	}
}

func (s *eventStream) deliverHandler(ctx context.Context, source string, event pdsclient.RepoEvent, when time.Time, ops []pdsclient.RepoOp) {
	name := strings.TrimSpace(s.es.Spec.Sink.Name)
	handler, ok := s.ctl.handlers.Lookup(name)
	if !ok {
		s.set(eventsv1alpha1.EventStreamPhaseFailed, s.isReady(), s.isLagging(), true, "the handler "+name+" is not registered")
		return
	}
	delivered := Event{
		Source: source,
		Seq:    event.Seq,
		Repo:   event.Repo,
		Commit: event.Commit.String(),
		Rev:    event.Rev.String(),
		Time:   when,
		Ops:    ops,
	}
	if err := handler.HandleEvent(ctx, delivered); err != nil {
		s.set(eventsv1alpha1.EventStreamPhaseFailed, s.isReady(), s.isLagging(), true, "the handler "+name+": "+err.Error())
		return
	}
	if s.isFailed() {
		s.set(eventsv1alpha1.EventStreamPhaseRunning, true, s.isLagging(), false, "")
	}
}

func (s *eventStream) bufferRecords(ctx context.Context, pds pdsclient.PDS, source string, event pdsclient.RepoEvent, ops []pdsclient.RepoOp) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	records := map[string]v1alpha1.WatchedRecord{}
	deletes := map[string]struct{}{}
	for _, op := range ops {
		collection, rkey, ok := splitPath(op.Path)
		if !ok {
			continue
		}
		uri := "at://" + event.Repo.String() + "/" + collection + "/" + rkey
		switch strings.ToLower(op.Action) {
		case "create", "update":
			record, err := pds.GetRecord(ctx, pdsclient.RepoPath{Repo: event.Repo, Collection: syntax.NSID(collection), Rkey: syntax.RecordKey(rkey)}, nil)
			if err != nil {
				s.ctl.log.Warn("fetching a watched record", "uri", uri, "err", err.Error())
				continue
			}
			value := record.Value
			if !json.Valid(value) {
				value = json.RawMessage(`{}`)
			}
			records[uri] = v1alpha1.WatchedRecord{
				Did:        event.Repo.String(),
				Uri:        uri,
				Cid:        record.CID.String(),
				Value:      value,
				VerifiedAt: now,
			}
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
	s.watchCursors[source] = event.Seq
	s.watchDirty = true
	s.mu.Unlock()
}

func (s *eventStream) publish(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	status, changed := s.snapshot()
	if changed {
		if err := writeEventStreamStatus(ctx, s.dyn.Resource(EventStreamGVR), s.es.Name, status); err != nil && ctx.Err() == nil {
			s.ctl.log.Warn("writing EventStream status", "name", s.es.Name, "err", err.Error())
		} else if err == nil {
			s.mu.Lock()
			s.hasStatus = true
			s.lastStatus = status
			s.mu.Unlock()
		}
	}
	s.flushWatch(ctx)
}

func (s *eventStream) snapshot() (eventsv1alpha1.EventStreamStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := eventsv1alpha1.EventStreamStatus{
		ObservedGeneration: s.es.Generation,
		Phase:              s.phase,
		Cursor:             s.cursor,
		Lag:                s.lag,
		LastEventAt:        s.lastEventAt,
	}
	status.Conditions = s.conditionsLocked(append([]metav1.Condition(nil), s.lastStatus.Conditions...))
	if s.hasStatus && reflect.DeepEqual(status, s.lastStatus) {
		return status, false
	}
	return status, true
}

func (s *eventStream) conditionsLocked(conditions []metav1.Condition) []metav1.Condition {
	readyReason := "StreamClosed"
	if s.ready {
		readyReason = "StreamOpen"
	}
	if s.phase == eventsv1alpha1.EventStreamPhaseSuspended {
		readyReason = "Suspended"
	}
	setCondition(&conditions, eventsv1alpha1.EventStreamConditionReady, s.ready, readyReason, s.message)
	lagReason := "UpToDate"
	lagMessage := "the stream is current"
	if s.lag > 0 {
		lagReason = "CursorGap"
		lagMessage = fmt.Sprintf("%d events behind", s.lag)
	}
	setCondition(&conditions, eventsv1alpha1.EventStreamConditionLagging, s.lagging, lagReason, lagMessage)
	failedReason := "NoError"
	failedMessage := "the stream is healthy"
	if s.failed {
		failedReason = "StreamError"
		failedMessage = s.message
	}
	setCondition(&conditions, eventsv1alpha1.EventStreamConditionFailed, s.failed, failedReason, failedMessage)
	for i := range conditions {
		conditions[i].ObservedGeneration = s.es.Generation
	}
	return conditions
}

func (s *eventStream) set(phase eventsv1alpha1.EventStreamPhase, ready, lagging, failed bool, message string) {
	s.mu.Lock()
	s.phase = phase
	s.ready = ready
	s.lagging = lagging
	s.failed = failed
	s.message = message
	s.mu.Unlock()
}

func (s *eventStream) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *eventStream) isLagging() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lagging
}

func (s *eventStream) isFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func (s *eventStream) flushWatch(ctx context.Context) {
	s.mu.Lock()
	if !s.watchDirty {
		s.mu.Unlock()
		return
	}
	records := s.records
	deletes := s.deletes
	cursors := s.watchCursors
	s.records = map[string]v1alpha1.WatchedRecord{}
	s.deletes = map[string]struct{}{}
	s.watchCursors = map[string]int64{}
	s.watchDirty = false
	s.mu.Unlock()

	workspace := strings.TrimSpace(s.es.Spec.Sink.Namespace)
	if workspace == "" {
		workspace = s.ctl.opts.Workspace
	}
	err := s.writeWatchStatus(ctx, workspace, records, deletes, cursors)
	if err != nil && ctx.Err() == nil {
		s.ctl.log.Warn("writing AtprotoRecordWatch status", "name", s.es.Spec.Sink.Name, "err", err.Error())
		s.mu.Lock()
		for uri, record := range records {
			if _, exists := s.records[uri]; !exists {
				s.records[uri] = record
			}
		}
		for uri := range deletes {
			s.deletes[uri] = struct{}{}
		}
		for source, cursor := range cursors {
			if prior, ok := s.watchCursors[source]; !ok || cursor > prior {
				s.watchCursors[source] = cursor
			}
		}
		s.watchDirty = true
		s.mu.Unlock()
	}
}

func (s *eventStream) writeWatchStatus(ctx context.Context, workspace string, records map[string]v1alpha1.WatchedRecord, deletes map[string]struct{}, cursors map[string]int64) error {
	dyn, err := s.ctl.opts.Dynamic(workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(RecordWatchGVR)
	for range 5 {
		current, err := ri.Get(ctx, s.es.Spec.Sink.Name, metav1.GetOptions{})
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
			replaced := false
			for i := range watch.Records {
				if watch.Records[i].Uri == uri {
					watch.Records[i] = record
					replaced = true
					break
				}
			}
			if !replaced {
				watch.Records = append(watch.Records, record)
			}
		}
		for uri := range deletes {
			kept := watch.Records[:0]
			for _, record := range watch.Records {
				if record.Uri != uri {
					kept = append(kept, record)
				}
			}
			watch.Records = kept
		}
		for source, cursor := range cursors {
			replaced := false
			for i := range watch.Cursors {
				if watch.Cursors[i].Source == source {
					watch.Cursors[i].Cursor = cursor
					replaced = true
					break
				}
			}
			if !replaced {
				watch.Cursors = append(watch.Cursors, v1alpha1.AtprotoRecordWatchCursor{Source: source, Cursor: cursor})
			}
		}
		raw, err := json.Marshal(watch)
		if err != nil {
			return err
		}
		object := map[string]any{}
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		current.Object["status"] = object
		if _, err := ri.UpdateStatus(ctx, current, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("eventstream: the status of AtprotoRecordWatch %s stayed in conflict", s.es.Spec.Sink.Name)
}

func writeEventStreamStatus(ctx context.Context, ri dynamic.ResourceInterface, name string, status eventsv1alpha1.EventStreamStatus) error {
	object, err := toUnstructured(status)
	if err != nil {
		return err
	}
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
		current.Object["status"] = object
		if _, err := ri.UpdateStatus(ctx, current, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("eventstream: the status of %s stayed in conflict", name)
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

func setCondition(conditions *[]metav1.Condition, kind string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message})
}

func filterOps(ops []pdsclient.RepoOp, collections []string) []pdsclient.RepoOp {
	if len(collections) == 0 {
		return ops
	}
	allowed := map[string]bool{}
	for _, collection := range collections {
		allowed[strings.TrimSpace(collection)] = true
	}
	var out []pdsclient.RepoOp
	for _, op := range ops {
		collection, _, ok := splitPath(op.Path)
		if ok && allowed[collection] {
			out = append(out, op)
		}
	}
	return out
}

func splitPath(path string) (string, string, bool) {
	collection, rkey, ok := strings.Cut(path, "/")
	if !ok || collection == "" || rkey == "" {
		return "", "", false
	}
	return collection, rkey, true
}

func allowedDID(repo string, dids []string) bool {
	if len(dids) == 0 {
		return true
	}
	for _, did := range dids {
		if strings.TrimSpace(did) == repo {
			return true
		}
	}
	return false
}

func nextDelay(current, maximum time.Duration) time.Duration {
	next := current * 2
	if next > maximum {
		return maximum
	}
	return next
}
