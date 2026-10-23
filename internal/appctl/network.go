package appctl

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	NetworkSet          = "market-network"
	NetworkProfile      = "market-network"
	BidderAccountDomain = "bob.test"
	NetworkServiceState = "state"
	NetworkPortsFile    = "ports.json"
	NetworkPortFirst    = 30000
	NetworkPortLast     = 39999
	FrontRebindAfter    = 30 * time.Second
)

const (
	NetworkPLCName           = "plc"
	NetworkRelayName         = "relay"
	NetworkIngressName       = "dispatcher"
	NetworkDispatcherTLSName = "dispatcher-tls"
	NetworkAlicePdsName      = "alice"
	NetworkBobPdsName        = "bob"

	NetworkPlcHost        = "plc.default.global.svc.kcp.local"
	NetworkRelayHost      = "relay.default.relay.svc.kcp.local"
	NetworkIngressHost    = "dispatcher.default.relay.svc.kcp.local"
	NetworkDispatcherHost = v1alpha1.NetworkDispatcherHost
	NetworkAlicePdsHost   = "pds.default.alice.svc.kcp.local"
	NetworkBobPdsHost     = "pds.default.bob.svc.kcp.local"
	NetworkPortPlc        = "Global/plc"
	NetworkPortRelay      = "Relay/relay"
	NetworkPortIngress    = "IngressProxy/http"
	NetworkPortDispatcher = "DispatcherTLS/tls"
	NetworkPortAlicePds   = "AlicePds/http"
	NetworkPortBobPds     = "BobPds/http"
)

var networkPortNames = []string{NetworkPortPlc, NetworkPortRelay, NetworkPortIngress, NetworkPortDispatcher, NetworkPortAlicePds, NetworkPortBobPds}

type networks struct {
	ctl      *Controller
	mu       sync.Mutex
	applied  map[string]int64
	pkiPEM   []byte
	peers    string
	value    *v1alpha1.MarketNetworkValue
	valueUID string
}

func newNetworks(ctl *Controller) *networks {
	return &networks{ctl: ctl, applied: map[string]int64{}}
}

func (n *networks) portLeases() (map[string]component.PortLease, error) {
	reg, err := component.OpenPortRegistry(component.PortRegistryOptions{StateDir: NetworkStateDir(n.ctl.c.Layout.StateDir),
		First: NetworkPortFirst, Last: NetworkPortLast})
	if err != nil {
		return nil, err
	}
	out := map[string]component.PortLease{}
	for _, name := range networkPortNames {
		lease, err := reg.Allocate(name)
		if err != nil {
			return nil, err
		}
		if lease.Port < NetworkPortFirst || lease.Port > NetworkPortLast {
			if err := reg.Release(name); err != nil {
				return nil, err
			}
			if lease, err = reg.Allocate(name); err != nil {
				return nil, err
			}
		}
		out[name] = lease
	}
	return out, nil
}

func leasePort(lease component.PortLease) int32 {
	_, p, err := net.SplitHostPort(lease.Address())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(p)
	return int32(port)
}

func networkValueFromLeases(leases map[string]component.PortLease, caBundle string) v1alpha1.MarketNetworkValue {
	plc, relay := leases[NetworkPortPlc], leases[NetworkPortRelay]
	ingress, dispatcher := leases[NetworkPortIngress], leases[NetworkPortDispatcher]
	alice, bob := leases[NetworkPortAlicePds], leases[NetworkPortBobPds]
	return v1alpha1.MarketNetworkValue{
		PLCDirectoryURL:  "http://" + plc.Address(),
		IngressProxyHost: GuestTLSHost + ":" + strconv.Itoa(int(leasePort(ingress))),
		RelayURLs:        []string{"http://" + relay.Address()},
		FirehoseURLs:     []string{},
		CABundle:         caBundle,
		Hosts: []v1alpha1.NetworkHost{
			{Name: NetworkPlcHost, Address: plc.Address()},
			{Name: NetworkRelayHost, Address: relay.Address()},
			{Name: NetworkIngressHost, Address: ingress.Address()},
			{Name: NetworkDispatcherHost, Address: dispatcher.Address()},
			{Name: NetworkAlicePdsHost, Address: alice.Address()},
			{Name: NetworkBobPdsHost, Address: bob.Address()},
		},
	}
}

func endpointAddress(e v1alpha1.Endpoint) string {
	if e.Address != "" {
		return e.Address
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

func endpointURL(e v1alpha1.Endpoint) string {
	if e.URL != "" {
		return e.URL
	}
	if e.Address != "" {
		return "http://" + e.Address
	}
	return ""
}

func componentEndpoint(v ComponentView, name string) (v1alpha1.Endpoint, bool) {
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

func exposureURL(snap componentSnapshot, name string) (string, bool) {
	v, ok := snap.view(ComponentKindExposure, name)
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

func exposureHost(snap componentSnapshot, name string) (string, error) {
	raw, ok := exposureURL(snap, name)
	if !ok {
		return "", fmt.Errorf("the Exposure %s is not ready", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the Exposure %s reports no address in %q", name, raw)
	}
	return u.Host, nil
}

func componentHost(snap componentSnapshot, kind, name string) (string, error) {
	v, ok := snap.view(kind, name)
	if !ok {
		return "", fmt.Errorf("%s %s is not there", kind, name)
	}
	e, ok := componentEndpoint(v, "")
	if !ok || endpointAddress(e) == "" {
		return "", fmt.Errorf("%s %s reports no endpoint", kind, name)
	}
	return endpointAddress(e), nil
}

func networkValueFromExposures(snap componentSnapshot, caBundle string) (v1alpha1.MarketNetworkValue, error) {
	plcURL, ok := exposureURL(snap, serviceNameFor(ComponentKindPlc, NetworkPLCName)+ExposureLocalSuffix)
	if !ok {
		return v1alpha1.MarketNetworkValue{}, fmt.Errorf("the Exposure %s is not ready", serviceNameFor(ComponentKindPlc, NetworkPLCName)+ExposureLocalSuffix)
	}
	relayURL, ok := exposureURL(snap, serviceNameFor(ComponentKindRelay, NetworkRelayName)+ExposureLocalSuffix)
	if !ok {
		return v1alpha1.MarketNetworkValue{}, fmt.Errorf("the Exposure %s is not ready", serviceNameFor(ComponentKindRelay, NetworkRelayName)+ExposureLocalSuffix)
	}
	aliceHost, err := exposureHost(snap, serviceNameFor(ComponentKindPds, NetworkAlicePdsName)+ExposureLocalSuffix)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	bobHost, err := exposureHost(snap, serviceNameFor(ComponentKindPds, NetworkBobPdsName)+ExposureLocalSuffix)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	plcHost, err := exposureHost(snap, serviceNameFor(ComponentKindPlc, NetworkPLCName)+ExposureLocalSuffix)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	relayHost, err := exposureHost(snap, serviceNameFor(ComponentKindRelay, NetworkRelayName)+ExposureLocalSuffix)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	ingressHost, err := componentHost(snap, ComponentKindIngressProxy, NetworkIngressName)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	dispatcherHost, err := componentHost(snap, ComponentKindDispatcherTLS, NetworkDispatcherTLSName)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	return v1alpha1.MarketNetworkValue{
		PLCDirectoryURL:  plcURL,
		IngressProxyHost: GuestTLSHost + ":" + portFromValue(ingressHost),
		RelayURLs:        []string{relayURL},
		FirehoseURLs:     []string{},
		CABundle:         caBundle,
		Hosts: []v1alpha1.NetworkHost{
			{Name: NetworkPlcHost, Address: plcHost},
			{Name: NetworkRelayHost, Address: relayHost},
			{Name: NetworkIngressHost, Address: ingressHost},
			{Name: NetworkDispatcherHost, Address: dispatcherHost},
			{Name: NetworkAlicePdsHost, Address: aliceHost},
			{Name: NetworkBobPdsHost, Address: bobHost},
		},
	}, nil
}

func networkValueHostURL(value v1alpha1.MarketNetworkValue, name string) string {
	for _, host := range value.Hosts {
		if host.Name == name && host.Address != "" {
			return "http://" + host.Address
		}
	}
	return ""
}

func portFromValue(address string) string {
	p, err := portFrom(address)
	if err != nil {
		return ""
	}
	return p
}

func networkPds(stateRoot string, value v1alpha1.MarketNetworkValue, lease component.PortLease, name, handle, host string,
	vault *commonv1.ObjectRef, publicURL string, suspend bool) desiredComponent {
	stateDir := ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindPds, name)
	port := leasePort(lease)
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	stableHost := ""
	if publicURL != "" {
		if base, stable, ok := schemeHostURL(publicURL); ok {
			publicURL, stableHost = base, stable
		}
	}
	var oauth *v1alpha1.PdsOAuth
	if publicURL != "" {
		oauth = &v1alpha1.PdsOAuth{Issuer: publicURL, AllowLocalhostClients: true}
	}
	return desiredComponent{Kind: ComponentKindPds, Name: name, Spec: v1alpha1.PdsSpec{
		ComponentSpec:    NewComponentSpec(stateDir, &value, suspend),
		Identity:         &v1alpha1.ComponentIdentity{Handle: handle},
		VaultRef:         vault,
		Hostnames:        []string{host},
		Serve:            componentServe("", port),
		Mode:             v1alpha1.PdsModeFull,
		PlcURL:           value.PLCDirectoryURL,
		PublicBaseURL:    publicURL,
		RequireIngress:   true,
		IngressRef:       ComponentRefFor(ComponentKindIngressProxy, NetworkIngressName),
		PublicURL:        publicURL,
		StableHost:       stableHost,
		AllowExplicitDID: false,
		OAuth:            oauth,
	}}
}

func networkDesired(o v1alpha1.MarketNetwork, ws, stateRoot string, leases map[string]component.PortLease,
	value v1alpha1.MarketNetworkValue, alicePublicURL, bobPublicURL string) ([]desiredComponent, error) {
	suspend := o.Spec.Suspend
	guestDir := filepath.Join(NetworkStateDir(stateRoot), GuestTLSDir)
	plc := desiredComponent{Kind: ComponentKindPlc, Name: NetworkPLCName, Spec: v1alpha1.PlcSpec{
		ComponentSpec: NewComponentSpec(ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindPlc, NetworkPLCName), &value, suspend),
		Hostnames:     []string{NetworkPlcHost},
		Serve:         componentServe("", leasePort(leases[NetworkPortPlc])),
	}}
	relaySpec := v1alpha1.RelaySpec{
		ComponentSpec: NewComponentSpec(ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindRelay, NetworkRelayName), &value, suspend),
		PdsRefs: []commonv1.ObjectRef{*ComponentRefFor(ComponentKindPds, NetworkAlicePdsName),
			*ComponentRefFor(ComponentKindPds, NetworkBobPdsName), {Kind: ComponentKindPds, Name: "*"}},
		Hostnames: []string{NetworkRelayHost},
		Serve:     componentServe("", leasePort(leases[NetworkPortRelay])),
	}
	if mode := marketCommsMode(o.Spec.Comms); mode == v1alpha1.MarketCommsModeFirehose || mode == v1alpha1.MarketCommsModeBoth {
		relaySpec.PdsRefs = nil
	}
	relay := desiredComponent{Kind: ComponentKindRelay, Name: NetworkRelayName, Spec: relaySpec}
	ingressCfg := NetworkIngressConfig(o.Spec.Ingress)
	ingressSpec := v1alpha1.IngressProxySpec{
		ComponentSpec: NewComponentSpec(ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindIngressProxy, NetworkIngressName), &value, suspend),
		Provider:      IngressProviderFor(ingressCfg),
		Identity:      &v1alpha1.ComponentIdentity{Handle: GuestTLSHost},
		RelayRef:      ComponentRefFor(ComponentKindRelay, NetworkRelayName),
		Hostnames:     []string{GuestTLSHost},
		Serve:         componentServe("", leasePort(leases[NetworkPortIngress])),
	}
	if cfg := ingressCfg; cfg != nil {
		ingressSpec.PublicHost = strings.TrimSpace(cfg.PublicHost)
		if cfg.Xrpc != nil {
			if host := strings.TrimSpace(cfg.Xrpc.IngressProxyHost); host != "" {
				ingressSpec.IngressProxyHost = host
			}
			if relay := strings.TrimSpace(cfg.Xrpc.RelayURL); relay != "" {
				ingressSpec.RelayURL = relay
			}
			if cfg.Xrpc.RelayRef != nil {
				ingressSpec.RelayRef = cfg.Xrpc.RelayRef
			}
			if cfg.Xrpc.IdentityRef != nil {
				ingressSpec.IdentityRef = cfg.Xrpc.IdentityRef
			}
		}
		if cfg.OpenTunnel != nil {
			ingressSpec.OpenTunnel = &v1alpha1.IngressProxyOpenTunnel{
				Command:       append([]string(nil), cfg.OpenTunnel.Command...),
				RouteName:     strings.TrimSpace(cfg.OpenTunnel.RouteName),
				ProxyProtocol: strings.TrimSpace(cfg.OpenTunnel.ProxyProtocol),
				LocalHost:     strings.TrimSpace(cfg.OpenTunnel.LocalHost),
			}
		}
		if host := ingressHostname(ingressSpec.PublicHost); host != "" && !slices.Contains(ingressSpec.Hostnames, host) {
			ingressSpec.Hostnames = append(ingressSpec.Hostnames, host)
		}
	}
	ingress := desiredComponent{Kind: ComponentKindIngressProxy, Name: NetworkIngressName, Spec: ingressSpec}
	dispatcherBase := NewComponentSpec(ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindDispatcherTLS, NetworkDispatcherTLSName), &value, suspend)
	dispatcherBase.TLS = &v1alpha1.ComponentTLS{Enabled: true, Hostnames: []string{GuestTLSHost, "*." + GuestTLSHost},
		CertRef: filepath.Join(guestDir, guestLeafFile), CABundle: value.CABundle}
	dispatcher := desiredComponent{Kind: ComponentKindDispatcherTLS, Name: NetworkDispatcherTLSName, Spec: v1alpha1.DispatcherTLSSpec{
		ComponentSpec: dispatcherBase,
		UpstreamRef:   ComponentRefFor(ComponentKindIngressProxy, NetworkIngressName),
		Hostnames:     []string{GuestTLSHost, "*." + GuestTLSHost},
		Storage:       &v1alpha1.ComponentStorage{CertsDir: guestDir},
		Serve:         componentServe("", leasePort(leases[NetworkPortDispatcher])),
	}}
	vault := ComponentRefFor("OpenBaoServer", NetworkVaultServer)
	alice := networkPds(stateRoot, value, leases[NetworkPortAlicePds], NetworkAlicePdsName, "alice.test", NetworkAlicePdsHost, vault, alicePublicURL, suspend)
	bob := networkPds(stateRoot, value, leases[NetworkPortBobPds], NetworkBobPdsName, "bob.test", NetworkBobPdsHost, vault, bobPublicURL, suspend)
	out := []desiredComponent{policyEngineDesired(suspend), plc, relay, ingress, dispatcher, alice, bob}
	out = append(out, httpServiceExposures(ws, ComponentKindPlc, NetworkPLCName, "http", &value, []string{NetworkPlcHost}, true, true, suspend)...)
	out = append(out, httpServiceExposures(ws, ComponentKindRelay, NetworkRelayName, "http", &value, []string{NetworkRelayHost}, true, true, suspend)...)
	out = append(out, pdsExposures(ws, NetworkAlicePdsName, &value, []string{NetworkAlicePdsHost}, suspend)...)
	out = append(out, pdsExposures(ws, NetworkBobPdsName, &value, []string{NetworkBobPdsHost}, suspend)...)
	return applyComponentOverrides(o.Spec.Suspend, o.Spec.Components, out)
}

func (n *networks) ensureVault(ctx context.Context, owner string) error {
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(NetworkSet)
	if !ok {
		return fmt.Errorf("appctl: deploy/sets.yaml names no %s set", NetworkSet)
	}
	var phases []manifests.Phase
	for _, p := range set.Phases {
		if p.Name == "vault" {
			phases = append(phases, p)
		}
	}
	if len(phases) == 0 {
		return fmt.Errorf("appctl: the %s set names no vault phase", NetworkSet)
	}
	set.Phases = phases
	ap, err := kcpapply.New(n.ctl.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, FieldManager: FieldManager, Base: n.ctl.c.workspace(),
		Labels: map[string]string{OwnerLabel: owner}, Log: n.ctl.c.log()})
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, set, &manifests.MarketNetwork{})
	return err
}

const (
	ConditionServicesReady  = "ServicesReady"
	ConditionVerifierPassed = "VerifierPassed"
	ConditionOneKcp         = "OneKcp"
)

func (n *networks) oneKcp(apps []v1alpha1.NetworkAppStatus) []string {
	front, _, _ := strings.Cut(strings.TrimSuffix(n.ctl.restCfg.Host, "/"), "/clusters/")
	workspaces := map[string]string{n.ctl.c.workspace(): "the network"}
	var wrong []string
	for _, a := range apps {
		if a.KcpURL == "" {
			continue
		}
		if f, _, _ := strings.Cut(strings.TrimSuffix(a.KcpURL, "/"), "/clusters/"); f != front {
			wrong = append(wrong, fmt.Sprintf("%s reports the kcp %s, not the network's %s", a.Name, f, front))
		}
		if a.Workspace == "" {
			wrong = append(wrong, a.Name+" reports no workspace")
			continue
		}
		if other, ok := workspaces[a.Workspace]; ok {
			wrong = append(wrong, fmt.Sprintf("%s shares the workspace %s with %s", a.Name, a.Workspace, other))
		}
		workspaces[a.Workspace] = a.Name
	}
	return wrong
}

func portFrom(address string) (string, error) {
	_, p, err := net.SplitHostPort(address)
	if err != nil || p == "" || p == "0" {
		return "", fmt.Errorf("no bound port in %q", address)
	}
	return p, nil
}

const NetworkCAFileName = "network-ca.pem"

func NetworkCAFile(dir string, n *v1alpha1.MarketNetworkValue) (string, error) {
	if n == nil || strings.TrimSpace(n.CABundle) == "" || strings.TrimSpace(dir) == "" {
		return "", nil
	}
	path := filepath.Join(dir, NetworkCAFileName)
	want := strings.TrimSpace(n.CABundle) + "\n"
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(want), 0o644)
}

func (n *networks) caBundle(ctx context.Context) (string, error) {
	dir := NetworkStateDir(n.ctl.c.Layout.StateDir)
	if len(n.pkiPEM) == 0 {
		v, err := n.vault(ctx)
		if err != nil {
			return "", err
		}
		if n.pkiPEM, err = pkiCA(ctx, v); err != nil {
			return "", err
		}
	}
	pki := n.pkiPEM
	guest, err := os.ReadFile(filepath.Join(dir, GuestTLSDir, guestCAFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pki)) + "\n" + strings.TrimSpace(string(guest)) + "\n", nil
}

func (n *networks) knownChildren() []string {
	entries, err := os.ReadDir(devinstance.DevRoot(filepath.Join(n.ctl.c.Layout.StateDir, AppsDir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func (n *networks) childByID(id string) child {
	name := strings.TrimPrefix(id, n.ctl.c.Layout.ID+"-")
	c, err := n.child(v1alpha1.NetworkApp{Name: name})
	if err != nil {
		l, _ := devinstance.NewLayout(id, filepath.Join(n.ctl.c.Layout.StateDir, AppsDir), filepath.Join(n.ctl.c.Layout.RuntimeDir, AppsDir))
		return child{id: id, layout: l}
	}
	return c
}

func (n *networks) teardownAll(ctx context.Context, keep []string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, id := range n.knownChildren() {
		if slices.Contains(keep, id) {
			continue
		}
		c := n.childByID(id)
		wg.Go(func() {
			if err := n.teardownChild(ctx, c); err != nil {
				mu.Lock()
				errs = append(errs, c.id+": "+err.Error())
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("tearing children down: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (n *networks) reconcile(ctx context.Context, u *unstructured.Unstructured) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var o v1alpha1.MarketNetwork
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	log := n.ctl.c.log().With("marketnetwork", o.Name)
	st := v1alpha1.MarketNetworkStatus{ObservedGeneration: o.Generation, Network: o.Status.Network, BidderPdsURL: o.Status.BidderPdsURL,
		Apps: o.Status.Apps, Conditions: o.Status.Conditions}
	write := func(phase v1alpha1.ApplicationPhase, msg string) error {
		st.Phase, st.Message = phase, msg
		return n.ctl.writeStatus(ctx, MarketNetworkGVR, o.Name, st)
	}
	if u.GetDeletionTimestamp() != nil {
		write(v1alpha1.ApplicationPhaseDeleting, "tearing down the child applications, then the network's components")
		if err := n.teardownAll(ctx, nil); err != nil {
			return write(v1alpha1.ApplicationPhaseDeleting, err.Error())
		}
		left, err := n.ctl.stopComponents(ctx, owner)
		if err != nil {
			return err
		}
		if left > 0 {
			return write(v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left))
		}
		gone, err := n.deleteVault(ctx)
		if err != nil {
			return err
		}
		if !gone {
			return write(v1alpha1.ApplicationPhaseDeleting, "the OpenBaoServer "+NetworkVaultServer+" is stopping")
		}
		delete(n.applied, string(o.UID))
		if n.valueUID == string(o.UID) {
			n.value, n.valueUID = nil, ""
		}
		log.Info("the market network is gone: children, components and its OpenBaoServer")
		return n.ctl.setFinalizer(ctx, MarketNetworkGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := n.ctl.setFinalizer(ctx, MarketNetworkGVR, u, true); err != nil {
			return err
		}
	}
	if o.Spec.Profile != NetworkProfile {
		return write(v1alpha1.ApplicationPhaseFailed, fmt.Sprintf("profile %q: the built MarketNetwork profile is %s", o.Spec.Profile, NetworkProfile))
	}
	stateRoot := n.ctl.c.Layout.StateDir
	if n.applied[string(o.UID)] != o.Generation {
		write(v1alpha1.ApplicationPhaseProvisioning, "applying the network's OpenBaoServer")
		if err := n.ensureVault(ctx, owner); err != nil {
			return write(v1alpha1.ApplicationPhaseProvisioning, "the network's OpenBaoServer: "+err.Error())
		}
		n.applied[string(o.UID)] = o.Generation
		log.Info("applied the market network's OpenBaoServer", "set", NetworkSet)
	}
	if _, err := n.vault(ctx); err != nil {
		delete(n.applied, string(o.UID))
		return write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's OpenBaoServer: "+err.Error())
	}
	guestDir := filepath.Join(NetworkStateDir(stateRoot), GuestTLSDir)
	if err := EnsureGuestTLS(guestDir); err != nil {
		return write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	leases, err := n.portLeases()
	if err != nil {
		return write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	bundle, err := n.caBundle(ctx)
	if err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, "the network CA bundle: "+err.Error())
	}
	value := networkValueFromLeases(leases, bundle)
	if n.value != nil && n.valueUID == string(o.UID) {
		value = *n.value
	}
	netIngress := NetworkIngressConfig(o.Spec.Ingress)
	value.Ingress = netIngress
	if err := networkIngressSelection(netIngress); err != nil {
		setPdsIngressConditionNamed(&st.Conditions, o.Generation, NetworkAlicePdsName, true, err)
		setPdsIngressConditionNamed(&st.Conditions, o.Generation, NetworkBobPdsName, true, err)
		return write(v1alpha1.ApplicationPhaseFailed, "the network Pds ingress: "+err.Error())
	}
	for _, name := range []string{NetworkAlicePdsName, NetworkBobPdsName} {
		stateDir := ComponentCRStateDir(NetworkStateDir(stateRoot), ComponentKindPds, name)
		if err := ensurePDSKeyFiles(stateDir); err != nil {
			return write(v1alpha1.ApplicationPhaseFailed, "the "+name+" PDS keys: "+err.Error())
		}
	}
	ws := n.ctl.c.workspace()
	alicePublicURL, aliceIngressErr := n.ctl.pdsPublicURLFor(ctx, ws, NetworkAlicePdsName, true)
	bobPublicURL, bobIngressErr := n.ctl.pdsPublicURLFor(ctx, ws, NetworkBobPdsName, true)
	for _, item := range []struct {
		name string
		err  error
	}{{NetworkAlicePdsName, aliceIngressErr}, {NetworkBobPdsName, bobIngressErr}} {
		if item.err == nil || pdsIngressReason(item.err) == PdsIngressReasonNotReady {
			continue
		}
		setPdsIngressConditionNamed(&st.Conditions, o.Generation, item.name, true, item.err)
		return write(v1alpha1.ApplicationPhaseFailed, "the "+item.name+" PDS ingress: "+item.err.Error())
	}
	setPdsIngressConditionNamed(&st.Conditions, o.Generation, NetworkAlicePdsName, true, aliceIngressErr)
	setPdsIngressConditionNamed(&st.Conditions, o.Generation, NetworkBobPdsName, true, bobIngressErr)
	desired, err := networkDesired(o, n.ctl.c.workspace(), stateRoot, leases, value, alicePublicURL, bobPublicURL)
	if err != nil {
		setNetworkOverrideFailure(&st, o.Generation, err)
		return write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	n.ctl.ensureRelayStatus(ctx, ComponentKey(ComponentKindRelay, NetworkRelayName))
	if o.Spec.Suspend {
		suspendDesired := desired
		if alicePublicURL == "" || bobPublicURL == "" {
			appliedAlice, err := n.ctl.pdsPublicURLApplied(ctx, ws, NetworkAlicePdsName)
			if err != nil {
				return err
			}
			appliedBob, err := n.ctl.pdsPublicURLApplied(ctx, ws, NetworkBobPdsName)
			if err != nil {
				return err
			}
			if appliedAlice && appliedBob {
				suspendDesired = withoutPdsNamed(desired, NetworkAlicePdsName, NetworkBobPdsName)
			}
		}
		if err := n.ctl.ensureDesired(ctx, u, suspendDesired); err != nil {
			return write(v1alpha1.ApplicationPhaseProvisioning, err.Error())
		}
		_ = n.ctl.pruneDesired(ctx, owner, desired)
		snap, err := n.ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		apps := make([]v1alpha1.NetworkAppStatus, len(o.Spec.Apps))
		for i, app := range o.Spec.Apps {
			apps[i] = n.suspendApp(ctx, app)
		}
		st.Apps = apps
		st.Components, st.Listeners = snap.Components, snap.Listeners
		fillNetworkComponentStatus(&st, o.Generation, o.Spec.Suspend, o.Spec.Components, desired, snap, nil, n.ctl.componentDrift(string(o.UID)))
		conds.Set(&st.Conditions, o.Generation, ConditionVerifierPassed, false, "Suspended", "the network is suspended")
		return write(v1alpha1.ApplicationPhasePending, "suspended")
	}
	applyDesired := desired
	if alicePublicURL == "" || bobPublicURL == "" {
		appliedAlice, err := n.ctl.pdsPublicURLApplied(ctx, ws, NetworkAlicePdsName)
		if err != nil {
			return err
		}
		appliedBob, err := n.ctl.pdsPublicURLApplied(ctx, ws, NetworkBobPdsName)
		if err != nil {
			return err
		}
		if appliedAlice && appliedBob {
			applyDesired = withoutPdsNamed(desired, NetworkAlicePdsName, NetworkBobPdsName)
		}
	}
	if err := n.ctl.ensureDesired(ctx, u, applyDesired); err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, err.Error())
	}
	if err := n.ctl.pruneDesired(ctx, owner, desired); err != nil {
		return err
	}
	snap, err := n.ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	st.Components, st.Listeners = snap.Components, snap.Listeners
	fillNetworkComponentStatus(&st, o.Generation, o.Spec.Suspend, o.Spec.Components, desired, snap, nil, n.ctl.componentDrift(string(o.UID)))
	if !snap.satisfied(desired) {
		var waiting []string
		for _, d := range desired {
			if !isExposureKind(d.Kind) && !snap.ready(d.Kind, d.Name) {
				waiting = append(waiting, ComponentKey(d.Kind, d.Name))
			}
		}
		conds.Set(&st.Conditions, o.Generation, ConditionServicesReady, false, "Waiting", strings.Join(waiting, "; "))
		return write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's components to report ready")
	}
	if alicePublicURL == "" || bobPublicURL == "" {
		return write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's PDS ingress URLs")
	}
	published, err := networkValueFromExposures(snap, bundle)
	if err != nil {
		if n.value == nil || n.valueUID != string(o.UID) {
			conds.Set(&st.Conditions, o.Generation, ConditionServicesReady, false, "Endpoints", err.Error())
			return write(v1alpha1.ApplicationPhaseProvisioning, "the network's Exposure endpoints: "+err.Error())
		}
		published = *n.value
	} else {
		published.Ingress = netIngress
		if n.value == nil || n.valueUID != string(o.UID) || !sameSpec(*n.value, published) {
			n.value = &published
			n.valueUID = string(o.UID)
			desired, err = networkDesired(o, n.ctl.c.workspace(), stateRoot, leases, published, alicePublicURL, bobPublicURL)
			if err != nil {
				setNetworkOverrideFailure(&st, o.Generation, err)
				return write(v1alpha1.ApplicationPhaseFailed, err.Error())
			}
			if err := n.ctl.ensureDesired(ctx, u, desired); err != nil {
				return write(v1alpha1.ApplicationPhaseProvisioning, err.Error())
			}
			if err := n.ctl.pruneDesired(ctx, owner, desired); err != nil {
				return err
			}
		}
	}
	published.Ingress = netIngress
	st.Network = &published
	pdsURL, _ := exposureURL(snap, serviceNameFor(ComponentKindPds, NetworkBobPdsName)+ExposureLocalSuffix)
	aliceURL, _ := exposureURL(snap, serviceNameFor(ComponentKindPds, NetworkAlicePdsName)+ExposureLocalSuffix)
	if pdsURL == "" {
		pdsURL = networkValueHostURL(published, NetworkBobPdsHost)
	}
	if aliceURL == "" {
		aliceURL = networkValueHostURL(published, NetworkAlicePdsHost)
	}
	if pdsURL == "" || aliceURL == "" {
		return write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's PDS Exposure URLs")
	}
	st.BidderPdsURL = pdsURL
	var keep []string
	for _, app := range o.Spec.Apps {
		if c, err := n.child(app); err == nil {
			keep = append(keep, c.id)
		}
	}
	if err := n.teardownAll(ctx, keep); err != nil {
		log.Warn("removing children the spec no longer names", "err", err.Error())
	}
	var bidderDids []string
	for _, a := range o.Status.Apps {
		if a.Kind == v1alpha1.ApplicationKindMarketBidder && a.Did != "" {
			bidderDids = append(bidderDids, a.Did)
		}
	}
	apps := make([]v1alpha1.NetworkAppStatus, len(o.Spec.Apps))
	var wg sync.WaitGroup
	for i, app := range o.Spec.Apps {
		wg.Go(func() {
			apps[i] = n.ensureApp(ctx, app, published, o.Spec.Comms, pdsURL, aliceURL, bidderDids)
		})
	}
	wg.Wait()
	n.declareOperatorVouches(ctx, o.Spec.Apps)
	st.Apps = apps
	var names []string
	for _, a := range o.Spec.Apps {
		names = append(names, a.Name)
	}
	if err := n.grantPeers(ctx, names); err != nil {
		log.Warn("granting the child applications reads of each other's MarketIdentity objects", "err", err.Error())
	}
	conds.Set(&st.Conditions, o.Generation, ConditionServicesReady, true, "Ready",
		fmt.Sprintf("%d component(s) are ready and report their endpoints", len(snap.Views)))

	if wrong := n.oneKcp(apps); len(wrong) > 0 {
		conds.Set(&st.Conditions, o.Generation, ConditionOneKcp, false, "Apart", strings.Join(wrong, "; "))
	} else {
		conds.Set(&st.Conditions, o.Generation, ConditionOneKcp, true, "Workspaces", "every child application runs in its own workspace of the network's kcp")
	}
	if err := n.verify(ctx, snap, apps); err != nil {
		conds.Set(&st.Conditions, o.Generation, ConditionVerifierPassed, false, "Probe", err.Error())
		return write(v1alpha1.ApplicationPhaseProvisioning, "the network verifier: "+err.Error())
	}
	conds.Set(&st.Conditions, o.Generation, ConditionVerifierPassed, true, "Verified",
		"the PLC, relay, PDS, ingress and child application endpoints answer")

	for _, t := range []string{ConditionServicesReady, ConditionOneKcp, ConditionVerifierPassed} {
		if c := meta.FindStatusCondition(st.Conditions, t); c == nil || c.Status != metav1.ConditionTrue {
			return write(v1alpha1.ApplicationPhaseProvisioning, "the child applications are ready; waiting for condition "+t)
		}
	}
	return write(v1alpha1.ApplicationPhaseReady, fmt.Sprintf("%d component(s) and %d child application(s), each in its own workspace of one kcp, are ready", len(snap.Views), len(apps)))
}

const VerifierProbeTimeout = 5 * time.Second

func verifierTransport(req *http.Request, handlers *component.HandlerRegistry) http.RoundTripper {
	if handlers == nil || req == nil || req.URL == nil {
		return nil
	}
	host := strings.ToLower(req.URL.Hostname())
	if host == "" {
		return nil
	}
	if _, ok := handlers.HandlerForHost(host); ok {
		return handlers.RoundTripper()
	}
	return nil
}

func (n *networks) verify(ctx context.Context, snap componentSnapshot, apps []v1alpha1.NetworkAppStatus) error {
	for _, check := range []struct{ kind, name, path string }{
		{ComponentKindPlc, NetworkPLCName, "/health"},
		{ComponentKindRelay, NetworkRelayName, "/xrpc/_health"},
		{ComponentKindPds, NetworkAlicePdsName, "/xrpc/_health"},
		{ComponentKindPds, NetworkBobPdsName, "/xrpc/_health"},
		{ComponentKindIngressProxy, NetworkIngressName, "/.well-known/did.json"},
	} {
		v, ok := snap.view(check.kind, check.name)
		if !ok {
			return fmt.Errorf("%s %s is not there", check.kind, check.name)
		}
		if v.Status.Phase == v1alpha1.ComponentPhaseSuspended {
			continue
		}
		if !v.Status.Ready {
			return fmt.Errorf("%s %s is not ready", check.kind, check.name)
		}
		e, ok := componentEndpoint(v, "http")
		if !ok {
			e, ok = componentEndpoint(v, "")
		}
		if !ok {
			return fmt.Errorf("%s %s reports no endpoint", check.kind, check.name)
		}
		base := endpointURL(e)
		if base == "" {
			return fmt.Errorf("%s %s reports no endpoint URL", check.kind, check.name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+check.path, nil)
		if err != nil {
			return err
		}
		if check.kind == ComponentKindIngressProxy {
			host := GuestTLSHost
			if n.value != nil && n.value.IngressProxyHost != "" {
				if h, _, ok := strings.Cut(n.value.IngressProxyHost, ":"); ok && h != "" {
					host = h
				} else {
					host = n.value.IngressProxyHost
				}
			}
			req.Host = host
		}
		client := &http.Client{Timeout: VerifierProbeTimeout}
		if n.ctl != nil {
			if transport := verifierTransport(req, n.ctl.c.Handlers); transport != nil {
				client.Transport = transport
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s at %s: %w", check.kind, check.name, check.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("%s %s at %s returned HTTP %d", check.kind, check.name, check.path, resp.StatusCode)
		}
	}
	for _, a := range apps {
		if a.Kind == v1alpha1.ApplicationKindMarketRequester || a.Kind == v1alpha1.ApplicationKindSshGateway {
			continue
		}
		if a.Message == StoppedMessage {
			continue
		}
		if a.Phase != v1alpha1.ApplicationPhaseReady {
			return fmt.Errorf("the child application %s is %s: %s", a.Name, a.Phase, a.Message)
		}
	}
	return nil
}

func (n *networks) suspendApp(ctx context.Context, app v1alpha1.NetworkApp) v1alpha1.NetworkAppStatus {
	out := v1alpha1.NetworkAppStatus{Name: app.Name, Kind: app.Kind, Phase: v1alpha1.ApplicationPhaseProvisioning}
	fail := func(err error) v1alpha1.NetworkAppStatus {
		out.Phase, out.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return out
	}
	if _, err := GVRFor(app.Kind); err != nil {
		return fail(err)
	}
	c, err := n.child(app)
	if err != nil {
		return fail(err)
	}
	out.Instance = c.id
	var rec devinstance.Record
	if fsx.ReadJSON(c.layout.Record, &rec) != nil || rec.AdminKubeconfig == "" {
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return out
	}
	ri, err := childClient(rec, app.Kind)
	if err != nil {
		return fail(err)
	}
	_, err = ri.Patch(ctx, ChildRootName, types.MergePatchType, []byte(`{"spec":{"suspend":true}}`), metav1.PatchOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return out
	}
	if err != nil {
		return fail(err)
	}
	rs, found, err := readRoot(ctx, rec, app.Kind)
	if err != nil {
		return fail(err)
	}
	if !found {
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return out
	}
	out.Phase, out.Message, out.Did, out.Components, out.Listeners = rs.Phase, rs.Message, rs.Did, rs.Components, rs.Listeners
	if out.Phase == "" {
		out.Phase = v1alpha1.ApplicationPhasePending
	}
	return out
}

func (n *networks) ensureApp(ctx context.Context, app v1alpha1.NetworkApp, value v1alpha1.MarketNetworkValue, comms *v1alpha1.MarketComms, pdsURL, aliceURL string, bidderDids []string) v1alpha1.NetworkAppStatus {
	out := v1alpha1.NetworkAppStatus{Name: app.Name, Kind: app.Kind, Phase: v1alpha1.ApplicationPhaseProvisioning}
	fail := func(err error) v1alpha1.NetworkAppStatus {
		out.Message = err.Error()
		n.ctl.c.log().Warn("a child application is not ready yet", "app", app.Name, "err", err.Error())
		return out
	}
	c, err := n.child(app)
	if err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	out.Instance = c.id
	if app.Stopped {
		if err := n.stopChild(ctx, c); err != nil {
			return fail(err)
		}
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, StoppedMessage
		return out
	}
	if _, err := GVRFor(app.Kind); err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	var gw *devinstance.GatewayChild
	if app.Kind == v1alpha1.ApplicationKindGateway {
		if gw, err = GatewayChildOf(ctx, c.id, c.layout.StateDir, app, value, aliceURL, bidderDids); err != nil {
			return fail(err)
		}
	}
	rec, err := n.placeChild(ctx, c, gw)
	if err != nil {
		return fail(err)
	}
	out.KcpURL, out.Workspace = rec.KcpURL, rec.BaseWorkspace
	meta := metav1.ObjectMeta{Name: ChildRootName}
	var root any
	switch app.Kind {
	case v1alpha1.ApplicationKindMarketBidder:
		if len(bidderDids) > 0 {
			meta.Annotations = map[string]string{BidderDIDsAnnotation: strings.Join(bidderDids, ",")}
		}
		handle := app.Name + "." + BidderAccountDomain
		var fc *v1alpha1.FirecrackerProvider
		if app.Firecracker != nil {
			f := *app.Firecracker
			fc = &f
		}
		var kcp *v1alpha1.KcpProvider
		if app.Kcp != nil {
			k := *app.Kcp
			k.Handle = KcpHandle(app)
			kcp = &k
			fc = nil
		}
		root = &v1alpha1.MarketBidder{ObjectMeta: meta, Spec: v1alpha1.MarketBidderSpec{Profile: app.Profile, Network: &value,
			Identity: v1alpha1.ApplicationIdentity{Handle: handle, PdsURL: pdsURL},
			Policy:   app.Policy, Firecracker: fc, Kcp: kcp, Options: app.Options, Quota: app.Quota, Comms: comms, Components: app.Components}}
	case v1alpha1.ApplicationKindMarketRequester:
		root = &v1alpha1.MarketRequester{ObjectMeta: meta, Spec: v1alpha1.MarketRequesterSpec{Profile: app.Profile, Network: &value,
			Identity:   v1alpha1.ApplicationIdentity{Association: v1alpha1.AssociationSkip},
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota, Comms: comms, Components: app.Components}}
	case v1alpha1.ApplicationKindSshGateway:
		root = &v1alpha1.SshGateway{ObjectMeta: meta, Spec: v1alpha1.SshGatewaySpec{Profile: app.Profile, Network: &value,
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota, OwnerSessionsFile: app.OwnerSessionsFile, Comms: comms, Components: app.Components}}
	}
	if root != nil {
		if err := applyRoot(ctx, rec, app.Kind, root); err != nil {
			return fail(fmt.Errorf("applying the root object into %s's workspace %s: %w", c.id, rec.BaseWorkspace, err))
		}
	}
	rs, found, err := readRoot(ctx, rec, app.Kind)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("the root object of %s is not readable yet", c.id))
	}
	out.Phase, out.Message, out.Did, out.Components, out.Listeners = rs.Phase, rs.Message, rs.Did, rs.Components, rs.Listeners
	if out.Phase == "" {
		out.Phase = v1alpha1.ApplicationPhasePending
	}
	return out
}
