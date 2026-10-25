package exposure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	ReasonIngressPending = "Pending"

	ReasonIngressRegistered = "Registered"
)

var subscriberReasons = map[exposurev1alpha1.IngressSubscriberState]string{
	exposurev1alpha1.IngressSubscriberConnecting:   "Connecting",
	exposurev1alpha1.IngressSubscriberRegistered:   ReasonIngressRegistered,
	exposurev1alpha1.IngressSubscriberReconnecting: "Reconnecting",
	exposurev1alpha1.IngressSubscriberWaiting:      "Waiting",
	exposurev1alpha1.IngressSubscriberClosed:       "Closed",
}

func SubscriberStateOf(status ingress.SubscriberStatus) exposurev1alpha1.ExposureIngressSubscriber {
	out := exposurev1alpha1.ExposureIngressSubscriber{
		State:        exposurev1alpha1.IngressSubscriberState(status.State),
		LastError:    status.LastError,
		CloseCode:    int32(status.CloseCode),
		RelayVersion: status.RelayVersion,
	}
	if !status.RegisteredAt.IsZero() {
		at := metav1.NewTime(status.RegisteredAt.UTC().Truncate(time.Second))
		out.RegisteredAt = &at
	}
	return out
}

func IngressRegistered(sub *exposurev1alpha1.ExposureIngressSubscriber) (bool, string, string) {
	if sub == nil || sub.State == "" {
		return false, ReasonIngressPending, "no ingress subscriber has reported its state"
	}
	reason, ok := subscriberReasons[sub.State]
	if !ok {
		return false, ReasonIngressPending, fmt.Sprintf("the ingress subscriber reports an unknown state %q", sub.State)
	}
	if sub.State == exposurev1alpha1.IngressSubscriberRegistered {
		message := "the ingress subscriber is registered at the relay"
		if sub.RegisteredAt != nil {
			message += " since " + sub.RegisteredAt.UTC().Format(time.RFC3339)
		}
		if sub.RelayVersion != "" {
			message += " (relay " + sub.RelayVersion + ")"
		}
		return true, reason, message
	}
	message := "the ingress subscriber is " + string(sub.State)
	if sub.CloseCode != 0 {
		message += fmt.Sprintf(" after close code %d", sub.CloseCode)
	}
	if sub.LastError != "" {
		message += ": " + sub.LastError
	}
	return false, reason, message
}

func SetIngressRegistered(conditions *[]metav1.Condition, generation int64, route *exposurev1alpha1.ExposureIngress,
	sub *exposurev1alpha1.ExposureIngressSubscriber) {
	if route == nil && sub == nil {
		meta.RemoveStatusCondition(conditions, exposurev1alpha1.ConditionIngressRegistered)
		return
	}
	ok, reason, message := IngressRegistered(sub)
	conds.Set(conditions, generation, exposurev1alpha1.ConditionIngressRegistered, ok, reason, message)
}

func IngressRegisteredCurrent(obj *exposurev1alpha1.Exposure) bool {
	var route *exposurev1alpha1.ExposureIngress
	var sub *exposurev1alpha1.ExposureIngressSubscriber
	if obj.Spec.Mode == exposurev1alpha1.ExposureModeIngress && obj.Status.Ingress != nil {
		route, sub = obj.Status.Ingress, obj.Status.Ingress.Subscriber
		if route.Host == "" && route.URL == "" {
			route = nil
		}
	}
	want := append([]metav1.Condition(nil), obj.Status.Conditions...)
	SetIngressRegistered(&want, obj.Generation, route, sub)
	have := meta.FindStatusCondition(obj.Status.Conditions, exposurev1alpha1.ConditionIngressRegistered)
	next := meta.FindStatusCondition(want, exposurev1alpha1.ConditionIngressRegistered)
	switch {
	case have == nil || next == nil:
		return have == nil && next == nil
	default:
		return have.Status == next.Status && have.Reason == next.Reason && have.Message == next.Message
	}
}

func SubscriberStateEqual(a, b *exposurev1alpha1.ExposureIngressSubscriber) bool {
	return equality.Semantic.DeepEqual(a, b)
}

const SubscriberStatusPath = "ingress.subscriber"

func SubscriberStatusFields(state exposurev1alpha1.ExposureIngressSubscriber) map[string]any {
	subscriber := map[string]any{"state": string(state.State), "registeredAt": nil, "lastError": nil, "closeCode": nil, "relayVersion": nil}
	if state.RegisteredAt != nil {
		subscriber["registeredAt"] = state.RegisteredAt.UTC().Format(time.RFC3339)
	}
	if state.LastError != "" {
		subscriber["lastError"] = state.LastError
	}
	if state.CloseCode != 0 {
		subscriber["closeCode"] = int64(state.CloseCode)
	}
	if state.RelayVersion != "" {
		subscriber["relayVersion"] = state.RelayVersion
	}
	return map[string]any{"ingress": map[string]any{"subscriber": subscriber}}
}

func ReportSubscriberState(ctx context.Context, kcp *rest.Config, workspace, exposureName string,
	state exposurev1alpha1.ExposureIngressSubscriber) error {
	workspace, exposureName = strings.TrimSpace(workspace), strings.TrimSpace(exposureName)
	if workspace == "" || exposureName == "" {
		return errors.New("exposure: reporting an ingress subscriber state needs a workspace and an Exposure name")
	}
	if _, ok := subscriberReasons[state.State]; !ok {
		return fmt.Errorf("exposure: the ingress subscriber state %q is not one of connecting, registered, reconnecting, waiting, closed", state.State)
	}
	cl, err := kube.ForWorkspace(kcp, workspace)
	if err != nil {
		return err
	}
	doc := kube.Doc{"apiVersion": exposurev1alpha1.GroupVersion.String(), "kind": "Exposure", "metadata": map[string]any{"name": exposureName},
		"status": SubscriberStatusFields(state)}
	_, err = kube.Apply[exposurev1alpha1.Exposure](ctx, cl.Resource(exposurev1alpha1.ExposureGVR), doc,
		kube.ApplyOptions{FieldManager: exposurev1alpha1.FieldManagerIngressSubscriber, Force: true, Status: true})
	if err != nil {
		return fmt.Errorf("exposure: reporting the ingress subscriber of %s in %s: %w", exposureName, workspace, err)
	}
	return nil
}
