package tlsproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
)

type Config struct {
	Hostnames []string
}

type Deps struct {
	Upstream func() (string, bool)

	DialTLS func(ctx context.Context, network, addr string) (net.Conn, error)

	Log *slog.Logger
}

type Proxy struct {
	hosts []string

	upstream func() (string, bool)

	log *slog.Logger

	proxy *httputil.ReverseProxy
}

var _ http.Handler = (*Proxy)(nil)

type targetKey struct{}

func New(cfg Config, deps Deps) (*Proxy, error) {
	if deps.Upstream == nil {
		return nil, errors.New("tlsproxy: an upstream resolver is required")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if deps.DialTLS != nil {
		transport.DialTLSContext = deps.DialTLS
	}
	p := &Proxy{hosts: AllowedHosts(cfg.Hostnames), upstream: deps.Upstream, log: log}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			target, _ := req.In.Context().Value(targetKey{}).(*url.URL)
			if target == nil {
				return
			}
			req.Out.URL.Scheme = target.Scheme
			req.Out.URL.Host = target.Host
			req.Out.Host = req.In.Host
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, proxyErr error) {
			log.Warn("dispatcher_tls_upstream_failed", "host", req.Host, "path", req.URL.Path, "err", proxyErr)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	return p, nil
}

func (p *Proxy) Hosts() []string { return append([]string(nil), p.hosts...) }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host := hostport.HostnameOnly(req.Host)
	if !HostAllowed(p.hosts, host) {
		p.log.Info("dispatcher_tls_refused", "host", host, "path", req.URL.Path)
		http.Error(w, "not a dispatcher name", http.StatusNotFound)
		return
	}
	raw, ok := p.upstream()
	if !ok {
		p.log.Warn("dispatcher_tls_upstream_unresolved", "host", req.Host, "path", req.URL.Path)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	target, err := ParseUpstream(raw)
	if err != nil {
		p.log.Warn("dispatcher_tls_upstream_invalid", "upstream", raw, "err", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	p.proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), targetKey{}, target)))
}

func ParseUpstream(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: parsing the upstream: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("tlsproxy: the upstream scheme %q is not http or https", target.Scheme)
	}
	if target.Host == "" {
		return nil, errors.New("tlsproxy: the upstream has no host")
	}
	return target, nil
}

func AllowedHosts(hostnames []string) []string {
	seen := map[string]struct{}{}
	hosts := []string{}
	for _, host := range hostnames {
		host = strings.ToLower(hostport.HostnameOnly(strings.TrimSpace(host)))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		hosts = []string{"localhost"}
	}
	return hosts
}

func HostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(hostport.HostnameOnly(host))
	for _, allowed := range hosts {
		if allowed == "*" || host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func ProbeHost(hosts []string) string {
	for _, host := range hosts {
		if host != "*" {
			return host
		}
	}
	return "localhost"
}
