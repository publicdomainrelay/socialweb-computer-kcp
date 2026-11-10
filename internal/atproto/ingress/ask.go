package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	askTimeout        = 3 * time.Second
	caddyConfigMax    = 4 << 20
	caddyConfigOrigin = "http://127.0.0.1/config/"
)

type caddyRoute struct {
	Match []caddyMatcher `json:"match"`
}

type caddyMatcher struct {
	Host []string `json:"host"`
}

type caddyServer struct {
	Routes []caddyRoute `json:"routes"`
}

type caddyConfig struct {
	Apps struct {
		HTTP struct {
			Servers map[string]caddyServer `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

func newCaddyAskClient(socket string) *http.Client {
	dialer := &net.Dialer{Timeout: askTimeout}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}
}

func (s *Server) AskHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host := normalizeAliasHost(req.URL.Query().Get("domain"))
		if !validAliasHost(host) || !s.hostInScope(host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), askTimeout)
		defer cancel()
		if s.caddyHostAllows(ctx, host) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if s.isControlHost(host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if _, ok := s.aliasSubdomain(ctx, host); ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "no alias", http.StatusForbidden)
	})
}

func (s *Server) caddyHostAllows(ctx context.Context, host string) bool {
	if s.caddyAsk == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caddyConfigOrigin, nil)
	if err != nil {
		return false
	}
	resp, err := s.caddyAsk.Do(req)
	if err != nil {
		s.log.Warn("ingress_ask_caddy_config_failed", "host", host, "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("ingress_ask_caddy_config_failed", "host", host, "status", resp.StatusCode)
		return false
	}
	var cfg caddyConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, caddyConfigMax)).Decode(&cfg); err != nil {
		s.log.Warn("ingress_ask_caddy_config_failed", "host", host, "err", err)
		return false
	}
	for _, server := range cfg.Apps.HTTP.Servers {
		for _, route := range server.Routes {
			for _, match := range route.Match {
				for _, pattern := range match.Host {
					if caddyHostPatternAllows(pattern, host) {
						return true
					}
				}
			}
		}
	}
	return false
}

func caddyHostPatternAllows(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || pattern == "*" {
		return false
	}
	if pattern == host {
		return true
	}
	if !strings.HasPrefix(pattern, "*.") || !strings.HasSuffix(host, pattern[1:]) {
		return false
	}
	label := strings.TrimSuffix(host, pattern[1:])
	return label != "" && !strings.Contains(label, ".")
}
