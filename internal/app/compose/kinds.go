package compose

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

const (
	OwnerLabel       = "apps.socialweb.computer/root"
	RootKindLabel    = "apps.socialweb.computer/root-kind"
	RootClusterLabel = "apps.socialweb.computer/root-cluster"
	ComponentLabel   = "apps.socialweb.computer/component"
	ManagedByLabel   = "app.kubernetes.io/managed-by"
	LinkSetLabel     = "apps.socialweb.computer/link-set"

	ComponentRestartAnnotation = "component.socialweb.computer/restart-at"

	ComponentKindBidder            = "Bidder"
	ComponentKindGuestControlPlane = "GuestControlPlane"
	ComponentKindPds               = "Pds"
	ComponentKindRequester         = "Requester"
	ComponentKindGatewayAPI        = "GatewayAPI"
	ComponentKindSshServer         = "SshServer"
	ComponentKindPlc               = "Plc"
	ComponentKindRelay             = "Relay"
	ComponentKindIngressProxy      = "IngressProxy"
	ComponentKindDispatcherTLS     = "DispatcherTLS"
	ComponentKindPolicyEngine      = "PolicyEngine"
	ComponentKindPolicyWorkflow    = "PolicyWorkflow"
	ComponentKindPolicyWorkflowRun = "PolicyWorkflowRun"
	ComponentKindPolicyTrigger     = "PolicyTrigger"
	ComponentKindService           = "Service"
	ComponentKindExposure          = "Exposure"
	ComponentKindComponentLink     = "ComponentLink"

	KindMarketIdentity  = "MarketIdentity"
	KindOpenBaoServer   = "OpenBaoServer"
	KindVault           = "Vault"
	KindAtprotoRepo     = "AtprotoRepo"
	KindLocalRepo       = "LocalRepo"
	KindRemoteRepo      = "RemoteRepo"
	KindRemotePDS       = "RemotePDS"
	KindComputeVM       = "ComputeVM"
	KindBid             = "Bid"
	KindComputeContract = "ComputeContract"
	KindStateSync       = "StateSync"
	KindMarketPresence  = "MarketPresence"
	KindHostProcess     = "HostProcess"

	ExposureLocalSuffix   = "-local"
	ExposureIngressSuffix = "-ingress"

	PdsComponent          = "pds"
	PolicyEngineComponent = "policy-engine"
	PdsVaultClient        = "pds"
	AppIngressName        = "ingress"
	NetworkIngressName    = "dispatcher"
	NetworkRelayName      = "relay"
	NetworkPLCName        = "plc"
	ChildRootName         = "app"
)

var (
	MarketNetworkGVR   = v1alpha1.MarketNetworkGVR
	MarketBidderGVR    = v1alpha1.MarketBidderGVR
	MarketRequesterGVR = v1alpha1.MarketRequesterGVR
	GatewayGVR         = v1alpha1.GatewayGVR
	SshGatewayGVR      = v1alpha1.SshGatewayGVR
	HostProcessGVR     = v1alpha1.HostProcessGVR

	BidderGVR            = v1alpha1.BidderGVR
	GuestControlPlaneGVR = v1alpha1.GuestControlPlaneGVR
	PdsGVR               = v1alpha1.PdsGVR
	RequesterGVR         = v1alpha1.RequesterGVR
	GatewayAPIGVR        = v1alpha1.GatewayAPIGVR
	SshServerGVR         = v1alpha1.SshServerGVR
	PlcGVR               = v1alpha1.PlcGVR
	RelayGVR             = v1alpha1.RelayGVR
	IngressProxyGVR      = v1alpha1.IngressProxyGVR
	DispatcherTLSGVR     = v1alpha1.DispatcherTLSGVR

	PolicyEngineGVR      = policyv1alpha1.PolicyEngineGVR
	PolicyWorkflowGVR    = policyv1alpha1.PolicyWorkflowGVR
	PolicyWorkflowRunGVR = policyv1alpha1.PolicyWorkflowRunGVR
	PolicyTriggerGVR     = policyv1alpha1.PolicyTriggerGVR

	ServiceGVR       = exposurev1alpha1.ServiceGVR
	ExposureGVR      = exposurev1alpha1.ExposureGVR
	ComponentLinkGVR = exposurev1alpha1.ComponentLinkGVR

	MarketIdentityGVR  = v1alpha1.MarketIdentityGVR
	OpenBaoServerGVR   = v1alpha1.OpenBaoServerGVR
	VaultGVR           = v1alpha1.VaultGVR
	ComputeContractGVR = v1alpha1.ComputeContractGVR
	AtprotoRecordGVR   = v1alpha1.AtprotoRecordGVR
	RecordWatchGVR     = v1alpha1.AtprotoRecordWatchGVR
	RemotePDSGVR       = pdsv1alpha1.RemotePDSGVR
	WorkspaceGVR       = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
	EventGVR           = schema.GroupVersionResource{Version: "v1", Resource: "events"}
)

type ComponentType struct {
	Kind string
	GVR  schema.GroupVersionResource
}

func (t ComponentType) APIVersion() string {
	return t.GVR.Group + "/" + t.GVR.Version
}

var ComponentTypes = []ComponentType{
	{ComponentKindBidder, BidderGVR},
	{ComponentKindGuestControlPlane, GuestControlPlaneGVR},
	{ComponentKindPds, PdsGVR},
	{ComponentKindRequester, RequesterGVR},
	{ComponentKindGatewayAPI, GatewayAPIGVR},
	{ComponentKindSshServer, SshServerGVR},
	{ComponentKindPlc, PlcGVR},
	{ComponentKindRelay, RelayGVR},
	{ComponentKindIngressProxy, IngressProxyGVR},
	{ComponentKindDispatcherTLS, DispatcherTLSGVR},
	{ComponentKindPolicyEngine, PolicyEngineGVR},
	{ComponentKindService, ServiceGVR},
	{ComponentKindExposure, ExposureGVR},
}

var PolicyTypes = []ComponentType{
	{ComponentKindPolicyEngine, PolicyEngineGVR},
	{ComponentKindPolicyWorkflow, PolicyWorkflowGVR},
	{ComponentKindPolicyWorkflowRun, PolicyWorkflowRunGVR},
	{ComponentKindPolicyTrigger, PolicyTriggerGVR},
}

var ComponentGVRs = map[string]schema.GroupVersionResource{}

func init() {
	for _, t := range ComponentTypes {
		ComponentGVRs[t.Kind] = t.GVR
	}
}

func ComponentTypeFor(kind string) (ComponentType, bool) {
	gvr, ok := ComponentGVRs[kind]
	return ComponentType{Kind: kind, GVR: gvr}, ok
}

func componentTypeFor(kind string) (ComponentType, error) {
	ct, ok := ComponentTypeFor(kind)
	if !ok {
		return ComponentType{}, failf("%q is not a component kind", kind)
	}
	return ct, nil
}

func ComponentKindsFor(rootKind string) []string {
	switch v1alpha1.ApplicationKind(rootKind) {
	case v1alpha1.ApplicationKindMarketBidder:
		return []string{ComponentKindBidder, ComponentKindGuestControlPlane, ComponentKindPds, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindMarketRequester:
		return []string{ComponentKindRequester, ComponentKindPds, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindGateway:
		return []string{ComponentKindGatewayAPI, ComponentKindPds, ComponentKindRequester, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindSshGateway:
		return []string{ComponentKindSshServer, ComponentKindRequester, ComponentKindPolicyEngine}
	case "MarketNetwork":
		return []string{ComponentKindPlc, ComponentKindRelay, ComponentKindIngressProxy, ComponentKindDispatcherTLS,
			ComponentKindPds, ComponentKindPolicyEngine}
	}
	return nil
}

func GVRFor(k v1alpha1.ApplicationKind) (schema.GroupVersionResource, error) {
	switch k {
	case v1alpha1.ApplicationKindMarketBidder:
		return MarketBidderGVR, nil
	case v1alpha1.ApplicationKindMarketRequester:
		return MarketRequesterGVR, nil
	case v1alpha1.ApplicationKindGateway:
		return GatewayGVR, nil
	case v1alpha1.ApplicationKindSshGateway:
		return SshGatewayGVR, nil
	}
	return schema.GroupVersionResource{}, fmt.Errorf("compose: %q is no application kind (MarketBidder, MarketRequester, Gateway, SshGateway)", k)
}

func ComponentRefFor(kind, name string) *commonv1.ObjectRef {
	return &commonv1.ObjectRef{Kind: kind, Name: name}
}

func ComponentKey(kind, name string) string {
	return kind + "/" + name
}

func NewComponentSpec(stateDir string, network *v1alpha1.MarketNetworkValue) v1alpha1.ComponentSpec {
	return v1alpha1.ComponentSpec{RestartPolicy: v1alpha1.ComponentRestartAlways, StateDir: stateDir, Network: network}
}

func IsExposureKind(kind string) bool {
	return kind == ComponentKindService || kind == ComponentKindExposure
}

type ComponentView struct {
	Kind       string
	Name       string
	Workspace  string
	Generation int64
	Status     v1alpha1.ComponentStatus
}

func (v ComponentView) ApplicationComponent() v1alpha1.ApplicationComponent {
	return v1alpha1.ApplicationComponent{Kind: v.Kind, Name: v.Name, Workspace: v.Workspace, Phase: string(v.Status.Phase),
		Ready: v.Status.Ready, Suspended: v.Status.Phase == v1alpha1.ComponentPhaseSuspended,
		ObservedGeneration: v.Status.ObservedGeneration, Restarts: v.Status.Restarts, Message: v.Status.Message}
}

func (v ComponentView) Listeners() []v1alpha1.ApplicationListener {
	out := make([]v1alpha1.ApplicationListener, 0, len(v.Status.Endpoints))
	for _, e := range v.Status.Endpoints {
		out = append(out, v1alpha1.ApplicationListener{Component: v.Name, Name: e.Name, Kind: e.Protocol, Address: cmp.Or(e.Address, e.URL)})
	}
	return out
}

func exposureViewOf(v ComponentView, kind string, m map[string]any) (ComponentView, error) {
	switch kind {
	case ComponentKindService:
		var st exposurev1alpha1.ServiceStatus
		if err := unstruct.Into(&unstructured.Unstructured{Object: m}, &st); err != nil {
			return v, err
		}
		v.Status.Phase = v1alpha1.ComponentPhase(st.Phase)
		v.Status.Ready = st.Ready
		for i, endpoint := range st.Endpoints {
			v.Status.Endpoints = append(v.Status.Endpoints, v1alpha1.Endpoint{Name: "endpoint-" + strconv.Itoa(i), URL: endpoint})
		}
	case ComponentKindExposure:
		var st exposurev1alpha1.ExposureStatus
		if err := unstruct.Into(&unstructured.Unstructured{Object: m}, &st); err != nil {
			return v, err
		}
		v.Status.Phase = v1alpha1.ComponentPhase(st.Phase)
		v.Status.Ready = st.Ready
		if st.Address != "" {
			v.Status.Endpoint = st.Address
			v.Status.Endpoints = []v1alpha1.Endpoint{{Name: "url", URL: st.Address, Address: st.Endpoint}}
		}
	}
	return v, nil
}

func ComponentViewOf(ws, kind string, u *unstructured.Unstructured) (ComponentView, error) {
	v := ComponentView{Kind: kind, Name: u.GetName(), Workspace: ws, Generation: u.GetGeneration()}
	m, ok, err := unstructured.NestedMap(u.Object, "status")
	if err != nil {
		return v, err
	}
	if !ok || len(m) == 0 {
		return v, nil
	}
	if IsExposureKind(kind) {
		return exposureViewOf(v, kind, m)
	}
	if err := unstruct.Into(&unstructured.Unstructured{Object: m}, &v.Status); err != nil {
		return v, err
	}
	return v, nil
}

func ComponentEndpoint(v ComponentView, name string) (v1alpha1.Endpoint, bool) {
	for _, e := range v.Status.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	if len(v.Status.Endpoints) > 0 {
		return v.Status.Endpoints[0], true
	}
	return v1alpha1.Endpoint{}, false
}

func EndpointURL(e v1alpha1.Endpoint) string {
	if e.URL != "" {
		return e.URL
	}
	if e.Address != "" {
		return "http://" + e.Address
	}
	return ""
}

func EventSourceRef(name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.EventSourceKind, Name: name}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
