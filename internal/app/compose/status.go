package compose

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	ConditionComponentsReady         = "ComponentsReady"
	ConditionComponentSuspended      = "ComponentSuspended"
	ConditionComponentOverridesValid = "ComponentOverridesValid"
	ConditionLimitsEnforced          = "LimitsEnforced"
	ConditionSuspended               = "Suspended"

	ReasonSuspended               = "Suspended"
	ReasonNotSuspended            = "NotSuspended"
	ReasonOverridesApplied        = "OverridesApplied"
	ReasonComponentLimitsInactive = "ComponentLimitsNotEnforced"
	ReasonParticipationSuspended  = "ParticipationSuspended"
	ReasonActive                  = "Active"

	SuspendedMessage = "suspended: no new participation; obligations already taken (running VMs, StateSync acks, offer withdrawal, credential renewal) continue"
)

var componentOwnedStatus = []string{"market"}

type Snapshot struct {
	Views      []ComponentView
	Components []v1alpha1.ApplicationComponent
	Listeners  []v1alpha1.ApplicationListener
}

func (c *Composer) Snapshot(ctx context.Context, ws, owner string) (Snapshot, error) {
	views, err := c.ListComponents(ctx, ws, owner)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Views: views}
	for _, v := range views {
		if !IsExposureKind(v.Kind) {
			snap.Components = append(snap.Components, v.ApplicationComponent())
		}
		snap.Listeners = append(snap.Listeners, v.Listeners()...)
	}
	sort.Slice(snap.Listeners, func(i, j int) bool {
		if snap.Listeners[i].Component != snap.Listeners[j].Component {
			return snap.Listeners[i].Component < snap.Listeners[j].Component
		}
		return snap.Listeners[i].Name < snap.Listeners[j].Name
	})
	return snap, nil
}

func (s Snapshot) View(kind, name string) (ComponentView, bool) {
	for _, v := range s.Views {
		if v.Kind == kind && v.Name == name {
			return v, true
		}
	}
	return ComponentView{}, false
}

func (s Snapshot) Ready(kind, name string) bool {
	v, ok := s.View(kind, name)
	return ok && v.Status.Ready
}

func (s Snapshot) ExposureURL(name string) (string, bool) {
	v, ok := s.View(ComponentKindExposure, name)
	if !ok || !v.Status.Ready {
		return "", false
	}
	for _, e := range v.Status.Endpoints {
		if e.URL != "" {
			return e.URL, true
		}
	}
	return "", false
}

func DesiredSuspended(d Desired) bool {
	m, err := SpecMap(d.Spec)
	if err != nil || m == nil {
		return false
	}
	v, _ := m["suspend"].(bool)
	return v
}

func ParticipationSuspended(d Desired) bool {
	m, err := SpecMap(d.Spec)
	if err != nil || m == nil {
		return false
	}
	v, _ := m["suspendParticipation"].(bool)
	return v
}

func (s Snapshot) Satisfied(desired []Desired) bool {
	if len(desired) == 0 {
		return false
	}
	for _, d := range desired {
		if IsExposureKind(d.Kind) {
			continue
		}
		v, ok := s.View(d.Kind, d.Name)
		if !ok {
			return false
		}
		if DesiredSuspended(d) {
			if v.Status.Phase != v1alpha1.ComponentPhaseSuspended {
				return false
			}
			continue
		}
		if !v.Status.Ready {
			return false
		}
	}
	return true
}

func Ready(st v1alpha1.ApplicationStatus, msg string) v1alpha1.ApplicationStatus {
	st.Phase, st.Message = v1alpha1.ApplicationPhaseReady, msg
	return st
}

func Suspended(st v1alpha1.ApplicationStatus) v1alpha1.ApplicationStatus {
	st.Phase, st.Message = v1alpha1.ApplicationPhaseSuspended, SuspendedMessage
	return st
}

func SetSuspendCondition(conditions *[]metav1.Condition, gen int64, suspend bool) {
	if suspend {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionSuspended, Status: metav1.ConditionTrue,
			Reason: ReasonParticipationSuspended, Message: SuspendedMessage, ObservedGeneration: gen})
		return
	}
	meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionSuspended, Status: metav1.ConditionFalse,
		Reason: ReasonActive, Message: "the root participates", ObservedGeneration: gen})
}

func FillComponentStatus(conditions *[]metav1.Condition, summaryOut **v1alpha1.ComponentSummary,
	overrideStatusOut *[]v1alpha1.ComponentOverrideStatus, gen int64, rootSuspend bool, overrides v1alpha1.ComponentOverrides,
	desired []Desired, snap Snapshot, overrideErr error) {
	summary := &v1alpha1.ComponentSummary{}
	var suspended, notReady []string
	for _, d := range desired {
		if IsExposureKind(d.Kind) {
			continue
		}
		summary.Desired++
		key := ComponentKey(d.Kind, d.Name)
		v, ok := snap.View(d.Kind, d.Name)
		if !ok {
			summary.NotReady++
			notReady = append(notReady, key)
			continue
		}
		if DesiredSuspended(d) || (ParticipationSuspended(d) && v.Status.Ready) {
			summary.Suspended++
			suspended = append(suspended, key)
			continue
		}
		if v.Status.Ready {
			summary.Ready++
		} else {
			summary.NotReady++
			notReady = append(notReady, key)
		}
	}
	*summaryOut = summary
	if summary.NotReady == 0 && summary.Desired > 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentsReady, Status: metav1.ConditionTrue,
			Reason: "Ready", Message: fmt.Sprintf("%d component(s) are ready or suspended by intent", summary.Desired), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentsReady, Status: metav1.ConditionFalse,
			Reason: "Waiting", Message: strings.Join(notReady, "; "), ObservedGeneration: gen})
	}
	if len(suspended) > 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentSuspended, Status: metav1.ConditionTrue,
			Reason: ReasonSuspended, Message: strings.Join(suspended, "; "), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentSuspended, Status: metav1.ConditionFalse,
			Reason: ReasonNotSuspended, Message: "no component is suspended by intent", ObservedGeneration: gen})
	}
	SetSuspendCondition(conditions, gen, rootSuspend)
	if overrideErr != nil {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionFalse,
			Reason: OverrideErrorReason(overrideErr), Message: overrideErr.Error(), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionTrue,
			Reason: ReasonOverridesApplied, Message: fmt.Sprintf("%d component override(s) resolved", len(overrides)), ObservedGeneration: gen})
	}
	var limited []string
	for key, ov := range overrides {
		if ov.Limits != nil {
			limited = append(limited, key)
		}
	}
	if len(limited) > 0 {
		sort.Strings(limited)
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionLimitsEnforced, Status: metav1.ConditionFalse,
			Reason: ReasonComponentLimitsInactive, Message: "the in-process component runtime does not enforce ComponentLimits for " + strings.Join(limited, "; "),
			ObservedGeneration: gen})
	}
	resolved, err := resolveOverrides(overrides, desired)
	if err == nil {
		keys := make([]string, 0, len(resolved))
		for key := range resolved {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]v1alpha1.ComponentOverrideStatus, 0, len(keys))
		for _, key := range keys {
			ov := resolved[key]
			kind, name, _ := strings.Cut(key, "/")
			item := v1alpha1.ComponentOverrideStatus{Component: key, RestartPolicy: string(ov.RestartPolicy),
				Suspend: ov.Suspend != nil && *ov.Suspend}
			if v, ok := snap.View(kind, name); ok {
				item.Generation = v.Generation
				item.Applied = true
			}
			out = append(out, item)
		}
		*overrideStatusOut = out
	}
}

func FillApplicationStatus(st *v1alpha1.ApplicationStatus, gen int64, rootSuspend bool, overrides v1alpha1.ComponentOverrides,
	desired []Desired, snap Snapshot, overrideErr error) {
	FillComponentStatus(&st.Conditions, &st.ComponentSummary, &st.ComponentOverrides, gen, rootSuspend, overrides, desired, snap, overrideErr)
}

func SetOverrideErrorIfAny(conditions *[]metav1.Condition, summary **v1alpha1.ComponentSummary, gen int64, err error) {
	if _, ok := err.(*OverrideError); ok {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionFalse,
			Reason: OverrideErrorReason(err), Message: err.Error(), ObservedGeneration: gen})
		*summary = &v1alpha1.ComponentSummary{}
	}
}

func NewStatus(gen int64, profile string, previous v1alpha1.ApplicationStatus) v1alpha1.ApplicationStatus {
	return v1alpha1.ApplicationStatus{ObservedGeneration: gen, Profile: profile, Did: previous.Did,
		Conditions: append([]metav1.Condition(nil), previous.Conditions...)}
}

func (c *Composer) WriteStatus(ctx context.Context, r Root, gvr schema.GroupVersionResource, status any) error {
	m, err := ToMap(status)
	if err != nil {
		return err
	}
	for _, k := range componentOwnedStatus {
		delete(m, k)
	}
	c.samples.into(ctx, c, m, jsonFields(status))
	u := NewDoc(r.APIVersion, r.Kind, r.Name)
	u["status"] = m
	ri, err := c.Client(r.Workspace, gvr)
	if err != nil {
		return err
	}
	if _, err := kube.Apply[kube.Doc](ctx, ri, u, kube.ApplyOptions{FieldManager: r.Manager(), Force: true, Status: true}); err != nil {
		return fmt.Errorf("compose: applying the status of %s %s: %w", r.Kind, r.Name, err)
	}
	return nil
}

func jsonFields(v any) map[string]bool {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]bool{}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

func ApplicationStatusOf(u *Generic) (v1alpha1.ApplicationStatus, error) {
	var st v1alpha1.ApplicationStatus
	m := u.Status
	if len(m) == 0 {
		return st, nil
	}
	return st, runtime.DefaultUnstructuredConverter.FromUnstructured(m, &st)
}
