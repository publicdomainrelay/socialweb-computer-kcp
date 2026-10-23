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
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
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

	pollInterval time.Duration

	server *gateway.Server

	mu sync.Mutex

	oauth *gatewayhttp.OAuth

	surface *xrpcserver.Server

	mapper *gatewayhttp.NodeClaimMapper

	stopCh chan struct{}

	stopOnce *sync.Once

	ready bool
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
	custody, err := newCustodySignIn(custodySignInOptions{
		Endpoint:       vault.endpoint,
		Role:           DefaultAtprotoRole,
		RenewBelow:     DefaultRenewBelow,
		RenewIncrement: DefaultRenewIncrement,
		Network:        s.spec.Network,
		TLSConfig:      s.deps.TLS,
		Log:            log,
	})
	if err != nil {
		return err
	}
	defer custody.Close()

	pinned, err := s.loadDownloads()
	if err != nil {
		return err
	}
	handler, oauth, surface, err := s.buildHandler(custody, pinned)
	if err != nil {
		return err
	}
	mapper, err := s.buildMapper(vault, pinned)
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
	s.mapper = mapper
	s.stopCh = stopCh
	s.stopOnce = &sync.Once{}
	s.ready = true
	s.mu.Unlock()

	if mapper != nil {
		mapper.Start(ctx)
	}
	log.Info("gatewayapi_registered", "handler", kcp.HandlerURL(s.workspace, Kind, s.deps.Self.Name), "network", networkName(s.spec.Network))

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

func (s *Service) buildHandler(signIn gatewayhttp.WebSignIn, pinned *gatewayhttp.PinnedDownloads) (http.Handler, *gatewayhttp.OAuth, *xrpcserver.Server, error) {
	log := s.log
	oauth, err := gatewayhttp.NewOAuth(gatewayhttp.OAuthOptions{
		SignIn:        signIn,
		PublicOrigin:  s.publicOrigin,
		PostSignInURL: s.spec.PostSignInURL,
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

	surface, err := xrpcserver.New(xrpcserver.Options{
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

func (s *Service) loadDownloads() (*gatewayhttp.PinnedDownloads, error) {
	dir := s.downloadsDir()
	if dir == "" {
		return nil, nil
	}
	pinned, err := gatewayhttp.LoadPinnedDownloads(dir)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the downloads dir: %w", err)
	}
	if len(s.spec.RequiredDownloads) > 0 {
		have := map[string]bool{}
		for _, download := range pinned.List() {
			have[download.Name] = true
		}
		missing := []string{}
		for _, name := range s.spec.RequiredDownloads {
			if !have[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("gatewayapi: required pinned downloads are missing from %s: %s", dir, strings.Join(missing, ", "))
		}
	}
	return pinned, nil
}

func (s *Service) buildMapper(vault *vaultRuntime, pinned *gatewayhttp.PinnedDownloads) (*gatewayhttp.NodeClaimMapper, error) {
	nodeMarket := s.spec.NodeMarket
	if nodeMarket == nil {
		return nil, nil
	}
	log := s.log
	if vault == nil || vault.client == nil {
		return nil, errors.New("gatewayapi: the node market needs a resolved vault client")
	}
	if !vault.authed {
		return nil, errors.New("gatewayapi: the node market needs a vault token or a client cert login")
	}
	homes := append([]string(nil), s.homeWorkspaces...)
	store, err := kcpStoreFor(s.deps.KCP, homes[0])
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the node market store: %w", err)
	}
	indexWorkspace, indexNamespace := s.workspace+":app", "default"
	if ref, err := kcpclient.ParseServiceAccountRef(os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT")); err == nil && ref != nil {
		indexWorkspace, indexNamespace = ref.Workspace, ref.Namespace
	} else if os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT") != "" {
		log.Warn("gatewayapi: the node claim agent ref did not parse", "ref", os.Getenv("SOCIALWEB_GATEWAY_NODECLAIM_AGENT"), "err", err)
	}
	log.Info("gatewayapi node market", "homes", homes, "indexWorkspace", indexWorkspace, "indexNamespace", indexNamespace,
		"contractWorkspace", s.contractWorkspace)
	mount := vault.mount
	if mount == "" {
		return nil, errors.New("gatewayapi: the node market needs a vault mount")
	}
	var policy *gatewayhttp.NodePolicy
	if nodeMarket.NodePolicy != "" {
		args := map[string]any{}
		maps.Copy(args, s.policyArgs)
		policy = &gatewayhttp.NodePolicy{Name: nodeMarket.NodePolicy, Args: args}
	}
	var userData string
	if nodeMarket.UserDataBaseFile != "" {
		raw, err := os.ReadFile(nodeMarket.UserDataBaseFile)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: reading the node user data base: %w", err)
		}
		userData = string(raw)
	}
	caFileOf := gatewayhttp.DefaultOpenBaoCAFile
	if dir := s.clusterCADir(); dir != "" {
		caFileOf = gatewayhttp.PublishedOpenBaoCAFile(dir)
	}
	var downloads []gatewayhttp.PinnedDownload
	if pinned != nil {
		downloads = pinned.List()
	}
	var mesh *gatewayhttp.MeshdPin
	var meshBase func(context.Context) (string, bool)
	var nodeAgent *gatewayhttp.MeshdPin
	var nodeAgentBase func(context.Context) (string, bool)
	if s.spec.Network != nil {
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
				resolved := s.downloadBase()
				if resolved == "" {
					resolved = s.localDownloadBase(ctx)
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
		mesh = gatewayhttp.MeshdPinOf(downloads, s.downloadBase())
		nodeAgent = gatewayhttp.NodeAgentPinOf(downloads, s.downloadBase())
	}
	return gatewayhttp.NewNodeClaimMapper(gatewayhttp.NodeClaimMapperOptions{
		Store:                 newClaimStore(store, newHomeIndex(store, indexWorkspace, indexNamespace, homes)),
		Links:                 newClusterLinkReader(store),
		Contracts:             newContractClient(store, s.contractWorkspace, 0),
		Vault:                 gatewayhttp.OpenBaoVault{Client: vault.client},
		VaultMount:            mount,
		VMReadyTimeoutSec:     DefaultVMReadySec,
		Policy:                policy,
		ExtraBidderDIDs:       append([]string(nil), nodeMarket.ExtraBidderDIDs...),
		UserDataBase:          userData,
		PollInterval:          s.pollInterval,
		OpenBaoCAFileOf:       caFileOf,
		Meshd:                 mesh,
		MeshdDownloadBase:     meshBase,
		NodeAgentPin:          nodeAgent,
		NodeAgentDownloadBase: nodeAgentBase,
		Log:                   log,
	})
}

func (s *Service) shutdown(ctx context.Context) {
	s.mu.Lock()
	mapper := s.mapper
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultShutdownTimeout)
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
