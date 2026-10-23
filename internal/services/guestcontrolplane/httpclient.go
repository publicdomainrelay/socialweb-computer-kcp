package guestcontrolplane

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
)

func networkHTTPClient(deps component.Deps, network v1alpha1.MarketNetworkValue, timeout time.Duration) *http.Client {
	entries := []networkdial.Host{}
	for _, host := range network.Hosts {
		entries = append(entries, networkdial.Host{Name: host.Name, Address: host.Address})
	}
	if name, address := guestTLSHostAddress(network); name != "" && address != "" {
		entries = append(entries, networkdial.Host{Name: name, Address: address})
	}
	var resolver networkdial.Resolver = deps.Hosts
	if mapped, err := networkdial.NewHostMap(entries); err == nil && mapped.Len() > 0 {
		resolver = mapped
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if bundle := strings.TrimSpace(network.CABundle); bundle != "" {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM([]byte(bundle)) {
			tlsConfig.RootCAs = pool
		}
	}
	transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{
		Hosts:          resolver,
		TLSConfig:      tlsConfig,
		AllowSystemDNS: true,
	})
	if err != nil {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func guestTLSHostAddress(network v1alpha1.MarketNetworkValue) (string, string) {
	raw := strings.TrimSpace(network.GuestTLSURL())
	if raw == "" {
		return "", ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	if port == "" {
		return "", ""
	}
	for _, host := range network.Hosts {
		if _, candidate, err := net.SplitHostPort(host.Address); err == nil && candidate == port {
			return parsed.Hostname(), host.Address
		}
	}
	return "", ""
}
