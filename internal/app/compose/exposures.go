package compose

import (
	"net"
	"net/url"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	DefaultIngressProxyHost = "xrpc.fedproxy.com"

	OpenTunnelExecutable = "opentunnel"
)

func ServiceNameFor(kind, name string) string {
	return "svc-" + strings.ToLower(kind) + "-" + name
}

func ServiceDesired(ws, kind, name string, protocol exposurev1alpha1.ServiceProtocol, handler string) Desired {
	return ServiceDesiredNamed(ws, kind, name, ServiceNameFor(kind, name), protocol, handler)
}

func ServiceDesiredNamed(ws, kind, name, serviceName string, protocol exposurev1alpha1.ServiceProtocol, handler string) Desired {
	return Desired{Kind: ComponentKindService, Name: serviceName, Spec: exposurev1alpha1.ServiceSpec{
		TargetRef: commonv1.ObjectRef{Workspace: ws, Kind: kind, Name: name},
		Protocol:  protocol,
		Handler:   handler,
	}}
}

func ExposureDesired(kind, name, suffix string, mode exposurev1alpha1.ExposureMode, hostname, path, ingressRef string,
	tls exposurev1alpha1.ExposureTLS) Desired {
	return ExposureDesiredForService(ServiceNameFor(kind, name), suffix, mode, hostname, path, ingressRef, tls)
}

func ExposureDesiredForService(serviceName, suffix string, mode exposurev1alpha1.ExposureMode, hostname, path, ingressRef string,
	tls exposurev1alpha1.ExposureTLS) Desired {
	return Desired{Kind: ComponentKindExposure, Name: serviceName + suffix, Spec: exposurev1alpha1.ExposureSpec{
		ServiceRef: serviceName,
		Mode:       mode,
		Hostname:   hostname,
		Path:       path,
		TLS:        tls,
		IngressRef: ingressRef,
	}}
}

func ExposureTLS(network *v1alpha1.MarketNetworkValue, enabled bool) exposurev1alpha1.ExposureTLS {
	out := exposurev1alpha1.ExposureTLS{Enabled: enabled}
	if enabled && network != nil {
		out.CABundle = network.CABundle
	}
	return out
}

func FirstHost(hostnames []string) string {
	for _, host := range hostnames {
		if strings.TrimSpace(host) != "" {
			return host
		}
	}
	return ""
}

func LocalExposure(kind, name string, tls exposurev1alpha1.ExposureTLS) Desired {
	return ExposureDesired(kind, name, ExposureLocalSuffix, exposurev1alpha1.ExposureModeLocal, "", "", "", tls)
}

func firstIngress(ingress []*v1alpha1.IngressConfig) *v1alpha1.IngressConfig {
	for _, value := range ingress {
		if value != nil {
			return value
		}
	}
	return nil
}

func IngressProviderFor(ingress *v1alpha1.IngressConfig) string {
	switch provider := ingress.ProviderName(); provider {
	case v1alpha1.IngressProviderXRPCSubscription, v1alpha1.IngressProviderOpenTunnel, v1alpha1.IngressProviderIroh, v1alpha1.IngressProviderWebRTC:
		return provider
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

func IngressHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return value
}

func IngressHostname(value string) string {
	host := IngressHost(value)
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return strings.Trim(host, "[]")
}

func IngressProxyDesired(network *v1alpha1.MarketNetworkValue, ingress *v1alpha1.IngressConfig) (Desired, bool) {
	name := IngressProxyName(ingress, nil)
	provider := IngressProviderFor(ingress)
	if ingress == nil || name == "" || provider == "" {
		return Desired{}, false
	}
	spec := v1alpha1.IngressProxySpec{
		ComponentSpec: NewComponentSpec("", network),
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
	if handle := IngressHostname(spec.PublicHost); handle != "" {
		spec.Hostnames = []string{handle}
		spec.Identity = &v1alpha1.ComponentIdentity{Handle: handle}
	}
	return Desired{Kind: ComponentKindIngressProxy, Name: name, Spec: spec}, true
}

func IngressExposure(kind, name, hostname string, network *v1alpha1.MarketNetworkValue, ingress ...*v1alpha1.IngressConfig) Desired {
	cfg := firstIngress(ingress)
	ingressRef := IngressProxyName(cfg, network)
	out := ExposureDesired(kind, name, ExposureIngressSuffix, exposurev1alpha1.ExposureModeIngress, hostname, "/",
		ingressRef, ExposureTLS(network, true))
	spec := out.Spec.(exposurev1alpha1.ExposureSpec)
	if cfg != nil {
		if cfg.Xrpc != nil {
			spec.IngressAddress = IngressHost(cfg.Xrpc.IngressProxyHost)
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
		return ""
	}
	for _, host := range n.Hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return n.IngressProxyHost
}

func HTTPServiceExposures(ws, kind, name, handler string, network *v1alpha1.MarketNetworkValue, hostnames []string,
	local, ingress bool, ingressConfig ...*v1alpha1.IngressConfig) []Desired {
	out := []Desired{ServiceDesired(ws, kind, name, exposurev1alpha1.ServiceProtocolHTTP, handler)}
	if local {
		out = append(out, LocalExposure(kind, name, exposurev1alpha1.ExposureTLS{}))
	}
	if ingress {
		out = append(out, IngressExposure(kind, name, FirstHost(hostnames), network, ingressConfig...))
	}
	return out
}

func PdsExposures(ws, name string, network *v1alpha1.MarketNetworkValue, hostnames []string, ingress ...*v1alpha1.IngressConfig) []Desired {
	cfg := firstIngress(ingress)
	out := make([]Desired, 0, 4)
	if proxy, ok := IngressProxyDesired(network, cfg); ok {
		out = append(out, proxy)
	}
	return append(out, HTTPServiceExposures(ws, ComponentKindPds, name, "http", network, hostnames, true, true, cfg)...)
}

func SSHExposures(ws, name string) []Desired {
	serviceName := ServiceNameFor(ComponentKindSshServer, name) + "-ssh"
	return []Desired{
		ServiceDesiredNamed(ws, ComponentKindSshServer, name, serviceName, exposurev1alpha1.ServiceProtocolSSH, "ssh"),
		ExposureDesiredForService(serviceName, ExposureLocalSuffix, exposurev1alpha1.ExposureModeLocal, "", "", "",
			exposurev1alpha1.ExposureTLS{}),
	}
}

func PolicyEngineDesired() Desired {
	return Desired{Kind: ComponentKindPolicyEngine, Name: PolicyEngineComponent,
		Spec: policyv1alpha1.PolicyEngineSpec{BundledActionsEnabled: true}}
}

func DefaultIngressConfig() *v1alpha1.IngressConfig {
	return &v1alpha1.IngressConfig{
		Provider:   v1alpha1.IngressProviderXRPCSubscription,
		IngressRef: ComponentRefFor(ComponentKindIngressProxy, AppIngressName),
		Xrpc:       &v1alpha1.IngressXrpcConfig{IngressProxyHost: DefaultIngressProxyHost},
	}
}

func AppIngressOrDefault(defaultIngress bool, spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue) *v1alpha1.IngressConfig {
	if spec != nil {
		return spec
	}
	if network != nil && network.Ingress != nil {
		return nil
	}
	if defaultIngress {
		return DefaultIngressConfig()
	}
	return nil
}

func NetworkIngressConfig(spec *v1alpha1.IngressConfig, workspace string) *v1alpha1.IngressConfig {
	out := &v1alpha1.IngressConfig{}
	if spec != nil {
		out = &v1alpha1.IngressConfig{
			Provider:   spec.Provider,
			PublicHost: spec.PublicHost,
			Xrpc:       spec.Xrpc,
			OpenTunnel: spec.OpenTunnel,
			IngressRef: spec.IngressRef,
		}
	}
	if IngressProviderFor(out) == "" && strings.TrimSpace(out.Provider) == "" {
		out.Provider = v1alpha1.IngressProviderXRPCSubscription
	}
	out.IngressRef = ComponentRefFor(ComponentKindIngressProxy, NetworkIngressName)
	out.IngressRef.Workspace = strings.TrimSpace(workspace)
	return out
}

func ComponentIdentity(id *v1alpha1.MarketIdentity, handle string) *v1alpha1.ComponentIdentity {
	out := &v1alpha1.ComponentIdentity{Handle: handle}
	if id != nil {
		out.MarketIdentityRef = ComponentRefFor(KindMarketIdentity, id.Name)
		out.Did = id.Status.Did
		if out.Handle == "" {
			out.Handle = id.Status.Handle
		}
	}
	return out
}

func ComponentServe(host string, port int32) *v1alpha1.ComponentServe {
	if host == "" {
		host = "127.0.0.1"
	}
	return &v1alpha1.ComponentServe{Host: host, Port: port}
}
