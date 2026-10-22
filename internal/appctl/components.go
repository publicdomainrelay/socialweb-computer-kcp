package appctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	ComponentLabel             = "apps.socialweb.computer/component"
	RootKindLabel              = "apps.socialweb.computer/root-kind"
	ManagedByLabel             = "app.kubernetes.io/managed-by"
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

	ExposureLocalSuffix   = "-local"
	ExposureIngressSuffix = "-ingress"
)

var (
	ServiceGVR  = schema.GroupVersionResource{Group: exposurev1alpha1.GroupName, Version: exposurev1alpha1.Version, Resource: "services"}
	ExposureGVR = schema.GroupVersionResource{Group: exposurev1alpha1.GroupName, Version: exposurev1alpha1.Version, Resource: "exposures"}
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

var ComponentGVRs = map[string]schema.GroupVersionResource{
	ComponentKindBidder:            BidderGVR,
	ComponentKindGuestControlPlane: GuestControlPlaneGVR,
	ComponentKindPds:               PdsGVR,
	ComponentKindRequester:         RequesterGVR,
	ComponentKindGatewayAPI:        GatewayAPIGVR,
	ComponentKindSshServer:         SshServerGVR,
	ComponentKindPlc:               PlcGVR,
	ComponentKindRelay:             RelayGVR,
	ComponentKindIngressProxy:      IngressProxyGVR,
	ComponentKindDispatcherTLS:     DispatcherTLSGVR,
	ComponentKindPolicyEngine:      PolicyEngineGVR,
	ComponentKindService:           ServiceGVR,
	ComponentKindExposure:          ExposureGVR,
}

var PolicyGVRs = map[string]schema.GroupVersionResource{
	ComponentKindPolicyEngine:      PolicyEngineGVR,
	ComponentKindPolicyWorkflow:    PolicyWorkflowGVR,
	ComponentKindPolicyWorkflowRun: PolicyWorkflowRunGVR,
	ComponentKindPolicyTrigger:     PolicyTriggerGVR,
}

func ComponentTypeFor(kind string) (ComponentType, bool) {
	gvr, ok := ComponentGVRs[kind]
	return ComponentType{Kind: kind, GVR: gvr}, ok
}

func componentTypeFor(kind string) (ComponentType, error) {
	ct, ok := ComponentTypeFor(kind)
	if !ok {
		return ComponentType{}, fmt.Errorf("appctl: %q is not a component kind", kind)
	}
	return ct, nil
}

func ComponentKindsFor(rootKind string) []string {
	var kinds []string
	switch v1alpha1.ApplicationKind(rootKind) {
	case v1alpha1.ApplicationKindMarketBidder:
		kinds = []string{ComponentKindBidder, ComponentKindGuestControlPlane, ComponentKindPds, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindMarketRequester:
		kinds = []string{ComponentKindRequester, ComponentKindPds, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindGateway:
		kinds = []string{ComponentKindGatewayAPI, ComponentKindPds, ComponentKindRequester, ComponentKindPolicyEngine}
	case v1alpha1.ApplicationKindSshGateway:
		kinds = []string{ComponentKindSshServer, ComponentKindRequester, ComponentKindPolicyEngine}
	case "MarketNetwork":
		kinds = []string{ComponentKindPlc, ComponentKindRelay, ComponentKindIngressProxy, ComponentKindDispatcherTLS,
			ComponentKindPds, ComponentKindPolicyEngine}
	}
	return kinds
}

func ComponentRefFor(kind, name string) *v1alpha1.ComponentRef {
	return &v1alpha1.ComponentRef{Kind: kind, Name: name}
}

func ComponentKey(kind, name string) string {
	return kind + "/" + name
}

func NewComponentSpec(stateDir string, network *v1alpha1.MarketNetworkValue, suspend bool) v1alpha1.ComponentSpec {
	return v1alpha1.ComponentSpec{Suspend: suspend, RestartPolicy: v1alpha1.ComponentRestartAlways, StateDir: stateDir, Network: network}
}

func ComponentCRStateDir(instanceState, kind, name string) string {
	return filepath.Join(instanceState, ComponentsDir, kind+"-"+name)
}

type ComponentView struct {
	Kind       string
	Name       string
	Workspace  string
	Generation int64
	Status     v1alpha1.ComponentStatus
}

func (v ComponentView) ApplicationComponent() v1alpha1.ApplicationComponent {
	return v1alpha1.ApplicationComponent{Kind: v.Kind, Name: v.Name, Workspace: v.Workspace, Phase: string(v.Status.Phase), Ready: v.Status.Ready}
}

func (v ComponentView) Listeners() []v1alpha1.ApplicationListener {
	out := make([]v1alpha1.ApplicationListener, 0, len(v.Status.Endpoints))
	for _, e := range v.Status.Endpoints {
		address := e.Address
		if address == "" {
			address = e.Url
		}
		out = append(out, v1alpha1.ApplicationListener{Component: v.Name, Name: e.Name, Kind: e.Protocol, Address: address})
	}
	return out
}

func isExposureKind(kind string) bool {
	return kind == ComponentKindService || kind == ComponentKindExposure
}

func pdsComponentSet(desired []desiredComponent) []desiredComponent {
	out := make([]desiredComponent, 0, 3)
	prefix := serviceNameFor(ComponentKindPds, PdsComponent)
	for _, d := range desired {
		switch {
		case d.Kind == ComponentKindPds && d.Name == PdsComponent:
			out = append(out, d)
		case d.Kind == ComponentKindService && d.Name == prefix:
			out = append(out, d)
		case d.Kind == ComponentKindExposure && strings.HasPrefix(d.Name, prefix+"-"):
			out = append(out, d)
		}
	}
	return out
}

func exposureViewOf(v ComponentView, kind string, m map[string]any) (ComponentView, error) {
	switch kind {
	case ComponentKindService:
		var st exposurev1alpha1.ServiceStatus
		if err := fromUnstructured(&unstructured.Unstructured{Object: m}, &st); err != nil {
			return v, err
		}
		v.Status.Phase = v1alpha1.ComponentPhase(st.Phase)
		v.Status.Ready = st.Ready
		for i, endpoint := range st.Endpoints {
			v.Status.Endpoints = append(v.Status.Endpoints, v1alpha1.Endpoint{Name: "endpoint-" + strconv.Itoa(i), Url: endpoint})
		}
	case ComponentKindExposure:
		var st exposurev1alpha1.ExposureStatus
		if err := fromUnstructured(&unstructured.Unstructured{Object: m}, &st); err != nil {
			return v, err
		}
		v.Status.Phase = v1alpha1.ComponentPhase(st.Phase)
		v.Status.Ready = st.Ready
		if st.URL != "" {
			v.Status.Endpoint = st.URL
			v.Status.Endpoints = []v1alpha1.Endpoint{{Name: "url", Url: st.URL, Address: st.Endpoint}}
		}
	}
	return v, nil
}

func componentViewOf(ws, kind string, u *unstructured.Unstructured) (ComponentView, error) {
	v := ComponentView{Kind: kind, Name: u.GetName(), Workspace: ws, Generation: u.GetGeneration()}
	m, ok, err := unstructured.NestedMap(u.Object, "status")
	if err != nil {
		return v, err
	}
	if !ok || len(m) == 0 {
		return v, nil
	}
	if isExposureKind(kind) {
		return exposureViewOf(v, kind, m)
	}
	if err := fromUnstructured(&unstructured.Unstructured{Object: m}, &v.Status); err != nil {
		return v, err
	}
	return v, nil
}

func serviceNameFor(kind, name string) string {
	return "svc-" + strings.ToLower(kind) + "-" + name
}

func serviceDesired(ws, kind, name string, protocol exposurev1alpha1.ServiceProtocol, handler string, suspend bool) desiredComponent {
	return serviceDesiredNamed(ws, kind, name, serviceNameFor(kind, name), protocol, handler, suspend)
}

func serviceDesiredNamed(ws, kind, name, serviceName string, protocol exposurev1alpha1.ServiceProtocol, handler string, suspend bool) desiredComponent {
	return desiredComponent{Kind: ComponentKindService, Name: serviceName, Spec: exposurev1alpha1.ServiceSpec{
		TargetRef: exposurev1alpha1.ServiceTargetRef{Workspace: ws, Kind: kind, Name: name},
		Protocol:  protocol,
		Handler:   handler,
		Suspend:   suspend,
	}}
}

func exposureDesired(kind, name, suffix string, mode exposurev1alpha1.ExposureMode, hostname, path, ingressRef string,
	tls exposurev1alpha1.ExposureTLS, suspend bool) desiredComponent {
	return exposureDesiredForService(serviceNameFor(kind, name), suffix, mode, hostname, path, ingressRef, tls, suspend)
}

func exposureDesiredForService(serviceName, suffix string, mode exposurev1alpha1.ExposureMode, hostname, path, ingressRef string,
	tls exposurev1alpha1.ExposureTLS, suspend bool) desiredComponent {
	return desiredComponent{Kind: ComponentKindExposure, Name: serviceName + suffix, Spec: exposurev1alpha1.ExposureSpec{
		ServiceRef: serviceName,
		Mode:       mode,
		Hostname:   hostname,
		Path:       path,
		TLS:        tls,
		IngressRef: ingressRef,
		Suspend:    suspend,
	}}
}

func exposureTLS(network *v1alpha1.MarketNetworkValue, enabled bool) exposurev1alpha1.ExposureTLS {
	out := exposurev1alpha1.ExposureTLS{Enabled: enabled}
	if enabled && network != nil {
		out.CABundleRef = network.CaBundleRef
	}
	return out
}

func firstHost(hostnames []string) string {
	for _, host := range hostnames {
		if strings.TrimSpace(host) != "" {
			return host
		}
	}
	return ""
}

func localExposure(kind, name string, tls exposurev1alpha1.ExposureTLS, suspend bool) desiredComponent {
	return exposureDesired(kind, name, ExposureLocalSuffix, exposurev1alpha1.ExposureModeLocal, "", "", "", tls, suspend)
}

const (
	AppIngressName = "ingress"

	OpenTunnelExecutable = "opentunnel"
)

func firstIngress(ingress []*v1alpha1.IngressConfig) *v1alpha1.IngressConfig {
	for _, value := range ingress {
		if value != nil {
			return value
		}
	}
	return nil
}

func IngressProviderFor(ingress *v1alpha1.IngressConfig) string {
	if ingress == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(ingress.Provider))
	switch provider {
	case v1alpha1.IngressProviderXRPCSubscription, v1alpha1.IngressProviderOpenTunnel, v1alpha1.IngressProviderIroh:
		return provider
	case "":
		if ingress.OpenTunnel != nil {
			return v1alpha1.IngressProviderOpenTunnel
		}
		if ingress.Xrpc != nil && strings.TrimSpace(ingress.Xrpc.IngressProxyHost) != "" {
			return v1alpha1.IngressProviderXRPCSubscription
		}
	}
	return ""
}

func IngressProxyName(ingress *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue) string {
	if ingress != nil && ingress.IngressRef != nil {
		if name := strings.TrimSpace(ingress.IngressRef.Name); name != "" {
			return name
		}
	}
	if IngressProviderFor(ingress) != "" {
		return AppIngressName
	}
	if network != nil {
		return NetworkIngressName
	}
	return ""
}

func ingressHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return value
}

func ingressHostname(value string) string {
	host := ingressHost(value)
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return strings.Trim(host, "[]")
}

func IngressProxyDesired(ws string, network *v1alpha1.MarketNetworkValue, ingress *v1alpha1.IngressConfig, suspend bool) (desiredComponent, bool) {
	name := IngressProxyName(ingress, nil)
	provider := IngressProviderFor(ingress)
	if ingress == nil || name == "" || provider == "" {
		return desiredComponent{}, false
	}
	spec := v1alpha1.IngressProxySpec{
		ComponentSpec: NewComponentSpec("", network, suspend),
		Provider:      provider,
		PublicHost:    strings.TrimSpace(ingress.PublicHost),
	}
	if ingress.Xrpc != nil {
		spec.IngressProxyHost = strings.TrimSpace(ingress.Xrpc.IngressProxyHost)
		spec.RelayURL = strings.TrimSpace(ingress.Xrpc.RelayURL)
		spec.RelayRef = ingress.Xrpc.RelayRef
		spec.IdentityRef = ingress.Xrpc.IdentityRef
	}
	if ingress.OpenTunnel != nil {
		spec.OpenTunnel = &v1alpha1.IngressProxyOpenTunnel{
			Command:       append([]string(nil), ingress.OpenTunnel.Command...),
			RouteName:     strings.TrimSpace(ingress.OpenTunnel.RouteName),
			ProxyProtocol: strings.TrimSpace(ingress.OpenTunnel.ProxyProtocol),
			LocalHost:     strings.TrimSpace(ingress.OpenTunnel.LocalHost),
		}
	}
	if handle := ingressHostname(spec.PublicHost); handle != "" {
		spec.Hostnames = []string{handle}
		spec.Identity = &v1alpha1.ComponentIdentity{Handle: handle}
	}
	return desiredComponent{Kind: ComponentKindIngressProxy, Name: name, Spec: spec}, true
}

func ingressExposure(kind, name, hostname string, network *v1alpha1.MarketNetworkValue, suspend bool,
	ingress ...*v1alpha1.IngressConfig) desiredComponent {
	cfg := firstIngress(ingress)
	ingressRef := IngressProxyName(cfg, network)
	out := exposureDesired(kind, name, ExposureIngressSuffix, exposurev1alpha1.ExposureModeIngress, hostname, "/",
		ingressRef, exposureTLS(network, true), suspend)
	spec := out.Spec.(exposurev1alpha1.ExposureSpec)
	if cfg != nil {
		if cfg.Xrpc != nil {
			spec.IngressAddress = ingressHost(cfg.Xrpc.IngressProxyHost)
		}
	} else if network != nil {
		spec.IngressAddress = network.IngressProxyHost
		spec.IngressDial = ingressConnectAddress(*network)
	}
	out.Spec = spec
	return out
}

func ingressConnectAddress(n v1alpha1.MarketNetworkValue) string {
	_, port, err := net.SplitHostPort(n.IngressProxyHost)
	if err != nil || port == "" {
		return n.IngressProxyHost
	}
	for _, host := range n.Hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return n.IngressProxyHost
}

func httpServiceExposures(ws, kind, name, handler string, network *v1alpha1.MarketNetworkValue, hostnames []string,
	local, ingress bool, suspend bool, ingressConfig ...*v1alpha1.IngressConfig) []desiredComponent {
	out := []desiredComponent{serviceDesired(ws, kind, name, exposurev1alpha1.ServiceProtocolHTTP, handler, suspend)}
	if local {
		out = append(out, localExposure(kind, name, exposurev1alpha1.ExposureTLS{}, suspend))
	}
	if ingress {
		out = append(out, ingressExposure(kind, name, firstHost(hostnames), network, suspend, ingressConfig...))
	}
	return out
}

func pdsExposures(ws, name string, network *v1alpha1.MarketNetworkValue, hostnames []string, suspend bool,
	ingress ...*v1alpha1.IngressConfig) []desiredComponent {
	cfg := firstIngress(ingress)
	out := make([]desiredComponent, 0, 4)
	if proxy, ok := IngressProxyDesired(ws, network, cfg, suspend); ok {
		out = append(out, proxy)
	}
	return append(out, httpServiceExposures(ws, ComponentKindPds, name, "http", network, hostnames, true, true, suspend, cfg)...)
}

func sshExposures(ws, name string, suspend bool) []desiredComponent {
	serviceName := serviceNameFor(ComponentKindSshServer, name) + "-ssh"
	return []desiredComponent{
		serviceDesiredNamed(ws, ComponentKindSshServer, name, serviceName, exposurev1alpha1.ServiceProtocolSSH, "ssh", suspend),
		exposureDesiredForService(serviceName, ExposureLocalSuffix, exposurev1alpha1.ExposureModeLocal, "", "", "",
			exposurev1alpha1.ExposureTLS{}, suspend),
	}
}

func componentSpecMap(spec any) (map[string]any, error) {
	if spec == nil {
		return nil, nil
	}
	if value := reflect.ValueOf(spec); value.Kind() != reflect.Pointer {
		spec = desiredSpecPtr(spec)
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec)
	if err != nil {
		return nil, fmt.Errorf("appctl: the component spec: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func componentLabels(root *unstructured.Unstructured, kind string) map[string]string {
	return map[string]string{
		OwnerLabel:     ownerOf(root),
		RootKindLabel:  root.GetKind(),
		ComponentLabel: kind,
		ManagedByLabel: FieldManager,
	}
}

func componentOwnerRefs(refs []metav1.OwnerReference, want metav1.OwnerReference) ([]metav1.OwnerReference, bool) {
	for i, ref := range refs {
		if ref.Kind != want.Kind || ref.Name != want.Name {
			continue
		}
		if ref.UID == want.UID && ref.APIVersion == want.APIVersion {
			return refs, false
		}
		refs[i] = want
		return refs, true
	}
	return append(refs, want), true
}

func (ctl *Controller) ensureComponent(ctx context.Context, root *unstructured.Unstructured, kind, name string, spec any) error {
	return ctl.ensureComponentIn(ctx, ctl.c.workspace(), root, kind, name, spec)
}

func (ctl *Controller) EnsureComponent(ctx context.Context, root *unstructured.Unstructured, kind, name string, spec any) error {
	return ctl.ensureComponent(ctx, root, kind, name, spec)
}

func (ctl *Controller) ensureComponentIn(ctx context.Context, ws string, root *unstructured.Unstructured, kind, name string, spec any) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	if root.GetUID() == "" {
		return fmt.Errorf("appctl: the %s %s has no uid; ensure it before its components", root.GetKind(), root.GetName())
	}
	desiredSpec, err := componentSpecMap(spec)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return err
	}
	cur, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		want := &unstructured.Unstructured{Object: map[string]any{"apiVersion": ct.APIVersion(), "kind": ct.Kind}}
		want.SetName(name)
		want.SetLabels(componentLabels(root, kind))
		want.SetOwnerReferences([]metav1.OwnerReference{ownerRefOf(root)})
		if desiredSpec != nil {
			want.Object["spec"] = desiredSpec
		}
		_, err = ri.Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager})
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	changed := false
	labels := cur.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range componentLabels(root, kind) {
		if labels[k] != v {
			labels[k] = v
			changed = true
		}
	}
	cur.SetLabels(labels)
	if refs, refChanged := componentOwnerRefs(cur.GetOwnerReferences(), ownerRefOf(root)); refChanged {
		cur.SetOwnerReferences(refs)
		changed = true
	}
	if desiredSpec != nil && !reflect.DeepEqual(cur.Object["spec"], desiredSpec) {
		cur.Object["spec"] = desiredSpec
		changed = true
	}
	if !changed {
		return nil
	}
	_, err = ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("appctl: the %s component %s changed under its update; the next pass retries", kind, name)
	}
	return err
}

func (ctl *Controller) GetComponent(ctx context.Context, kind, name string) (ComponentView, bool, error) {
	return ctl.GetComponentIn(ctx, ctl.c.workspace(), kind, name)
}

func (ctl *Controller) GetComponentIn(ctx context.Context, ws, kind, name string) (ComponentView, bool, error) {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return ComponentView{}, false, err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return ComponentView{}, false, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ComponentView{Kind: kind, Name: name, Workspace: ws}, false, nil
	}
	if err != nil {
		return ComponentView{}, false, err
	}
	v, err := componentViewOf(ws, kind, u)
	return v, err == nil, err
}

func (ctl *Controller) ListComponents(ctx context.Context, owner string) ([]ComponentView, error) {
	return ctl.ListComponentsIn(ctx, ctl.c.workspace(), owner)
}

func (ctl *Controller) ListComponentsIn(ctx context.Context, ws, owner string) ([]ComponentView, error) {
	var out []ComponentView
	for _, ct := range ComponentTypes {
		ri, err := ctl.client(ws, ct.GVR)
		if err != nil {
			return nil, err
		}
		list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			v, err := componentViewOf(ws, ct.Kind, &list.Items[i])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (ctl *Controller) DeleteComponent(ctx context.Context, kind, name string) error {
	return ctl.DeleteComponentIn(ctx, ctl.c.workspace(), kind, name)
}

func (ctl *Controller) DeleteComponentIn(ctx context.Context, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return err
	}
	err = ri.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (ctl *Controller) DeleteComponents(ctx context.Context, owner string) (int, error) {
	return ctl.DeleteComponentsExcept(ctx, owner, nil)
}

func (ctl *Controller) DeleteComponentsExcept(ctx context.Context, owner string, keep []string) (int, error) {
	return ctl.DeleteComponentsExceptIn(ctx, ctl.c.workspace(), owner, keep)
}

func (ctl *Controller) DeleteComponentsExceptIn(ctx context.Context, ws, owner string, keep []string) (int, error) {
	views, err := ctl.ListComponentsIn(ctx, ws, owner)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, v := range views {
		if slices.Contains(keep, ComponentKey(v.Kind, v.Name)) {
			continue
		}
		left++
		if err := ctl.DeleteComponentIn(ctx, ws, v.Kind, v.Name); err != nil {
			return left, err
		}
		ctl.c.log().Info("deleted a component the root object omits", "owner", owner, "kind", v.Kind, "name", v.Name, "workspace", ws)
	}
	return left, nil
}

func (ctl *Controller) RestartComponent(ctx context.Context, kind, name string) error {
	return ctl.RestartComponentIn(ctx, ctl.c.workspace(), kind, name)
}

func (ctl *Controller) RestartComponentIn(ctx context.Context, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		ComponentRestartAnnotation: time.Now().UTC().Format(time.RFC3339Nano)}}})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func (ctl *Controller) SuspendComponent(ctx context.Context, kind, name string, suspend bool) error {
	return ctl.SuspendComponentIn(ctx, ctl.c.workspace(), kind, name, suspend)
}

func (ctl *Controller) SuspendComponentIn(ctx context.Context, ws, kind, name string, suspend bool) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func (ctl *Controller) writeComponentStatusIn(ctx context.Context, ws, kind, name string, status v1alpha1.ComponentStatus) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, ct.GVR)
	if err != nil {
		return err
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		cur.Object["status"] = m
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("appctl: the status of the %s component %s stayed in conflict", kind, name)
}

func (ctl *Controller) WriteComponentStatus(ctx context.Context, kind, name string, status v1alpha1.ComponentStatus) error {
	return ctl.writeComponentStatusIn(ctx, ctl.c.workspace(), kind, name, status)
}

type ComponentPass struct {
	Components []v1alpha1.ApplicationComponent
	Listeners  []v1alpha1.ApplicationListener
	Ready      int
	AllReady   bool
}

func (ctl *Controller) ComponentPass(ctx context.Context, owner string) (ComponentPass, error) {
	return ctl.ComponentPassIn(ctx, ctl.c.workspace(), owner)
}

func (ctl *Controller) ComponentPassIn(ctx context.Context, ws, owner string) (ComponentPass, error) {
	views, err := ctl.ListComponentsIn(ctx, ws, owner)
	if err != nil {
		return ComponentPass{}, err
	}
	pass := ComponentPass{AllReady: len(views) > 0}
	for _, v := range views {
		if isExposureKind(v.Kind) {
			pass.Listeners = append(pass.Listeners, v.Listeners()...)
			continue
		}
		pass.Components = append(pass.Components, v.ApplicationComponent())
		pass.Listeners = append(pass.Listeners, v.Listeners()...)
		if v.Status.Ready {
			pass.Ready++
		} else {
			pass.AllReady = false
		}
	}
	sort.Slice(pass.Listeners, func(i, j int) bool {
		if pass.Listeners[i].Component != pass.Listeners[j].Component {
			return pass.Listeners[i].Component < pass.Listeners[j].Component
		}
		return pass.Listeners[i].Name < pass.Listeners[j].Name
	})
	return pass, nil
}

func (ctl *Controller) AggregateComponents(ctx context.Context, st *v1alpha1.ApplicationStatus, owner string) error {
	return ctl.aggregateComponentsIn(ctx, ctl.c.workspace(), st, owner)
}

func (ctl *Controller) aggregateComponentsIn(ctx context.Context, ws string, st *v1alpha1.ApplicationStatus, owner string) error {
	pass, err := ctl.ComponentPassIn(ctx, ws, owner)
	if err != nil {
		return err
	}
	st.Components = append(st.Components, pass.Components...)
	st.Listeners = append(st.Listeners, pass.Listeners...)
	return nil
}

type ComponentEvent struct {
	Type      watch.EventType
	Component ComponentView
	Err       error
}

func (ctl *Controller) WatchComponents(ctx context.Context, owner string) (<-chan ComponentEvent, error) {
	return ctl.WatchComponentsIn(ctx, ctl.c.workspace(), owner)
}

func (ctl *Controller) WatchComponentsIn(ctx context.Context, ws, owner string) (<-chan ComponentEvent, error) {
	out := make(chan ComponentEvent)
	var wg sync.WaitGroup
	for _, ct := range ComponentTypes {
		ri, err := ctl.client(ws, ct.GVR)
		if err != nil {
			close(out)
			return nil, err
		}
		wg.Add(1)
		go func(ct ComponentType, ri dynamic.ResourceInterface) {
			defer wg.Done()
			ctl.watchComponentType(ctx, ws, ct, ri, owner, out)
		}(ct, ri)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

func (ctl *Controller) watchComponentType(ctx context.Context, ws string, ct ComponentType, ri dynamic.ResourceInterface, owner string, out chan<- ComponentEvent) {
	selector := OwnerLabel + "=" + owner
	for ctx.Err() == nil {
		w, err := ri.Watch(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			if apierrors.IsNotFound(err) {
				if !sleep(ctx, RetryAfter) {
					return
				}
				continue
			}
			select {
			case out <- ComponentEvent{Type: watch.Error, Component: ComponentView{Kind: ct.Kind, Workspace: ws}, Err: err}:
			case <-ctx.Done():
				return
			}
			if !sleep(ctx, RetryAfter) {
				return
			}
			continue
		}
		for event := range w.ResultChan() {
			if event.Type == watch.Error {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			v, err := componentViewOf(ws, ct.Kind, u)
			select {
			case out <- ComponentEvent{Type: event.Type, Component: v, Err: err}:
			case <-ctx.Done():
				w.Stop()
				return
			}
		}
		w.Stop()
		if !sleep(ctx, RetryAfter) {
			return
		}
	}
}

const (
	ComponentStopWait    = 30 * time.Second
	pdsRepoKeyDir        = "repo-keys"
	pdsSessionSecretFile = "session-secret"
	pdsRotationKeyFile   = "plc-rotation.key"
)

func optionInt(opts []v1alpha1.ApplicationOption, name string) int {
	raw := optionValue(opts, name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

func requesterMode(mode v1alpha1.MarketIdentityMode) string {
	switch mode {
	case v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthDelegated:
		return "oauth"
	case v1alpha1.MarketIdentityModePdsAccount:
		return "callback"
	}
	return "local"
}

func requesterComponentFields(opts []v1alpha1.ApplicationOption, mode v1alpha1.MarketIdentityMode) (string, string, string, string, string) {
	keys := optionValue(opts, "guest-ssh-keys-file")
	if keys == "" {
		keys = optionValue(opts, "ssh-keys-file")
	}
	return requesterMode(mode), optionValue(opts, publicOriginOption), optionValue(opts, "oauth-client-id"),
		optionValue(opts, "oauth-redirect-uri"), keys
}

func annotationList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func ensurePDSKeyFiles(stateDir string) error {
	session := filepath.Join(stateDir, pdsSessionSecretFile)
	if _, err := os.Stat(session); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if err := writeSecret(session, []byte(hex.EncodeToString(buf)+"\n")); err != nil {
			return err
		}
	}
	rotation := filepath.Join(stateDir, pdsRotationKeyFile)
	if _, err := os.Stat(rotation); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		key, err := atcrypto.GeneratePrivateKeyK256()
		if err != nil {
			return err
		}
		if err := writeSecret(rotation, []byte(key.Multibase()+"\n")); err != nil {
			return err
		}
	}
	return nil
}
