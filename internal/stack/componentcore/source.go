package componentcore

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const RestartFieldManager = "socialweb.computer/dev-restart"

type DynamicFactory = kube.Factory

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
			u, err := kube.Get[metav1.PartialObjectMetadata](ctx, dyn.Resource(candidate.GVR), name)
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
			list, err := kube.List[metav1.PartialObjectMetadata](ctx, dyn.Resource(kind.GVR), metav1.ListOptions{})
			if err != nil {
				continue
			}
			for i := range list {
				out = append(out, workspace+":"+kind.Kind+"/"+list[i].GetName())
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
	u, err := kube.Get[componentStatus](ctx, dyn.Resource(ref.GVR), ref.Name)
	if err != nil {
		return RestartStatus{}, err
	}
	return u.Status, nil
}

func Restart(ctx context.Context, dynamic DynamicFactory, ref ComponentRef) (string, error) {
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	body := kube.Doc{"apiVersion": ref.GVR.GroupVersion().String(), "kind": ref.Kind,
		"metadata": map[string]any{"name": ref.Name}, "spec": map[string]any{"restartedAt": stamp}}
	if _, err := kube.Apply[kube.Doc](ctx, dyn.Resource(ref.GVR), body,
		kube.ApplyOptions{FieldManager: RestartFieldManager, Force: true}); err != nil {
		return "", fmt.Errorf("componentcore: requesting the restart of %s/%s: %w", ref.Kind, ref.Name, err)
	}
	return stamp, nil
}

func WaitRestarted(ctx context.Context, dynamic DynamicFactory, ref ComponentRef, stamp string, before RestartStatus, timeout time.Duration) (RestartStatus, error) {
	requested, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return RestartStatus{}, err
	}
	dyn, err := dynamic(ref.Workspace)
	if err != nil {
		return RestartStatus{}, err
	}
	last := before
	_, err = kube.Await(ctx, dyn.Resource(ref.GVR), ref.Name, timeout, func(u *componentStatus) (bool, error) {
		if u == nil {
			return false, nil
		}
		last = u.Status
		return last.Ready && restartedAfter(last, before, requested), nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return last, fmt.Errorf("componentcore: %s/%s in %s did not restart within %s (phase %s, ready %v, restarts %d, startedAt %s)",
			ref.Kind, ref.Name, ref.Workspace, timeout, last.Phase, last.Ready, last.Restarts, last.StartedAt)
	}
	return last, err
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

type componentStatus struct {
	Status RestartStatus `json:"status"`
}
