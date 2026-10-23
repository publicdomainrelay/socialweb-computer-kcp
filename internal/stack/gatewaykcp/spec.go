package gatewaykcp

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/providermain"
)

const (
	DefaultWebAppFront    = "127.0.0.1:8788"
	PortZeroWebAppFront   = "127.0.0.1:0"
	OptionGatewayAudience = "gateway-audience"
	OptionKcpPublicHost   = "kcp-public-host"
)

func ReadSpec(path string) (v1alpha1.GatewaySpec, error) {
	var s v1alpha1.GatewaySpec
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("gatewaykcp: the Gateway spec file: %w", err)
	}
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return s, fmt.Errorf("gatewaykcp: the Gateway spec file %s: %w", path, err)
	}
	return s, nil
}

func ClusterProviderEnv(c *v1alpha1.GatewayClusterProvider) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	if c.ScaleHold != "" {
		out[providermain.EnvScaleHold] = c.ScaleHold
	}
	if c.ScaleMaxNodes != 0 {
		out[providermain.EnvScaleMaxNodes] = strconv.Itoa(int(c.ScaleMaxNodes))
	}
	if c.ScalePodsPerNode != 0 {
		out[providermain.EnvScalePodsPerNode] = strconv.Itoa(int(c.ScalePodsPerNode))
	}
	return out
}

func ClusterProviderFromEnv(lookup func(string) (string, bool), have *v1alpha1.GatewayClusterProvider) (*v1alpha1.GatewayClusterProvider, error) {
	out := v1alpha1.GatewayClusterProvider{}
	if have != nil {
		out = *have
	}
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	if v := get(providermain.EnvScaleHold); v != "" && out.ScaleHold == "" {
		if _, err := time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("%s is not a duration: %w", providermain.EnvScaleHold, err)
		}
		out.ScaleHold = v
	}
	for _, f := range []struct {
		env string
		dst *int32
	}{{providermain.EnvScaleMaxNodes, &out.ScaleMaxNodes}, {providermain.EnvScalePodsPerNode, &out.ScalePodsPerNode}} {
		v := get(f.env)
		if v == "" || *f.dst != 0 {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s is %q, not a whole number of at least 1", f.env, v)
		}
		*f.dst = int32(n)
	}
	if out == (v1alpha1.GatewayClusterProvider{}) {
		return nil, nil
	}
	return &out, nil
}

func hasOption(s v1alpha1.GatewaySpec, name string) bool {
	return slices.ContainsFunc(s.Options, func(o v1alpha1.ApplicationOption) bool { return o.Name == name })
}

func Spec(s v1alpha1.GatewaySpec, profile string, environ []string) v1alpha1.GatewaySpec {
	e := newEnv(environ)
	s.Profile = profile
	n := v1alpha1.MarketNetworkValue{}
	if s.Network != nil {
		n = *s.Network
	}
	if n.PLCDirectoryURL == "" {
		n.PLCDirectoryURL = e.get("SOCIALWEB_GATEWAY_PLC_URL")
	}
	n.PLCDirectoryURL = cmp.Or(n.PLCDirectoryURL, DefaultPLC)
	if n.IngressProxyHost == "" {
		n.IngressProxyHost = e.get("SOCIALWEB_GATEWAY_INGRESS_PROXY_HOST")
	}
	n.IngressProxyHost = cmp.Or(n.IngressProxyHost, DefaultIngress)
	if n.RelayURLs == nil {
		n.RelayURLs = []string{}
	}
	if n.FirehoseURLs == nil {
		n.FirehoseURLs = []string{}
	}
	s.Network = &n
	if s.Ingress == nil && profile == appctl.ProfileGatewayProd {
		s.Ingress = appctl.DefaultIngressConfig()
		if n.IngressProxyHost != "" {
			s.Ingress.Xrpc.IngressProxyHost = n.IngressProxyHost
		}
	}
	if s.WebAppFront == "" {
		s.WebAppFront = PortZeroWebAppFront
		if profile == appctl.ProfileGatewayProd {
			s.WebAppFront = DefaultWebAppFront
		}
	}
	s.Options = slices.Clone(s.Options)
	if !hasOption(s, OptionGatewayAudience) {
		first, _, _ := strings.Cut(e.get("SOCIALWEB_GATEWAY_AUDIENCES"), ",")
		if first = strings.TrimSpace(first); first == "" {
			first = DefaultAudience
		}
		s.Options = append(s.Options, v1alpha1.ApplicationOption{Name: OptionGatewayAudience, Value: first})
	}
	if h := e.get("SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST"); h != "" && !hasOption(s, OptionKcpPublicHost) {
		s.Options = append(s.Options, v1alpha1.ApplicationOption{Name: OptionKcpPublicHost, Value: h})
	}
	return s
}
