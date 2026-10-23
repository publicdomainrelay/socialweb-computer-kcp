package marketnetwork

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

func (ctl *Controller) portLeases() (map[string]component.PortLease, error) {
	reg, err := component.OpenPortRegistry(component.PortRegistryOptions{StateDir: NetworkStateDir(ctl.c.StateRoot()),
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

func exposureHost(snap compose.Snapshot, name string) (string, error) {
	raw, ok := snap.ExposureURL(name)
	if !ok {
		return "", fmt.Errorf("the Exposure %s is not ready", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the Exposure %s reports no address in %q", name, raw)
	}
	return u.Host, nil
}

func componentHost(snap compose.Snapshot, kind, name string) (string, error) {
	v, ok := snap.View(kind, name)
	if !ok {
		return "", fmt.Errorf("%s %s is not there", kind, name)
	}
	e, ok := compose.ComponentEndpoint(v, "")
	if !ok || endpointAddress(e) == "" {
		return "", fmt.Errorf("%s %s reports no endpoint", kind, name)
	}
	return endpointAddress(e), nil
}

func localExposureName(kind, name string) string {
	return compose.ServiceNameFor(kind, name) + compose.ExposureLocalSuffix
}

func networkValueFromExposures(snap compose.Snapshot, caBundle string) (v1alpha1.MarketNetworkValue, error) {
	plcURL, ok := snap.ExposureURL(localExposureName(compose.ComponentKindPlc, compose.NetworkPLCName))
	if !ok {
		return v1alpha1.MarketNetworkValue{}, fmt.Errorf("the Exposure %s is not ready", localExposureName(compose.ComponentKindPlc, compose.NetworkPLCName))
	}
	if _, ok := snap.ExposureURL(localExposureName(compose.ComponentKindRelay, compose.NetworkRelayName)); !ok {
		return v1alpha1.MarketNetworkValue{}, fmt.Errorf("the Exposure %s is not ready", localExposureName(compose.ComponentKindRelay, compose.NetworkRelayName))
	}
	aliceHost, err := exposureHost(snap, localExposureName(compose.ComponentKindPds, NetworkAlicePdsName))
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	bobHost, err := exposureHost(snap, localExposureName(compose.ComponentKindPds, NetworkBobPdsName))
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	plcHost, err := exposureHost(snap, localExposureName(compose.ComponentKindPlc, compose.NetworkPLCName))
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	relayHost, err := exposureHost(snap, localExposureName(compose.ComponentKindRelay, compose.NetworkRelayName))
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	ingressHost, err := componentHost(snap, compose.ComponentKindIngressProxy, compose.NetworkIngressName)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	dispatcherHost, err := componentHost(snap, compose.ComponentKindDispatcherTLS, NetworkDispatcherTLSName)
	if err != nil {
		return v1alpha1.MarketNetworkValue{}, err
	}
	return v1alpha1.MarketNetworkValue{
		PLCDirectoryURL:  plcURL,
		IngressProxyHost: GuestTLSHost + ":" + portFromValue(ingressHost),
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

func portFrom(address string) (string, error) {
	_, p, err := net.SplitHostPort(address)
	if err != nil || p == "" || p == "0" {
		return "", fmt.Errorf("no bound port in %q", address)
	}
	return p, nil
}

func portFromValue(address string) string {
	p, err := portFrom(address)
	if err != nil {
		return ""
	}
	return p
}

func networkPds(stateRoot string, value v1alpha1.MarketNetworkValue, lease component.PortLease, name, handle, host string,
	vault *commonv1.ObjectRef, publicURL string) compose.Desired {
	stateDir := compose.ComponentCRStateDir(NetworkStateDir(stateRoot), compose.ComponentKindPds, name)
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	stableHost := ""
	if publicURL != "" {
		if base, stable, ok := compose.SchemeHostURL(publicURL); ok {
			publicURL, stableHost = base, stable
		}
	}
	var oauth *v1alpha1.PdsOAuth
	if publicURL != "" {
		oauth = &v1alpha1.PdsOAuth{Issuer: publicURL, AllowLocalhostClients: true}
	}
	return compose.Desired{Kind: compose.ComponentKindPds, Name: name, Spec: v1alpha1.PdsSpec{
		ComponentSpec:    compose.NewComponentSpec(stateDir, &value),
		Identity:         &v1alpha1.ComponentIdentity{Handle: handle},
		VaultRef:         vault,
		Hostnames:        []string{host},
		Serve:            compose.ComponentServe("", leasePort(lease)),
		Mode:             v1alpha1.PdsModeFull,
		PlcURL:           value.PLCDirectoryURL,
		PublicBaseURL:    publicURL,
		RequireIngress:   true,
		IngressRef:       compose.ComponentRefFor(compose.ComponentKindIngressProxy, compose.NetworkIngressName),
		PublicURL:        publicURL,
		StableHost:       stableHost,
		AllowExplicitDID: false,
		OAuth:            oauth,
	}}
}

func networkDesired(o *v1alpha1.MarketNetwork, ws, stateRoot string, leases map[string]component.PortLease,
	value v1alpha1.MarketNetworkValue, alicePublicURL, bobPublicURL string) ([]compose.Desired, error) {
	netState := NetworkStateDir(stateRoot)
	guestDir := filepath.Join(netState, GuestTLSDir)
	plc := compose.Desired{Kind: compose.ComponentKindPlc, Name: compose.NetworkPLCName, Spec: v1alpha1.PlcSpec{
		ComponentSpec: compose.NewComponentSpec(compose.ComponentCRStateDir(netState, compose.ComponentKindPlc, compose.NetworkPLCName), &value),
		Hostnames:     []string{NetworkPlcHost},
		Serve:         compose.ComponentServe("", leasePort(leases[NetworkPortPlc])),
	}}
	relaySpec := v1alpha1.RelaySpec{
		ComponentSpec: compose.NewComponentSpec(compose.ComponentCRStateDir(netState, compose.ComponentKindRelay, compose.NetworkRelayName), &value),
		PdsRefs: []commonv1.ObjectRef{*compose.ComponentRefFor(compose.ComponentKindPds, NetworkAlicePdsName),
			*compose.ComponentRefFor(compose.ComponentKindPds, NetworkBobPdsName), {Kind: compose.ComponentKindPds, Name: "*"}},
		Hostnames: []string{NetworkRelayHost},
		Serve:     compose.ComponentServe("", leasePort(leases[NetworkPortRelay])),
	}
	if mode := compose.MarketCommsMode(o.Spec.Comms); mode == v1alpha1.MarketCommsModeFirehose || mode == v1alpha1.MarketCommsModeBoth {
		relaySpec.PdsRefs = nil
	}
	relay := compose.Desired{Kind: compose.ComponentKindRelay, Name: compose.NetworkRelayName, Spec: relaySpec}
	ingressCfg := compose.NetworkIngressConfig(o.Spec.Ingress)
	ingressSpec := v1alpha1.IngressProxySpec{
		ComponentSpec: compose.NewComponentSpec(compose.ComponentCRStateDir(netState, compose.ComponentKindIngressProxy, compose.NetworkIngressName), &value),
		Provider:      compose.IngressProviderFor(ingressCfg),
		Identity:      &v1alpha1.ComponentIdentity{Handle: GuestTLSHost},
		RelayRef:      compose.ComponentRefFor(compose.ComponentKindRelay, compose.NetworkRelayName),
		Hostnames:     []string{GuestTLSHost},
		Serve:         compose.ComponentServe("", leasePort(leases[NetworkPortIngress])),
	}
	if cfg := ingressCfg; cfg != nil {
		ingressSpec.PublicHost = strings.TrimSpace(cfg.PublicHost)
		if cfg.Xrpc != nil {
			if host := strings.TrimSpace(cfg.Xrpc.IngressProxyHost); host != "" {
				ingressSpec.IngressProxyHost = host
			}
			if relayURL := strings.TrimSpace(cfg.Xrpc.RelayURL); relayURL != "" {
				ingressSpec.RelayURL = relayURL
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
		if host := compose.IngressHostname(ingressSpec.PublicHost); host != "" && !slices.Contains(ingressSpec.Hostnames, host) {
			ingressSpec.Hostnames = append(ingressSpec.Hostnames, host)
		}
	}
	ingress := compose.Desired{Kind: compose.ComponentKindIngressProxy, Name: compose.NetworkIngressName, Spec: ingressSpec}
	dispatcherValue := value
	dispatcherValue.CABundle = ""
	dispatcherBase := compose.NewComponentSpec(compose.ComponentCRStateDir(netState, compose.ComponentKindDispatcherTLS, NetworkDispatcherTLSName), &dispatcherValue)
	dispatcherBase.TLS = &v1alpha1.ComponentTLS{Enabled: true, Hostnames: []string{GuestTLSHost, "*." + GuestTLSHost},
		CertRef: filepath.Join(guestDir, guestLeafFile), CARef: &commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: o.Name, Workspace: ws}}
	dispatcher := compose.Desired{Kind: compose.ComponentKindDispatcherTLS, Name: NetworkDispatcherTLSName, Spec: v1alpha1.DispatcherTLSSpec{
		ComponentSpec: dispatcherBase,
		UpstreamRef:   compose.ComponentRefFor(compose.ComponentKindIngressProxy, compose.NetworkIngressName),
		Hostnames:     []string{GuestTLSHost, "*." + GuestTLSHost},
		Storage:       &v1alpha1.ComponentStorage{CertsDir: guestDir},
		Serve:         compose.ComponentServe("", leasePort(leases[NetworkPortDispatcher])),
	}}
	vault := compose.ComponentRefFor(compose.KindOpenBaoServer, NetworkVaultServer)
	alice := networkPds(stateRoot, value, leases[NetworkPortAlicePds], NetworkAlicePdsName, "alice.test", NetworkAlicePdsHost, vault, alicePublicURL)
	bob := networkPds(stateRoot, value, leases[NetworkPortBobPds], NetworkBobPdsName, "bob.test", NetworkBobPdsHost, vault, bobPublicURL)
	out := []compose.Desired{compose.PolicyEngineDesired(), plc, relay, ingress, dispatcher, alice, bob}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindPlc, compose.NetworkPLCName, "http", &value, []string{NetworkPlcHost}, true, true)...)
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRelay, compose.NetworkRelayName, "http", &value, []string{NetworkRelayHost}, true, true)...)
	out = append(out, compose.PdsExposures(ws, NetworkAlicePdsName, &value, []string{NetworkAlicePdsHost})...)
	out = append(out, compose.PdsExposures(ws, NetworkBobPdsName, &value, []string{NetworkBobPdsHost})...)
	return compose.ApplyOverrides(o.Spec.Components, out)
}
