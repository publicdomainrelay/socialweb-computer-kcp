package gatewayapi

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/utils/ptr"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const Kind = "GatewayAPI"

const (
	DefaultShutdownTimeout = 30 * time.Second
	DefaultDirectoryTTL    = 60 * time.Second
	DefaultDirectoryKeys   = 10_000
	DefaultVMReadySec      = 120
	DefaultAtprotoMount    = "atproto"
	DefaultAtprotoRole     = "user"
	DefaultCustodyTimeout  = 15 * time.Second
	DefaultVaultClient     = "socialweb-computer-k8s"
	DefaultRenewBelow      = 30 * time.Minute
	DefaultRenewIncrement  = time.Hour
)

type Deps struct {
	component.Instance

	KCP component.KCPClients

	TLS *tls.Config

	NodeClaimAgent *kcpclient.ServiceAccountRef
}

func New(spec v1alpha1.GatewayAPISpec, deps Deps) (*Service, error) {
	s := &Service{spec: spec, deps: deps}
	s.log = deps.Log
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.workspace = cmp.Or(deps.Self.Workspace, "root")
	s.stateDir = deps.StateDir
	if spec.Storage != nil && spec.Storage.DataDir != "" {
		s.stateDir = spec.Storage.DataDir
	}
	s.publicOrigin = cmp.Or(spec.PublicOrigin, publicOriginFor(spec.Hostnames, spec.TLS))
	var serviceAudiences []string
	if spec.ServiceAuth != nil {
		serviceAudiences = spec.ServiceAuth.Audience
	}
	s.audiences = uniqueStrings(append(gatewayAudiences(spec.Audience, serviceAudiences, spec.Identity),
		gatewayHostAudiences(spec.Hostnames, spec.TLS, spec.PublicOrigin, spec.Network)...))
	if nm := spec.NodeMarket; nm != nil {
		args, err := parseNodePolicyArgs(nm.NodePolicyArgs)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: nodeMarket.nodePolicyArgs: %w", err)
		}
		s.policyArgs = args
		s.homeWorkspaces = []string{s.workspace}
		if nm.HomeWorkspace != "" {
			s.homeWorkspaces = []string{nm.HomeWorkspace}
		}
		s.contractWorkspace = cmp.Or(nm.ContractWorkspace, s.workspace)
		s.pollInterval = time.Duration(nm.PollIntervalSeconds) * time.Second
	}
	if poll := ptr.Deref(spec.NodePollIntervalSeconds, 0); poll > 0 {
		s.pollInterval = time.Duration(poll) * time.Second
	}
	if spec.ClusterProvider != nil {
		if err := checkClusterProvider(*spec.ClusterProvider); err != nil {
			return nil, err
		}
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	caFile := cmp.Or(spec.KubeconfigCAFile, os.Getenv("SOCIALWEB_GATEWAY_KCP_PUBLIC_CA_FILE"))
	server, err := gatewayServerFor(deps.KCP, deps.NodeClaimAgent, s.workspace, kubeconfigIssuer(spec.ClusterStateDir, caFile),
		os.Getenv("SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST"), spec.ClusterStateDir)
	if err != nil {
		return nil, err
	}
	s.server = server
	return s, nil
}

func (s *Service) check() error {
	switch {
	case len(s.audiences) == 0:
		return errors.New("gatewayapi: audience, serviceAuth.audience, identity.did or a hostname is required")
	case !s.vaultConfigured():
		return errors.New("gatewayapi: a vault ref is required for sign-in")
	}
	if s.spec.Network != nil {
		for _, host := range s.spec.Network.Hosts {
			if host.Name == "" || host.Address == "" {
				return errors.New("gatewayapi: every network host needs a name and an address")
			}
		}
	}
	if s.publicOrigin != "" {
		if err := checkOrigin(s.publicOrigin); err != nil {
			return fmt.Errorf("gatewayapi: PublicOrigin: %w", err)
		}
	}
	if len(s.spec.RequiredDownloads) > 0 && s.downloadsDir() == "" {
		return errors.New("gatewayapi: RequiredDownloads needs DownloadsDir")
	}
	if needsMeshd(s.spec.RequiredDownloads) && s.spec.Network == nil && s.downloadBase() == "" {
		return errors.New("gatewayapi: socialweb-meshd is required but no download base is set")
	}
	if s.spec.Serve != nil {
		if err := checkLoopbackHost(s.spec.Serve.Host); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) vaultConfigured() bool {
	return s.spec.VaultRef != nil && s.spec.VaultRef.Name != ""
}

func (s *Service) downloadsDir() string {
	if s.spec.Storage == nil {
		return ""
	}
	return s.spec.Storage.BlobsDir
}

func (s *Service) clusterCADir() string {
	if s.spec.Storage == nil {
		return ""
	}
	return s.spec.Storage.CertsDir
}

func needsMeshd(required []string) bool {
	for _, name := range required {
		if strings.HasPrefix(name, gatewayhttp.MeshdDownloadPrefix) {
			return true
		}
	}
	return false
}

func (s *Service) downloadBase() string {
	return strings.TrimRight(s.publicOrigin, "/")
}

func (s *Service) localDownloadBase(ctx context.Context) string {
	workspace := s.workspace
	store, err := kcpStoreFor(s.deps.KCP, workspace)
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
		targetWorkspace = cmp.Or(targetWorkspace, workspace)
		if target.Kind == Kind && target.Name == s.deps.Self.Name && targetWorkspace == workspace {
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
		if strs.LastSegment(exp.Spec.ServiceRef) != serviceName {
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

func kubeconfigIssuer(clusterStateDir, caFile string) *clientcert.Issuer {
	if clusterStateDir == "" {
		return nil
	}
	return &clientcert.Issuer{StateDir: clusterStateDir, CAFile: caFile}
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
		return errors.New("gatewayapi: clusterProvider.scaleMaxNodes belongs to the clusterprovider host process; the GatewayAPI component cannot honor it")
	}
	if cp.ScalePodsPerNode != 0 {
		return errors.New("gatewayapi: clusterProvider.scalePodsPerNode belongs to the clusterprovider host process; the GatewayAPI component cannot honor it")
	}
	return nil
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
