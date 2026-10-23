package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	policyengine "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/status"
)

const TriggerActionComputeVM = "ComputeVM"

var ErrNoTriggerActionHandler = errors.New("no action handler is registered for the policy trigger action")

type ComputeVMActionHandler interface {
	DispatchComputeVM(ctx context.Context, action policyv1alpha1.PolicyAction, outcome Outcome) error
}

type ComputeVMActionHandlerFunc func(ctx context.Context, action policyv1alpha1.PolicyAction, outcome Outcome) error

func (f ComputeVMActionHandlerFunc) DispatchComputeVM(ctx context.Context, action policyv1alpha1.PolicyAction, outcome Outcome) error {
	return f(ctx, action, outcome)
}

type TriggerActionRegistry struct {
	mu       sync.RWMutex
	handlers map[string]ActionHandler
}

func (r *TriggerActionRegistry) Register(handler ActionHandler) error {
	if handler == nil {
		return errors.New("policy trigger: action handler is required")
	}
	kind := strings.TrimSpace(handler.Kind())
	if kind == "" {
		return errors.New("policy trigger: action handler kind is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = make(map[string]ActionHandler)
	}
	if _, exists := r.handlers[kind]; exists {
		return fmt.Errorf("policy trigger: action handler for %s is already registered", kind)
	}
	r.handlers[kind] = handler
	return nil
}

func (r *TriggerActionRegistry) RegisterComputeVM(handler ComputeVMActionHandler) error {
	if handler == nil {
		return errors.New("policy trigger: ComputeVM action handler is required")
	}
	return r.Register(computeVMActionAdapter{handler: handler})
}

func (r *TriggerActionRegistry) Lookup(kind string) (ActionHandler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[strings.TrimSpace(kind)]
	return handler, ok
}

func (r *TriggerActionRegistry) Dispatch(ctx context.Context, action policyv1alpha1.PolicyAction, outcome Outcome) error {
	kind := strings.TrimSpace(action.Kind)
	handler, ok := r.Lookup(kind)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoTriggerActionHandler, kind)
	}
	return handler.Dispatch(ctx, action.Spec, outcome)
}

func (r *TriggerActionRegistry) Handlers() map[string]ActionHandler {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ActionHandler, len(r.handlers))
	maps.Copy(out, r.handlers)
	return out
}

type computeVMActionAdapter struct {
	handler ComputeVMActionHandler
}

func (computeVMActionAdapter) Kind() string {
	return TriggerActionComputeVM
}

func (a computeVMActionAdapter) Dispatch(ctx context.Context, spec json.RawMessage, outcome Outcome) error {
	return a.handler.DispatchComputeVM(ctx, policyv1alpha1.PolicyAction{Kind: TriggerActionComputeVM, Spec: spec}, outcome)
}

type TriggerReconciler struct {
	Registry *TriggerActionRegistry
	Handlers map[string]ActionHandler
	Now      func() time.Time
}

func (r *TriggerReconciler) ReconcilePolicyTrigger(ctx context.Context, trigger *policyv1alpha1.PolicyTrigger) (TriggerReconcileState, error) {
	if trigger == nil {
		return TriggerReconcileState{}, fmt.Errorf("policy trigger is required")
	}
	if trigger.DeletionTimestamp != nil {
		state := TriggerReconcileState{}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Terminating",
			Message: "the policy trigger is being deleted",
		})
		return state, nil
	}
	if trigger.Spec.Suspend {
		state := TriggerReconcileState{}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    policyengine.ConditionSuspended,
			Status:  metav1.ConditionTrue,
			Reason:  "Suspended",
			Message: "the policy trigger is suspended",
		})
		return state, nil
	}
	kind := strings.TrimSpace(trigger.Spec.Action.Kind)
	if kind == "" {
		return triggerInvalid("action.kind is required"), nil
	}
	if _, ok := r.lookup(kind); !ok {
		return triggerInvalid("no action handler is registered for " + kind), nil
	}
	switch trigger.Spec.Source.Type {
	case policyv1alpha1.PolicyTriggerSourceRecord, policyv1alpha1.PolicyTriggerSourceRelay:
	case policyv1alpha1.PolicyTriggerSourceCron:
		if trigger.Spec.Source.Cron == nil || strings.TrimSpace(trigger.Spec.Source.Cron.Schedule) == "" {
			return triggerInvalid("source.cron.schedule is required"), nil
		}
	case policyv1alpha1.PolicyTriggerSourceKCP:
		if trigger.Spec.Source.KCP == nil || strings.TrimSpace(trigger.Spec.Source.KCP.Resource) == "" {
			return triggerInvalid("source.kcp.resource is required"), nil
		}
	default:
		return triggerInvalid("source.type must be Record, Relay, Cron or KCP"), nil
	}
	state := TriggerReconcileState{}
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    policyengine.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "the policy trigger is ready",
	})
	return state, nil
}

func (r *TriggerReconciler) Handle(ctx context.Context, trigger *policyv1alpha1.PolicyTrigger, outcome Outcome) (TriggerReconcileState, error) {
	if trigger == nil {
		return TriggerReconcileState{}, fmt.Errorf("policy trigger is required")
	}
	if trigger.Spec.Suspend {
		return TriggerReconcileState{}, nil
	}
	kind := strings.TrimSpace(trigger.Spec.Action.Kind)
	handler, ok := r.lookup(kind)
	if !ok {
		return triggerInvalid("no action handler is registered for " + kind), fmt.Errorf("unknown action handler %s", kind)
	}
	if !MatchesOutputs(outcome.Result.Outputs, trigger.Spec.Match) {
		state := TriggerReconcileState{}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    "Matched",
			Status:  metav1.ConditionFalse,
			Reason:  "NoMatch",
			Message: "the run outputs did not match the trigger",
		})
		return state, nil
	}
	if err := handler.Dispatch(ctx, trigger.Spec.Action.Spec, outcome); err != nil {
		state := TriggerReconcileState{Active: 1}
		meta.SetStatusCondition(&state.Conditions, metav1.Condition{
			Type:    "Dispatched",
			Status:  metav1.ConditionFalse,
			Reason:  "DispatchFailed",
			Message: err.Error(),
		})
		return state, err
	}
	now := metav1.NewTime(r.now())
	state := TriggerReconcileState{LastRun: &now, Active: 1}
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    "Dispatched",
		Status:  metav1.ConditionTrue,
		Reason:  "Matched",
		Message: "the run outputs matched and the action was dispatched",
	})
	return state, nil
}

func (r *TriggerReconciler) lookup(kind string) (ActionHandler, bool) {
	kind = strings.TrimSpace(kind)
	if r.Registry != nil {
		return r.Registry.Lookup(kind)
	}
	if r.Handlers == nil {
		return nil, false
	}
	handler, ok := r.Handlers[kind]
	return handler, ok
}

func (r *TriggerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func triggerInvalid(message string) TriggerReconcileState {
	state := TriggerReconcileState{}
	meta.SetStatusCondition(&state.Conditions, metav1.Condition{
		Type:    policyengine.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Invalid",
		Message: message,
	})
	return state
}

func MatchesOutputs(outputs models.Outputs, match map[string]string) bool {
	if len(match) == 0 {
		return true
	}
	for path, expected := range match {
		value, ok := lookupOutput(outputs, path)
		if !ok {
			return false
		}
		if !sameValue(value, expected) {
			return false
		}
	}
	return true
}

func lookupOutput(outputs models.Outputs, path string) (any, bool) {
	var current any = map[string]any(outputs)
	for part := range strings.SplitSeq(path, ".") {
		switch object := current.(type) {
		case map[string]any:
			value, exists := object[part]
			if !exists {
				return nil, false
			}
			current = value
		case map[string]string:
			value, exists := object[part]
			if !exists {
				return nil, false
			}
			current = value
		case models.Outputs:
			value, exists := object[part]
			if !exists {
				return nil, false
			}
			current = value
		default:
			return nil, false
		}
	}
	return current, true
}

func sameValue(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case bool:
		return strconv.FormatBool(typed) == expected
	case nil:
		return expected == "" || expected == "null"
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed) == expected
		}
		return string(data) == expected
	}
}
