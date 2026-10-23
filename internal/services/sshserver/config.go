package sshserver

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const Kind = "SshServer"

const (
	DefaultSSHHost = "127.0.0.1"

	DefaultDefaultCommand = "bash"

	DefaultCocoreAPIBaseURL = "https://cocore.dev/api"

	DefaultCocoreAppName = "socialweb-computer-ssh"

	DefaultCocoreTTL = sshtransport.DefaultCocorePairingTTL

	DefaultShutdownTimeout = 30 * time.Second

	directoryTTL = 60 * time.Second

	directoryKeys = 10_000
)

type CommandRunner interface {
	RunCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error
}

type legacyCommandRunner struct {
	runner sshtransport.ComputeCommandRunner
}

func (l legacyCommandRunner) RunCommand(ctx context.Context, account sshtransport.AuthorizedAccount, command string, env map[string]string, io sshtransport.CommandIO) error {
	return l.runner.Run(ctx, account, command, env, io)
}

type Deps struct {
	component.Instance

	Registry *component.Registry

	Hosts component.NetworkHosts

	TLS *tls.Config
}

type OwnerSession = sshtransport.OwnerSession

func New(spec v1alpha1.SshServerSpec, deps Deps) (*Service, error) {
	s := &Service{spec: spec, deps: deps, stateDir: deps.StateDir, tls: deps.TLS}
	if spec.Identity != nil {
		s.did = spec.Identity.Did
	}
	if spec.Storage != nil && spec.Storage.DataDir != "" {
		s.stateDir = spec.Storage.DataDir
	}
	if spec.OwnerSessionsFile != "" {
		sessions, did, _, err := sshtransport.LoadOwnerSessions(spec.OwnerSessionsFile)
		if err != nil {
			return nil, err
		}
		s.ownerSessions = sessions
		s.did = cmp.Or(s.did, did)
	}
	s.hostKeyPath = spec.HostKeyRef
	if s.hostKeyPath == "" && s.stateDir != "" {
		s.hostKeyPath = filepath.Join(s.stateDir, "ssh_host_ed25519_key")
	}
	if s.hostKeyPath == "" {
		return nil, errors.New("sshserver: the component names no host key path")
	}
	network := s.network()
	if tlsConfig := networkTLSConfig(network.CABundle); tlsConfig != nil {
		s.tls = tlsConfig
	}
	if network.PLCDirectoryURL != "" && deps.Bus != nil {
		s.directory = s.directoryFor(network.PLCDirectoryURL)
	}
	s.trustedHosts = networkTrustedHosts(network, spec.Hostnames)
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s.log = log.With("component", "sshserver", "did", s.did)
	return s, nil
}

func (s *Service) network() v1alpha1.MarketNetworkValue {
	if s.spec.Network == nil {
		return v1alpha1.MarketNetworkValue{}
	}
	return *s.spec.Network
}

func (s *Service) workspace() string {
	return cmp.Or(s.deps.Self.Workspace, "root")
}

func (s *Service) directoryFor(raw string) identity.Directory {
	base := identity.DefaultDirectory()
	raw = strings.TrimRight(raw, "/")
	if raw == "" || raw == strings.TrimRight(identity.DefaultPLCURL, "/") {
		return base
	}
	cached, ok := base.(*identity.CacheDirectory)
	if !ok {
		return base
	}
	inner := cached.Inner
	if directory, ok := inner.(*identity.BaseDirectory); ok {
		client := s.httpClient(30 * time.Second)
		inner = &identity.BaseDirectory{
			PLCURL:                 raw,
			PLCLimiter:             directory.PLCLimiter,
			DIDWebLimitFunc:        directory.DIDWebLimitFunc,
			HTTPClient:             *client,
			PLCClient:              client,
			TryAuthoritativeDNS:    directory.TryAuthoritativeDNS,
			SkipDNSDomainSuffixes:  append([]string(nil), directory.SkipDNSDomainSuffixes...),
			FallbackDNSServers:     append([]string(nil), directory.FallbackDNSServers...),
			SkipHandleVerification: directory.SkipHandleVerification,
			UserAgent:              directory.UserAgent,
		}
	}
	return identity.NewCacheDirectory(inner, directoryKeys, directoryTTL, directoryTTL, directoryTTL)
}

func (s *Service) httpClient(timeout time.Duration) *http.Client {
	external := httpclient.External(httpclient.Options{Hosts: networkHostsResolver(s.network(), s.deps.Hosts), TLS: s.tls}).Transport
	if external == nil {
		external = http.DefaultTransport
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: s.deps.Bus.Transport(external),
	}
}

type chainedHosts []networkdial.Resolver

func (c chainedHosts) Address(hostport string) (string, bool) {
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

func networkTLSConfig(caBundle string) *tls.Config {
	raw := strings.TrimSpace(caBundle)
	if raw == "" {
		return nil
	}
	cfg, err := httpclient.TLSConfig(httpclient.TLSOptions{CAPEM: []byte(raw)})
	if err != nil {
		return nil
	}
	return cfg
}

func networkHostsResolver(network v1alpha1.MarketNetworkValue, fallback networkdial.Resolver) networkdial.Resolver {
	entries := make([]networkdial.Host, 0, len(network.Hosts))
	for _, host := range network.Hosts {
		entries = append(entries, networkdial.Host{Name: host.Name, Address: host.Address})
	}
	resolvers := make([]networkdial.Resolver, 0, 2)
	if mapped, err := networkdial.NewHostMap(entries); err == nil && mapped.Len() > 0 {
		resolvers = append(resolvers, mapped)
	}
	if fallback != nil {
		resolvers = append(resolvers, fallback)
	}
	switch len(resolvers) {
	case 0:
		return nil
	case 1:
		return resolvers[0]
	default:
		return chainedHosts(resolvers)
	}
}

func internalHosts(registry *component.Registry) []string {
	if registry == nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		host := raw
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			host = parsed.Hostname()
		} else if before, _, found := strings.Cut(raw, ":"); found && !strings.Contains(before, "/") {
			host = before
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	for _, entry := range registry.List("") {
		for _, endpoint := range entry.Endpoints {
			add(endpoint.URL)
			add(endpoint.Address)
		}
	}
	return out
}

func networkTrustedHosts(network v1alpha1.MarketNetworkValue, hostnames []string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		host := raw
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			host = parsed.Hostname()
		} else if before, _, found := strings.Cut(raw, ":"); found && !strings.Contains(before, "/") {
			host = before
		}
		host = strings.Trim(host, "[]")
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		out = append(out, host)
	}
	for _, value := range hostnames {
		add(value)
	}
	for _, host := range network.Hosts {
		add(host.Name)
	}
	add(network.IngressProxyHost)
	add(network.GuestTLSURL())
	add(network.QrOrigin)
	add(network.PLCDirectoryURL)
	for _, raw := range network.Relays() {
		add(raw)
	}
	return out
}
