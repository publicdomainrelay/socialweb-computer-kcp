package participant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	OutboxFinalizer = "statesync.socialweb.computer/release"

	DefaultOutboxMaxAttempts = 5

	DefaultOutboxBackoff = 2 * time.Second

	outboxStallRetry = 5 * time.Minute

	outboxMaxBackoff = 5 * time.Minute
)

var (
	ErrRemoteNotFound = errors.New("participant: the remote object does not exist")

	ErrRejected = errors.New("participant: the target rejected the update")
)

type OutboxRequest struct {
	Key string

	Payload json.RawMessage

	Sink v1alpha1.StateSyncSink

	Source *v1alpha1.StateSyncSource

	UID string
}

type Observation struct {
	Exists bool

	Ref string

	CID string

	Hash string

	At string

	Meta map[string]string

	Cursor *int64
}

type Adapter interface {
	Name() string

	Observe(ctx context.Context, request OutboxRequest) (Observation, error)

	Apply(ctx context.Context, request OutboxRequest) (Observation, error)

	Delete(ctx context.Context, request OutboxRequest) error
}

type OutboxWriter interface {
	ApplyStateSyncStatus(ctx context.Context, name string, status v1alpha1.StateSyncStatus) error

	SetStateSyncFinalizer(ctx context.Context, name, finalizer string, present bool) error
}

type Outbox struct {
	writer OutboxWriter

	now func() time.Time

	adapters map[string]Adapter

	mu sync.Mutex
}

func NewOutbox(writer OutboxWriter, now func() time.Time, adapters ...Adapter) (*Outbox, error) {
	if writer == nil {
		return nil, errors.New("participant: the outbox needs a status writer")
	}
	if now == nil {
		now = time.Now
	}
	o := &Outbox{writer: writer, now: now, adapters: map[string]Adapter{}}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("participant: a nil outbox adapter")
		}
		name := adapterKey(adapter.Name())
		if name == "" {
			return nil, errors.New("participant: an outbox adapter needs a name")
		}
		if _, ok := o.adapters[name]; ok {
			return nil, fmt.Errorf("participant: an outbox adapter named %q is already registered", adapter.Name())
		}
		o.adapters[name] = adapter
	}
	return o, nil
}

func InboxObservation(request OutboxRequest, ref string) Observation {
	sum := sha256.Sum256(request.Payload)
	return Observation{Exists: true, Ref: ref, Hash: "sha256:" + hex.EncodeToString(sum[:]), At: time.Now().UTC().Format(time.RFC3339)}
}

func (o *Outbox) Handles(obj *v1alpha1.StateSync) bool {
	if o == nil || obj == nil {
		return false
	}
	_, ok := o.adapters[adapterKey(obj.Spec.Sink.Adapter)]
	return ok
}

func (o *Outbox) Reconcile(ctx context.Context, obj *v1alpha1.StateSync) (time.Duration, error) {
	if o == nil || obj == nil {
		return 0, nil
	}
	adapter, ok := o.adapters[adapterKey(obj.Spec.Sink.Adapter)]
	if !ok {
		return 0, nil
	}
	if obj.DeletionTimestamp == nil && obj.Status.Phase == v1alpha1.StateSyncPhaseAcked && obj.Status.ObservedGeneration == obj.Generation &&
		obj.Status.DesiredHash == hashPayload(obj.Spec.Payload) {
		return 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r := &outboxRun{o: o, obj: obj, adapter: adapter, generation: obj.Generation}
	return r.reconcile(ctx)
}

type outboxRun struct {
	o *Outbox

	obj *v1alpha1.StateSync

	adapter Adapter

	generation int64

	status v1alpha1.StateSyncStatus
}

func (r *outboxRun) stamp() string {
	return r.o.now().UTC().Format(time.RFC3339)
}

func (r *outboxRun) write(ctx context.Context) error {
	return r.o.writer.ApplyStateSyncStatus(ctx, r.obj.Name, r.status)
}

func (r *outboxRun) request() OutboxRequest {
	return OutboxRequest{Key: r.obj.Spec.IdempotencyKey, Payload: append(json.RawMessage(nil), r.obj.Spec.Payload...),
		Sink: r.obj.Spec.Sink, Source: r.obj.Spec.Source, UID: string(r.obj.UID)}
}

func (r *outboxRun) set(kind string, ok bool, reason, message string) {
	conds.Set(&r.status.Conditions, r.generation, kind, ok, reason, message)
}

func (r *outboxRun) reconcile(ctx context.Context) (time.Duration, error) {
	spec := r.obj.Spec
	prior := r.obj.Status.ObservedGeneration
	r.status = r.obj.Status
	if prior != r.generation {
		r.status.Attempts = 0
		r.status.LastError = ""
	}
	r.status.ObservedGeneration = r.generation
	r.status.Conditions = append(r.status.Conditions[:0:0], r.status.Conditions...)
	if r.obj.DeletionTimestamp != nil {
		return 0, r.reconcileDelete(ctx)
	}
	if err := r.o.writer.SetStateSyncFinalizer(ctx, r.obj.Name, OutboxFinalizer, deletionPolicyOf(spec) != v1alpha1.StateSyncDeletionRetain); err != nil {
		return 0, err
	}
	retryMax := maxAttempts(spec.Retry)
	if r.status.Phase == v1alpha1.StateSyncPhaseFailed && r.status.Attempts >= retryMax && prior == r.generation {
		return 0, r.write(ctx)
	}
	if err := validateOutboxSpec(spec); err != nil {
		r.status.Phase = v1alpha1.StateSyncPhaseFailed
		r.status.LastError = err.Error()
		r.set(v1alpha1.StateSyncConditionAcked, false, "InvalidSpec", err.Error())
		r.set(v1alpha1.StateSyncConditionStalled, false, "InvalidSpec", err.Error())
		return 0, r.write(ctx)
	}
	if spec.Suspend {
		r.status.Phase = v1alpha1.StateSyncPhasePending
		r.set(v1alpha1.StateSyncConditionAcked, false, "Suspended", "the StateSync is suspended")
		r.set(v1alpha1.StateSyncConditionDrifted, false, "Suspended", "the StateSync is suspended")
		r.set(v1alpha1.StateSyncConditionStalled, false, "Suspended", "the StateSync is suspended")
		return 0, r.write(ctx)
	}
	desired := hashPayload(spec.Payload)
	r.status.DesiredHash = desired
	if r.status.Attempts > 0 && prior == r.generation {
		if wait := r.attemptWait(); wait > 0 {
			return wait, r.write(ctx)
		}
	}
	request := r.request()
	r.status.LastAttemptAt = r.stamp()
	observation, err := r.adapter.Observe(ctx, request)
	if err != nil {
		return r.fail(ctx, retryMax, err)
	}
	storeObservation(&r.status, observation)
	if observation.Exists && observationMatches(observation, desired, r.status.Acked) {
		return 0, r.acked(ctx, observation, "Acked", "the observation matches the desired state", "NoDrift", "the observation matches the desired state")
	}
	if directionOf(spec) == v1alpha1.StateSyncDirectionInbound {
		if observation.Exists {
			storeAcknowledged(&r.status, observation)
			r.status.Phase = v1alpha1.StateSyncPhaseAcked
			r.status.Attempts = 0
			r.status.LastError = ""
			r.set(v1alpha1.StateSyncConditionAcked, true, "Acked", "the inbound observation is acknowledged")
			r.set(v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
			r.set(v1alpha1.StateSyncConditionDrifted, false, "Inbound", "an inbound StateSync does not apply its payload")
			r.set(v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target exists")
		} else {
			r.status.Phase = v1alpha1.StateSyncPhasePending
			r.set(v1alpha1.StateSyncConditionAcked, false, "NotAcked", "the target does not exist")
			r.set(v1alpha1.StateSyncConditionObserved, false, "NotFound", "the target does not exist")
			r.set(v1alpha1.StateSyncConditionDrifted, false, "Inbound", "an inbound StateSync does not apply its payload")
			r.set(v1alpha1.StateSyncConditionStalled, false, "NotStalled", "waiting for the target")
		}
		return 0, r.write(ctx)
	}
	if observation.Exists {
		r.set(v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
		r.set(v1alpha1.StateSyncConditionDrifted, true, "HashMismatch", "the observed hash differs from the desired hash")
		switch conflictPolicyOf(spec) {
		case v1alpha1.StateSyncConflictTargetWins:
			storeAcknowledged(&r.status, observation)
			r.status.Phase = v1alpha1.StateSyncPhaseAcked
			r.status.Attempts = 0
			r.status.LastError = ""
			r.set(v1alpha1.StateSyncConditionAcked, true, "TargetWins", "the target state is acknowledged")
			r.set(v1alpha1.StateSyncConditionDrifted, true, "TargetWins", "the target state is kept")
			r.set(v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target state is kept")
			return 0, r.write(ctx)
		case v1alpha1.StateSyncConflictReportOnly:
			r.status.Phase = v1alpha1.StateSyncPhaseDrifted
			r.set(v1alpha1.StateSyncConditionAcked, false, "Drift", "report only: the target is not changed")
			r.set(v1alpha1.StateSyncConditionStalled, false, "NotStalled", "drift is reported")
			return 0, r.write(ctx)
		}
	} else {
		r.set(v1alpha1.StateSyncConditionObserved, false, "NotFound", "the target does not exist")
		r.set(v1alpha1.StateSyncConditionDrifted, false, "NoDrift", "there is no observation to drift")
	}
	if r.status.Phase == v1alpha1.StateSyncPhaseStalled && prior == r.generation {
		if wait := stallWait(r.status.LastAttemptAt, r.o.now()); wait > 0 {
			r.set(v1alpha1.StateSyncConditionAcked, false, "Stalled", "the attempts are exhausted")
			r.set(v1alpha1.StateSyncConditionStalled, true, "AttemptsExhausted", "the attempts are exhausted")
			return wait, r.write(ctx)
		}
		r.status.Attempts = 0
		r.status.Phase = v1alpha1.StateSyncPhasePending
	}
	r.status.Phase = v1alpha1.StateSyncPhaseDelivering
	if err := r.write(ctx); err != nil {
		return 0, err
	}
	r.status.LastAttemptAt = r.stamp()
	if _, err := r.adapter.Apply(ctx, request); err != nil {
		return r.fail(ctx, retryMax, err)
	}
	applied, err := r.adapter.Observe(ctx, request)
	r.status.LastAttemptAt = r.stamp()
	if err != nil {
		return r.fail(ctx, retryMax, err)
	}
	storeObservation(&r.status, applied)
	if !applied.Exists {
		return r.fail(ctx, retryMax, errors.New("participant: the observation does not exist after apply"))
	}
	if !observationMatches(applied, desired, r.status.Acked) {
		return r.fail(ctx, retryMax, errors.New("participant: the observation does not match the desired state after apply"))
	}
	return 0, r.acked(ctx, applied, "Acked", "the applied state is observed", "NoDrift", "the observation matches the desired state")
}

func (r *outboxRun) acked(ctx context.Context, observation Observation, reason, message, driftReason, driftMessage string) error {
	storeAcknowledged(&r.status, observation)
	r.status.Phase = v1alpha1.StateSyncPhaseAcked
	r.status.Attempts = 0
	r.status.LastError = ""
	r.set(v1alpha1.StateSyncConditionAcked, true, reason, message)
	r.set(v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
	r.set(v1alpha1.StateSyncConditionDrifted, false, driftReason, driftMessage)
	r.set(v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target is current")
	return r.write(ctx)
}

func (r *outboxRun) reconcileDelete(ctx context.Context) error {
	spec := r.obj.Spec
	policy := deletionPolicyOf(spec)
	if policy == v1alpha1.StateSyncDeletionRetain {
		return r.o.writer.SetStateSyncFinalizer(ctx, r.obj.Name, OutboxFinalizer, false)
	}
	retryMax := maxAttempts(spec.Retry)
	request := r.request()
	if r.status.Attempts < retryMax {
		if policy == v1alpha1.StateSyncDeletionAckThenDelete {
			observation, err := r.adapter.Observe(ctx, request)
			r.status.LastAttemptAt = r.stamp()
			if err != nil {
				return r.failDelete(ctx, retryMax, err)
			}
			storeObservation(&r.status, observation)
			if observation.Exists {
				storeAcknowledged(&r.status, observation)
			}
		}
		err := r.adapter.Delete(ctx, request)
		r.status.LastAttemptAt = r.stamp()
		if err != nil && !errors.Is(err, ErrRemoteNotFound) {
			return r.failDelete(ctx, retryMax, err)
		}
	}
	return r.o.writer.SetStateSyncFinalizer(ctx, r.obj.Name, OutboxFinalizer, false)
}

func (r *outboxRun) fail(ctx context.Context, retryMax int, cause error) (time.Duration, error) {
	r.status.Attempts++
	r.status.LastError = cause.Error()
	if r.status.LastAttemptAt == "" {
		r.status.LastAttemptAt = r.stamp()
	}
	if errors.Is(cause, ErrRejected) {
		r.status.Attempts = retryMax
		r.status.Phase = v1alpha1.StateSyncPhaseFailed
		r.set(v1alpha1.StateSyncConditionAcked, false, "Rejected", cause.Error())
		r.set(v1alpha1.StateSyncConditionStalled, false, "Rejected", cause.Error())
		return 0, r.write(ctx)
	}
	if r.status.Attempts >= retryMax {
		r.status.Phase = v1alpha1.StateSyncPhaseStalled
		r.set(v1alpha1.StateSyncConditionAcked, false, "AttemptsExhausted", cause.Error())
		r.set(v1alpha1.StateSyncConditionStalled, true, "AttemptsExhausted", cause.Error())
		return outboxStallRetry, r.write(ctx)
	}
	r.status.Phase = v1alpha1.StateSyncPhaseFailed
	r.set(v1alpha1.StateSyncConditionAcked, false, "AttemptFailed", cause.Error())
	r.set(v1alpha1.StateSyncConditionStalled, false, "Retrying", cause.Error())
	if err := r.write(ctx); err != nil {
		return 0, err
	}
	return backoffFor(r.status.Attempts, r.obj.Spec.Retry), nil
}

func (r *outboxRun) failDelete(ctx context.Context, retryMax int, cause error) error {
	r.status.Attempts++
	r.status.LastError = cause.Error()
	if r.status.LastAttemptAt == "" {
		r.status.LastAttemptAt = r.stamp()
	}
	if r.status.Attempts >= retryMax {
		r.status.Phase = v1alpha1.StateSyncPhaseStalled
		r.set(v1alpha1.StateSyncConditionStalled, true, "ReleaseFailed", cause.Error())
		_ = r.write(ctx)
		return r.o.writer.SetStateSyncFinalizer(ctx, r.obj.Name, OutboxFinalizer, false)
	}
	r.status.Phase = v1alpha1.StateSyncPhaseFailed
	r.set(v1alpha1.StateSyncConditionStalled, false, "RetryingRelease", cause.Error())
	if err := r.write(ctx); err != nil {
		return err
	}
	return cause
}

func (r *outboxRun) attemptWait() time.Duration {
	if r.status.LastAttemptAt == "" {
		return 0
	}
	at, err := time.Parse(time.RFC3339, r.status.LastAttemptAt)
	if err != nil {
		return 0
	}
	return at.Add(backoffFor(r.status.Attempts, r.obj.Spec.Retry)).Sub(r.o.now().UTC())
}

func stallWait(lastAttemptAt string, now time.Time) time.Duration {
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(lastAttemptAt))
	if err != nil {
		return 0
	}
	return last.Add(outboxStallRetry).Sub(now)
}

func validateOutboxSpec(spec v1alpha1.StateSyncSpec) error {
	if strings.TrimSpace(spec.Sink.Adapter) == "" {
		return errors.New("participant: sink.adapter is required")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return errors.New("participant: idempotencyKey is required")
	}
	if len(spec.Payload) > 0 && !json.Valid(spec.Payload) {
		return errors.New("participant: payload is not valid JSON")
	}
	switch directionOf(spec) {
	case v1alpha1.StateSyncDirectionOutbound, v1alpha1.StateSyncDirectionInbound, v1alpha1.StateSyncDirectionBoth:
	default:
		return fmt.Errorf("participant: direction %q is unknown", spec.Direction)
	}
	switch conflictPolicyOf(spec) {
	case v1alpha1.StateSyncConflictSourceWins, v1alpha1.StateSyncConflictTargetWins, v1alpha1.StateSyncConflictReportOnly:
	default:
		return fmt.Errorf("participant: conflictPolicy %q is unknown", spec.ConflictPolicy)
	}
	switch deletionPolicyOf(spec) {
	case v1alpha1.StateSyncDeletionRetain, v1alpha1.StateSyncDeletionDeleteRemote, v1alpha1.StateSyncDeletionAckThenDelete:
	default:
		return fmt.Errorf("participant: deletionPolicy %q is unknown", spec.DeletionPolicy)
	}
	if spec.Retry != nil && (spec.Retry.MaxAttempts < 0 || spec.Retry.BackoffSeconds < 0) {
		return errors.New("participant: retry.maxAttempts and retry.backoffSeconds cannot be negative")
	}
	if spec.Ack != nil && len(spec.Ack.Paths) > 0 {
		return errors.New("participant: ack.paths are not supported by the market outbox")
	}
	if source := spec.Source; source != nil && source.Generation < 0 {
		return errors.New("participant: source.generation cannot be negative")
	}
	return nil
}

func adapterKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func directionOf(spec v1alpha1.StateSyncSpec) v1alpha1.StateSyncDirection {
	if spec.Direction == "" {
		return v1alpha1.StateSyncDirectionOutbound
	}
	return spec.Direction
}

func conflictPolicyOf(spec v1alpha1.StateSyncSpec) v1alpha1.StateSyncConflictPolicy {
	if spec.ConflictPolicy == "" {
		return v1alpha1.StateSyncConflictReportOnly
	}
	return spec.ConflictPolicy
}

func deletionPolicyOf(spec v1alpha1.StateSyncSpec) v1alpha1.StateSyncDeletionPolicy {
	if spec.DeletionPolicy == "" {
		return v1alpha1.StateSyncDeletionRetain
	}
	return spec.DeletionPolicy
}

func maxAttempts(retry *v1alpha1.StateSyncRetry) int {
	if retry != nil && retry.MaxAttempts > 0 {
		return retry.MaxAttempts
	}
	return DefaultOutboxMaxAttempts
}

func backoffFor(attempts int, retry *v1alpha1.StateSyncRetry) time.Duration {
	base := DefaultOutboxBackoff
	if retry != nil && retry.BackoffSeconds > 0 {
		base = time.Duration(retry.BackoffSeconds) * time.Second
	}
	backoff := base
	for i := 1; i < attempts; i++ {
		if backoff >= outboxMaxBackoff/2 {
			return outboxMaxBackoff
		}
		backoff *= 2
	}
	return min(backoff, outboxMaxBackoff)
}

func hashPayload(payload json.RawMessage) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func observationMatches(observation Observation, desiredHash string, acked *v1alpha1.StateSyncAcknowledged) bool {
	if !observation.Exists {
		return false
	}
	if observation.Hash != "" {
		left := strings.TrimPrefix(strings.TrimSpace(observation.Hash), "sha256:")
		right := strings.TrimPrefix(strings.TrimSpace(desiredHash), "sha256:")
		return strings.EqualFold(left, right)
	}
	return acked != nil && acked.Ref != "" && acked.Ref == observation.Ref
}

func storeObservation(status *v1alpha1.StateSyncStatus, observation Observation) {
	observed := &v1alpha1.StateSyncObservation{Exists: observation.Exists, Ref: observation.Ref, Cid: observation.CID,
		Hash: observation.Hash, At: observation.At}
	if observation.Meta != nil {
		observed.Meta = maps.Clone(observation.Meta)
	}
	status.Observed = observed
	if observation.Cursor != nil {
		cursor := *observation.Cursor
		status.Cursor = &cursor
	}
}

func storeAcknowledged(status *v1alpha1.StateSyncStatus, observation Observation) {
	acknowledged := v1alpha1.StateSyncAcknowledged{}
	if status.Acked != nil {
		acknowledged = *status.Acked
	}
	if observation.Ref != "" {
		acknowledged.Ref = observation.Ref
	}
	if observation.CID != "" {
		acknowledged.Cid = observation.CID
	}
	if observation.At != "" {
		acknowledged.At = observation.At
	}
	if value := observation.Meta["eventUri"]; value != "" {
		acknowledged.EventURI = value
	}
	if value := observation.Meta["eventCid"]; value != "" {
		acknowledged.EventCid = value
	}
	status.Acked = &acknowledged
}
