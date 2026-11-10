package ingress

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	aliasTXTTTL       = time.Minute
	aliasDNSTimeout   = 5 * time.Second
	aliasCacheEntries = 4096
	maxAliasesPerPeer = 16
	aliasTXTPrefix    = "_xrpc."
)

func newAliasTXTCache() *ttlcache.Cache[string, []string] {
	return ttlcache.New[string, []string](ttlcache.Options{TTL: aliasTXTTTL, MaxEntries: aliasCacheEntries})
}

func normalizeAliasHost(raw string) string {
	return hostport.Normalize(raw)
}

func validAliasHost(host string) bool {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func (s *Server) aliasTXTValues(ctx context.Context, host string) ([]string, error) {
	return s.aliasTXT.GetOrLoad(ctx, host, func(ctx context.Context) ([]string, time.Duration, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, aliasDNSTimeout)
		defer cancel()
		values, err := net.DefaultResolver.LookupTXT(lookupCtx, aliasTXTPrefix+host)
		if err == nil {
			return values, aliasTXTTTL, nil
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, aliasTXTTTL, nil
		}
		return nil, 0, err
	})
}

func (s *Server) aliasTargetValue(value string) (string, bool) {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "\""))
	if strings.HasPrefix(value, "did:key:") {
		return hostport.DIDLabel(value), true
	}
	if _, sub, ok := splitDataHost(value, s.dataBases()); ok {
		return sub, true
	}
	return "", false
}

func (s *Server) aliasSubdomain(ctx context.Context, host string) (string, bool) {
	s.mu.Lock()
	p := s.aliases[host]
	live := p != nil && s.peers[p.subdomain] == p
	s.mu.Unlock()
	if live {
		return p.subdomain, true
	}
	values, err := s.aliasTXTValues(ctx, host)
	if err != nil {
		return "", false
	}
	for _, value := range values {
		sub, ok := s.aliasTargetValue(value)
		if !ok {
			continue
		}
		s.mu.Lock()
		live := s.peers[sub] != nil
		s.mu.Unlock()
		if live {
			return sub, true
		}
	}
	return "", false
}

func (s *Server) handleAliasFrame(p *peer, msg frame) {
	accepted := []string{}
	rejected := map[string]string{}
	seen := map[string]struct{}{}
	for _, raw := range msg.Hosts {
		host := normalizeAliasHost(raw)
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		if len(seen) > maxAliasesPerPeer {
			rejected[host] = "too many hosts in one frame"
			continue
		}
		ok, reason := s.registerAlias(p, host)
		if !ok {
			rejected[host] = reason
			s.log.Warn("ingress_alias_rejected", "subdomain", p.subdomain, "host", host, "reason", reason)
			continue
		}
		accepted = append(accepted, host)
		s.log.Info("ingress_alias_registered", "subdomain", p.subdomain, "host", host)
	}
	if len(accepted) == 0 && len(rejected) == 0 {
		return
	}
	_ = p.writeJSON(map[string]any{
		"$type":    AliasResultType,
		"accepted": accepted,
		"rejected": rejected,
	})
}

func (s *Server) registerAlias(p *peer, host string) (bool, string) {
	if !validAliasHost(host) {
		return false, "invalid hostname"
	}
	if !s.hostInScope(host) {
		return false, "host not allowed"
	}
	if s.isControlHost(host) {
		return false, "reserved host"
	}
	if _, _, ok := splitDataHost(host, s.dataBases()); ok {
		return false, "reserved host"
	}
	s.mu.Lock()
	if s.peers[p.subdomain] != p {
		s.mu.Unlock()
		return false, "subscriber is gone"
	}
	if owner := s.aliases[host]; owner != nil && owner != p {
		s.mu.Unlock()
		return false, "claimed by another subscriber"
	}
	if s.aliases[host] == p {
		s.mu.Unlock()
		return true, ""
	}
	if len(p.aliases) >= maxAliasesPerPeer {
		s.mu.Unlock()
		return false, "alias limit reached"
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), aliasDNSTimeout)
	defer cancel()
	values, err := s.aliasTXTValues(ctx, host)
	if err != nil {
		return false, "alias lookup failed"
	}
	verified := false
	for _, value := range values {
		if sub, ok := s.aliasTargetValue(value); ok && sub == p.subdomain {
			verified = true
			break
		}
	}
	if !verified {
		return false, "no matching " + aliasTXTPrefix + host + " TXT record"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers[p.subdomain] != p {
		return false, "subscriber is gone"
	}
	if owner := s.aliases[host]; owner != nil && owner != p {
		return false, "claimed by another subscriber"
	}
	if len(p.aliases) >= maxAliasesPerPeer && s.aliases[host] != p {
		return false, "alias limit reached"
	}
	if p.aliases == nil {
		p.aliases = map[string]struct{}{}
	}
	p.aliases[host] = struct{}{}
	s.aliases[host] = p
	return true, ""
}

func (s *Server) dropAliasesLocked(p *peer) {
	for host := range p.aliases {
		if s.aliases[host] == p {
			delete(s.aliases, host)
		}
	}
	p.aliases = map[string]struct{}{}
}
