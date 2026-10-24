package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	contractnodeclaim "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/custody"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/xrpcserver"
)

type Service struct {
	spec v1alpha1.GatewayAPISpec

	deps Deps

	log *slog.Logger

	workspace string

	stateDir string

	publicOrigin string

	audiences []string

	policyArgs map[string]any

	homeWorkspaces []string

	contractWorkspace string

	server *gateway.Server

	mu sync.Mutex

	oauth *gatewayhttp.OAuth

	surface *xrpcserver.Server

	nodeClaims *contractnodeclaim.Binding

	stopCh chan struct{}

	stopOnce *sync.Once

	ready bool

	readiness component.ReadySignal
}

func (s *Service) Start(ctx context.Context) error {
	log := s.log
	if s.stateDir == "" {
		return errors.New("gatewayapi: no state dir")
	}
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return fmt.Errorf("gatewayapi: creating the state dir: %w", err)
	}

	vault, err := s.resolveVault(ctx)
	if err != nil {
		return err
	}
	if vault == nil {
		return errors.New("gatewayapi: no sign-in backend")
	}
	signIn, err := custody.New(ctx, custody.Config{
		Endpoint:       vault.endpoint,
		Role:           DefaultAtprotoRole,
		RenewBelow:     DefaultRenewBelow,
		RenewIncrement: DefaultRenewIncrement,
		Network:        s.spec.Network,
	}, custody.Deps{TLS: s.deps.TLS, Log: log})
	if err != nil {
		return err
	}
	defer signIn.Close()
	tasks := supervise.New(ctx)
	defer func() { _ = tasks.Stop() }()

	pinned, err := s.loadDownloads()
	if err != nil {
		return err
	}
	handler, oauth, surface, err := s.buildHandler(ctx, signIn, pinned, tasks)
	if err != nil {
		return err
	}
	nodeClaims, err := s.buildNodeClaims(vault, pinned)
	if err != nil {
		if surface != nil {
			surface.Close()
		}
		return err
	}

	stopCh := make(chan struct{})

	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		if surface != nil {
			surface.Close()
		}
		return errors.New("gatewayapi: the service already started")
	}
	if s.deps.Bus == nil {
		s.mu.Unlock()
		if surface != nil {
			surface.Close()
		}
		return errors.New("gatewayapi: the handler registry is not available")
	}
	key := component.Key{Workspace: s.workspace, Kind: Kind, Name: s.deps.Self.Name}
	if err := s.deps.Bus.RegisterHTTP(key, handler); err != nil {
		s.mu.Unlock()
		if surface != nil {
			surface.Close()
		}
		return err
	}
	s.oauth = oauth
	s.surface = surface
	s.nodeClaims = nodeClaims
	s.stopCh = stopCh
	s.stopOnce = &sync.Once{}
	s.ready = true
	s.mu.Unlock()

	if nodeClaims != nil {
		s.deps.NodeClaims.Bind(*nodeClaims)
	}
	log.Info("gatewayapi_registered", "handler", kcp.HandlerURL(s.workspace, Kind, s.deps.Self.Name), "network", networkName(s.spec.Network))
	s.readiness.Signal()

	select {
	case <-ctx.Done():
	case <-stopCh:
	}

	s.shutdown(ctx)
	s.finish()
	return nil
}

func (s *Service) Ready() component.Readiness {
	return &s.readiness
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return nil
	}
	return []component.Endpoint{
		{Name: "http", Protocol: component.ProtocolHTTP},
	}
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	stopCh := s.stopCh
	stopOnce := s.stopOnce
	s.mu.Unlock()
	if stopCh == nil || stopOnce == nil {
		return nil
	}
	stopOnce.Do(func() { close(stopCh) })
	s.shutdown(ctx)
	return nil
}

func (s *Service) OAuth() *gatewayhttp.OAuth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.oauth
}

func (s *Service) buildHandler(ctx context.Context, signIn gatewayhttp.WebSignIn, pinned *pinneddownloads.Set, tasks *supervise.Group) (http.Handler, *gatewayhttp.OAuth, *xrpcserver.Server, error) {
	log := s.log
	oauth, err := gatewayhttp.NewOAuth(gatewayhttp.OAuthOptions{
		SignIn:        signIn,
		PublicOrigin:  s.publicOrigin,
		PostSignInURL: s.spec.PostSignInURL,
		Tasks:         tasks,
		Log:           log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	cluster, err := gatewayhttp.NewClusterAPI(gatewayhttp.ClusterOptions{
		Gateway: gatewayhttp.DirectClusterGateway{Server: s.server},
		Caller: func(ctx context.Context, r *http.Request) (gatewayhttp.ClusterCaller, error) {
			return oauth.RequestCaller(r)
		},
		Log: log,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	downloads := gatewayhttp.NewDownloads(gatewayhttp.DownloadsOptions{Downloads: pinned, Log: log})

	surface, err := xrpcserver.New(ctx, xrpcserver.Options{
		Gateway:   s.server,
		Audiences: append([]string(nil), s.audiences...),
		Directory: directoryFor(plcURL(s.spec.Network)),
	})
	if err != nil {
		return nil, nil, nil, err
	}

	mux := http.NewServeMux()
	cluster.Register(mux)
	oauth.Register(mux)
	downloads.Register(mux)
	xrpcHandler := surface.Handler()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, xrpcserver.XRPCPrefix) {
			xrpcHandler.ServeHTTP(w, r)
			return
		}
		writeAPINotFound(w)
	}))
	return securityHeaders(mux), oauth, surface, nil
}

func (s *Service) loadDownloads() (*pinneddownloads.Set, error) {
	dir := s.downloadsDir()
	if dir == "" {
		return nil, nil
	}
	pinned, err := pinneddownloads.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the downloads dir: %w", err)
	}
	if missing := pinned.Missing(s.spec.RequiredDownloads); len(missing) > 0 {
		return nil, fmt.Errorf("gatewayapi: required pinned downloads are missing from %s: %s", dir, strings.Join(missing, ", "))
	}
	return pinned, nil
}

func (s *Service) buildNodeClaims(vault *vaultRuntime, pinned *pinneddownloads.Set) (*contractnodeclaim.Binding, error) {
	nodeMarket := s.spec.NodeMarket
	if nodeMarket == nil {
		return nil, nil
	}
	log := s.log
	if s.deps.NodeClaims == nil {
		return nil, errors.New("gatewayapi: the node market needs the NodeClaim controller")
	}
	if vault == nil || vault.mount == "" {
		return nil, errors.New("gatewayapi: the node market needs a vault mount")
	}
	homes := append([]string(nil), s.homeWorkspaces...)
	indexWorkspace, indexNamespace := s.workspace+":"+contractnodeclaim.AgentWorkspace, contractnodeclaim.AgentNamespace
	if ref, err := kcpclient.ParseServiceAccountRef(os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT")); err == nil && ref != nil {
		indexWorkspace, indexNamespace = ref.Workspace, ref.Namespace
	} else if os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT") != "" {
		log.Warn("gatewayapi: the node claim agent ref did not parse", "ref", os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT"), "err", err)
	}
	log.Info("gatewayapi node market", "homes", homes, "indexWorkspace", indexWorkspace, "indexNamespace", indexNamespace,
		"contractWorkspace", s.contractWorkspace)
	cfg := nodeclaim.Config{VaultMount: vault.mount, VMReadyTimeoutSec: DefaultVMReadySec}
	if nodeMarket.NodePolicy != "" {
		args := map[string]any{}
		maps.Copy(args, s.policyArgs)
		cfg.Policy = &nodeclaim.Policy{Name: nodeMarket.NodePolicy, Args: args}
	}
	if nodeMarket.UserDataBaseFile != "" {
		raw, err := os.ReadFile(nodeMarket.UserDataBaseFile)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: reading the node user data base: %w", err)
		}
		cfg.UserDataBase = string(raw)
	}
	deps := nodeclaim.Deps{CAFileOf: nodeclaim.DefaultOpenBaoCAFile, Log: log}
	if dir := s.clusterCADir(); dir != "" {
		deps.CAFileOf = nodeclaim.PublishedOpenBaoCAFile(dir)
	}
	var downloads []pinneddownloads.PinnedDownload
	if pinned != nil {
		downloads = pinned.List()
	}
	meshPins, agentPins := gatewayhttp.MeshdPinsOf(downloads), gatewayhttp.NodeAgentPinsOf(downloads)
	if s.spec.Network != nil {
		if meshPins != nil {
			cfg.Meshd = &nodeclaim.Pin{Sha256: meshPins}
		}
		if agentPins != nil {
			cfg.NodeAgent = &nodeclaim.Pin{Sha256: agentPins}
		}
		if cfg.Meshd != nil || cfg.NodeAgent != nil {
			baseOf := s.cachedDownloadBase()
			deps.MeshdBase, deps.NodeAgentBase = baseOf, baseOf
		}
	} else if base := s.downloadBase(); base != "" {
		if meshPins != nil {
			cfg.Meshd = &nodeclaim.Pin{DownloadBase: base, Sha256: meshPins}
		}
		if agentPins != nil {
			cfg.NodeAgent = &nodeclaim.Pin{DownloadBase: base, Sha256: agentPins}
		}
	}
	mapper, err := nodeclaim.New(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: %w", err)
	}
	return &contractnodeclaim.Binding{Owner: s.selfRef(), Mapper: mapper, Vault: s.vaultRef(), ContractWorkspace: s.contractWorkspace,
		IndexWorkspace: indexWorkspace, IndexNamespace: indexNamespace, Homes: homes, Workspace: s.workspace, Record: nodeMarket.InfraRecord.DeepCopy()}, nil
}

func (s *Service) cachedDownloadBase() func(context.Context) (string, bool) {
	var mu sync.Mutex
	base := ""
	return func(ctx context.Context) (string, bool) {
		mu.Lock()
		cached := base
		mu.Unlock()
		if cached != "" {
			return cached, true
		}
		resolved := strings.TrimRight(strings.TrimSpace(s.spec.PublicOrigin), "/")
		if resolved == "" {
			resolved = s.localDownloadBase(ctx)
		}
		if resolved == "" {
			return "", false
		}
		mu.Lock()
		base = resolved
		mu.Unlock()
		return resolved, true
	}
}

func (s *Service) selfRef() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace}
}

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	nodeClaims := s.nodeClaims
	s.mu.Unlock()
	if nodeClaims != nil {
		s.deps.NodeClaims.Unbind(nodeClaims.Owner)
	}
	s.mu.Lock()
	surface := s.surface
	s.mu.Unlock()
	if surface != nil {
		surface.Close()
	}
}

func (s *Service) finish() {
	s.mu.Lock()
	s.ready = false
	s.stopCh = nil
	s.stopOnce = nil
	s.nodeClaims = nil
	s.surface = nil
	s.mu.Unlock()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/session" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeAPINotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"NotFound","message":"no such API route"}` + "\n"))
}

func networkName(network *v1alpha1.MarketNetworkValue) string {
	if network == nil {
		return ""
	}
	return network.PLCDirectoryURL
}

func plcURL(network *v1alpha1.MarketNetworkValue) string {
	if network == nil || network.PLCDirectoryURL == "" {
		return identity.DefaultPLCURL
	}
	return strings.TrimRight(network.PLCDirectoryURL, "/")
}

func directoryFor(raw string) identity.Directory {
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
		inner = &identity.BaseDirectory{
			PLCURL:                 raw,
			PLCLimiter:             directory.PLCLimiter,
			DIDWebLimitFunc:        directory.DIDWebLimitFunc,
			HTTPClient:             directory.HTTPClient,
			PLCClient:              directory.PLCClient,
			TryAuthoritativeDNS:    directory.TryAuthoritativeDNS,
			SkipDNSDomainSuffixes:  directory.SkipDNSDomainSuffixes,
			FallbackDNSServers:     directory.FallbackDNSServers,
			SkipHandleVerification: directory.SkipHandleVerification,
			UserAgent:              directory.UserAgent,
		}
	}
	return identity.NewCacheDirectory(inner, DefaultDirectoryKeys, DefaultDirectoryTTL, DefaultDirectoryTTL, DefaultDirectoryTTL)
}
