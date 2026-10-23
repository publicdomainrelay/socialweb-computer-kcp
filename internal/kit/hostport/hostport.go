package hostport

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
)

func HostnameOnly(host string) string {
	h := host
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end != -1 {
			h = h[1:end]
		}
	}
	if idx := strings.LastIndex(h, ":"); idx != -1 && idx > strings.Index(h, "]") {
		h = h[:idx]
	}
	return h
}

func URLHostname(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if parsed, err := url.Parse("//" + raw); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if name, _, err := net.SplitHostPort(raw); err == nil {
		return name
	}
	return strings.Trim(raw, "[]")
}

func Normalize(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	value = strings.TrimSuffix(value, ".")
	return strings.ToLower(value)
}

func FromRequest(req *http.Request, fallback string) string {
	if req.Host != "" {
		return req.Host
	}
	if req.URL != nil && req.URL.Host != "" {
		return req.URL.Host
	}
	return fallback
}

func Listen(address, host string, port int) string {
	if address != "" {
		return address
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func Primary(host string, hostnames []string) string {
	if host != "" {
		return host
	}
	for _, candidate := range hostnames {
		if candidate != "" {
			return candidate
		}
	}
	return "localhost"
}

func ForURL(raw string, hosts []v1alpha1.NetworkHost) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
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
		return ""
	}
	for _, host := range hosts {
		if _, candidate, splitErr := net.SplitHostPort(host.Address); splitErr == nil && candidate == port {
			return host.Address
		}
	}
	return ""
}

func NetworkHostnames(network *v1alpha1.MarketNetworkValue, port int) []string {
	if network == nil || port <= 0 {
		return nil
	}
	wanted := strconv.Itoa(port)
	var out []string
	for _, host := range network.Hosts {
		_, value, err := net.SplitHostPort(host.Address)
		if err != nil || value != wanted {
			continue
		}
		out = append(out, host.Name)
	}
	return out
}

func EndpointURL(endpoint component.Endpoint) (string, bool) {
	if endpoint.URL != "" {
		return endpoint.URL, true
	}
	if endpoint.Address == "" {
		return "", false
	}
	scheme := strings.TrimSpace(endpoint.Protocol)
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + endpoint.Address, true
}

func IsInternalKCP(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "svc.kcp.local" || strings.HasSuffix(host, ".svc.kcp.local")
}

func WebDID(host string) string {
	return "did:web:" + HostnameOnly(host)
}

func DIDLabel(did string) string {
	return strings.ToLower(strings.ReplaceAll(did, ":", "-"))
}
