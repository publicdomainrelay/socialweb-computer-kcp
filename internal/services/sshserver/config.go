package sshserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const Kind = "SshServer"

type IdentityMode string

const (
	IdentityModeOwner IdentityMode = "owner"

	IdentityModeLocal IdentityMode = "local"
)

const (
	DefaultHTTPHost = "127.0.0.1"

	DefaultSSHHost = "127.0.0.1"

	DefaultDefaultCommand = "bash"

	DefaultMaxConnections = sshtransport.DefaultMaxConnections

	DefaultMaxSessions = sshtransport.DefaultMaxSessions

	DefaultSessionsPerAccount = sshtransport.DefaultSessionsPerAccount

	DefaultMaxAuthAttempts = sshtransport.DefaultMaxAuthAttempts

	DefaultAuthTimeout = sshtransport.DefaultAuthTimeout

	DefaultSessionMax = time.Hour

	DefaultAssociationTTL = sshtransport.DefaultCacheTTL

	DefaultNegativeTTL = sshtransport.DefaultNegativeCacheTTL

	DefaultMissRecheck = sshtransport.DefaultMissRecheck

	DefaultCacheEntries = sshtransport.DefaultMaxCacheEntries

	DefaultCocoreAPIBaseURL = "https://cocore.dev/api"

	DefaultCocoreAppName = "socialweb-computer-ssh"

	DefaultCocoreTTL = sshtransport.DefaultCocorePairingTTL

	DefaultShutdownTimeout = 30 * time.Second

	DefaultPublicOrigin = ""
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

type Config struct {
	Deps component.Deps

	Key component.Key

	StateDir string

	Workspace string

	Namespace string

	ComponentName string

	Network v1alpha1.MarketNetworkValue

	Identity v1alpha1.ComponentIdentity

	IdentityMode IdentityMode

	DID string

	Handle string

	RequesterRef *v1alpha1.ComponentRef

	VaultRef *v1alpha1.ComponentRef

	Hostnames []string

	HostKeyRef string

	HostKeyPath string

	OwnerSessionsFile string

	OwnerSessions map[string]OwnerSession

	HTTPHost string

	HTTPPort int

	SSHHost string

	SSHPort int

	SSHPublicHost string

	PublicHostname string

	PublicOrigin string

	DefaultCommand string

	MaxConnections int

	MaxSessions int

	SessionsPerAccount int

	MaxAuthAttempts int

	AuthTimeout time.Duration

	SessionMax time.Duration

	AssociationTTL time.Duration

	NegativeCacheTTL time.Duration

	MissRecheck time.Duration

	CacheEntries int

	TrustedHosts []string

	Directory identity.Directory

	DirectoryTTL time.Duration

	DirectoryKeys int

	KeyAuthorizer sshtransport.KeyAuthorizer

	IdentityResolver sshtransport.IdentityResolver

	RecordSource sshtransport.RecordSource

	Requester CommandRunner

	Auth sshtransport.Authenticator

	Agents sshtransport.AgentProvider

	SignIn coreapi.WebSignIn

	SessionCookie string

	CocoreAPIBaseURL string

	CocoreAppName string

	CocoreAppDID string

	CocoreKeyName string

	CocoreReturnURL string

	CocoreTTL time.Duration

	CocoreTokens sshtransport.CocoreTokenStore

	ShutdownTimeout time.Duration

	Now func() time.Time

	Log *slog.Logger
}

type OwnerSession = sshtransport.OwnerSession

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.SshServerSpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("sshserver: decoding the component spec: %w", err)
	}
	cfg := Config{
		Deps:          deps,
		Key:           component.Key{Workspace: spec.Workspace, Kind: spec.Kind, Name: spec.Name},
		StateDir:      spec.StateDir,
		Workspace:     spec.Workspace,
		Namespace:     spec.Namespace,
		ComponentName: spec.Name,
		RequesterRef:  decoded.RequesterRef,
		VaultRef:      decoded.VaultRef,
		Hostnames:     append([]string(nil), decoded.Hostnames...),
		HostKeyRef:    decoded.HostKeyRef,
	}
	if cfg.StateDir == "" {
		cfg.StateDir = deps.StateDir
	}
	if decoded.Identity != nil {
		cfg.Identity = *decoded.Identity
		cfg.DID = decoded.Identity.Did
		cfg.Handle = decoded.Identity.Handle
	}
	if decoded.Network != nil {
		cfg.Network = *decoded.Network
	}
	if decoded.Serve != nil {
		if decoded.Serve.Host != "" {
			cfg.HTTPHost = decoded.Serve.Host
		}
		cfg.HTTPPort = int(decoded.Serve.Port)
	}
	if decoded.Storage != nil && decoded.Storage.DataDir != "" {
		cfg.StateDir = decoded.Storage.DataDir
	}
	if decoded.OwnerSessionsFile != "" {
		sessions, did, handle, err := sshtransport.LoadOwnerSessions(decoded.OwnerSessionsFile)
		if err != nil {
			return Config{}, err
		}
		cfg.OwnerSessionsFile = decoded.OwnerSessionsFile
		cfg.OwnerSessions = sessions
		if cfg.DID == "" {
			cfg.DID = did
		}
		if cfg.Handle == "" {
			cfg.Handle = handle
		}
		if cfg.Identity.Did == "" {
			cfg.Identity.Did = did
		}
		if cfg.Identity.Handle == "" {
			cfg.Identity.Handle = handle
		}
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "root"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	cfg.defaults()
	return cfg, nil
}

func (c Config) withDeps(deps component.Deps) Config {
	out := c
	if out.Deps.Log == nil {
		out.Deps.Log = deps.Log
	}
	if out.Deps.Registry == nil {
		out.Deps.Registry = deps.Registry
	}
	if out.Deps.Handlers == nil {
		out.Deps.Handlers = deps.Handlers
	}
	if out.Deps.KCP == nil {
		out.Deps.KCP = deps.KCP
	}
	if out.Deps.Credentials == nil {
		out.Deps.Credentials = deps.Credentials
	}
	if out.Deps.Vault == nil {
		out.Deps.Vault = deps.Vault
	}
	if out.Deps.Hosts == nil {
		out.Deps.Hosts = deps.Hosts
	}
	if out.Deps.TLS == nil {
		out.Deps.TLS = deps.TLS
	}
	if out.StateDir == "" {
		out.StateDir = deps.StateDir
	}
	if out.Log == nil {
		out.Log = deps.Log
	}
	out.defaults()
	return out
}

func (c *Config) defaults() {
	if c.HTTPHost == "" {
		c.HTTPHost = DefaultHTTPHost
	}
	if c.SSHHost == "" {
		c.SSHHost = DefaultSSHHost
	}
	if c.DefaultCommand == "" {
		c.DefaultCommand = DefaultDefaultCommand
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.SessionsPerAccount <= 0 {
		c.SessionsPerAccount = DefaultSessionsPerAccount
	}
	if c.MaxAuthAttempts <= 0 {
		c.MaxAuthAttempts = DefaultMaxAuthAttempts
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = DefaultAuthTimeout
	}
	if c.SessionMax <= 0 {
		c.SessionMax = DefaultSessionMax
	}
	if c.AssociationTTL <= 0 {
		c.AssociationTTL = DefaultAssociationTTL
	}
	if c.NegativeCacheTTL <= 0 {
		c.NegativeCacheTTL = DefaultNegativeTTL
	}
	if c.MissRecheck <= 0 {
		c.MissRecheck = DefaultMissRecheck
	}
	if c.CacheEntries <= 0 {
		c.CacheEntries = DefaultCacheEntries
	}
	if c.DirectoryTTL <= 0 {
		c.DirectoryTTL = 60 * time.Second
	}
	if c.DirectoryKeys <= 0 {
		c.DirectoryKeys = 10_000
	}
	if c.CocoreAPIBaseURL == "" {
		c.CocoreAPIBaseURL = DefaultCocoreAPIBaseURL
	}
	if c.CocoreAppName == "" {
		c.CocoreAppName = DefaultCocoreAppName
	}
	if c.CocoreKeyName == "" {
		c.CocoreKeyName = c.CocoreAppName
	}
	if c.CocoreTTL <= 0 {
		c.CocoreTTL = DefaultCocoreTTL
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.IdentityMode == "" {
		c.IdentityMode = IdentityModeOwner
	}
	if c.HostKeyPath == "" {
		if c.HostKeyRef != "" {
			c.HostKeyPath = c.HostKeyRef
		} else if c.StateDir != "" {
			c.HostKeyPath = filepath.Join(c.StateDir, "ssh_host_ed25519_key")
		}
	}
	if tlsConfig := networkTLSConfig(c.Network.CaBundleRef); tlsConfig != nil {
		c.Deps.TLS = tlsConfig
	}
	if c.Directory == nil && c.Network.PlcDirectoryUrl != "" && c.Deps.Handlers != nil {
		c.Directory = directoryFor(*c, c.Network.PlcDirectoryUrl, c.DirectoryKeys, c.DirectoryTTL)
	}
	if len(c.TrustedHosts) == 0 {
		c.TrustedHosts = networkTrustedHosts(c.Network, c.Hostnames, c.PublicHostname)
	}
	if c.CocoreAppDID == "" {
		c.CocoreAppDID = c.DID
	}
	if c.CocoreReturnURL == "" {
		if c.PublicOrigin != "" {
			c.CocoreReturnURL = strings.TrimRight(c.PublicOrigin, "/") + "/"
		} else if c.SSHPublicHost != "" {
			c.CocoreReturnURL = "http://" + c.SSHPublicHost + "/"
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil && c.Deps.Log != nil {
		c.Log = c.Deps.Log
	}
}

func (c Config) check() error {
	switch {
	case c.HostKeyPath == "":
		return errors.New("sshserver: the component names no host key path")
	case c.IdentityMode != IdentityModeOwner && c.IdentityMode != IdentityModeLocal:
		return fmt.Errorf("sshserver: unknown identity mode %q", c.IdentityMode)
	}
	return nil
}

func Factory(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
	cfg, err := ConfigFrom(deps, spec)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory)
}

func directoryFor(cfg Config, raw string, capacity int, ttl time.Duration) identity.Directory {
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
		client := serviceHTTPClient(cfg, 30*time.Second)
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
	return identity.NewCacheDirectory(inner, capacity, ttl, ttl, ttl)
}

type handlerAwareRoundTripper struct {
	handlers *component.HandlerRegistry
	external http.RoundTripper
}

func (t handlerAwareRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && req.URL.Scheme == "http" && t.handlers != nil {
		if _, ok := t.handlers.HandlerForHost(req.URL.Host); ok {
			return t.handlers.RoundTripper().RoundTrip(req)
		}
	}
	external := t.external
	if external == nil {
		external = http.DefaultTransport
	}
	return external.RoundTrip(req)
}

func serviceHTTPClient(cfg Config, timeout time.Duration) *http.Client {
	var external http.RoundTripper = http.DefaultTransport
	if transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{
		Hosts:          networkHostsResolver(cfg.Network, cfg.Deps.Hosts),
		TLSConfig:      cfg.Deps.TLS,
		AllowSystemDNS: true,
	}); err == nil {
		external = transport
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: handlerAwareRoundTripper{handlers: cfg.Deps.Handlers, external: external},
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

func networkTLSConfig(caBundleRef string) *tls.Config {
	raw := strings.TrimSpace(caBundleRef)
	if raw == "" {
		return nil
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
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

func internalHosts(deps component.Deps) []string {
	if deps.Registry == nil {
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
	for _, entry := range deps.Registry.List("") {
		for _, endpoint := range entry.Endpoints {
			add(endpoint.URL)
			add(endpoint.Address)
		}
	}
	return out
}

func networkTrustedHosts(network v1alpha1.MarketNetworkValue, hostnames []string, extra ...string) []string {
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
	for _, value := range extra {
		add(value)
	}
	for _, host := range network.Hosts {
		add(host.Name)
	}
	add(network.IngressProxyHost)
	add(network.GuestTlsUrl)
	add(network.QrOrigin)
	add(network.PlcDirectoryUrl)
	for _, raw := range network.RelayUrls {
		add(raw)
	}
	return out
}
