package componentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type componentKind struct {
	Kind string
	GVR  schema.GroupVersionResource
}

var runtimeKinds = []componentKind{
	{compose.ComponentKindBidder, compose.BidderGVR},
	{compose.ComponentKindGuestControlPlane, compose.GuestControlPlaneGVR},
	{compose.ComponentKindPds, compose.PdsGVR},
	{compose.ComponentKindRequester, compose.RequesterGVR},
	{compose.ComponentKindGatewayAPI, compose.GatewayAPIGVR},
	{compose.ComponentKindSshServer, compose.SshServerGVR},
	{compose.ComponentKindPlc, compose.PlcGVR},
	{compose.ComponentKindRelay, compose.RelayGVR},
	{compose.ComponentKindIngressProxy, compose.IngressProxyGVR},
	{compose.ComponentKindDispatcherTLS, compose.DispatcherTLSGVR},
}

type ComponentRef struct {
	Workspace string
	Kind      string
	Name      string
	GVR       schema.GroupVersionResource
}

type RestartStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Phase              string              `json:"phase,omitempty"`
	Ready              bool                `json:"ready"`
	Message            string              `json:"message,omitempty"`
	Restarts           int32               `json:"restarts,omitempty"`
	StartedAt          string              `json:"startedAt,omitempty"`
	Endpoints          []v1alpha1.Endpoint `json:"endpoints,omitempty"`
}

func FindComponents(ctx context.Context, dynamic DynamicFactory, workspaces []string, kind, name string) ([]ComponentRef, error) {
	var out []ComponentRef
	for _, workspace := range workspaces {
		dyn, err := dynamic(workspace)
		if err != nil {
			return nil, err
		}
		for _, candidate := range runtimeKinds {
			if kind != "" && candidate.Kind != kind {
				continue
			}
			u, err := dyn.Resource(candidate.GVR).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("componentcore: reading %s %s in %s: %w", candidate.Kind, name, workspace, err)
			}
			out = append(out, ComponentRef{Workspace: workspace, Kind: candidate.Kind, Name: u.GetName(), GVR: candidate.GVR})
		}
	}
	return out, nil
}

func DescribeComponents(ctx context.Context, dynamic DynamicFactory, workspaces []string) []string {
	var out []string
	for _, workspace := range workspaces {
		dyn, err := dynamic(workspace)
		if err != nil {
			continue
		}
		for _, kind := range runtimeKinds {
			list, err := dyn.Resource(kind.GVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for i := range list.Items {
				out = append(out, workspace+":"+kind.Kind+"/"+list.Items[i].GetName())
			}
		}
	}
	return out
}

func ReadStatus(ctx context.Context, dynamic DynamicFactory, ref ComponentRef) (RestartStatus, error) {
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return RestartStatus{}, err
	}
	u, err := dyn.Resource(ref.GVR).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return RestartStatus{}, err
	}
	return statusOf(u), nil
}

func Restart(ctx context.Context, dynamic DynamicFactory, ref ComponentRef) (string, error) {
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		component.AnnotationRestartAt: stamp}}})
	if err != nil {
		return "", err
	}
	if _, err := dyn.Resource(ref.GVR).Patch(ctx, ref.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return "", fmt.Errorf("componentcore: requesting the restart of %s/%s: %w", ref.Kind, ref.Name, err)
	}
	return stamp, nil
}

func WaitRestarted(ctx context.Context, dynamic DynamicFactory, ref ComponentRef, stamp string, before RestartStatus, timeout time.Duration) (RestartStatus, error) {
	requested, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return RestartStatus{}, err
	}
	deadline := time.Now().Add(timeout)
	last := before
	for {
		cur, err := ReadStatus(ctx, dynamic, ref)
		if err == nil {
			last = cur
			if cur.Ready && restartedAfter(cur, before, requested) {
				return cur, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("componentcore: %s/%s in %s did not restart within %s (phase %s, ready %v, restarts %d, startedAt %s)",
				ref.Kind, ref.Name, ref.Workspace, timeout, last.Phase, last.Ready, last.Restarts, last.StartedAt)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func restartedAfter(cur, before RestartStatus, requested time.Time) bool {
	if cur.StartedAt != "" {
		if started, err := time.Parse(time.RFC3339, cur.StartedAt); err == nil {
			if !started.Before(requested.Truncate(time.Second)) {
				return true
			}
		}
	}
	if cur.Restarts > before.Restarts {
		return true
	}
	return false
}

func statusOf(u *unstructured.Unstructured) RestartStatus {
	var st RestartStatus
	status, ok := u.Object["status"].(map[string]any)
	if !ok {
		return st
	}
	st.ObservedGeneration, _, _ = unstructured.NestedInt64(status, "observedGeneration")
	st.Phase, _, _ = unstructured.NestedString(status, "phase")
	st.Ready, _, _ = unstructured.NestedBool(status, "ready")
	st.Message, _, _ = unstructured.NestedString(status, "message")
	restarts, _, _ := unstructured.NestedInt64(status, "restarts")
	st.Restarts = int32(restarts)
	st.StartedAt, _, _ = unstructured.NestedString(status, "startedAt")
	if endpoints, ok, _ := unstructured.NestedSlice(status, "endpoints"); ok {
		for _, entry := range endpoints {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			var endpoint v1alpha1.Endpoint
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &endpoint); err == nil {
				st.Endpoints = append(st.Endpoints, endpoint)
			}
		}
	}
	return st
}
