package networkdial

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

var (
	ErrNoResolver       = errors.New("networkdial: no host resolver was given")
	ErrUnknownHost      = errors.New("networkdial: the host is not in the network host map")
	ErrInvalidHost      = errors.New("networkdial: invalid host map entry")
	ErrNoCABundle       = errors.New("networkdial: no network CA bundle was given")
	ErrInvalidCABundle  = errors.New("networkdial: the network CA bundle has no certificates")
	ErrSystemDNSBlocked = errors.New("networkdial: system DNS is not allowed")
)

type Host struct {
	Name    string
	Address string
}

type Resolver interface {
	Address(hostport string) (string, bool)
}

type HostMap struct {
	hosts map[string]string
}

func NewHostMap(hosts []Host) (*HostMap, error) {
	m := &HostMap{hosts: map[string]string{}}
	for _, host := range hosts {
		if err := m.add(host.Name, host.Address); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func HostMapFromPairs(pairs map[string]string) (*HostMap, error) {
	m := &HostMap{hosts: map[string]string{}}
	for name, address := range pairs {
		if err := m.add(name, address); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func ParseHostMap(value string) (*HostMap, error) {
	m := &HostMap{hosts: map[string]string{}}
	for _, pair := range strings.Split(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.IndexByte(pair, '=')
		if i <= 0 {
			return nil, fmt.Errorf("%w: %q is not name=address", ErrInvalidHost, pair)
		}
		if err := m.add(pair[:i], pair[i+1:]); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *HostMap) add(name, address string) error {
	host := normalizeHost(name)
	if host == "" {
		return fmt.Errorf("%w: %q has no host name", ErrInvalidHost, name)
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return fmt.Errorf("%w: %q has no address", ErrInvalidHost, name)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q address %q is not host:port: %v", ErrInvalidHost, name, address, err)
	}
	if port == "" {
		return fmt.Errorf("%w: %q address %q has no port", ErrInvalidHost, name, address)
	}
	if _, ok := m.hosts[host]; ok {
		return fmt.Errorf("%w: %q appears more than once", ErrInvalidHost, host)
	}
	m.hosts[host] = address
	return nil
}

func (m *HostMap) Lookup(host string) (string, bool) {
	if m == nil {
		return "", false
	}
	name := normalizeHost(host)
	if address, ok := m.hosts[name]; ok {
		return address, true
	}
	for base, address := range m.hosts {
		if strings.HasSuffix(name, "."+base) {
			return address, true
		}
	}
	return "", false
}

func (m *HostMap) Address(hostport string) (string, bool) {
	return m.Lookup(hostport)
}

func (m *HostMap) Hosts() map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m.hosts))
	for name, address := range m.hosts {
		out[name] = address
	}
	return out
}

func (m *HostMap) Names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.hosts))
	for name := range m.hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (m *HostMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.hosts)
}

func (m *HostMap) TLSConfig(caBundlePEM []byte, host string) (*tls.Config, error) {
	name := normalizeHost(host)
	if name == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHost, host)
	}
	if _, ok := m.Lookup(name); !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownHost, name)
	}
	return TLSConfigFromCA(caBundlePEM, name)
}

func normalizeHost(value string) string {
	host := strings.TrimSpace(value)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	if strings.ContainsAny(host, "/?#@") {
		return ""
	}
	return host
}
