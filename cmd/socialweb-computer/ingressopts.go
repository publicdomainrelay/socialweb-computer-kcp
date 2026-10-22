package main

import (
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func applyIngressOption(ingress *v1alpha1.IngressConfig, name, value string) (*v1alpha1.IngressConfig, bool, error) {
	switch name {
	case "ingress-provider", "ingress-public-host", "ingress-xrpc-host", "ingress-relay-url",
		"ingress-opentunnel-command", "ingress-opentunnel-route-name", "ingress-opentunnel-local-host", "ingress-opentunnel-proxy-protocol":
	default:
		return ingress, false, nil
	}
	if ingress == nil {
		ingress = &v1alpha1.IngressConfig{}
	}
	switch name {
	case "ingress-provider":
		ingress.Provider = value
	case "ingress-public-host":
		ingress.PublicHost = value
	case "ingress-xrpc-host":
		if ingress.Xrpc == nil {
			ingress.Xrpc = &v1alpha1.IngressXrpcConfig{}
		}
		ingress.Xrpc.IngressProxyHost = value
	case "ingress-relay-url":
		if ingress.Xrpc == nil {
			ingress.Xrpc = &v1alpha1.IngressXrpcConfig{}
		}
		ingress.Xrpc.RelayURL = value
	case "ingress-opentunnel-command", "ingress-opentunnel-route-name", "ingress-opentunnel-local-host", "ingress-opentunnel-proxy-protocol":
		if ingress.OpenTunnel == nil {
			ingress.OpenTunnel = &v1alpha1.IngressProxyOpenTunnel{}
		}
		switch name {
		case "ingress-opentunnel-command":
			ingress.OpenTunnel.Command = splitList(value)
		case "ingress-opentunnel-route-name":
			ingress.OpenTunnel.RouteName = value
		case "ingress-opentunnel-local-host":
			ingress.OpenTunnel.LocalHost = value
		case "ingress-opentunnel-proxy-protocol":
			ingress.OpenTunnel.ProxyProtocol = value
		}
	default:
		return ingress, false, fmt.Errorf("--%s is not an ingress option", name)
	}
	return ingress, true, nil
}
