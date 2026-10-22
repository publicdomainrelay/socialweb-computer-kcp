package childapplication

import (
	"context"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	coreUnitPrefix   = "socialweb-core-"
	coreUnitSuffix   = "-dev.service"
	coreObjectPrefix = "core-"
	coreObjectSuffix = "-dev"
)

type componentKind struct {
	kind string
	gvr  schema.GroupVersionResource
}

var childComponentKinds = []componentKind{
	{kind: "Bidder", gvr: v1alpha1.GroupVersion.WithResource("bidders")},
	{kind: "GuestControlPlane", gvr: v1alpha1.GroupVersion.WithResource("guestcontrolplanes")},
	{kind: "Pds", gvr: v1alpha1.GroupVersion.WithResource("pds")},
	{kind: "Requester", gvr: v1alpha1.GroupVersion.WithResource("requesters")},
	{kind: "GatewayAPI", gvr: v1alpha1.GroupVersion.WithResource("gatewayapis")},
	{kind: "SshServer", gvr: v1alpha1.GroupVersion.WithResource("sshservers")},
	{kind: "Plc", gvr: v1alpha1.GroupVersion.WithResource("plcs")},
	{kind: "Relay", gvr: v1alpha1.GroupVersion.WithResource("relays")},
	{kind: "IngressProxy", gvr: v1alpha1.GroupVersion.WithResource("ingressproxies")},
	{kind: "DispatcherTLS", gvr: v1alpha1.GroupVersion.WithResource("dispatchertlses")},
	{kind: "PolicyEngine", gvr: policyv1alpha1.GroupVersion.WithResource("policyengines")},
}

func (c *Controller) observeCore(ctx context.Context, instance string) (string, bool, bool, error) {
	if instance == "" {
		return "", false, false, nil
	}
	dyn, err := c.client(devWorkspacePrefix + ":" + instance)
	if err != nil {
		return "", false, false, err
	}
	list, err := dyn.Resource(HostProcessGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		unit := item.GetAnnotations()[hostProcessUnitLabel]
		if unit == "" {
			unit, _, _ = unstructured.NestedString(item.Object, "spec", "unit")
		}
		if !coreUnit(unit, item.GetName()) {
			continue
		}
		ref := unit
		if ref == "" {
			ref = item.GetName()
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		ready := v1alpha1.HostProcessPhase(phase) == v1alpha1.HostProcessPhaseRunning
		failed := v1alpha1.HostProcessPhase(phase) == v1alpha1.HostProcessPhaseFailed ||
			v1alpha1.HostProcessPhase(phase) == v1alpha1.HostProcessPhaseLost
		return ref, ready, failed, nil
	}
	return "", false, false, nil
}

func coreUnit(unit, name string) bool {
	if strings.HasPrefix(unit, coreUnitPrefix) && strings.HasSuffix(unit, coreUnitSuffix) {
		return true
	}
	return strings.HasPrefix(name, coreObjectPrefix) && strings.HasSuffix(name, coreObjectSuffix)
}

func (c *Controller) observeComponents(ctx context.Context, workspace string) ([]v1alpha1.ChildApplicationComponent, bool, bool, error) {
	dyn, err := c.client(workspace + ":" + componentWorkspace)
	if err != nil {
		return nil, false, false, err
	}
	var out []v1alpha1.ChildApplicationComponent
	failed := false
	for _, ct := range childComponentKinds {
		list, err := dyn.Resource(ct.gvr).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, false, false, err
		}
		for i := range list.Items {
			ready, _, _ := unstructured.NestedBool(list.Items[i].Object, "status", "ready")
			phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
			if phase == string(v1alpha1.ComponentPhaseFailed) {
				failed = true
			}
			out = append(out, v1alpha1.ChildApplicationComponent{Kind: ct.kind, Name: list.Items[i].GetName(), Ready: ready})
		}
	}
	sortComponents(out)
	return out, failed, allReady(out), nil
}

func fallbackComponents(appStatus *v1alpha1.NetworkAppStatus) ([]v1alpha1.ChildApplicationComponent, bool, bool) {
	if appStatus == nil {
		return nil, false, false
	}
	out := make([]v1alpha1.ChildApplicationComponent, 0, len(appStatus.Components))
	failed := false
	for _, component := range appStatus.Components {
		if component.Phase == string(v1alpha1.ComponentPhaseFailed) {
			failed = true
		}
		out = append(out, v1alpha1.ChildApplicationComponent{Kind: component.Kind, Name: component.Name, Ready: component.Ready})
	}
	sortComponents(out)
	return out, failed, allReady(out)
}

func sortComponents(components []v1alpha1.ChildApplicationComponent) {
	sort.Slice(components, func(i, j int) bool {
		if components[i].Kind != components[j].Kind {
			return components[i].Kind < components[j].Kind
		}
		return components[i].Name < components[j].Name
	})
}

func allReady(components []v1alpha1.ChildApplicationComponent) bool {
	if len(components) == 0 {
		return false
	}
	for _, component := range components {
		if !component.Ready {
			return false
		}
	}
	return true
}
