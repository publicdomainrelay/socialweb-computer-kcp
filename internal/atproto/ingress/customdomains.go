package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	caddyAdminOrigin      = "http://127.0.0.1"
	CustomDomainsRouteID  = "route-xrpc-custom-domains"
	customDomainsPolicyID = "tls-policy-wildcard-"
)

func (s *Server) caddyAdmin(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, caddyAdminOrigin+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.caddyAsk.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, caddyConfigMax))
	return resp.StatusCode, data, err
}

func customDomainsRoute(zone, upstreamSocket string) map[string]any {
	return map[string]any{
		"@id": CustomDomainsRouteID,
		"match": []any{map[string]any{"not": []any{map[string]any{
			"host": []string{zone, "*." + zone, "*.*." + zone, "*.*.*." + zone},
		}}}},
		"handle": []any{map[string]any{"handler": "subroute", "routes": []any{map[string]any{
			"handle": []any{map[string]any{
				"handler":            "reverse_proxy",
				"flush_interval":     -1,
				"stream_close_delay": "5m",
				"transport":          map[string]any{"protocol": "http", "keep_alive": map[string]any{"probe_interval": "30s"}},
				"upstreams":          []any{map[string]any{"dial": "unix/" + upstreamSocket}},
			}},
		}}}},
		"terminal": true,
	}
}

type caddyIssuer struct {
	Module     string `json:"module"`
	Challenges struct {
		DNS json.RawMessage `json:"dns"`
	} `json:"challenges"`
}

func (s *Server) ReconcileCaddyCustomDomains(ctx context.Context, zone, upstreamSocket string) error {
	if s.caddyAsk == nil {
		return errors.New("no caddy admin socket")
	}
	code, _, err := s.caddyAdmin(ctx, http.MethodGet, "/id/"+CustomDomainsRouteID, nil)
	switch {
	case err != nil:
		return fmt.Errorf("reading the route: %w", err)
	case code == http.StatusNotFound:
		code, body, err := s.caddyAdmin(ctx, http.MethodPost, "/config/apps/http/servers/srv0/routes", customDomainsRoute(zone, upstreamSocket))
		if err != nil {
			return fmt.Errorf("adding the route: %w", err)
		}
		if code >= 300 {
			return fmt.Errorf("adding the route: caddy answered %d %s", code, strings.TrimSpace(string(body)))
		}
		s.log.Info("ingress_caddy_custom_domains_route_added", "route", CustomDomainsRouteID, "zone", zone)
	case code != http.StatusOK:
		return fmt.Errorf("reading the route: caddy answered %d", code)
	}

	issuersPath := "/id/" + customDomainsPolicyID + zone + "/issuers"
	code, body, err := s.caddyAdmin(ctx, http.MethodGet, issuersPath, nil)
	if err != nil {
		return fmt.Errorf("reading the issuers: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("reading the issuers of %s%s: caddy answered %d (atprp-ssh-relay creates that policy)", customDomainsPolicyID, zone, code)
	}
	var issuers []caddyIssuer
	if err := json.Unmarshal(body, &issuers); err != nil {
		return errors.New("reading the issuers: not a JSON array")
	}
	for _, issuer := range issuers {
		if issuer.Module == "acme" && (len(issuer.Challenges.DNS) == 0 || string(issuer.Challenges.DNS) == "null") {
			return nil
		}
	}
	code, _, err = s.caddyAdmin(ctx, http.MethodPost, issuersPath, map[string]string{"module": "acme"})
	if err != nil {
		return fmt.Errorf("adding the fallback issuer: %w", err)
	}
	if code >= 300 {
		return fmt.Errorf("adding the fallback issuer: caddy answered %d", code)
	}
	s.log.Info("ingress_caddy_custom_domains_issuer_added", "policy", customDomainsPolicyID+zone)
	return nil
}

func (s *Server) KeepCaddyCustomDomains(ctx context.Context, zone, upstreamSocket string, interval time.Duration) {
	for {
		pass, cancel := context.WithTimeout(ctx, interval)
		err := s.ReconcileCaddyCustomDomains(pass, zone, upstreamSocket)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.log.Warn("ingress_caddy_custom_domains_failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
