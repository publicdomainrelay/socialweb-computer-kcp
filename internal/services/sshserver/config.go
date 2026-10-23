package sshserver

import (
	"cmp"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const Kind = "SshServer"

const (
	RequesterKind = "Requester"

	DefaultShutdownTimeout = 30 * time.Second

	requestTimeout = 30 * time.Second

	directoryTTL = 60 * time.Second

	directoryKeys = 10_000
)

type Deps struct {
	component.Instance

	Hosts component.NetworkHosts

	TLS *tls.Config
}

func New(spec v1alpha1.SshServerSpec, deps Deps) (*Service, error) {
	s := &Service{spec: spec, deps: deps, tls: deps.TLS}
	stateDir := deps.StateDir
	if spec.Storage != nil && spec.Storage.DataDir != "" {
		stateDir = spec.Storage.DataDir
	}
	did := ""
	if spec.Identity != nil {
		did = spec.Identity.Did
	}
	if spec.OwnerSessionsFile != "" {
		sessions, owner, _, err := sshtransport.LoadOwnerSessions(spec.OwnerSessionsFile)
		if err != nil {
			return nil, err
		}
		s.ownerSessions = sessions
		did = cmp.Or(did, owner)
	}
	hostKeyPath := spec.HostKeyRef
	if hostKeyPath == "" && stateDir != "" {
		hostKeyPath = filepath.Join(stateDir, "ssh_host_ed25519_key")
	}
	if hostKeyPath == "" {
		return nil, errors.New("sshserver: the component names no host key path")
	}
	network := s.network()
	s.trust = component.NewTrust(spec.TLS.CA(), deps.Log)
	if s.trust.Enabled() {
		s.tls = s.trust.Client(nil)
	}
	if network.PLCDirectoryURL != "" && deps.Bus != nil {
		s.directory = s.directoryFor(network.PLCDirectoryURL)
	}
	s.cfg = gateway.Config{
		Hostname:           gateway.DefaultHostname,
		HostKeyPath:        hostKeyPath,
		StateDir:           stateDir,
		DID:                did,
		DefaultCommand:     gateway.DefaultCommand,
		MaxConnections:     int(spec.MaxConnections),
		MaxSessions:        int(spec.MaxSessions),
		SessionsPerAccount: int(spec.SessionsPerAccount),
		MaxAuthAttempts:    int(spec.MaxAuthAttempts),
		AuthTimeout:        seconds(spec.AuthTimeoutSeconds),
		SessionMax:         seconds(spec.SessionMaxSeconds),
		CacheEntries:       int(spec.CacheEntries),
		TrustedHosts:       networkTrustedHosts(network, spec.Hostnames),
		CocoreAPIBaseURL:   gateway.DefaultCocoreAPIBaseURL,
		CocoreAppName:      gateway.DefaultCocoreAppName,
		CocoreTTL:          gateway.DefaultCocoreTTL,
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s.log = log.With("component", "sshserver", "did", did)
	return s, nil
}

func seconds(value *int32) time.Duration {
	if value == nil {
		return 0
	}
	return time.Duration(*value) * time.Second
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

func (s *Service) requesterRef() (commonv1.ObjectRef, error) {
	ref := s.spec.RequesterRef
	if ref == nil || ref.Name == "" {
		return commonv1.ObjectRef{}, errors.New("sshserver: no Requester is configured")
	}
	return commonv1.ObjectRef{Group: ref.Group, Kind: cmp.Or(ref.Kind, RequesterKind), Name: ref.Name, Workspace: cmp.Or(ref.Workspace, s.workspace())}, nil
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
		client := s.httpClient(requestTimeout)
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
		Transport: callerTransport{instance: s.deps.Instance, next: s.deps.Bus.Transport(external)},
	}
}

func (s *Service) inProcess(host string) bool {
	_, ok := s.deps.Bus.HandlerForHost(host)
	return ok
}

type callerTransport struct {
	instance component.Instance

	next http.RoundTripper
}

func (t callerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(req.WithContext(t.instance.Caller(req.Context())))
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
