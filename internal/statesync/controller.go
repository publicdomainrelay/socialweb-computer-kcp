package statesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	GroupName = v1alpha1.GroupName

	Version = v1alpha1.Version

	FieldManager = "statesync-controller"

	Finalizer = "statesync.socialweb.computer/release"

	DefaultRetryAfter = 2 * time.Second

	DefaultMaxAttempts = 5

	DefaultBackoff = 2 * time.Second

	stallRetryInterval = 5 * time.Minute

	maxBackoff = 5 * time.Minute
)

var StateSyncGVR = v1alpha1.StateSyncGVR

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Workspace string

	Dynamic DynamicFactory

	Registry *Registry

	Log *slog.Logger

	RetryAfter time.Duration

	Interval time.Duration

	Kick <-chan struct{}
}

type Controller struct {
	workspace string

	dynamic DynamicFactory

	registry *Registry

	log *slog.Logger

	retry time.Duration

	interval time.Duration

	kick <-chan struct{}
}

func NewController(opts Options) (*Controller, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("statesync: a workspace is required for the StateSync controller")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("statesync: a dynamic client factory is required for the StateSync controller")
	}
	if opts.Registry == nil {
		return nil, errors.New("statesync: an adapter registry is required for the StateSync controller")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	return &Controller{
		workspace: workspace,
		dynamic:   opts.Dynamic,
		registry:  opts.Registry,
		log:       log.With("component", "statesync-controller"),
		retry:     opts.RetryAfter,
		interval:  opts.Interval,
		kick:      opts.Kick,
	}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	w := &watcher{
		workspace: c.workspace,
		dynamic:   c.dynamic,
		gvr:       StateSyncGVR,
		log:       c.log,
		retry:     c.retry,
		interval:  c.interval,
		kick:      c.kick,
		reconcile: c.Reconcile,
	}
	return w.Run(ctx)
}

func (c *Controller) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	ri := dyn.Resource(StateSyncGVR)
	name := u.GetName()
	obj, err := decodeStateSync(u)
	if err != nil {
		return fmt.Errorf("statesync: decoding StateSync %s: %w", name, err)
	}
	generation := u.GetGeneration()
	priorGeneration := obj.Status.ObservedGeneration
	status := obj.Status
	if priorGeneration != generation {
		status.Attempts = 0
		status.LastError = ""
	}
	status.ObservedGeneration = generation
	status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	if obj.GetDeletionTimestamp() != nil {
		return c.reconcileDelete(ctx, dyn, ri, name, obj, &status, generation)
	}
	if err := ensureFinalizer(ctx, ri, name, Finalizer, deletionPolicyOf(obj.Spec) != v1alpha1.StateSyncDeletionRetain); err != nil {
		return err
	}
	if status.Phase == v1alpha1.StateSyncPhaseFailed && status.Attempts >= maxAttempts(obj.Spec.Retry) && priorGeneration == generation {
		return writeStatus(ctx, ri, name, &status)
	}
	if err := validateSpec(obj.Spec); err != nil {
		status.Phase = v1alpha1.StateSyncPhaseFailed
		status.LastError = err.Error()
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "InvalidSpec", err.Error())
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "InvalidSpec", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	if obj.Spec.Suspend {
		status.Phase = v1alpha1.StateSyncPhasePending
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "Suspended", "the StateSync is suspended")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "Suspended", "the StateSync is suspended")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "Suspended", "the StateSync is suspended")
		return writeStatus(ctx, ri, name, &status)
	}
	adapter, err := c.registry.Lookup(obj.Spec.Sink.Adapter)
	if err != nil {
		status.Phase = v1alpha1.StateSyncPhaseFailed
		status.LastError = err.Error()
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "UnknownAdapter", err.Error())
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "UnknownAdapter", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	desiredHash := hashPayload(obj.Spec.Payload)
	status.DesiredHash = desiredHash
	if status.Attempts > 0 && priorGeneration == generation && !attemptDue(&status, obj.Spec.Retry) {
		return writeStatus(ctx, ri, name, &status)
	}
	request := Request{
		Key:     obj.Spec.IdempotencyKey,
		Payload: append(json.RawMessage(nil), obj.Spec.Payload...),
		Sink:    obj.Spec.Sink,
		Source:  obj.Spec.Source,
		UID:     string(obj.GetUID()),
	}
	status.LastAttemptAt = now()
	observation, err := adapter.Observe(ctx, request)
	if err != nil {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
	}
	storeObservation(&status, observation)
	if observation.Exists && observationMatches(observation, desiredHash, status.Acked) {
		if err := c.acknowledge(ctx, dyn, obj, observation); err != nil {
			return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
		}
		storeAcknowledged(&status, observation)
		status.Phase = v1alpha1.StateSyncPhaseAcked
		status.Attempts = 0
		status.LastError = ""
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, true, "Acked", "the observation matches the desired state")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "NoDrift", "the observation matches the desired state")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target is current")
		return writeStatus(ctx, ri, name, &status)
	}
	direction := directionOf(obj.Spec)
	if direction == v1alpha1.StateSyncDirectionInbound {
		if observation.Exists {
			if err := c.acknowledge(ctx, dyn, obj, observation); err != nil {
				return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
			}
			storeAcknowledged(&status, observation)
			status.Phase = v1alpha1.StateSyncPhaseAcked
			status.Attempts = 0
			status.LastError = ""
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, true, "Acked", "the inbound observation is acknowledged")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "Inbound", "an inbound StateSync does not apply its payload")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target exists")
		} else {
			status.Phase = v1alpha1.StateSyncPhasePending
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "NotAcked", "the target does not exist")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, false, "NotFound", "the target does not exist")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "Inbound", "an inbound StateSync does not apply its payload")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "waiting for the target")
		}
		return writeStatus(ctx, ri, name, &status)
	}
	if observation.Exists {
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, true, "HashMismatch", "the observed hash differs from the desired hash")
		switch conflictPolicyOf(obj.Spec) {
		case v1alpha1.StateSyncConflictTargetWins:
			if err := c.acknowledge(ctx, dyn, obj, observation); err != nil {
				return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
			}
			storeAcknowledged(&status, observation)
			status.Phase = v1alpha1.StateSyncPhaseAcked
			status.Attempts = 0
			status.LastError = ""
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, true, "TargetWins", "the target state is acknowledged")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, true, "TargetWins", "the target state is kept")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target state is kept")
			return writeStatus(ctx, ri, name, &status)
		case v1alpha1.StateSyncConflictReportOnly:
			status.Phase = v1alpha1.StateSyncPhaseDrifted
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "Drift", "report only: the target is not changed")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "drift is reported")
			return writeStatus(ctx, ri, name, &status)
		}
	} else {
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, false, "NotFound", "the target does not exist")
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "NoDrift", "there is no observation to drift")
	}
	if status.Phase == v1alpha1.StateSyncPhaseStalled && priorGeneration == generation {
		if !stallDue(status.LastAttemptAt, time.Now().UTC()) {
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "Stalled", "the attempts are exhausted")
			conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, true, "AttemptsExhausted", "the attempts are exhausted")
			return writeStatus(ctx, ri, name, &status)
		}
		status.Attempts = 0
		status.Phase = v1alpha1.StateSyncPhasePending
	}
	status.Phase = v1alpha1.StateSyncPhaseDelivering
	if err := writeStatus(ctx, ri, name, &status); err != nil {
		return err
	}
	status.LastAttemptAt = now()
	if _, err := adapter.Apply(ctx, request); err != nil {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
	}
	applied, err := adapter.Observe(ctx, request)
	status.LastAttemptAt = now()
	if err != nil {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
	}
	storeObservation(&status, applied)
	if !applied.Exists {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry),
			errors.New("statesync: the observation does not exist after apply"))
	}
	if !observationMatches(applied, desiredHash, status.Acked) {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry),
			errors.New("statesync: the observation does not match the desired state after apply"))
	}
	if err := c.acknowledge(ctx, dyn, obj, applied); err != nil {
		return c.fail(ctx, ri, name, &status, generation, maxAttempts(obj.Spec.Retry), err)
	}
	storeAcknowledged(&status, applied)
	status.Phase = v1alpha1.StateSyncPhaseAcked
	status.Attempts = 0
	status.LastError = ""
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, true, "Acked", "the applied state is observed")
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionObserved, true, "Exists", "the target exists")
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionDrifted, false, "NoDrift", "the observation matches the desired state")
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "NotStalled", "the target is current")
	return writeStatus(ctx, ri, name, &status)
}

func (c *Controller) reconcileDelete(ctx context.Context, dyn dynamic.Interface, ri dynamic.ResourceInterface, name string, obj *v1alpha1.StateSync, status *v1alpha1.StateSyncStatus, generation int64) error {
	policy := deletionPolicyOf(obj.Spec)
	if policy == v1alpha1.StateSyncDeletionRetain {
		return ensureFinalizer(ctx, ri, name, Finalizer, false)
	}
	retryMax := maxAttempts(obj.Spec.Retry)
	adapter, err := c.registry.Lookup(obj.Spec.Sink.Adapter)
	if err != nil {
		return c.failDelete(ctx, ri, name, status, generation, retryMax, err)
	}
	request := Request{
		Key:     obj.Spec.IdempotencyKey,
		Payload: append(json.RawMessage(nil), obj.Spec.Payload...),
		Sink:    obj.Spec.Sink,
		Source:  obj.Spec.Source,
		UID:     string(obj.GetUID()),
	}
	if status.Attempts < retryMax {
		if policy == v1alpha1.StateSyncDeletionAckThenDelete {
			observation, err := adapter.Observe(ctx, request)
			status.LastAttemptAt = now()
			if err != nil {
				return c.failDelete(ctx, ri, name, status, generation, retryMax, err)
			}
			storeObservation(status, observation)
			if observation.Exists {
				if err := c.acknowledge(ctx, dyn, obj, observation); err != nil {
					return c.failDelete(ctx, ri, name, status, generation, retryMax, err)
				}
				storeAcknowledged(status, observation)
			}
		}
		err := adapter.Delete(ctx, request)
		status.LastAttemptAt = now()
		if err != nil && !isNotFound(err) {
			return c.failDelete(ctx, ri, name, status, generation, retryMax, err)
		}
	}
	return ensureFinalizer(ctx, ri, name, Finalizer, false)
}

func (c *Controller) fail(ctx context.Context, ri dynamic.ResourceInterface, name string, status *v1alpha1.StateSyncStatus, generation int64, retryMax int, cause error) error {
	status.Attempts++
	status.LastError = cause.Error()
	if status.LastAttemptAt == "" {
		status.LastAttemptAt = now()
	}
	if errors.Is(cause, ErrRejected) {
		status.Attempts = retryMax
		status.Phase = v1alpha1.StateSyncPhaseFailed
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "Rejected", cause.Error())
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "Rejected", cause.Error())
		return writeStatus(ctx, ri, name, status)
	}
	if status.Attempts >= retryMax {
		status.Phase = v1alpha1.StateSyncPhaseStalled
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "AttemptsExhausted", cause.Error())
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, true, "AttemptsExhausted", cause.Error())
		if err := writeStatus(ctx, ri, name, status); err != nil {
			return err
		}
		return nil
	}
	status.Phase = v1alpha1.StateSyncPhaseFailed
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionAcked, false, "AttemptFailed", cause.Error())
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "Retrying", cause.Error())
	if err := writeStatus(ctx, ri, name, status); err != nil {
		return err
	}
	return cause
}

func (c *Controller) failDelete(ctx context.Context, ri dynamic.ResourceInterface, name string, status *v1alpha1.StateSyncStatus, generation int64, retryMax int, cause error) error {
	status.Attempts++
	status.LastError = cause.Error()
	if status.LastAttemptAt == "" {
		status.LastAttemptAt = now()
	}
	if status.Attempts >= retryMax {
		status.Phase = v1alpha1.StateSyncPhaseStalled
		conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, true, "ReleaseFailed", cause.Error())
		_ = writeStatus(ctx, ri, name, status)
		return ensureFinalizer(ctx, ri, name, Finalizer, false)
	}
	status.Phase = v1alpha1.StateSyncPhaseFailed
	conds.Set(&status.Conditions, generation, v1alpha1.StateSyncConditionStalled, false, "RetryingRelease", cause.Error())
	if err := writeStatus(ctx, ri, name, status); err != nil {
		return err
	}
	return cause
}

func validateSpec(spec v1alpha1.StateSyncSpec) error {
	if strings.TrimSpace(spec.Sink.Adapter) == "" {
		return errors.New("statesync: sink.adapter is required")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return errors.New("statesync: idempotencyKey is required")
	}
	if len(spec.Payload) > 0 && !json.Valid(spec.Payload) {
		return errors.New("statesync: payload is not valid JSON")
	}
	switch directionOf(spec) {
	case v1alpha1.StateSyncDirectionOutbound, v1alpha1.StateSyncDirectionInbound, v1alpha1.StateSyncDirectionBoth:
	default:
		return fmt.Errorf("statesync: direction %q is unknown", spec.Direction)
	}
	switch conflictPolicyOf(spec) {
	case v1alpha1.StateSyncConflictSourceWins, v1alpha1.StateSyncConflictTargetWins, v1alpha1.StateSyncConflictReportOnly:
	default:
		return fmt.Errorf("statesync: conflictPolicy %q is unknown", spec.ConflictPolicy)
	}
	switch deletionPolicyOf(spec) {
	case v1alpha1.StateSyncDeletionRetain, v1alpha1.StateSyncDeletionDeleteRemote, v1alpha1.StateSyncDeletionAckThenDelete:
	default:
		return fmt.Errorf("statesync: deletionPolicy %q is unknown", spec.DeletionPolicy)
	}
	if spec.Retry != nil {
		if spec.Retry.MaxAttempts < 0 {
			return errors.New("statesync: retry.maxAttempts cannot be negative")
		}
		if spec.Retry.BackoffSeconds < 0 {
			return errors.New("statesync: retry.backoffSeconds cannot be negative")
		}
	}
	if spec.Ack != nil && len(spec.Ack.Paths) > 0 {
		if spec.Source == nil || strings.TrimSpace(spec.Source.Name) == "" {
			return errors.New("statesync: ack.paths need spec.source.name")
		}
		seen := map[string]bool{}
		paths := make([]string, 0, len(spec.Ack.Paths))
		for _, path := range spec.Ack.Paths {
			segments, err := splitAckPath(path)
			if err != nil {
				return err
			}
			normalized := strings.Join(segments, ".")
			if seen[normalized] {
				return fmt.Errorf("statesync: ack path %q is listed twice", path)
			}
			seen[normalized] = true
			paths = append(paths, normalized)
		}
		sort.Strings(paths)
		for i := 1; i < len(paths); i++ {
			if strings.HasPrefix(paths[i], paths[i-1]+".") {
				return fmt.Errorf("statesync: ack path %q overlaps %q", paths[i], paths[i-1])
			}
		}
	}
	if source := spec.Source; source != nil {
		if source.Generation < 0 {
			return errors.New("statesync: source.generation cannot be negative")
		}
		if strings.TrimSpace(source.Name) != "" {
			if strings.TrimSpace(source.ApiVersion) == "" {
				return errors.New("statesync: source.apiVersion is required with source.name")
			}
			if strings.TrimSpace(source.Kind) == "" {
				return errors.New("statesync: source.kind is required with source.name")
			}
			if _, err := schema.ParseGroupVersion(strings.TrimSpace(source.ApiVersion)); err != nil {
				return fmt.Errorf("statesync: source.apiVersion %q is invalid: %w", source.ApiVersion, err)
			}
			if pluralResource(source.Kind) == "" {
				return errors.New("statesync: source.kind is required with source.name")
			}
		}
	}
	return nil
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
	return DefaultMaxAttempts
}

func backoffFor(attempts int, retry *v1alpha1.StateSyncRetry) time.Duration {
	base := DefaultBackoff
	if retry != nil && retry.BackoffSeconds > 0 {
		base = time.Duration(retry.BackoffSeconds) * time.Second
	}
	if attempts < 1 {
		attempts = 1
	}
	backoff := base
	for i := 1; i < attempts; i++ {
		if backoff >= maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func attemptDue(status *v1alpha1.StateSyncStatus, retry *v1alpha1.StateSyncRetry) bool {
	if status.LastAttemptAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, status.LastAttemptAt)
	if err != nil {
		return true
	}
	return !time.Now().UTC().Before(at.Add(backoffFor(status.Attempts, retry)))
}

func stallDue(lastAttemptAt string, at time.Time) bool {
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(lastAttemptAt))
	if err != nil {
		return true
	}
	return !at.Before(last.Add(stallRetryInterval))
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
		return equalHash(observation.Hash, desiredHash)
	}
	if acked != nil && acked.Ref != "" {
		return acked.Ref == observation.Ref
	}
	return false
}

func equalHash(left, right string) bool {
	left = strings.TrimPrefix(strings.TrimSpace(left), "sha256:")
	right = strings.TrimPrefix(strings.TrimSpace(right), "sha256:")
	return strings.EqualFold(left, right)
}

func storeObservation(status *v1alpha1.StateSyncStatus, observation Observation) {
	observed := &v1alpha1.StateSyncObservation{
		Exists: observation.Exists,
		Ref:    observation.Ref,
		Cid:    observation.CID,
		Hash:   observation.Hash,
		At:     observation.At,
	}
	if observation.Meta != nil {
		observed.Meta = make(map[string]string, len(observation.Meta))
		maps.Copy(observed.Meta, observation.Meta)
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
	if value, ok := observation.Meta["eventUri"]; ok && value != "" {
		acknowledged.EventURI = value
	}
	if value, ok := observation.Meta["eventCid"]; ok && value != "" {
		acknowledged.EventCid = value
	}
	status.Acked = &acknowledged
}

func decodeStateSync(u *unstructured.Unstructured) (*v1alpha1.StateSync, error) {
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	var obj v1alpha1.StateSync
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func writeStatus(ctx context.Context, ri dynamic.ResourceInterface, name string, status *v1alpha1.StateSyncStatus) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return err
	}
	if object == nil {
		object = map[string]any{}
	}
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		existing, _, _ := unstructured.NestedMap(cur.Object, "status")
		if reflect.DeepEqual(existing, object) {
			return nil
		}
		cur.Object["status"] = object
		if _, err := ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("statesync: the status of %s stayed in conflict", name)
}

func ensureFinalizer(ctx context.Context, ri dynamic.ResourceInterface, name, finalizer string, want bool) error {
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		finalizers := cur.GetFinalizers()
		has := containsString(finalizers, finalizer)
		if want == has {
			return nil
		}
		if want {
			finalizers = append(finalizers, finalizer)
		} else {
			kept := finalizers[:0]
			for _, value := range finalizers {
				if value != finalizer {
					kept = append(kept, value)
				}
			}
			finalizers = kept
		}
		cur.SetFinalizers(finalizers)
		if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("statesync: the finalizers of %s stayed in conflict", name)
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || apierrors.IsNotFound(err)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

type watcher struct {
	workspace string

	dynamic DynamicFactory

	gvr schema.GroupVersionResource

	log *slog.Logger

	retry time.Duration

	interval time.Duration

	kick <-chan struct{}

	reconcile func(context.Context, dynamic.Interface, *unstructured.Unstructured) error
}

func (w *watcher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := w.round(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("watching StateSync resources", "workspace", w.workspace, "err", err.Error())
		}
		if !wait(ctx, w.retry, w.kick) {
			return nil
		}
	}
	return nil
}

func (w *watcher) round(ctx context.Context) error {
	dyn, err := w.dynamic(w.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(w.gvr)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := w.reconcile(ctx, dyn, &list.Items[i]); err != nil && ctx.Err() == nil {
			w.log.Warn("reconciling a StateSync", "workspace", w.workspace, "name", list.Items[i].GetName(), "err", err.Error())
		}
	}
	wc, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wc.Stop()
	retryTimer := time.NewTimer(w.retry)
	defer retryTimer.Stop()
	var resyncC <-chan time.Time
	if w.interval > 0 {
		resyncTimer := time.NewTimer(w.interval)
		defer resyncTimer.Stop()
		resyncC = resyncTimer.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-retryTimer.C:
			return nil
		case <-resyncC:
			return nil
		case _, ok := <-w.kick:
			if !ok {
				w.kick = nil
			}
			return nil
		case event, ok := <-wc.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Bookmark || event.Type == watch.Deleted {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			if err := w.reconcile(ctx, dyn, u); err != nil && ctx.Err() == nil {
				w.log.Warn("reconciling a StateSync", "workspace", w.workspace, "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func wait(ctx context.Context, d time.Duration, kick <-chan struct{}) bool {
	if d <= 0 {
		d = DefaultRetryAfter
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-kick:
		return true
	}
}
