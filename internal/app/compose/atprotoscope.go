package compose

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	infrarecord "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	AtprotoPluginName         = "atproto"
	AtprotoMountPath          = "atproto"
	AtprotoUserRole           = "user"
	AtprotoSelfPolicy         = "atproto-self"
	AtprotoSessionProxyPolicy = "atproto-session-proxy"
	AtprotoClientMetadataPath = "/oauth-client-metadata.json"
	AtprotoCallbackPath       = "/oauth/callback"
	AtprotoUserTokenTTL       = "1h"
	AtprotoUserTokenMaxTTL    = "720h"
	atprotoClientName         = "socialweb-computer"
	PublicOriginOption        = "public-origin"
)

var requesterScope = []string{
	"atproto",
	"repo:com.publicdomainrelay.temp.compute.vm?action=create&action=update",
	"repo:com.publicdomainrelay.temp.market.rfp?action=create&action=update",
	"repo:com.publicdomainrelay.temp.market.accept?action=create&action=update",
	"repo:com.publicdomainrelay.temp.market.event?action=create&action=update",
	"repo:com.publicdomainrelay.temp.compute.events.vm.delete?action=create&action=update",
	"repo:com.publicdomainrelay.temp.compute.events.vm.onNetwork?action=create&action=update",
	"repo:com.publicdomainrelay.temp.compute.events.guest.publicKey?action=create&action=update",
	"repo:com.publicdomainrelay.temp.compute.events.guest.signalWebRTC?action=create&action=update",
	"repo:com.fedproxy.rbac?action=create&action=update&action=delete",
	"repo:computer.socialweb.temp.policy.ghalite?action=create&action=update",
	"repo:computer.socialweb.temp.policy.typescript?action=create&action=update",
	"repo:com.publicdomainrelay.temp.badgeBlueKeys?action=create&action=update&action=delete",
	"repo:com.publicdomainrelay.temp.market.heartbeat?action=create&action=update",
	"repo:" + infrarecord.InfraNSID + "?action=create&action=update&action=delete",
	"repo:" + infrarecord.EphemeralInfraNSID + "?action=create&action=update&action=delete",
	"rpc:com.publicdomainrelay.temp.market.submitRfp?aud=*",
	"rpc:com.publicdomainrelay.temp.market.submitAccept?aud=*",
	"rpc:com.publicdomainrelay.temp.market.submitBid?aud=*",
	"rpc:com.publicdomainrelay.temp.market.submitEvent?aud=*",
}

var clusterScope = []string{
	"rpc:com.publicdomainrelay.temp.cluster.createCluster?aud=*",
	"rpc:com.publicdomainrelay.temp.cluster.listClusters?aud=*",
	"rpc:com.publicdomainrelay.temp.cluster.getKubeconfig?aud=*",
	"rpc:com.publicdomainrelay.temp.cluster.controlPlaneUp?aud=*",
	"rpc:com.publicdomainrelay.temp.cluster.controlPlaneDown?aud=*",
	"rpc:com.publicdomainrelay.temp.cluster.deleteCluster?aud=*",
	"repo:" + infrarecord.InfraNSID + "?action=create&action=update&action=delete",
}

func GatewayAtprotoScope() string {
	var out []string
	for _, scope := range append(slices.Clone(requesterScope), clusterScope...) {
		if !slices.Contains(out, scope) {
			out = append(out, scope)
		}
	}
	return strings.Join(out, " ")
}

func SshGatewayAtprotoScope() string {
	return strings.Join(requesterScope, " ")
}

func OptionValue(opts []v1alpha1.ApplicationOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return strings.TrimRight(strings.TrimSpace(o.Value), "/")
		}
	}
	return ""
}

func AtprotoPolicies() []v1alpha1.OpenBaoPolicy {
	own := "auth/" + AtprotoMountPath + "/session/{{identity.entity.aliases." + openbaoctl.AccessorPlaceholder(AtprotoMountPath) + ".name}}"
	anyDID := "auth/" + AtprotoMountPath + "/session/+"
	cu := []string{"create", "update"}
	return []v1alpha1.OpenBaoPolicy{
		{Name: AtprotoSelfPolicy, Rules: []v1alpha1.OpenBaoPolicyRule{
			{Path: own, Capabilities: []string{"read", "delete"}},
			{Path: own + "/proof", Capabilities: cu},
		}},
		{Name: AtprotoSessionProxyPolicy, Rules: []v1alpha1.OpenBaoPolicyRule{
			{Path: anyDID, Capabilities: []string{"read"}},
			{Path: anyDID + "/proof", Capabilities: cu},
		}},
	}
}

func atprotoTrust(n *v1alpha1.MarketNetworkValue) (plc string, hosts []string, caPEM string, err error) {
	if n == nil {
		return "", nil, "", nil
	}
	for _, h := range n.Hosts {
		switch {
		case strings.HasPrefix(h.Name, "plc."):
			plc = plcScheme(n) + "://" + h.Name
			hosts = append(hosts, h.Name+"="+h.Address)
		case strings.HasPrefix(h.Name, "pds."):
			hosts = append(hosts, h.Name+"="+h.Address)
		}
	}
	if u, uerr := url.Parse(n.GuestTLSURL()); n.GuestTLSURL() != "" && uerr == nil && u.Port() != "" {
		hosts = append(hosts, "*."+u.Hostname()+"=127.0.0.1:"+u.Port())
	}
	if plc == "" && n.PLCDirectoryURL != "" {
		plc = n.PLCDirectoryURL
		if rest, ok := strings.CutPrefix(plc, "https://"); ok {
			hosts = append(hosts, strings.TrimRight(rest, "/"))
		}
	}
	if len(hosts) > 0 {
		caPEM = strings.TrimSpace(n.CABundle)
	}
	return plc, hosts, caPEM, nil
}

func AtprotoPlugin(opts []v1alpha1.ApplicationOption, n *v1alpha1.MarketNetworkValue, scope string) (v1alpha1.OpenBaoPlugin, error) {
	plc, hosts, caPEM, err := atprotoTrust(n)
	if err != nil {
		return v1alpha1.OpenBaoPlugin{}, err
	}
	a := &v1alpha1.OpenBaoAtprotoPlugin{Roles: []v1alpha1.OpenBaoAtprotoRole{{Name: AtprotoUserRole, BoundDIDs: []string{"*"},
		Policies: []string{AtprotoSelfPolicy}, TokenTTL: AtprotoUserTokenTTL, TokenMaxTTL: AtprotoUserTokenMaxTTL}}}
	if origin := OptionValue(opts, PublicOriginOption); origin != "" {
		if trusted := loopbackOriginHost(origin); trusted != "" && !slices.Contains(hosts, trusted) {
			hosts = append(hosts, trusted)
		}
		a.Config = v1alpha1.OpenBaoAtprotoConfig{ClientID: origin + AtprotoClientMetadataPath, RedirectURIs: []string{origin + AtprotoCallbackPath},
			ClientName: atprotoClientName, Scope: scope, PLCDirectoryURL: plc, TrustedHosts: hosts, TrustedCAPEM: caPEM}
	}
	return v1alpha1.OpenBaoPlugin{Name: AtprotoPluginName, Type: "auth", Asset: v1alpha1.HostProcessAsset{Name: openbaoctl.AtprotoPluginAsset},
		MountPath: AtprotoMountPath, Atproto: a}, nil
}

func loopbackOriginHost(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "http") {
		return ""
	}
	host := parsed.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return ""
		}
	}
	return parsed.Host
}

func plcScheme(n *v1alpha1.MarketNetworkValue) string {
	if n != nil {
		if parsed, err := url.Parse(n.PLCDirectoryURL); err == nil && strings.EqualFold(parsed.Scheme, "http") {
			return "http"
		}
	}
	return "https"
}
