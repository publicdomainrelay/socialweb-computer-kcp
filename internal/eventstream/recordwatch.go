package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
)

const (
	RecordWatchLabel = "events.socialweb.computer/atprotorecordwatch"

	RecordWatchConditionReady       = "Ready"
	RecordWatchConditionProgressing = "Progressing"
	RecordWatchConditionFailed      = "Failed"

	EventStreamNamePrefix = "watch-"
	maxEventStreamName    = 253
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
		if !sleep(ctx, c.opts.RetryAfter) {
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
	name := u.GetName()
	spec, status, err := decodeRecordWatch(u)
	if err != nil {
		return err
	}
	plans, err := c.planWatchSources(ctx, dyn, name, spec)
	if err != nil {
		c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
			condition(RecordWatchConditionReady, false, "InvalidSpec", err.Error(), u.GetGeneration()),
			condition(RecordWatchConditionProgressing, false, "InvalidSpec", err.Error(), u.GetGeneration()),
			condition(RecordWatchConditionFailed, true, "InvalidSpec", err.Error(), u.GetGeneration()),
		})
		return err
	}
	if _, err := c.resolveMatches(ctx, dyn, spec); err != nil {
		c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
			condition(RecordWatchConditionReady, false, "MatchPending", err.Error(), u.GetGeneration()),
			condition(RecordWatchConditionProgressing, true, "MatchPending", err.Error(), u.GetGeneration()),
			condition(RecordWatchConditionFailed, false, "NoError", err.Error(), u.GetGeneration()),
		})
		return err
	}
	wanted := map[string]bool{}
	for _, plan := range plans {
		wanted[plan.Name] = true
		if err := c.ensureWatchEventStream(ctx, dyn, name, plan, status); err != nil {
			c.writeRecordWatchConditions(ctx, dyn, name, []metav1.Condition{
				condition(RecordWatchConditionReady, false, "ReconcileFailed", err.Error(), u.GetGeneration()),
				condition(RecordWatchConditionProgressing, false, "ReconcileFailed", err.Error(), u.GetGeneration()),
				condition(RecordWatchConditionFailed, true, "ReconcileFailed", err.Error(), u.GetGeneration()),
			})
			return err
		}
	}
	c.gcStaleEventStreams(ctx, dyn, name, wanted)
	conditions, err := c.watchConditions(ctx, dyn, plans, u.GetGeneration())
	if err != nil {
		return err
	}
	return c.writeRecordWatchConditions(ctx, dyn, name, conditions)
}

func (c *Controller) ensureWatchEventStream(ctx context.Context, dyn dynamic.Interface, name string, plan watchSourcePlan, status recordWatchStatus) error {
	eventStreamName := plan.Name
	desiredSpec := eventsapi.EventStreamSpec{
		Source: plan.Source,
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
			condition(RecordWatchConditionReady, false, "StreamFailed", latestFailed.Message, generation),
			condition(RecordWatchConditionProgressing, false, "StreamFailed", latestFailed.Message, generation),
			condition(RecordWatchConditionFailed, true, "StreamFailed", latestFailed.Message, generation),
		}, nil
	}
	if readyCount == len(plans) && !starting {
		return []metav1.Condition{
			condition(RecordWatchConditionReady, true, "StreamReady", "every EventStream is open", generation),
			condition(RecordWatchConditionProgressing, false, "StreamReady", "every EventStream is open", generation),
			condition(RecordWatchConditionFailed, false, "NoError", "every EventStream is open", generation),
		}, nil
	}
	return []metav1.Condition{
		condition(RecordWatchConditionReady, false, "Starting", "the EventStreams are starting", generation),
		condition(RecordWatchConditionProgressing, true, "Starting", "the EventStreams are starting", generation),
		condition(RecordWatchConditionFailed, false, "NoError", "the EventStreams are starting", generation),
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

func condition(kind string, ok bool, reason, message string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation}
}

func mergeLabels(current, wanted map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range current {
		out[key] = value
	}
	for key, value := range wanted {
		out[key] = value
	}
	return out
}
