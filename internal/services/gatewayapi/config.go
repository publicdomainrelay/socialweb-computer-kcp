package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "GatewayAPI"

const (
	DefaultAddress         = "127.0.0.1:0"
	DefaultHost            = "127.0.0.1"
	DefaultShutdownTimeout = 30 * time.Second
	DefaultDirectoryTTL    = 60 * time.Second
	DefaultDirectoryKeys   = 10_000
	DefaultVMReadySec      = 120
	DefaultPollInterval    = 2 * time.Second
	DefaultAtprotoMount    = "atproto"
	DefaultAtprotoRole     = "user"
	DefaultCustodyTimeout  = 15 * time.Second
	DefaultRequesterKind   = "Requester"
	DefaultGatewayKind     = "Gateway"
	DefaultVaultKind       = "OpenBaoServer"
	DefaultVaultClient     = "socialweb-computer-k8s"
)

type Ref struct {
	Workspace string

	Kind string

	Name string

	Did string
}

type Config struct {
	Deps component.Deps

	StateDir string

	Workspace string

	ComponentName string

	Address string

	Host string

	Port int

	Identity *v1alpha1.ComponentIdentity

	PDSRef *commonv1.ObjectRef

	Hostnames []string

	TLS *v1alpha1.ComponentTLS

	PublicOrigin string

	ClusterProvider *v1alpha1.GatewayClusterProvider

	ClusterStateDir string

	KubeconfigCAFile string

	Gateway GatewayConfig

	Requester RequesterConfig

	Vault VaultConfig

	Network *v1alpha1.MarketNetworkValue

	DownloadsDir string

	RequiredDownloads []string

	NodeMarket NodeMarketConfig

	Session SessionConfig

	ShutdownTimeout time.Duration

	Log *slog.Logger
}

type GatewayConfig struct {
	Ref Ref

	URL string

	Audience string

	Audiences []string

	Directory identity.Directory

	Server *gateway.Server

	HTTPClient *http.Client

	Timeout time.Duration

	KCPPublicHost string

	MaxKubeconfigTTLSec int64

	MaxBodyBytes int64
}

type RequesterConfig struct {
	Ref Ref

	Workspace string
}

func (r RequesterConfig) kcpWorkspace(fallback string) string {
	if r.Workspace != "" {
		return r.Workspace
	}
	if r.Ref.Workspace != "" {
		return r.Ref.Workspace
	}
	return fallback
}

type VaultConfig struct {
	Ref Ref

	ClientName string

	Mount string

	Address string

	Token string

	CACert []byte

	ClientCert []byte

	ClientKey []byte

	CertAuthMount string

	CertAuthRole string

	Client *openbao.Client

	Timeout time.Duration
}

func (v VaultConfig) configured() bool {
	return v.Client != nil || v.Address != "" || v.Ref.Name != ""
}

type NodeMarketConfig struct {
	Enabled bool

	OwnerSessionsFile string

	HomeWorkspaces []string

	ContractWorkspace string

	ContractQuota int64

	VMReadyTimeoutSec int

	PollInterval time.Duration

	PolicyName string

	PolicyArgs map[string]any

	ExtraBidderDIDs []string

	UserDataBaseFile string

	ClusterCADir string

	DownloadBase string

	VaultMount string
}

type SessionConfig struct {
	SignIn coreapi.WebSignIn

	PublicOrigin string

	PostSignInURL string

	CookieName string

	CookieMaxAge time.Duration

	ClientMetadataPath string

	ClientMetadataCache time.Duration

	MaxBodyBytes int64

	OnSignedIn func(context.Context, *coreapi.WebUser) error

	OnSignedOut func(context.Context, *coreapi.WebUser) error

	CustodyMount string

	AtprotoRole string

	RenewBelow time.Duration

	RenewIncrement time.Duration

	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Vault.ClientName == "" {
		c.Vault.ClientName = DefaultVaultClient
	}
	if c.Address == "" && c.Host == "" && c.Port == 0 {
		c.Address = DefaultAddress
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Session.PublicOrigin == "" {
		c.Session.PublicOrigin = c.PublicOrigin
	}
	if c.Session.CustodyMount == "" {
		c.Session.CustodyMount = DefaultAtprotoMount
	}
	if c.Session.AtprotoRole == "" {
		c.Session.AtprotoRole = DefaultAtprotoRole
	}
	if c.Session.Timeout <= 0 {
		c.Session.Timeout = DefaultCustodyTimeout
	}
	if c.Session.RenewBelow <= 0 {
		c.Session.RenewBelow = 30 * time.Minute
	}
	if c.Session.RenewIncrement <= 0 {
		c.Session.RenewIncrement = time.Hour
	}
	if c.NodeMarket.VMReadyTimeoutSec <= 0 {
		c.NodeMarket.VMReadyTimeoutSec = DefaultVMReadySec
	}
	if c.NodeMarket.PollInterval <= 0 {
		c.NodeMarket.PollInterval = DefaultPollInterval
	}
	if c.NodeMarket.ContractWorkspace == "" {
		c.NodeMarket.ContractWorkspace = c.Requester.kcpWorkspace(c.Workspace)
	}
	if len(c.NodeMarket.HomeWorkspaces) == 0 && c.Workspace != "" {
		c.NodeMarket.HomeWorkspaces = []string{c.Workspace}
	}
	if c.Gateway.Ref.Kind == "" {
		c.Gateway.Ref.Kind = DefaultGatewayKind
	}
	if c.Requester.Ref.Kind == "" {
		c.Requester.Ref.Kind = DefaultRequesterKind
	}
	if c.Vault.Ref.Kind == "" {
		c.Vault.Ref.Kind = DefaultVaultKind
	}
	if c.Vault.Ref.Workspace == "" {
		c.Vault.Ref.Workspace = c.Workspace
	}
	if c.Vault.Timeout <= 0 {
		c.Vault.Timeout = DefaultCustodyTimeout
	}
	return c
}

func (c Config) Check() error {
	switch {
	case c.Gateway.Server == nil:
		return errors.New("gatewayapi: Gateway.Server is required; it serves the cluster XRPC surface")
	case len(c.Gateway.audiences()) == 0:
		return errors.New("gatewayapi: Gateway.Audience or Gateway.Audiences is required")
	case c.Session.SignIn == nil && !c.Vault.configured():
		return errors.New("gatewayapi: Session.SignIn or a vault ref is required for sign-in")
	}
	if c.PublicOrigin != "" {
		if err := checkOrigin(c.PublicOrigin); err != nil {
			return fmt.Errorf("gatewayapi: PublicOrigin: %w", err)
		}
	}
	if c.Session.PublicOrigin != "" {
		if err := checkOrigin(c.Session.PublicOrigin); err != nil {
			return fmt.Errorf("gatewayapi: Session.PublicOrigin: %w", err)
		}
	}
	if c.Session.ClientMetadataPath != "" {
		if err := checkAPIPath(c.Session.ClientMetadataPath); err != nil {
			return fmt.Errorf("gatewayapi: Session.ClientMetadataPath: %w", err)
		}
	}
	if c.Gateway.URL != "" {
		if err := checkHTTPURL(c.Gateway.URL); err != nil {
			return fmt.Errorf("gatewayapi: Gateway.URL: %w", err)
		}
	}
	if c.Vault.Address != "" {
		if err := checkVaultAddress(c.Vault.Address); err != nil {
			return fmt.Errorf("gatewayapi: Vault.Address: %w", err)
		}
	}
	if c.Gateway.Ref.Name != "" && c.Gateway.Ref.Workspace == "" {
		return errors.New("gatewayapi: Gateway.Ref has no workspace")
	}
	if c.Requester.Ref.Name != "" && c.Requester.Ref.Workspace == "" {
		return errors.New("gatewayapi: Requester.Ref has no workspace")
	}
	if c.NodeMarket.Enabled {
		if c.NodeMarket.ContractWorkspace == "" {
			return errors.New("gatewayapi: NodeMarket.ContractWorkspace is required when the node market is enabled")
		}
		if len(c.NodeMarket.HomeWorkspaces) == 0 {
			return errors.New("gatewayapi: NodeMarket.HomeWorkspaces is required when the node market is enabled")
		}
		if !c.Vault.configured() {
			return errors.New("gatewayapi: a vault ref is required when the node market is enabled")
		}
	}
	if c.Network != nil {
		for _, host := range c.Network.Hosts {
			if host.Name == "" || host.Address == "" {
				return errors.New("gatewayapi: every network host needs a name and an address")
			}
		}
	}
	if len(c.RequiredDownloads) > 0 && c.DownloadsDir == "" {
		return errors.New("gatewayapi: RequiredDownloads needs DownloadsDir")
	}
	if needsMeshd(c.RequiredDownloads) && c.Network == nil && c.downloadBase() == "" {
		return errors.New("gatewayapi: socialweb-meshd is required but no download base is set")
	}
	if c.Address != "" {
		if err := checkLoopback(c.Address); err != nil {
			return err
		}
	} else if c.Host != "" {
		if err := checkLoopbackHost(c.Host); err != nil {
			return err
		}
	}
	return nil
}

func (g GatewayConfig) audiences() []string {
	if len(g.Audiences) > 0 {
		return append([]string(nil), g.Audiences...)
	}
	if g.Audience != "" {
		return []string{g.Audience}
	}
	return nil
}

func needsMeshd(required []string) bool {
	for _, name := range required {
		if strings.HasPrefix(name, coreapi.MeshdDownloadPrefix) {
			return true
		}
	}
	return false
}

func (c Config) downloadBase() string {
	if c.NodeMarket.DownloadBase != "" {
		return strings.TrimRight(c.NodeMarket.DownloadBase, "/")
	}
	if c.Session.PublicOrigin != "" {
		return strings.TrimRight(c.Session.PublicOrigin, "/")
	}
	return strings.TrimRight(c.PublicOrigin, "/")
}

func (c Config) localDownloadBase(ctx context.Context, deps component.Deps) string {
	workspace := c.Workspace
	if workspace == "" {
		workspace = "root"
	}
	store, err := kcpStoreFor(deps, workspace)
	if err != nil {
		return ""
	}
	services, err := kcpstore.Of[exposurev1alpha1.Service](store, exposureServiceGVR).List(ctx, workspace)
	if err != nil {
		return ""
	}
	serviceName := ""
	for i := range services {
		target := services[i].Spec.TargetRef
		targetWorkspace := target.Workspace
		if targetWorkspace == "" {
			targetWorkspace = workspace
		}
		if target.Kind == Kind && target.Name == c.ComponentName && targetWorkspace == workspace {
			serviceName = services[i].Name
			break
		}
	}
	if serviceName == "" {
		return ""
	}
	exposures, err := kcpstore.Of[exposurev1alpha1.Exposure](store, exposureGVR).List(ctx, workspace)
	if err != nil {
		return ""
	}
	for i := range exposures {
		exp := &exposures[i]
		if exp.Spec.Mode != exposurev1alpha1.ExposureModeLocal {
			continue
		}
		if exposureServiceRefName(exp.Spec.ServiceRef) != serviceName {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(exp.Status.Address))
		if err != nil || parsed.Port() == "" {
			continue
		}
		return "http://" + guestnet.HostAddress + ":" + parsed.Port()
	}
	return ""
}

func exposureServiceRefName(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "/"); index >= 0 {
		value = value[index+1:]
	}
	return strings.TrimSpace(value)
}

func checkOrigin(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an absolute http or https origin", raw)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("%q is plain http on a non-loopback host, so the session cookie could not be Secure", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q carries a query or fragment", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%q carries a path, not an origin", raw)
	}
	return nil
}

func checkAPIPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return errors.New("it must start with a slash")
	}
	if path == "/" || path == "/session" {
		return errors.New("it collides with a mounted route")
	}
	for _, reserved := range []string{"/api/", "/downloads/", "/oauth/", "/xrpc/"} {
		if strings.HasPrefix(path, reserved) {
			return errors.New("it collides with " + reserved)
		}
	}
	return nil
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an absolute http or https URL", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q carries a query or fragment", raw)
	}
	return nil
}

func checkVaultAddress(raw string) error {
	if err := checkHTTPURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	if u.User != nil {
		return fmt.Errorf("%q carries user info", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%q carries a path", raw)
	}
	return nil
}

func checkLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("gatewayapi: Address %q is not host:port: %w", address, err)
	}
	return checkLoopbackHost(host)
}

func loopbackHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkLoopbackHost(host string) error {
	if loopbackHost(host) {
		return nil
	}
	return fmt.Errorf("gatewayapi: host %q is not loopback; this listener carries session cookies and service-auth tokens, so it binds 127.0.0.1 or [::1] only", host)
}

func ConfigFrom(deps component.Deps, spec component.ComponentSpec) (Config, error) {
	var decoded v1alpha1.GatewayAPISpec
	if err := spec.Spec.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("gatewayapi: decoding the component spec: %w", err)
	}
	workspace := spec.Workspace
	if workspace == "" {
		workspace = "root"
	}
	stateDir := deps.StateDir
	if spec.StateDir != "" {
		stateDir = spec.StateDir
	}
	cfg := Config{
		Deps:              deps,
		StateDir:          stateDir,
		Workspace:         workspace,
		ComponentName:     spec.Name,
		Host:              DefaultHost,
		Identity:          decoded.Identity,
		PDSRef:            decoded.PdsRef,
		Hostnames:         append([]string(nil), decoded.Hostnames...),
		TLS:               decoded.TLS,
		PublicOrigin:      decoded.PublicOrigin,
		Requester:         RequesterConfig{Ref: refFromComponent(decoded.RequesterRef), Workspace: workspace},
		Vault:             VaultConfig{Ref: refFromComponent(decoded.VaultRef)},
		RequiredDownloads: append([]string(nil), decoded.RequiredDownloads...),
	}
	cfg.Requester.Ref.Workspace = workspace
	if decoded.ServiceAuth != nil && decoded.ServiceAuth.Audience != nil {
		cfg.Gateway.Audiences = append([]string(nil), decoded.ServiceAuth.Audience...)
	}
	cfg.Gateway.Audience = decoded.Audience
	cfg.Gateway.Audiences = gatewayAudiences(cfg.Gateway.Audience, cfg.Gateway.Audiences, decoded.Identity)
	cfg.Gateway.Audiences = uniqueStrings(append(cfg.Gateway.Audiences, gatewayHostAudiences(decoded.Hostnames, decoded.TLS, cfg.PublicOrigin, decoded.Network)...))
	cfg.Session.PostSignInURL = decoded.PostSignInURL
	if cfg.PublicOrigin == "" {
		cfg.PublicOrigin = publicOriginFor(decoded.Hostnames, decoded.TLS)
	}
	if decoded.Network != nil {
		network := *decoded.Network
		cfg.Network = &network
	}
	if decoded.Storage != nil {
		if decoded.Storage.DataDir != "" {
			cfg.StateDir = decoded.Storage.DataDir
		}
		if decoded.Storage.BlobsDir != "" {
			cfg.DownloadsDir = decoded.Storage.BlobsDir
		}
	}
	if decoded.Serve != nil {
		if decoded.Serve.Host != "" {
			cfg.Host = decoded.Serve.Host
		}
		cfg.Port = int(decoded.Serve.Port)
	}
	if decoded.NodeMarket != nil {
		nodeMarket, err := nodeMarketFrom(decoded.NodeMarket)
		if err != nil {
			return Config{}, err
		}
		cfg.NodeMarket = nodeMarket
	}
	if decoded.NodePollIntervalSeconds != nil && *decoded.NodePollIntervalSeconds > 0 {
		cfg.NodeMarket.PollInterval = time.Duration(*decoded.NodePollIntervalSeconds) * time.Second
	}
	if decoded.ClusterProvider != nil {
		clusterProvider := *decoded.ClusterProvider
		if err := checkClusterProvider(clusterProvider); err != nil {
			return Config{}, err
		}
		cfg.ClusterProvider = &clusterProvider
	}
	cfg.ClusterStateDir = decoded.ClusterStateDir
	cfg.KubeconfigCAFile = decoded.KubeconfigCAFile
	if cfg.KubeconfigCAFile == "" {
		cfg.KubeconfigCAFile = os.Getenv("SOCIALWEB_GATEWAY_KCP_PUBLIC_CA_FILE")
	}
	publicHost := os.Getenv("SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST")
	if decoded.Storage != nil && decoded.Storage.CertsDir != "" {
		cfg.NodeMarket.ClusterCADir = decoded.Storage.CertsDir
	}
	server, err := gatewayServerFor(deps, workspace, kubeconfigIssuer(cfg), publicHost, cfg.ClusterStateDir)
	if err != nil {
		return Config{}, err
	}
	cfg.Gateway.Server = server
	return cfg.withDefaults(), nil
}

func kubeconfigIssuer(cfg Config) *clientcert.Issuer {
	if cfg.ClusterStateDir == "" {
		return nil
	}
	return &clientcert.Issuer{StateDir: cfg.ClusterStateDir, CAFile: cfg.KubeconfigCAFile}
}

func checkClusterProvider(cp v1alpha1.GatewayClusterProvider) error {
	if raw := strings.TrimSpace(cp.ScaleHold); raw != "" {
		hold, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("gatewayapi: clusterProvider.scaleHold %q is not a duration: %w", raw, err)
		}
		if hold < 0 {
			return fmt.Errorf("gatewayapi: clusterProvider.scaleHold %q must not be negative", raw)
		}
	}
	if cp.ScaleMaxNodes != 0 {
		if cp.ScaleMaxNodes < 0 {
			return fmt.Errorf("gatewayapi: clusterProvider.scaleMaxNodes %d must not be negative", cp.ScaleMaxNodes)
		}
		return errors.New("gatewayapi: clusterProvider.scaleMaxNodes belongs to the clusterprovider host process; the GatewayAPI component cannot honor it")
	}
	if cp.ScalePodsPerNode != 0 {
		if cp.ScalePodsPerNode < 0 {
			return fmt.Errorf("gatewayapi: clusterProvider.scalePodsPerNode %d must not be negative", cp.ScalePodsPerNode)
		}
		return errors.New("gatewayapi: clusterProvider.scalePodsPerNode belongs to the clusterprovider host process; the GatewayAPI component cannot honor it")
	}
	return nil
}

func nodeMarketFrom(spec *v1alpha1.GatewayNodeMarket) (NodeMarketConfig, error) {
	args, err := parseNodePolicyArgs(spec.NodePolicyArgs)
	if err != nil {
		return NodeMarketConfig{}, fmt.Errorf("gatewayapi: nodeMarket.nodePolicyArgs: %w", err)
	}
	out := NodeMarketConfig{
		Enabled:           true,
		OwnerSessionsFile: spec.OwnerSessionsFile,
		UserDataBaseFile:  spec.UserDataBaseFile,
		ContractWorkspace: spec.ContractWorkspace,
		PolicyName:        spec.NodePolicy,
		PolicyArgs:        args,
		ExtraBidderDIDs:   append([]string(nil), spec.ExtraBidderDIDs...),
	}
	if spec.HomeWorkspace != "" {
		out.HomeWorkspaces = []string{spec.HomeWorkspace}
	}
	if spec.PollIntervalSeconds > 0 {
		out.PollInterval = time.Duration(spec.PollIntervalSeconds) * time.Second
	}
	return out, nil
}

func parseNodePolicyArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if value == nil {
		return nil, nil
	}
	args, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("must be a JSON object")
	}
	return args, nil
}

func gatewayAudiences(audience string, audiences []string, identity *v1alpha1.ComponentIdentity) []string {
	out := []string{}
	if trimmed := strings.TrimSpace(audience); trimmed != "" {
		out = append(out, trimmed)
	}
	for _, value := range audiences {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 && identity != nil {
		if did := strings.TrimSpace(identity.Did); did != "" {
			out = append(out, did)
		}
	}
	return uniqueStrings(out)
}

func gatewayHostAudiences(hostnames []string, tls *v1alpha1.ComponentTLS, publicOrigin string, network *v1alpha1.MarketNetworkValue) []string {
	candidates := append([]string(nil), hostnames...)
	if tls != nil {
		candidates = append(candidates, tls.Hostnames...)
	}
	if publicOrigin != "" {
		candidates = append(candidates, publicOrigin)
	}
	if network != nil {
		candidates = append(candidates, network.IngressProxyHost)
	}
	out := []string{}
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		host := raw
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			host = parsed.Host
		}
		hosts := []string{host, strings.ToLower(host)}
		if name, _, err := net.SplitHostPort(host); err == nil && name != "" {
			hosts = append(hosts, name, strings.ToLower(name))
		}
		for _, value := range hosts {
			if value == "" {
				continue
			}
			out = append(out, "did:web:"+value)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func publicOriginFor(hostnames []string, tls *v1alpha1.ComponentTLS) string {
	if tls == nil || !tls.Enabled {
		return ""
	}
	candidates := hostnames
	if len(tls.Hostnames) > 0 {
		candidates = tls.Hostnames
	}
	for _, hostname := range candidates {
		hostname = strings.TrimSpace(hostname)
		hostname = strings.TrimPrefix(hostname, "https://")
		hostname = strings.TrimPrefix(hostname, "http://")
		hostname = strings.TrimRight(hostname, "/")
		if hostname == "" {
			continue
		}
		return "https://" + hostname
	}
	return ""
}

func refFromComponent(ref *commonv1.ObjectRef) Ref {
	if ref == nil {
		return Ref{}
	}
	return Ref{Kind: ref.Kind, Name: ref.Name}
}

func Factory(ctx context.Context, deps component.Deps, spec component.ComponentSpec) (component.Service, error) {
	cfg, err := ConfigFrom(deps, spec)
	if err != nil {
		return nil, err
	}
	return New(cfg), nil
}

func Register(runtime *component.Runtime) error {
	return runtime.Register(Kind, Factory)
}
