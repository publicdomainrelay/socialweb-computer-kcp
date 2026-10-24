package gatewaymain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/plcresolver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/plcsweep"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sweepkcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tenantapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/xrpcserver"
)

const directoryCacheCapacity = 10_000

func Main(ctx context.Context, inv foldmain.Invocation) int {
	if err := run(ctx, inv); err != nil {
		fmt.Fprintln(inv.Stderr, "socialweb-gateway:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, inv foldmain.Invocation) error {
	cfg, err := load(inv.Env)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(inv.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	deps, err := buildDeps(cfg)
	if err != nil {
		return err
	}
	g, err := Build(ctx, cfg, deps, log)
	if err != nil {
		return err
	}

	log.Info("serving",
		"addr", cfg.Addr,
		"kcp", cfg.KCPHost,
		"kcpKubeconfig", cfg.KCPKubeconfig,
		"audiences", cfg.Audiences,
		"xrpc", xrpcserver.XRPCPrefix,
	)
	return serve(ctx, cfg, g, log, inv)
}

func buildDeps(cfg Config) (Deps, error) {
	dir, err := directory(cfg)
	if err != nil {
		return Deps{}, err
	}

	kcpRestConfig, err := clientcmd.BuildConfigFromFlags("", cfg.KCPKubeconfig)
	if err != nil {
		return Deps{}, fmt.Errorf("reading %s: %w", envKCPKubeconfig, err)
	}
	kcpserver.FollowTokenFile(kcpRestConfig)
	owners := ownerCredentials(cfg.ClusterStateDir)
	tenantAPI, err := tenantapply.New(owners)
	if err != nil {
		return Deps{}, err
	}

	certs := &clientcert.Issuer{StateDir: cfg.ClusterStateDir, CAFile: cfg.KCPPublicCAFile, Logins: openbao.NewLogins(nil), Log: slog.Default()}

	client, err := kcpclient.New(kcpclient.Options{
		Host:           cfg.KCPHost,
		PublicHost:     cfg.KCPPublicHost,
		RestConfig:     kcpRestConfig,
		Kubeconfigs:    certs,
		Tenant:         kcpclient.TenantConfig(owners),
		NodeClaimAgent: cfg.NodeClaimAgent,
		NodeClaimQuota: cfg.NodeClaimQuota,

		HostProcessWorkspace: cfg.HostProcessWorkspace,
	})
	if err != nil {
		return Deps{}, err
	}

	certs.Recorder = client

	worklist, err := sweepkcp.NewWorklist(sweepkcp.Options{Host: cfg.KCPHost, RestConfig: kcpRestConfig})
	if err != nil {
		return Deps{}, fmt.Errorf("the sweep's worklist: %w", err)
	}
	resolver, err := plcresolver.New(plcresolver.Options{
		URL:    cfg.PLCURL,
		Client: &http.Client{Timeout: cfg.PLCTimeout},
	})
	if err != nil {
		return Deps{}, fmt.Errorf("the sweep's resolver: %w", err)
	}

	revoker := &sweepkcp.Revoker{Deny: client, FanOut: client, Erase: client, State: client}

	return Deps{
		Store:     client,
		Deny:      client,
		Feed:      client,
		Directory: dir,

		Revoker: client,

		Certs: certs,

		Tenant: tenantAPI,

		Sweep: &plcsweep.Sweeper{
			Worklist: worklist,
			Resolver: resolver,
			Revoker:  revoker,
		},
	}, nil
}

const ListenerName = "xrpc"

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func directory(cfg Config) (identity.Directory, error) {
	hardened := identity.DefaultDirectory()
	cached, ok := hardened.(*identity.CacheDirectory)
	if !ok {
		return nil, fmt.Errorf(
			"indigo's identity.DefaultDirectory() is a %T, not a *identity.CacheDirectory, so its "+
				"revalidation window cannot be set", hardened)
	}
	base, ok := cached.Inner.(*identity.BaseDirectory)
	if !ok {
		return nil, fmt.Errorf(
			"indigo's identity.DefaultDirectory() wraps a %T, not a *identity.BaseDirectory, so the "+
				"PLC, did:web and handle HTTP clients cannot be bounded", cached.Inner)
	}
	timeout := cfg.PLCTimeout
	timeout = opt.Positive(timeout, DefaultPLCTimeout)
	if cfg.PLCURL != "" {
		base.PLCURL = strings.TrimSuffix(cfg.PLCURL, "/")
	}
	base.PLCClient = &http.Client{Timeout: timeout}
	if loopbackURL(base.PLCURL) {
		base.HTTPClient = http.Client{Timeout: timeout}
	} else {
		base.HTTPClient.Timeout = timeout
	}
	return identity.NewCacheDirectory(
		base,
		directoryCacheCapacity,
		cfg.DirectoryCacheTTL,
		cfg.DirectoryCacheTTL,
		cfg.DirectoryCacheTTL,
	), nil
}

func serve(ctx context.Context, cfg Config, g *Gateway, log *slog.Logger, inv foldmain.Invocation) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Handler:           g.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String())
	inv.Listened(ListenerName, ln.Addr())

	var background sync.WaitGroup
	background.Go(func() {
		if err := g.Run(ctx); err != nil {
			log.Error("the background work stopped", "err", err)
		}
	})

	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()

	var serveErr error
	select {
	case serveErr = <-served:
		stop()
	case <-ctx.Done():
		log.Info("signal received, draining")
	}

	g.Close()
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	drainErr := server.Shutdown(drainCtx)
	if serveErr == nil {
		serveErr = <-served
	}
	if drainErr != nil {
		return fmt.Errorf("draining after %s: %w", cfg.ShutdownTimeout, drainErr)
	}
	background.Wait()

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
