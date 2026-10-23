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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/xrpcserver"
)

type Service struct {
	cfg Config

	mu sync.Mutex

	log *slog.Logger

	oauth *gatewayhttp.OAuth

	surface *xrpcserver.Server

	mapper *gatewayhttp.NodeClaimMapper

	stopCh chan struct{}

	stopOnce *sync.Once

	ready bool
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg}
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	cfg := s.cfg.withDefaults()
	cfg.Deps = cfg.Deps.Or(deps)
	if err := cfg.Check(); err != nil {
		return err
	}
	if cfg.Log != nil {
		cfg.Deps.Log = cfg.Log
	}
	log := cfg.Deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = cfg.Deps.StateDir
	}
	if stateDir == "" {
		return errors.New("gatewayapi: no state dir")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("gatewayapi: creating the state dir: %w", err)
	}

	vault, err := resolveVault(ctx, cfg, cfg.Deps)
	if err != nil {
		return err
	}
	defer vault.close()
	signIn := cfg.Session.SignIn
	if signIn == nil {
		if vault == nil {
			return errors.New("gatewayapi: no sign-in backend")
		}
		endpoint := vault.endpoint
		if cfg.Session.Timeout > 0 {
			endpoint.Timeout = cfg.Session.Timeout
		}
		custody, err := newCustodySignIn(custodySignInOptions{
			Endpoint:       endpoint,
			Role:           cfg.Session.AtprotoRole,
			RenewBelow:     cfg.Session.RenewBelow,
			RenewIncrement: cfg.Session.RenewIncrement,
			Network:        cfg.Network,
			TLSConfig:      cfg.Deps.TLS,
			Log:            log,
		})
		if err != nil {
			return err
		}
		defer custody.Close()
		signIn = custody
	}

	pinned, err := loadDownloads(cfg)
	if err != nil {
		return err
	}
	handler, oauth, surface, err := s.buildHandler(cfg, log, signIn, pinned)
	if err != nil {
		return err
	}
	mapper, err := buildMapper(cfg, cfg.Deps, vault, pinned, log)
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
	if cfg.Deps.Handlers == nil {
		s.mu.Unlock()
		if surface != nil {
			surface.Close()
		}
		return errors.New("gatewayapi: the handler registry is not available")
	}
	key := component.Key{Workspace: cfg.Workspace, Kind: Kind, Name: cfg.ComponentName}
	if err := cfg.Deps.Handlers.RegisterHTTP(key, handler); err != nil {
		s.mu.Unlock()
		if surface != nil {
			surface.Close()
		}
		return err
	}
	s.cfg = cfg
	s.log = log
	s.oauth = oauth
	s.surface = surface
	s.mapper = mapper
	s.stopCh = stopCh
	s.stopOnce = &sync.Once{}
	s.ready = true
	s.mu.Unlock()

	if mapper != nil {
		mapper.Start(ctx)
	}
	log.Info("gatewayapi_registered", "handler", kcp.HandlerURL(cfg.Workspace, Kind, cfg.ComponentName), "network", networkName(cfg.Network))

	select {
	case <-ctx.Done():
	case <-stopCh:
	}

	s.shutdown(ctx)
	s.finish()
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		return errors.New("gatewayapi: the service is not registered")
	}
	return nil
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

func (s *Service) buildHandler(cfg Config, log *slog.Logger, signIn gatewayhttp.WebSignIn, pinned *gatewayhttp.PinnedDownloads) (http.Handler, *gatewayhttp.OAuth, *xrpcserver.Server, error) {
	publicOrigin := cfg.Session.PublicOrigin
	oauth, err := gatewayhttp.NewOAuth(gatewayhttp.OAuthOptions{
		SignIn:              signIn,
		PublicOrigin:        publicOrigin,
		PostSignInURL:       cfg.Session.PostSignInURL,
		CookieName:          cfg.Session.CookieName,
		CookieMaxAge:        cfg.Session.CookieMaxAge,
		ClientMetadataPath:  cfg.Session.ClientMetadataPath,
		ClientMetadataCache: cfg.Session.ClientMetadataCache,
		MaxBodyBytes:        cfg.Session.MaxBodyBytes,
		OnSignedIn:          cfg.Session.OnSignedIn,
		OnSignedOut:         cfg.Session.OnSignedOut,
		Log:                 log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	clusterGateway, err := clusterGatewayFor(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	cluster, err := gatewayhttp.NewClusterAPI(gatewayhttp.ClusterOptions{
		Gateway: clusterGateway,
		Caller: func(ctx context.Context, r *http.Request) (gatewayhttp.ClusterCaller, error) {
			return oauth.RequestCaller(r)
		},
		MaxKubeconfigTTLSec: cfg.Gateway.MaxKubeconfigTTLSec,
		KCPPublicHost:       cfg.Gateway.KCPPublicHost,
		MaxBodyBytes:        cfg.Gateway.MaxBodyBytes,
		Log:                 log,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	downloads := gatewayhttp.NewDownloads(gatewayhttp.DownloadsOptions{Downloads: pinned, Log: log})

	directory := cfg.Gateway.Directory
	if directory == nil {
		directory = directoryFor(plcURL(cfg.Network))
	}
	surface, err := xrpcserver.New(xrpcserver.Options{
		Gateway:   cfg.Gateway.Server,
		Audiences: cfg.Gateway.audiences(),
		Directory: directory,
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

func loadDownloads(cfg Config) (*gatewayhttp.PinnedDownloads, error) {
	if cfg.DownloadsDir == "" {
		return nil, nil
	}
	pinned, err := gatewayhttp.LoadPinnedDownloads(cfg.DownloadsDir)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the downloads dir: %w", err)
	}
	if len(cfg.RequiredDownloads) > 0 {
		have := map[string]bool{}
		for _, download := range pinned.List() {
			have[download.Name] = true
		}
		missing := []string{}
		for _, name := range cfg.RequiredDownloads {
			if !have[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("gatewayapi: required pinned downloads are missing from %s: %s", cfg.DownloadsDir, strings.Join(missing, ", "))
		}
	}
	return pinned, nil
}

func buildMapper(cfg Config, deps component.Deps, vault *vaultRuntime, pinned *gatewayhttp.PinnedDownloads, log *slog.Logger) (*gatewayhttp.NodeClaimMapper, error) {
	if !cfg.NodeMarket.Enabled {
		return nil, nil
	}
	if vault == nil || vault.client == nil {
		return nil, errors.New("gatewayapi: the node market needs a resolved vault client")
	}
	if vault.client.Token() == "" {
		return nil, errors.New("gatewayapi: the node market needs a vault token or a client cert login")
	}
	homes := append([]string(nil), cfg.NodeMarket.HomeWorkspaces...)
	if len(homes) == 0 {
		homes = []string{cfg.Workspace}
	}
	store, err := kcpStoreFor(deps, homes[0])
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the node market store: %w", err)
	}
	indexWorkspace, indexNamespace := homes[0], "default"
	if cfg.Workspace != "" {
		indexWorkspace, indexNamespace = cfg.Workspace+":app", "default"
	}
	if ref, err := kcpclient.ParseServiceAccountRef(os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT")); err == nil && ref != nil {
		indexWorkspace, indexNamespace = ref.Workspace, ref.Namespace
	} else if os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT") != "" {
		log.Warn("gatewayapi: the node claim agent ref did not parse", "ref", os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT"), "err", err)
	}
	log.Info("gatewayapi node market", "homes", homes, "indexWorkspace", indexWorkspace, "indexNamespace", indexNamespace,
		"contractWorkspace", cfg.NodeMarket.ContractWorkspace)
	mount := cfg.NodeMarket.VaultMount
	if mount == "" {
		mount = vault.mount
	}
	if mount == "" {
		return nil, errors.New("gatewayapi: the node market needs a vault mount")
	}
	var policy *gatewayhttp.NodePolicy
	if cfg.NodeMarket.PolicyName != "" {
		args := map[string]any{}
		maps.Copy(args, cfg.NodeMarket.PolicyArgs)
		policy = &gatewayhttp.NodePolicy{Name: cfg.NodeMarket.PolicyName, Args: args}
	}
	var userData string
	if cfg.NodeMarket.UserDataBaseFile != "" {
		raw, err := os.ReadFile(cfg.NodeMarket.UserDataBaseFile)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: reading the node user data base: %w", err)
		}
		userData = string(raw)
	}
	caFileOf := gatewayhttp.DefaultOpenBaoCAFile
	if cfg.NodeMarket.ClusterCADir != "" {
		caFileOf = gatewayhttp.PublishedOpenBaoCAFile(cfg.NodeMarket.ClusterCADir)
	}
	var downloads []gatewayhttp.PinnedDownload
	if pinned != nil {
		downloads = pinned.List()
	}
	var mesh *gatewayhttp.MeshdPin
	var meshBase func(context.Context) (string, bool)
	var nodeAgent *gatewayhttp.MeshdPin
	var nodeAgentBase func(context.Context) (string, bool)
	if cfg.Network != nil {
		meshPins := gatewayhttp.MeshdPinsOf(downloads)
		agentPins := gatewayhttp.NodeAgentPinsOf(downloads)
		if meshPins != nil {
			mesh = &gatewayhttp.MeshdPin{Sha256: meshPins}
		}
		if agentPins != nil {
			nodeAgent = &gatewayhttp.MeshdPin{Sha256: agentPins}
		}
		if mesh != nil || nodeAgent != nil {
			var baseMu sync.Mutex
			base := ""
			baseOf := func(ctx context.Context) (string, bool) {
				baseMu.Lock()
				cached := base
				baseMu.Unlock()
				if cached != "" {
					return cached, true
				}
				resolved := cfg.downloadBase()
				if resolved == "" {
					resolved = cfg.localDownloadBase(ctx, deps)
				}
				if resolved == "" {
					return "", false
				}
				baseMu.Lock()
				base = resolved
				baseMu.Unlock()
				return resolved, true
			}
			meshBase = baseOf
			nodeAgentBase = baseOf
		}
	} else {
		mesh = gatewayhttp.MeshdPinOf(downloads, cfg.downloadBase())
		nodeAgent = gatewayhttp.NodeAgentPinOf(downloads, cfg.downloadBase())
	}
	return gatewayhttp.NewNodeClaimMapper(gatewayhttp.NodeClaimMapperOptions{
		Store:                 newClaimStore(store, newHomeIndex(store, indexWorkspace, indexNamespace, homes)),
		Links:                 newClusterLinkReader(store),
		Contracts:             newContractClient(store, cfg.NodeMarket.ContractWorkspace, cfg.NodeMarket.ContractQuota),
		Vault:                 gatewayhttp.OpenBaoVault{Client: vault.client},
		VaultMount:            mount,
		VMReadyTimeoutSec:     cfg.NodeMarket.VMReadyTimeoutSec,
		Policy:                policy,
		ExtraBidderDIDs:       append([]string(nil), cfg.NodeMarket.ExtraBidderDIDs...),
		UserDataBase:          userData,
		PollInterval:          cfg.NodeMarket.PollInterval,
		OpenBaoCAFileOf:       caFileOf,
		Meshd:                 mesh,
		MeshdDownloadBase:     meshBase,
		NodeAgentPin:          nodeAgent,
		NodeAgentDownloadBase: nodeAgentBase,
		Log:                   log,
	})
}

func clusterGatewayFor(cfg Config) (gatewayhttp.ClusterGateway, error) {
	if cfg.Gateway.URL != "" || cfg.Gateway.Ref.Name != "" {
		if cfg.Gateway.Audience == "" {
			return nil, errors.New("gatewayapi: a remote gateway needs Gateway.Audience for service auth")
		}
		resolver := &gatewayURLResolver{cfg: cfg.Gateway}
		client := cfg.Gateway.HTTPClient
		if client == nil && cfg.Deps.Handlers != nil {
			client = cfg.Deps.Handlers.HTTPClient()
		}
		return &gatewayhttp.XRPCClusterGateway{
			BaseURL:  resolver.baseURL,
			Audience: cfg.Gateway.Audience,
			Client:   client,
			Timeout:  cfg.Gateway.Timeout,
		}, nil
	}
	return gatewayhttp.DirectClusterGateway{Server: cfg.Gateway.Server}, nil
}

type gatewayURLResolver struct {
	cfg GatewayConfig
}

func (r *gatewayURLResolver) baseURL(ctx context.Context) (string, error) {
	if r.cfg.URL != "" {
		return r.cfg.URL, nil
	}
	raw := kcp.HandlerURL(r.cfg.Ref.Workspace, r.cfg.Ref.Kind, r.cfg.Ref.Name)
	if raw == "" {
		return "", fmt.Errorf("gatewayapi: the gateway ref %s/%s names no component", r.cfg.Ref.Kind, r.cfg.Ref.Name)
	}
	return raw, nil
}

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	mapper := s.mapper
	timeout := s.cfg.ShutdownTimeout
	s.mu.Unlock()
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if mapper != nil {
		if err := mapper.Stop(ctx); err != nil {
			s.log.Warn("gatewayapi_node_mapper_stop_failed", "err", err)
		}
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
	s.mapper = nil
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
