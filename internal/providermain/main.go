package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodemarket"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/scaledriver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultcert"
)

func vaultTLSFor() func(string, ...string) (clusterprovider.VaultTLS, error) {
	return func(dataDir string, hosts ...string) (clusterprovider.VaultTLS, error) {
		tls, err := vaultcert.EnsureVaultTLSCertificate(dataDir, hosts...)
		if err != nil {
			return clusterprovider.VaultTLS{}, err
		}
		if err := publishCA(clusterstack.PublicOpenBaoCAPath(dataDir), tls.CAPEM); err != nil {
			return clusterprovider.VaultTLS{}, err
		}
		return clusterprovider.VaultTLS{CertFile: tls.CertFile, KeyFile: tls.KeyFile, CAPEM: tls.CAPEM}, nil
	}
}

func publishCA(path string, pem []byte) error {
	if len(pem) == 0 {
		return nil
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(pem) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

const nodeID = "node1"

func subnetLeasePath(stateDir string) string {
	return filepath.Join(stateDir, "networks.json")
}

func Main(ctx context.Context, inv foldmain.Invocation) int {
	if err := run(ctx, inv); err != nil {
		fmt.Fprintln(inv.Stderr, "socialweb-clusterprovider:", err)
		return 1
	}
	return 0
}

const migrateSubcommand = "migrate"

func run(ctx context.Context, inv foldmain.Invocation) error {
	cfg, err := loadFor(inv.Env, inv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(inv.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(inv.Args) > 0 && inv.Args[0] == migrateSubcommand {
		return migrateOnly(ctx, cfg, log)
	}

	d, err := buildDeps(ctx, cfg, log)
	if err != nil {
		return err
	}
	p, err := Build(cfg, d, log)
	if err != nil {
		return err
	}

	var rolling sync.WaitGroup
	rollCtx, stopRoll := context.WithCancel(ctx)
	if d.Roller != nil {
		rolling.Go(func() { d.Roller.Run(rollCtx) })
	}
	runErr := p.Run(ctx)
	stopRoll()
	rolling.Wait()
	if err := p.Shutdown(ctx); err != nil {
		log.Error("cluster provider: the shutdown reap did not complete", "error", err.Error())
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return nil
		}
		return runErr
	}
	return nil
}

type inputs struct {
	restCfg *rest.Config

	registry clusterprovider.Clusters

	alloc *portallocator.Allocator

	networks *subnetallocator.Allocator
}

func buildInputs(cfg Config) (inputs, error) {
	restCfg, err := clientcmd.BuildConfigFromFlags("", cfg.KCPKubeconfig)
	if err != nil {
		return inputs{}, fmt.Errorf("reading %s: %w", cfg.KCPKubeconfig, err)
	}
	kcpserver.FollowTokenFile(restCfg)
	kcpserver.Throughput(restCfg)
	registry, err := clusterprovider.NewRegistry(clusterprovider.RegistryOptions{
		Host:       cfg.KCPHost,
		StateDir:   cfg.StateDir,
		RestConfig: restCfg,
	})
	if err != nil {
		return inputs{}, err
	}

	alloc, err := portallocator.Open(portallocator.Config{
		LeasePath:    cfg.LeasePath,
		ReclaimGrace: DefaultReclaimGrace,
	})
	if err != nil {
		return inputs{}, fmt.Errorf("opening the lease file %s: %w", cfg.LeasePath, err)
	}

	networks, err := subnetallocator.Open(subnetallocator.Config{
		LeasePath: subnetLeasePath(cfg.StateDir),
	})
	if err != nil {
		return inputs{}, fmt.Errorf("opening the subnet lease file %s: %w. A cluster's range is "+
			"leased for as long as the cluster is, and its first usable address is the gateway its "+
			"guests dial the apiserver through, so a gateway with no subnet lease has no range to "+
			"address a node in and no gateway to advertise; it refuses to start rather than "+
			"falling back to an address of its own, which is how a node's InternalIP became the "+
			"address of the machine running the gateway", subnetLeasePath(cfg.StateDir), err)
	}

	return inputs{restCfg: restCfg, registry: registry, alloc: alloc, networks: networks}, nil
}

func mountChildWriter(cfg Config) (clusterprovider.MountChildWriter, error) {
	restCfg, err := kcpclient.ConfigFromContext(cfg.KCPKubeconfig, cfg.MountContext)
	if err != nil {
		return nil, fmt.Errorf("the kcp context the mount child is written as (%s names %q): %w. "+
			"The child carries spec.URL, kcp accepts that field only from a system privileged "+
			"user, and without it a cluster's kubeconfig reaches a workspace that proxies kcp "+
			"itself", envMountContext, cfg.MountContext, err)
	}
	client, err := kcpclient.NewMountChild(kcpclient.Options{Host: cfg.KCPHost, RestConfig: restCfg})
	if err != nil {
		return nil, fmt.Errorf("the mount-child client: %w", err)
	}
	return client, nil
}

func migrateOnly(ctx context.Context, cfg Config, log *slog.Logger) error {
	in, err := buildInputs(cfg)
	if err != nil {
		return err
	}
	return migrateLegacyKeys(ctx, in.registry, legacyKeys{
		StateDir: cfg.StateDir,
		Ports:    in.alloc,
		Networks: in.networks,
		Log:      log,
	})
}

func buildDeps(ctx context.Context, cfg Config, log *slog.Logger) (Deps, error) {
	in, err := buildInputs(cfg)
	if err != nil {
		return Deps{}, err
	}
	restCfg, registry, alloc, networks := in.restCfg, in.registry, in.alloc, in.networks

	tokens := newTokenStore(cfg.StateDir, log)

	units, err := clusterprovider.NewUnitsHost(clusterprovider.UnitsConfig{
		RestConfig:    restCfg,
		Host:          cfg.KCPHost,
		Workspace:     cfg.HostProcessWorkspace,
		Prefix:        cfg.UnitPrefix,
		StateDir:      cfg.StateDir,
		VaultStateDir: cfg.VaultStateDir,
		Resolve:       cfg.lookPath,
		Log:           log,
	})
	if err != nil {
		return Deps{}, err
	}
	alloc.SetHolder(units.Hold)

	stack := append(clusterprovider.Stack{}, clusterprovider.DesignedStack...)
	stack = append(stack, procgroup.ComponentApiserverFront)
	stack = append(stack, procgroup.ComponentCloudControllerManager)
	stack = append(stack, clusterprovider.ExternalSecretsComponents...)
	purposes, err := clusterprovider.PurposesFor(stack, false)
	if err != nil {
		return Deps{}, err
	}

	linkClient, err := kcpclient.New(kcpclient.Options{Host: cfg.KCPHost, RestConfig: restCfg, NodeClaimAgent: cfg.NodeClaimAgent})
	if err != nil {
		return Deps{}, err
	}
	mountChild, err := mountChildWriter(cfg)
	if err != nil {
		return Deps{}, err
	}
	links := clusterLinks(cfg, linkClient, registry, log)
	log.Info("cluster provider: cluster links", "on", links != nil, "dumbpipe", cfg.Dumbpipe,
		"reason", "each cluster gets a dumbpipe listener in front of its apiserver and one in front of its "+
			"vault when "+envDumbpipe+" names the binary; unset, no cluster has a link and no node can join without ssh")

	minter := instanceMinter()
	stackCfg := clusterprovider.StackConfig{
		Stack:                      stack,
		Admin:                      adminForCluster(cfg.StateDir),
		StateDir:                   cfg.StateDir,
		StoreEndpoint:              storeEndpoint(cfg, units),
		VaultStoreDSN:              vaultStoreDSN(units),
		AdvertiseAddress:           cfg.AdvertiseAddress,
		MarketDialled:              true,
		BindAddress:                portallocator.LoopbackAdvertiseAddress,
		VaultTLS:                   vaultTLSFor(),
		AuditPolicyPath:            cfg.AuditPolicyPath,
		CloudControllerManagerExec: cfg.CloudControllerManagerExec,
		FrontExec:                  cfg.FrontExec,
		HostnameExec:               cfg.HostnameExec,
		K3sExec:                    cfg.K3s,
		KineExec:                   cfg.Kine,
		PgDumpExec:                 cfg.PgDump,
		LookPath:                   cfg.lookPath,
		Slot:                       units.SlotState,
		LinkSlot:                   units.LinkState,
		ExternalSecrets:            externalSecretsFor(cfg),
		Harvest: func(ctx context.Context, cluster string) error {
			if err := clusterprovider.HarvestBundle(cfg.StateDir, cluster); err != nil {
				return err
			}
			port := 0
			if lease, ok := alloc.PortFor(cluster, portallocator.PurposeOpenBao); ok {
				port = lease.Port
			}
			dsn := ""
			if s, ok := units.Store(cluster); ok {
				dsn = s.DSN()
			}
			return clusterprovider.CustodyClusterVault(ctx, clusterprovider.CustodyOptions{
				StateDir:    cfg.StateDir,
				Cluster:     cluster,
				VaultPort:   port,
				PostgresDSN: dsn,
			})
		},
		GuardRides: true,
		Links:      links,
	}
	decider, err := materialiser.New(materialiser.Options{
		Host:     units,
		Ports:    alloc,
		Networks: networks,
		Steps:    clusterprovider.StepsFor(stackCfg),
		Front: func(ctx context.Context, cluster string) error {
			st, err := stackCfg.FrontStep(cluster)
			if err != nil {
				return err
			}
			if err := units.EnsureFront(ctx, cluster, st); err != nil {
				return err
			}
			if _, err := units.EnsureStore(ctx, cluster); err != nil {
				return fmt.Errorf("the store: %w", err)
			}
			return nil
		},
		Identity:    clusterprovider.GuardFor(guardDeps(cfg, tokens, units, log)),
		NewInstance: minter,
		Now:         time.Now,
		Purposes:    purposes,
	})
	if err != nil {
		return Deps{}, err
	}

	if !cfg.ObserveOnly() {
		if err := migrateLegacyKeys(ctx, registry, legacyKeys{
			StateDir: cfg.StateDir,
			Ports:    alloc,
			Networks: networks,
			Log:      log,
		}); err != nil {
			return Deps{}, err
		}
		if err := Startup(ctx, registry, units, log); err != nil {
			return Deps{}, err
		}
	}

	tenantConfig := tenantFromClusterCA(alloc, cfg.StateDir, tenantCredentialSource())
	tenant := tenantClients(registry, tenantConfig)
	nodeObserver := clusterprovider.NodeObserver{
		Client:    tenant,
		Operators: scale.OurOperators,
		Baseline: func(cluster string) (scale.Baseline, error) {
			dataDir, err := clusterprovider.ClusterDataDir(cfg.StateDir, cluster)
			if err != nil {
				return scale.Baseline{}, err
			}
			b, _, err := clusterprovider.ReadAddOnBaseline(dataDir)
			return b, err
		},
	}
	claims, err := kcpclient.New(kcpclient.Options{Host: cfg.KCPHost, RestConfig: restCfg})
	if err != nil {
		return Deps{}, err
	}
	nodeHosts, err := newNodeHosts(nodeHostsOptions{
		Allocator:           alloc,
		Registry:            registry,
		Claims:              claims,
		StateDir:            cfg.StateDir,
		AdvertiseAddress:    cfg.AdvertiseAddress,
		DenyBidderDids:      cfg.MarketDenyBidderDids,
		ClaimReleaseTimeout: cfg.MarketReleaseTimeout,
		NodeJoinGrace:       cfg.NodeJoinGrace,
		Tenant:              tenant,
		Log:                 log,
		Links:               claims,
		NodeSecrets: func(cluster, clusterDir string) nodemarket.NodeSecrets {
			return clusterprovider.NewNodeVault(units, cluster, clusterDir).WithLogger(log)
		},
		Observe: nodeObserver.Observe,
		PathUp: clusterPathUp(tenant, claims, func(ctx context.Context, cluster string) (string, error) {
			target, err := claimClusterFor(registry, cluster)(ctx)
			if err != nil {
				return "", err
			}
			return target.Workspace, nil
		}),
	})
	if err != nil {
		return Deps{}, err
	}
	scaleDriver, err := scaledriver.New(nodeHosts.forCluster, scaledriver.Options{
		Observe: nodeHosts.observeHeld(nodeObserver.Observe),
		Scale: scale.Options{
			Hold:        cfg.ScaleHold,
			MaxNodes:    cfg.ScaleMaxNodes,
			PodsPerNode: cfg.ScalePodsPerNode,
		},
		Log: log,
	})
	if err != nil {
		return Deps{}, err
	}
	log.Info("cluster provider: node scale settings", "hold", cfg.ScaleHold.String(),
		"maxNodes", cfg.ScaleMaxNodes, "podsPerNode", cfg.ScalePodsPerNode)

	deps := Deps{
		Rest:        restCfg,
		Tenants:     tenant,
		Registry:    registry,
		Decider:     decider,
		Allocator:   alloc,
		Networks:    networks,
		Tokens:      tokens,
		Tenant:      tenantConfig,
		Units:       units,
		NewInstance: minter,
		NodeScale:   scaleDriver,

		MountChild: mountChild,

		Homes: linkClient,

		InstallTenant: tenantAddOns(cfg, log),
	}
	if links != nil {
		deps.Links = &linkRecorder{client: linkClient, registry: registry}
		deps.ClusterLinks = links
	}
	if !cfg.ObserveOnly() {
		deps.Host = units
		deps.NodeHosts = nodeHosts
		deps.Roller = newRoller(units, stackCfg, registry, alloc, purposes,
			fmt.Sprintf("cluster provider pid %d (kcp kubeconfig %s, state %s)", os.Getpid(), cfg.KCPKubeconfig, stackCfg.StateDir), log)
	}
	return deps, nil
}

func storeEndpoint(cfg Config, units *clusterprovider.UnitsHost) func(cluster string) (string, error) {
	if cfg.StoreEndpoint != "" {
		return func(string) (string, error) { return cfg.StoreEndpoint, nil }
	}
	return func(cluster string) (string, error) {
		s, ok := units.Store(cluster)
		if !ok {
			return "", fmt.Errorf("clusterprovider: the Postgres object of %s is not Ready in this process, so there is no DSN to give kine", cluster)
		}
		return s.DSN(), nil
	}
}

func vaultStoreDSN(units *clusterprovider.UnitsHost) func(cluster string) (string, error) {
	return func(cluster string) (string, error) {
		s, ok := units.Store(cluster)
		if !ok {
			return "", fmt.Errorf("clusterprovider: the Postgres object of %s is not Ready in this process, so there is no DSN to give the vault", cluster)
		}
		return s.VaultDSN(), nil
	}
}

const TenantClientReuse = 5 * time.Minute

type cachedTenant struct {
	of     string
	minted time.Time
	client kubernetes.Interface
}

func tenantClients(registry clusterprovider.Clusters, mint func(context.Context, clusterprovider.Ref, *v1alpha1.Cluster) (*rest.Config, error)) func(context.Context, string) (kubernetes.Interface, error) {
	var mu sync.Mutex
	clients := map[string]cachedTenant{}
	return func(ctx context.Context, cluster string) (kubernetes.Interface, error) {
		ref, cl, err := tenantCluster(ctx, registry, cluster)
		if err != nil {
			return nil, err
		}
		of := string(cl.UID) + "/" + cl.Status.Instance
		mu.Lock()
		for k, c := range clients {
			if time.Since(c.minted) >= TenantClientReuse {
				delete(clients, k)
			}
		}
		c, ok := clients[cluster]
		mu.Unlock()
		if ok && c.of == of {
			return c.client, nil
		}
		cfg, err := mint(ctx, ref, cl)
		if err != nil {
			return nil, err
		}
		client, err := kubernetes.NewForConfig(kcpserver.Throughput(cfg))
		if err != nil {
			return nil, err
		}
		mu.Lock()
		clients[cluster] = cachedTenant{of: of, minted: time.Now(), client: client}
		mu.Unlock()
		return client, nil
	}
}

func tenantCluster(ctx context.Context, reg clusterprovider.Clusters, key string) (clusterprovider.Ref, *v1alpha1.Cluster, error) {
	refs, err := reg.Refs(ctx)
	if err != nil {
		return clusterprovider.Ref{}, nil, err
	}
	for _, r := range refs {
		found, err := r.Key()
		if err != nil {
			return clusterprovider.Ref{}, nil, err
		}
		if found != key {
			continue
		}
		cl, err := reg.Read(ctx, r)
		if err != nil {
			return clusterprovider.Ref{}, nil, err
		}
		return r, cl, nil
	}
	return clusterprovider.Ref{}, nil, fmt.Errorf("clusterprovider: no Cluster CR with the key %s", key)
}

func guardDeps(cfg Config, tokens *tokenStore, units *clusterprovider.UnitsHost, log *slog.Logger) clusterprovider.GuardDeps {
	return clusterprovider.GuardDeps{
		StateDir: cfg.StateDir,
		Token:    tokens.token,
		HasRow:   bootstrapRowObserved(cfg, units, log),
		DeleteRow: func(ctx context.Context, cluster string) error {
			s, ok := units.Store(cluster)
			if !ok {
				return fmt.Errorf("clusterprovider: the Postgres object of %s is not Ready in this process", cluster)
			}
			return s.DeleteBootstrapRows(ctx)
		},
		SeedBundle: func(ctx context.Context, cluster string) error {
			return clusterprovider.SeedBundleForWake(ctx, cfg.StateDir, cluster)
		},
		Bundle: clusterprovider.BundleGuard(cfg.StateDir),
		Log:    log,
	}
}

func bootstrapRowObserved(cfg Config, units *clusterprovider.UnitsHost, log *slog.Logger) func(cluster string) bool {
	return func(cluster string) bool {
		if cfg.StoreEndpoint != "" {
			log.Warn("cluster provider: the store is external, so whether it holds k3s's bootstrap "+
				"row cannot be observed from here; falling back to the operator's declaration",
				"cluster", cluster, "datastoreHasRow", cfg.DatastoreHasRow)
			return cfg.DatastoreHasRow
		}
		s, ok := units.Store(cluster)
		if !ok {
			log.Error("cluster provider: the Postgres object is not Ready, so the bootstrap row cannot be observed",
				"cluster", cluster)
			return cfg.DatastoreHasRow
		}
		has, err := s.HasBootstrapRow(context.Background())
		if err != nil {
			log.Info("cluster provider: reading the bootstrap row failed, so the operator's "+
				"declaration is used instead", "cluster", cluster, "error", err.Error(),
				"datastoreHasRow", cfg.DatastoreHasRow)
			return cfg.DatastoreHasRow
		}
		log.Info("cluster provider: the store was read for k3s's bootstrap row",
			"cluster", cluster, "hasBootstrapRow", has, "declared", cfg.DatastoreHasRow)
		return has
	}
}

func adminForCluster(stateDir string) clusterprovider.AdminClient {
	return func(_ context.Context, cluster, apiserver string) (kubernetes.Interface, error) {
		dataDir, err := clusterprovider.ClusterDataDir(stateDir, cluster)
		if err != nil {
			return nil, err
		}
		path := clusterstack.AdminKubeconfig(dataDir)
		cfg, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf(
				"clusterprovider: the cluster's own admin credential at %s (k3s writes it there "+
					"because ServerArgv passes --write-kubeconfig): %w. Without it the PSA labels "+
					"and automountServiceAccountToken=false cannot be written or read back", path, err)
		}
		if apiserver != "" {
			cfg.Host = apiserver
		}
		return kubernetes.NewForConfig(kcpserver.Throughput(cfg))
	}
}

func instanceMinter() func() string {
	start := time.Now().UTC().Format("20060102T150405Z")
	var n atomic.Int64
	return func() string {
		return fmt.Sprintf("gen-%s-%d", start, n.Add(1))
	}
}

func tenantCredentialSource() clusterprovider.CredentialSource {
	return clusterprovider.CredentialSourceClusterCA
}

func tenantFromClusterCA(alloc *portallocator.Allocator, stateDir string, source clusterprovider.CredentialSource) func(context.Context, clusterprovider.Ref, *v1alpha1.Cluster) (*rest.Config, error) {
	return func(ctx context.Context, ref clusterprovider.Ref, cl *v1alpha1.Cluster) (*rest.Config, error) {
		key, err := ref.Key()
		if err != nil {
			return nil, err
		}
		lease, err := alloc.Allocate(portallocator.Request{
			Cluster: key, Purpose: portallocator.PurposeK3sSecure,
		})
		if err != nil {
			return nil, fmt.Errorf("clusterprovider: the apiserver lease for %s, which is the "+
				"address this cluster's apiserver answers on: %w", cl.Name, err)
		}
		return clusterprovider.MintTenantCredentialForCluster(ctx, stateDir, key, clusterprovider.APIServerURL(lease.Port), source)
	}
}
