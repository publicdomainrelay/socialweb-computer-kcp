package component

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
)

type NetworkHost struct {
	Key string

	Workspace string

	Kind string

	Name string

	Endpoint string

	Protocol string

	Host string

	Address string

	URL string
}

type NetworkHostOptions struct {
	Domain string

	Namespace string

	Endpoint string
}

func BuildNetworkHosts(entries []Entry, opts NetworkHostOptions) []NetworkHost {
	domain := strings.TrimSpace(opts.Domain)
	if domain == "" {
		domain = kcp.DefaultServiceDomain
	}
	namespace := strings.TrimSpace(opts.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	out := make([]NetworkHost, 0, len(entries))
	for _, entry := range entries {
		primary, ok := PrimaryEndpoint(entry.Endpoints)
		if !ok {
			continue
		}
		base := kcp.ServiceFQDN(strings.ToLower(entry.Key.Kind), namespace, entry.Key.Workspace, domain)
		for _, endpoint := range entry.Endpoints {
			if opts.Endpoint != "" && endpoint.Name != opts.Endpoint {
				continue
			}
			address := endpointHostPort(endpoint)
			if address == "" {
				continue
			}
			host := base
			key := entry.Key.Kind + "/" + entry.Key.Name
			if endpoint.Name != primary.Name {
				host = "_" + endpoint.Name + "." + base
				key += "/" + endpoint.Name
			}
			out = append(out, NetworkHost{
				Key:       key,
				Workspace: entry.Key.Workspace,
				Kind:      entry.Key.Kind,
				Name:      entry.Key.Name,
				Endpoint:  endpoint.Name,
				Protocol:  endpoint.Protocol,
				Host:      host,
				Address:   address,
				URL:       endpoint.URL,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func NetworkHostsFromRegistry(reg *Registry) []NetworkHost {
	return BuildNetworkHosts(NetworkEntries(reg), NetworkHostOptions{})
}

func NetworkEntries(reg *Registry) []Entry {
	if reg == nil {
		return nil
	}
	return reg.List("")
}

func endpointHostPort(endpoint Endpoint) string {
	address := strings.TrimSpace(endpoint.Address)
	if address != "" {
		if _, _, err := net.SplitHostPort(address); err == nil {
			return address
		}
	}
	if endpoint.URL == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(parsed.Host); err != nil {
		return ""
	}
	return parsed.Host
}

type NetworkHostMap struct {
	hosts map[string]string

	aliases map[string]string

	byKey map[string]int

	status []NetworkHost
}

var _ NetworkHosts = (*NetworkHostMap)(nil)

func NewNetworkHostMap(hosts []NetworkHost) (*NetworkHostMap, error) {
	out := &NetworkHostMap{
		hosts:   map[string]string{},
		aliases: map[string]string{},
		byKey:   map[string]int{},
		status:  append([]NetworkHost(nil), hosts...),
	}
	for i, host := range hosts {
		if strings.TrimSpace(host.Address) == "" {
			return nil, fmt.Errorf("component: the network host %s has no address", networkHostLabel(host))
		}
		if _, _, err := net.SplitHostPort(host.Address); err != nil {
			return nil, fmt.Errorf("component: the network host %s address %q is not host:port: %w", networkHostLabel(host), host.Address, err)
		}
		name := hostport.Normalize(host.Host)
		if name == "" {
			return nil, fmt.Errorf("component: the network host %s has no host name", networkHostLabel(host))
		}
		if current, ok := out.hosts[name]; ok && current != host.Address {
			return nil, fmt.Errorf("component: the network host %s is %s and %s", name, current, host.Address)
		}
		out.hosts[name] = host.Address
		out.byKey[strings.ToLower(host.Key)] = i
		for _, alias := range networkHostAliases(host) {
			if alias == "" {
				continue
			}
			key := strings.ToLower(alias)
			if current, ok := out.aliases[key]; ok && current != host.Address {
				return nil, fmt.Errorf("component: the network alias %s is %s and %s", alias, current, host.Address)
			}
			out.aliases[key] = host.Address
		}
	}
	return out, nil
}

func NewNetworkHostMapFromRegistry(reg *Registry) (*NetworkHostMap, error) {
	return NewNetworkHostMap(NetworkHostsFromRegistry(reg))
}

func networkHostLabel(host NetworkHost) string {
	if host.Host != "" {
		return host.Host
	}
	return host.Key
}

func networkHostAliases(host NetworkHost) []string {
	kind := host.Kind
	name := host.Name
	if kind == "" || name == "" {
		return nil
	}
	base := kind + "/" + name
	var aliases []string
	if host.Key == "" || host.Key == base || host.Endpoint == "" {
		aliases = append(aliases, base, host.Workspace+"/"+base)
	}
	if host.Key != "" {
		aliases = append(aliases, host.Key)
	}
	if host.Endpoint != "" {
		aliases = append(aliases, base+"/"+host.Endpoint)
	}
	return aliases
}

func (m *NetworkHostMap) Address(target string) (string, bool) {
	if m == nil {
		return "", false
	}
	value := strings.TrimSpace(target)
	if address, ok := m.aliases[strings.ToLower(value)]; ok {
		return address, true
	}
	name := hostport.Normalize(value)
	if address, ok := m.aliases[name]; ok {
		return address, true
	}
	address, ok := m.hosts[name]
	return address, ok
}

func (m *NetworkHostMap) Hosts() map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m.hosts))
	maps.Copy(out, m.hosts)
	return out
}

func (m *NetworkHostMap) Status() []NetworkHost {
	if m == nil {
		return nil
	}
	return append([]NetworkHost(nil), m.status...)
}

func (m *NetworkHostMap) Lookup(key string) (NetworkHost, bool) {
	if m == nil {
		return NetworkHost{}, false
	}
	index, ok := m.byKey[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		return NetworkHost{}, false
	}
	if index < 0 || index >= len(m.status) {
		return NetworkHost{}, false
	}
	return m.status[index], true
}
