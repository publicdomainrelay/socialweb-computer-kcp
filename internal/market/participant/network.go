package participant

import (
	"net/url"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	contractcomponent "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/networkdial"
)

type ChainedResolver []networkdial.Resolver

func (c ChainedResolver) Address(hostport string) (string, bool) {
	for _, resolver := range c {
		if resolver == nil {
			continue
		}
		if address, ok := resolver.Address(hostport); ok {
			return address, true
		}
	}
	return "", false
}

func (c ChainedResolver) Hosts() map[string]string {
	out := map[string]string{}
	for _, resolver := range c {
		source, ok := resolver.(interface{ Hosts() map[string]string })
		if !ok {
			continue
		}
		for name, address := range source.Hosts() {
			if _, exists := out[name]; !exists {
				out[name] = address
			}
		}
	}
	return out
}

func NetworkHosts(n v1alpha1.MarketNetworkValue, fallback networkdial.Resolver) contractcomponent.NetworkHosts {
	entries := make([]networkdial.Host, 0, len(n.Hosts)+1)
	for _, host := range n.Hosts {
		entries = append(entries, networkdial.Host{Name: host.Name, Address: host.Address})
	}
	if name := hostport.URLHostname(n.GuestTLSURL()); name != "" {
		if address := hostport.ForURL(n.GuestTLSURL(), n.Hosts); address != "" {
			entries = append(entries, networkdial.Host{Name: name, Address: address})
		}
	}
	resolvers := make([]networkdial.Resolver, 0, 2)
	if mapped, err := networkdial.NewHostMap(entries); err == nil && mapped.Len() > 0 {
		resolvers = append(resolvers, mapped)
	}
	if fallback != nil {
		resolvers = append(resolvers, fallback)
	}
	if len(resolvers) == 0 {
		return nil
	}
	return ChainedResolver(resolvers)
}

func AbsoluteHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}
