package childapplication

import (
	"context"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	PolicyExport = "policy"

	coreUnitPrefix   = "socialweb-core-"
	coreUnitSuffix   = "-dev.service"
	coreObjectPrefix = "core-"
	coreObjectSuffix = "-dev"
)

type component struct {
	v1alpha1.ChildApplicationComponent

	failed bool
}

type componentKind struct {
	read func(ctx context.Context, l Lister, workspace string) ([]component, error)

	watch func(mapFunc func(ctx context.Context, src ctlruntime.Request) []ctlruntime.Request) ctlruntime.Option
}

func kindOf[L any, PL interface {
	*L
	client.ObjectList
}, T any, PT interface {
	*T
	ctlruntime.Object
}](export, kind string, items func(*L) []T, state func(PT) (bool, bool)) componentKind {
	return componentKind{
		read: func(ctx context.Context, l Lister, workspace string) ([]component, error) {
			var list L
			if err := l.List(ctx, workspace, PL(&list)); err != nil {
				return nil, err
			}
			src := items(&list)
			out := make([]component, 0, len(src))
			for i := range src {
				obj := PT(&src[i])
				ready, failed := state(obj)
				out = append(out, component{ChildApplicationComponent: v1alpha1.ChildApplicationComponent{Kind: kind, Name: obj.GetName(), Ready: ready}, failed: failed})
			}
			return out, nil
		},
		watch: func(mapFunc func(ctx context.Context, src ctlruntime.Request) []ctlruntime.Request) ctlruntime.Option {
			return ctlruntime.WatchesIn(export, func(ctx context.Context, src ctlruntime.Request, _ PT) []ctlruntime.Request {
				return mapFunc(ctx, src)
			})
		},
	}
}

func componentState(s v1alpha1.ComponentStatus) (bool, bool) {
	return s.Ready, s.Phase == v1alpha1.ComponentPhaseFailed
}

var componentKinds = []componentKind{
	kindOf(Export, "Bidder", func(l *v1alpha1.BidderList) []v1alpha1.Bidder { return l.Items },
		func(o *v1alpha1.Bidder) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "GuestControlPlane", func(l *v1alpha1.GuestControlPlaneList) []v1alpha1.GuestControlPlane { return l.Items },
		func(o *v1alpha1.GuestControlPlane) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "Pds", func(l *v1alpha1.PdsList) []v1alpha1.Pds { return l.Items },
		func(o *v1alpha1.Pds) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "Requester", func(l *v1alpha1.RequesterList) []v1alpha1.Requester { return l.Items },
		func(o *v1alpha1.Requester) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "GatewayAPI", func(l *v1alpha1.GatewayAPIList) []v1alpha1.GatewayAPI { return l.Items },
		func(o *v1alpha1.GatewayAPI) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "SshServer", func(l *v1alpha1.SshServerList) []v1alpha1.SshServer { return l.Items },
		func(o *v1alpha1.SshServer) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "Plc", func(l *v1alpha1.PlcList) []v1alpha1.Plc { return l.Items },
		func(o *v1alpha1.Plc) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "Relay", func(l *v1alpha1.RelayList) []v1alpha1.Relay { return l.Items },
		func(o *v1alpha1.Relay) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "IngressProxy", func(l *v1alpha1.IngressProxyList) []v1alpha1.IngressProxy { return l.Items },
		func(o *v1alpha1.IngressProxy) (bool, bool) { return componentState(o.Status) }),
	kindOf(Export, "DispatcherTLS", func(l *v1alpha1.DispatcherTLSList) []v1alpha1.DispatcherTLS { return l.Items },
		func(o *v1alpha1.DispatcherTLS) (bool, bool) { return componentState(o.Status) }),
	kindOf(PolicyExport, "PolicyEngine", func(l *policyv1alpha1.PolicyEngineList) []policyv1alpha1.PolicyEngine { return l.Items },
		func(o *policyv1alpha1.PolicyEngine) (bool, bool) {
			return o.Status.Ready, o.Status.Phase == policyv1alpha1.PolicyEngineFailed
		}),
}

func (c *Controller) observeCore(ctx context.Context, instance string) (string, bool, bool, error) {
	var list v1alpha1.HostProcessList
	if err := c.objects.List(ctx, DevWorkspacePrefix+":"+instance, &list); err != nil {
		if absent(err) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		unit := item.Spec.Unit
		if !coreUnit(unit, item.Name) {
			continue
		}
		ref := unit
		if ref == "" {
			ref = item.Name
		}
		phase := item.Status.Phase
		ready := phase == v1alpha1.HostProcessPhaseRunning
		failed := phase == v1alpha1.HostProcessPhaseFailed || phase == v1alpha1.HostProcessPhaseLost
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
	var out []v1alpha1.ChildApplicationComponent
	failed := false
	for _, kind := range componentKinds {
		items, err := kind.read(ctx, c.objects, workspace+":"+ComponentWorkspace)
		if absent(err) {
			continue
		}
		if err != nil {
			return nil, false, false, err
		}
		for _, item := range items {
			failed = failed || item.failed
			out = append(out, item.ChildApplicationComponent)
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
