package componentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type componentKind struct {
	Kind string
	GVR  schema.GroupVersionResource
}

func ComponentKinds() []string {
	out := make([]string, 0, len(appctl.ComponentTypes))
	for _, kind := range appctl.ComponentTypes {
		out = append(out, kind.Kind)
	}
	return out
}

func RuntimeKinds() []string {
	out := make([]string, 0, len(runtimeKinds))
	for _, kind := range runtimeKinds {
		out = append(out, kind.Kind)
	}
	return out
}

var runtimeKinds = []componentKind{
	{appctl.ComponentKindBidder, appctl.BidderGVR},
	{appctl.ComponentKindGuestControlPlane, appctl.GuestControlPlaneGVR},
	{appctl.ComponentKindPds, appctl.PdsGVR},
	{appctl.ComponentKindRequester, appctl.RequesterGVR},
	{appctl.ComponentKindGatewayAPI, appctl.GatewayAPIGVR},
	{appctl.ComponentKindSshServer, appctl.SshServerGVR},
	{appctl.ComponentKindPlc, appctl.PlcGVR},
	{appctl.ComponentKindRelay, appctl.RelayGVR},
	{appctl.ComponentKindIngressProxy, appctl.IngressProxyGVR},
	{appctl.ComponentKindDispatcherTLS, appctl.DispatcherTLSGVR},
}

type source struct {
	workspaces func(context.Context) []string
	dynamic    DynamicFactory
	log        *slog.Logger
	kinds      []componentKind
}

func newSource(cfg Config, dynamic DynamicFactory) *source {
	workspaces := cfg.WorkspacesFunc
	if workspaces == nil {
		list := append([]string(nil), cfg.Workspaces...)
		workspaces = func(context.Context) []string { return list }
	}
	return &source{workspaces: workspaces, dynamic: dynamic, log: cfg.Log, kinds: runtimeKinds}
}

func (s *source) List(ctx context.Context) ([]component.ComponentSpec, error) {
	var out []component.ComponentSpec
	for _, workspace := range s.workspaces(ctx) {
		dyn, err := s.dynamic(workspace)
		if err != nil {
			s.log.Warn("component: the dynamic client", "workspace", workspace, "err", err.Error())
			continue
		}
		for _, kind := range s.kinds {
			list, err := dyn.Resource(kind.GVR).List(ctx, metav1.ListOptions{})
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				continue
			}
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("component: listing components", "workspace", workspace, "kind", kind.Kind, "err", err.Error())
				}
				continue
			}
			for i := range list.Items {
				spec, err := specOf(kind.Kind, workspace, &list.Items[i])
				if err != nil {
					s.log.Warn("component: the component object", "workspace", workspace, "kind", kind.Kind, "name", list.Items[i].GetName(), "err", err.Error())
					continue
				}
				out = append(out, spec)
			}
		}
	}
	return out, nil
}

func (s *source) Watch(ctx context.Context) (<-chan component.Event, error) {
	out := make(chan component.Event, 256)
	var wg sync.WaitGroup
	for _, workspace := range s.workspaces(ctx) {
		for _, kind := range s.kinds {
			wg.Add(1)
			go func(workspace string, kind componentKind) {
				defer wg.Done()
				s.watch(ctx, workspace, kind, out)
			}(workspace, kind)
		}
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

func (s *source) watch(ctx context.Context, workspace string, kind componentKind, out chan<- component.Event) {
	for ctx.Err() == nil {
		dyn, err := s.dynamic(workspace)
		if err != nil {
			sleep(ctx, RetryAfter)
			continue
		}
		w, err := dyn.Resource(kind.GVR).Watch(ctx, metav1.ListOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) && ctx.Err() == nil {
				s.log.Warn("component: watching components", "workspace", workspace, "kind", kind.Kind, "err", err.Error())
			}
			sleep(ctx, RetryAfter)
			continue
		}
		for event := range w.ResultChan() {
			if ctx.Err() != nil {
				w.Stop()
				return
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			spec, err := specOf(kind.Kind, workspace, u)
			if err != nil {
				s.log.Warn("component: the watched component", "workspace", workspace, "kind", kind.Kind, "name", u.GetName(), "err", err.Error())
				continue
			}
			var eventType component.EventType
			switch event.Type {
			case "ADDED":
				eventType = component.EventAdded
			case "MODIFIED":
				eventType = component.EventModified
			case "DELETED":
				eventType = component.EventDeleted
			default:
				continue
			}
			select {
			case out <- component.Event{Type: eventType, Spec: spec}:
			case <-ctx.Done():
				w.Stop()
				return
			}
		}
		w.Stop()
		sleep(ctx, RetryAfter)
	}
}

func specOf(kind, workspace string, u *unstructured.Unstructured) (component.ComponentSpec, error) {
	spec := component.ComponentSpec{
		UID:         u.GetUID(),
		Kind:        kind,
		Name:        u.GetName(),
		Namespace:   u.GetNamespace(),
		Workspace:   workspace,
		Generation:  u.GetGeneration(),
		Deleting:    u.GetDeletionTimestamp() != nil,
		Annotations: u.GetAnnotations(),
		Labels:      u.GetLabels(),
		Group:       u.GroupVersionKind().Group,
	}
	object, ok := u.Object["spec"].(map[string]any)
	if !ok {
		return spec, nil
	}
	spec.Suspend, _, _ = unstructured.NestedBool(object, "suspend")
	spec.RestartPolicy = component.RestartPolicy(nestedString(object, "restartPolicy"))
	spec.StateDir = nestedString(object, "stateDir")
	raw, err := json.Marshal(object)
	if err != nil {
		return spec, fmt.Errorf("component: the spec of %s/%s: %w", kind, spec.Name, err)
	}
	spec.Spec = component.RawSpec(raw)
	return spec, nil
}

func nestedString(object map[string]any, field string) string {
	value, _, _ := unstructured.NestedString(object, field)
	return value
}

type statusWriter struct {
	dynamic DynamicFactory
	log     *slog.Logger
}

func (w *statusWriter) WriteStatus(ctx context.Context, spec component.ComponentSpec, status component.ComponentStatus) error {
	if spec.Kind == appctl.ComponentKindPolicyEngine {
		return nil
	}
	kind, ok := kindFor(spec.Kind)
	if !ok {
		return fmt.Errorf("component: %q is not a component kind", spec.Kind)
	}
	dyn, err := w.dynamic(spec.Workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(kind.GVR)
	apiStatus := status.APIStatus()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&apiStatus)
	if err != nil {
		return fmt.Errorf("component: the status of %s/%s: %w", spec.Kind, spec.Name, err)
	}
	for range 5 {
		cur, err := ri.Get(ctx, spec.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		cur.Object["status"] = m
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: appctl.FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("component: the status of %s/%s stayed in conflict", spec.Kind, spec.Name)
}

func kindFor(name string) (componentKind, bool) {
	for _, kind := range runtimeKinds {
		if kind.Kind == name {
			return kind, true
		}
	}
	return componentKind{}, false
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
