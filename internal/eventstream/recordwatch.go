package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	RecordWatchLabel = "events.socialweb.computer/atprotorecordwatch"

	RecordWatchConditionReady       = "Ready"
	RecordWatchConditionProgressing = "Progressing"
	RecordWatchConditionFailed      = "Failed"

	EventStreamNamePrefix = "watch-"
	maxEventStreamName    = 253

	RecordWatchConditionVerified = v1alpha1.ConditionVerified

	watchVerifyInterval = 5 * time.Minute
	watchVerifyRetry    = 30 * time.Second
)

func EventStreamNameForWatch(name string) string {
	candidate := EventStreamNamePrefix + name
	if len(candidate) <= maxEventStreamName {
		return candidate
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:4])
	keep := maxEventStreamName - len(suffix) - 1
	return candidate[:keep] + "-" + suffix
}

func (c *Controller) runRecordWatches(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.recordWatchRound(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching AtprotoRecordWatches", "workspace", c.opts.Workspace, "err", err.Error())
		}
		if !clock.Sleep(ctx, opt.Positive(c.opts.RetryAfter, DefaultRetryAfter)) {
			return
		}
	}
}

func (c *Controller) recordWatchRound(ctx context.Context) error {
	dyn, err := c.opts.Dynamic(c.opts.Workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(RecordWatchGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	watches := map[string]bool{}
	for i := range list.Items {
		item := &list.Items[i]
		watches[item.GetName()] = true
		if err := c.reconcileRecordWatch(ctx, dyn, item); err != nil && ctx.Err() == nil {
			c.log.Warn("reconciling an AtprotoRecordWatch", "name", item.GetName(), "err", err.Error())
		}
	}
	c.gcOrphanEventStreams(ctx, dyn, watches)
	wc, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wc.Stop()
	timer := time.NewTimer(c.opts.RetryAfter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case event, ok := <-wc.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Bookmark {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			if event.Type == watch.Deleted {
				c.gcRecordWatch(ctx, dyn, u.GetName())
				continue
			}
			if err := c.reconcileRecordWatch(ctx, dyn, u); err != nil && ctx.Err() == nil {
				c.log.Warn("reconciling an AtprotoRecordWatch", "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func (c *Controller) reconcileRecordWatch(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	ctx = bus.WithObject(ctx, c.opts.Workspace, u)
	name := u.GetName()
	spec, status, err := decodeRecordWatch(u)
	if err != nil {
		return err
	}
	plans, err := c.planWatchSources(ctx, dyn, name, spec)
	if err != nil {
		c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
			conds.New(RecordWatchConditionReady, false, "InvalidSpec", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionProgressing, false, "InvalidSpec", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionFailed, true, "InvalidSpec", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionVerified, false, "InvalidSpec", err.Error(), u.GetGeneration()),
		})
		return err
	}
	matches, err := c.resolveMatches(ctx, dyn, spec)
	if err != nil {
		c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
			conds.New(RecordWatchConditionReady, false, "MatchPending", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionProgressing, true, "MatchPending", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionFailed, false, "NoError", err.Error(), u.GetGeneration()),
			conds.New(RecordWatchConditionVerified, false, "MatchPending", err.Error(), u.GetGeneration()),
		})
		return err
	}
	wanted := map[string]bool{}
	for _, plan := range plans {
		wanted[plan.Name] = true
		if err := c.ensureWatchEventStream(ctx, dyn, name, plan, status); err != nil {
			c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
				conds.New(RecordWatchConditionReady, false, "ReconcileFailed", err.Error(), u.GetGeneration()),
				conds.New(RecordWatchConditionProgressing, false, "ReconcileFailed", err.Error(), u.GetGeneration()),
				conds.New(RecordWatchConditionFailed, true, "ReconcileFailed", err.Error(), u.GetGeneration()),
				conds.New(RecordWatchConditionVerified, false, "ReconcileFailed", err.Error(), u.GetGeneration()),
			})
			return err
		}
	}
	c.gcStaleEventStreams(ctx, dyn, name, wanted)
	verified, verifyMessage := c.verifyWatchRecords(ctx, dyn, name, u.GetGeneration(), status, matches)
	conditions, err := c.watchConditions(ctx, dyn, plans, u.GetGeneration())
	if err != nil {
		return err
	}
	streamReady := false
	if ready := meta.FindStatusCondition(conditions, RecordWatchConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
		streamReady = true
	}
	verifyReason := "Verified"
	if verified && streamReady {
		verifyReason = "Verified"
	} else if !streamReady {
		verified = false
		verifyReason = "StreamNotReady"
		verifyMessage = "the EventStreams are starting"
	} else {
		verifyReason = "VerifyFailed"
	}
	conditions = append(conditions, conds.New(RecordWatchConditionVerified, verified, verifyReason, verifyMessage, u.GetGeneration()))
	return c.writeRecordWatchConditions(ctx, dyn, name, conditions)
}

func (c *Controller) verifyWatchRecords(ctx context.Context, dyn dynamic.Interface, name string, generation int64,
	status recordWatchStatus, matches []resolvedMatch) (bool, string) {
	key := name + "/" + fmt.Sprint(generation)
	if !c.verifyAt.Add(key, struct{}{}, 0) {
		c.verifyAt.Set(key, struct{}{})
		if existing := meta.FindStatusCondition(status.Conditions, RecordWatchConditionVerified); existing != nil {
			return existing.Status == metav1.ConditionTrue, existing.Message
		}
		return true, "the watched records are verified"
	}
	records := make([]watchedRecord, 0, len(status.Records))
	var problems []string
	changed := false
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, record := range status.Records {
		uri, err := syntax.ParseATURI(strings.TrimSpace(record.Uri))
		if err != nil {
			changed = true
			continue
		}
		did, err := syntax.ParseDID(strings.TrimSpace(record.Did))
		if err != nil || uri.Authority().String() != did.String() {
			changed = true
			continue
		}
		pds, err := c.authorPDS(ctx, did)
		if err != nil {
			problems = append(problems, record.Uri+": "+err.Error())
			records = append(records, record)
			continue
		}
		fetched, err := pds.GetRecord(ctx, pdsclient.RepoPath{Repo: did, Collection: uri.Collection(), Rkey: uri.RecordKey()}, nil)
		if err != nil {
			var xe *pdsclient.XRPCError
			if errors.As(err, &xe) && (xe.Code == "RecordNotFound" || xe.Code == "RepoNotFound" || xe.StatusCode == http.StatusNotFound) {
				changed = true
				continue
			}
			problems = append(problems, record.Uri+": "+err.Error())
			records = append(records, record)
			continue
		}
		if fetched.URI.String() != record.Uri || !matchesRecord(fetched.Value, matches) {
			changed = true
			continue
		}
		if record.Cid == fetched.CID.String() && reflect.DeepEqual(record.Value, fetched.Value) {
			records = append(records, record)
			continue
		}
		changed = true
		record.Cid = fetched.CID.String()
		record.Value = fetched.Value
		record.VerifiedAt = now
		records = append(records, record)
	}
	if changed {
		if err := c.writeWatchRecords(ctx, dyn, name, records); err != nil {
			c.log.Warn("writing the re-verified AtprotoRecordWatch records", "name", name, "err", err.Error())
		}
	}
	if len(problems) > 0 {
		c.verifyAt.SetTTL(key, struct{}{}, watchVerifyRetry)
		return false, "the watched records could not be verified: " + strings.Join(problems, "; ")
	}
	return true, fmt.Sprintf("%d watched record(s) verified from their author PDS", len(records))
}

func (c *Controller) writeWatchRecords(ctx context.Context, dyn dynamic.Interface, name string, records []watchedRecord) error {
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
		var status recordWatchStatus
		if len(statusMap) > 0 {
			raw, err := json.Marshal(statusMap)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				return err
			}
		}
		if reflect.DeepEqual(status.Records, records) {
			return nil
		}
		status.Records = records
		raw, err := json.Marshal(status)
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
	return fmt.Errorf("eventstream: the re-verified status of AtprotoRecordWatch %s stayed in conflict", name)
}

func (c *Controller) ensureWatchEventStream(ctx context.Context, dyn dynamic.Interface, name string, plan watchSourcePlan, status recordWatchStatus) error {
	if err := c.ensureWatchEventStreamObject(ctx, dyn, name, plan, status); err != nil {
		return err
	}
	stream, err := dyn.Resource(EventStreamGVR).Get(ctx, plan.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return EnsureEventSource(ctx, DynamicResources(dyn), FieldManager, stream, plan.Source)
}

func (c *Controller) ensureWatchEventStreamObject(ctx context.Context, dyn dynamic.Interface, name string, plan watchSourcePlan, status recordWatchStatus) error {
	eventStreamName := plan.Name
	desiredSpec := eventsapi.EventStreamSpec{
		SourceRef: SourceRef(plan.Name),
		Filter:    plan.Filter,
		Sink: eventsapi.EventStreamSink{
			Kind:      eventsapi.EventStreamSinkRecordWatch,
			Name:      name,
			Namespace: c.opts.Workspace,
		},
	}
	if cursor := watchCursorFromStatus(status, plan.Key); cursor != nil {
		desiredSpec.Cursor = cursor
	}
	labels := map[string]string{RecordWatchLabel: name}
	ri := dyn.Resource(EventStreamGVR)
	if _, err := ri.Get(ctx, eventStreamName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		desired := &eventsapi.EventStream{
			TypeMeta:   metav1.TypeMeta{APIVersion: GroupName + "/" + Version, Kind: "EventStream"},
			ObjectMeta: metav1.ObjectMeta{Name: eventStreamName, Labels: labels},
			Spec:       desiredSpec,
		}
		object, err := toUnstructured(desired)
		if err != nil {
			return err
		}
		if _, err := ri.Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("eventstream: creating the EventStream for AtprotoRecordWatch %s: %w", name, err)
			}
		} else {
			return nil
		}
	} else if err != nil {
		return err
	}
	for range 5 {
		existing, err := ri.Get(ctx, eventStreamName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		specMap, _, _ := unstructured.NestedMap(existing.Object, "spec")
		var currentSpec eventsapi.EventStreamSpec
		if len(specMap) > 0 {
			raw, err := json.Marshal(specMap)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &currentSpec); err != nil {
				return err
			}
		}
		desiredSpec.Cursor = currentSpec.Cursor
		desiredSpec.Suspend = currentSpec.Suspend
		desiredMap, err := toUnstructured(desiredSpec)
		if err != nil {
			return err
		}
		changed := !reflect.DeepEqual(specMap, desiredMap)
		if existing.GetLabels()[RecordWatchLabel] != name {
			existing.SetLabels(mergeLabels(existing.GetLabels(), labels))
			changed = true
		}
		if !changed {
			return nil
		}
		existing.Object["spec"] = desiredMap
		if _, err := ri.Update(ctx, existing, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return fmt.Errorf("eventstream: updating the EventStream for AtprotoRecordWatch %s: %w", name, err)
		}
	}
	return fmt.Errorf("eventstream: the EventStream for AtprotoRecordWatch %s stayed in conflict", name)
}

func (c *Controller) watchConditions(ctx context.Context, dyn dynamic.Interface, plans []watchSourcePlan, generation int64) ([]metav1.Condition, error) {
	ri := dyn.Resource(EventStreamGVR)
	var latestFailed *metav1.Condition
	readyCount := 0
	starting := false
	for _, plan := range plans {
		u, err := ri.Get(ctx, plan.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			starting = true
			continue
		}
		if err != nil {
			return nil, err
		}
		statusMap, _, _ := unstructured.NestedMap(u.Object, "status")
		var status eventsapi.EventStreamStatus
		if len(statusMap) > 0 {
			raw, err := json.Marshal(statusMap)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				return nil, err
			}
		}
		if failed := meta.FindStatusCondition(status.Conditions, eventsapi.EventStreamConditionFailed); failed != nil && failed.Status == metav1.ConditionTrue {
			failedCopy := *failed
			latestFailed = &failedCopy
			continue
		}
		if ready := meta.FindStatusCondition(status.Conditions, eventsapi.EventStreamConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
			readyCount++
			continue
		}
		starting = true
	}
	if latestFailed != nil {
		return []metav1.Condition{
			conds.New(RecordWatchConditionReady, false, "StreamFailed", latestFailed.Message, generation),
			conds.New(RecordWatchConditionProgressing, false, "StreamFailed", latestFailed.Message, generation),
			conds.New(RecordWatchConditionFailed, true, "StreamFailed", latestFailed.Message, generation),
		}, nil
	}
	if readyCount == len(plans) && !starting {
		return []metav1.Condition{
			conds.New(RecordWatchConditionReady, true, "StreamReady", "every EventStream is open", generation),
			conds.New(RecordWatchConditionProgressing, false, "StreamReady", "every EventStream is open", generation),
			conds.New(RecordWatchConditionFailed, false, "NoError", "every EventStream is open", generation),
		}, nil
	}
	return []metav1.Condition{
		conds.New(RecordWatchConditionReady, false, "Starting", "the EventStreams are starting", generation),
		conds.New(RecordWatchConditionProgressing, true, "Starting", "the EventStreams are starting", generation),
		conds.New(RecordWatchConditionFailed, false, "NoError", "the EventStreams are starting", generation),
	}, nil
}

func (c *Controller) writeRecordWatchConditions(ctx context.Context, dyn dynamic.Interface, name string, wanted []metav1.Condition) error {
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
		var status recordWatchStatus
		if len(statusMap) > 0 {
			raw, err := json.Marshal(statusMap)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				return err
			}
		}
		conditions := append([]metav1.Condition(nil), status.Conditions...)
		for _, want := range wanted {
			meta.SetStatusCondition(&conditions, want)
		}
		if reflect.DeepEqual(conditions, status.Conditions) {
			return nil
		}
		status.Conditions = conditions
		raw, err := json.Marshal(status)
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
			return fmt.Errorf("eventstream: writing AtprotoRecordWatch %s conditions: %w", name, err)
		}
	}
	return fmt.Errorf("eventstream: the status of AtprotoRecordWatch %s stayed in conflict", name)
}

func (c *Controller) gcRecordWatch(ctx context.Context, dyn dynamic.Interface, watchName string) {
	ri := dyn.Resource(EventStreamGVR)
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: RecordWatchLabel})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return
	}
	if err != nil {
		c.log.Warn("listing EventStreams to collect", "watch", watchName, "err", err.Error())
		return
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetLabels()[RecordWatchLabel] != watchName {
			continue
		}
		if err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("collecting an EventStream", "name", item.GetName(), "err", err.Error())
		}
	}
}

func (c *Controller) gcStaleEventStreams(ctx context.Context, dyn dynamic.Interface, watchName string, wanted map[string]bool) {
	ri := dyn.Resource(EventStreamGVR)
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: RecordWatchLabel})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return
	}
	if err != nil {
		c.log.Warn("listing EventStreams to collect", "watch", watchName, "err", err.Error())
		return
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetLabels()[RecordWatchLabel] != watchName || wanted[item.GetName()] {
			continue
		}
		if err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("collecting a stale EventStream", "name", item.GetName(), "err", err.Error())
		}
	}
}

func (c *Controller) gcOrphanEventStreams(ctx context.Context, dyn dynamic.Interface, watches map[string]bool) {
	ri := dyn.Resource(EventStreamGVR)
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: RecordWatchLabel})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return
	}
	if err != nil {
		c.log.Warn("listing EventStreams to collect", "err", err.Error())
		return
	}
	for i := range list.Items {
		item := &list.Items[i]
		owner := item.GetLabels()[RecordWatchLabel]
		if owner == "" || watches[owner] {
			continue
		}
		if err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("collecting an orphan EventStream", "name", item.GetName(), "err", err.Error())
		}
	}
}

func decodeRecordWatch(u *unstructured.Unstructured) (recordWatchSpec, recordWatchStatus, error) {
	var spec recordWatchSpec
	var status recordWatchStatus
	specMap, _, _ := unstructured.NestedMap(u.Object, "spec")
	if len(specMap) > 0 {
		raw, err := json.Marshal(specMap)
		if err != nil {
			return spec, status, err
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			return spec, status, fmt.Errorf("eventstream: decoding AtprotoRecordWatch %s spec: %w", u.GetName(), err)
		}
	}
	statusMap, _, _ := unstructured.NestedMap(u.Object, "status")
	if len(statusMap) > 0 {
		raw, err := json.Marshal(statusMap)
		if err != nil {
			return spec, status, err
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return spec, status, fmt.Errorf("eventstream: decoding AtprotoRecordWatch %s status: %w", u.GetName(), err)
		}
	}
	return spec, status, nil
}

func watchCursorFromStatus(status recordWatchStatus, sourceKey string) *int64 {
	want := strings.TrimSpace(sourceKey)
	for _, cursor := range status.Cursors {
		if cursor.Source == want {
			value := cursor.Cursor
			return &value
		}
	}
	if len(status.Cursors) == 1 && want == "" {
		value := status.Cursors[0].Cursor
		return &value
	}
	return nil
}

func mergeLabels(current, wanted map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, current)
	maps.Copy(out, wanted)
	return out
}
