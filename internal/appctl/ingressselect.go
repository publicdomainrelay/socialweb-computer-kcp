package appctl

import (
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const DefaultIngressProxyHost = "xrpc.fedproxy.com"

func DefaultIngressConfig() *v1alpha1.IngressConfig {
	return &v1alpha1.IngressConfig{
		Provider:   v1alpha1.IngressProviderXRPCSubscription,
		IngressRef: ComponentRefFor(ComponentKindIngressProxy, AppIngressName),
		Xrpc:       &v1alpha1.IngressXrpcConfig{IngressProxyHost: DefaultIngressProxyHost},
	}
}

func DefaultIngressProfile(profile string) bool {
	switch profile {
	case BidderProfileProdQr, BidderProfileNetwork, RequesterProfileDefault, ProfileGatewayProd:
		return true
	}
	return false
}

func AppIngressOrDefault(profile string, spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue) *v1alpha1.IngressConfig {
	if spec != nil {
		return spec
	}
	if network != nil && network.Ingress != nil {
		return nil
	}
	if DefaultIngressProfile(profile) {
		return DefaultIngressConfig()
	}
	return nil
}

func NetworkIngressConfig(spec *v1alpha1.IngressConfig) *v1alpha1.IngressConfig {
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
	return out
}
