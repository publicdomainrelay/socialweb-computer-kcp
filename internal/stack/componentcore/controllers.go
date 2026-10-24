package componentcore

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component/ports"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotosession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/childapplication"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/cluster"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/clusterroll"
	componentctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/componentlink"
	computevmctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/computevm"
	eventsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/events"
	exposurectl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/guestidentitygrant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/hostinstall"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/hostrelease"
	infractl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/infracollection"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/kcpinstallation"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketintents"
	marketpresencectl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketpresence"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/openbaoserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/pdsaccount"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/plcidentity"
	policyctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/portlease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/postgres"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/relaystatus"
	remoteopenbaoctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/remoteopenbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/repos"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	servicectl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/service"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/subnetlease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/ttl"
	vaultctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vaultcertificate"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vaultcredential"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostlifecycle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/lease/local"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	presence "github.com/johnandersen777/socialweb-computer-kcp/internal/marketpresence"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	policycontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultmain"
)

const (
	pdsClientTimeout = 30 * time.Second

	identityResolverTimeout = 10 * time.Second

	identityHTTPTimeout = 20 * time.Second

	defaultQROrigin = "https://qr.fedfork.com"
)

func (c *Core) runControllers(ctx context.Context) error {
	cfg := c.cfg
	log := cfg.Log
	restCfg, err := c.managerRestConfig()
	if err != nil {
		return fmt.Errorf("componentcore: the controller manager: %w", err)
	}
	mgr, err := ctlruntime.NewManager(ctlruntime.Config{Rest: restCfg, AddToScheme: schemes, Workspaces: c.scope.Contains, Log: log})
	if err != nil {
		return fmt.Errorf("componentcore: the controller manager: %w", err)
	}
	clients, err := newWorkspaceClients(restCfg)
	if err != nil {
		return fmt.Errorf("componentcore: the workspace clients: %w", err)
	}
	g := supervise.New(ctx)
	defer g.Stop()
	ctx = g.Context()
	c.scope.start(ctx)
	c.registerLocal(ctx, mgr, clients)
	if err := c.registerNetwork(mgr, clients, g); err != nil {
		log.Warn("the network controllers are not wired", "err", err.Error())
	}
	if cfg.Compute != nil {
		compute := *cfg.Compute
		compute.Logins = c.logins
		g.Go("computevm", func(ctx context.Context) error { return computevmctl.Run(ctx, compute) })
	}
	g.Go("controller-manager", mgr.Start)
	return g.Wait()
}

func (c *Core) registerLocal(ctx context.Context, mgr *ctlruntime.Manager, clients *workspaceClients) {
	cfg := c.cfg
	local := c.scope.Contains
	base := c.scope.Base
	warn := func(what string, err error) {
		if err != nil {
			cfg.Log.Warn("a controller is not wired", "controllers", what, "err", err.Error())
		}
	}
	exposures, err := c.registerExposure(mgr, local)
	warn("Service, Exposure, ComponentLink", err)
	for _, changed := range exposures {
		c.closers = append(c.closers, c.registry.Subscribe(changed))
	}
	_, err = componentctl.Register(mgr, componentctl.Options{Runtime: c.runtime, Scope: local, Changed: exposures, Log: cfg.Log})
	warn("components", err)
	warn("GuestControlPlane inputs", c.registerGuestControlPlane(mgr, local))
	warn("MarketIdentity", c.registerIdentity(mgr, local))
	warn("NodeClaim", c.registerNodeClaims(mgr, c.nodeClaims))
	_, err = marketintents.Register(mgr, marketintents.Options{Bus: c.handlers, Clients: clients.client, Scope: local, Log: cfg.Log})
	warn("market intents", err)
	if cfg.DynamicFactory != nil {
		_, err = infractl.Register(mgr, infractl.Options{Bus: c.handlers, Dynamic: cfg.DynamicFactory, Scope: local, Log: cfg.Log})
		warn("Infra, EphemeralInfra", err)
		_, err = infracollection.Register(mgr, infracollection.Options{Bus: c.handlers, Dynamic: cfg.DynamicFactory, Scope: local, Log: cfg.Log})
		warn("InfraCollection, EphemeralInfraCollection", err)
	}
	_, err = marketpresencectl.Register(mgr, marketpresencectl.Options{Heartbeats: presence.New(presence.Config{HTTPClient: c.handlers.Client()}),
		Bidders: bidders{clients: clients}, Scope: local, Log: cfg.Log})
	warn("MarketPresence", err)
	warn("leases", c.registerLeases(ctx, mgr, local, base))
	warn("status objects", c.registerStatus(mgr, base))
	warn("host lifecycle", c.registerHost(mgr, clients, base))
	if cfg.Apps != nil {
		warn("application roots", app.Register(ctx, mgr, *cfg.Apps))
	}
	if cfg.TTLScope != nil {
		tc, err := ttl.Register(mgr, ttl.Options{Log: cfg.Log, Scope: cfg.TTLScope})
		warn("TTL", err)
		if err == nil && cfg.OnTTL != nil {
			cfg.OnTTL(tc.Report)
		}
	}
}

func (c *Core) registerExposure(mgr *ctlruntime.Manager, scope func(string) bool) ([]func(component.Key), error) {
	log := c.cfg.Log
	services, err := servicectl.Register(mgr, servicectl.Options{Handlers: c.handlers, Registry: c.registry, Scope: scope, Log: log})
	if err != nil {
		return nil, err
	}
	publisher := exposure.NewPublisher(exposure.PublisherConfig{Handlers: c.handlers, Ports: c.ports, TLS: c.tls,
		Registrar: c.ingressRegistrar, Log: log})
	exposures, err := exposurectl.Register(mgr, exposurectl.Options{Publisher: publisher, Scope: scope, Log: log})
	if err != nil {
		return nil, err
	}
	c.closers = append(c.closers, exposures.Close)
	links, err := componentlink.Register(mgr, componentlink.Options{Policy: c.linkPolicy, Registry: c.registry, Providers: c.handlers, Scope: scope, Log: log})
	if err != nil {
		return nil, err
	}
	return []func(component.Key){services.ComponentChanged, exposures.ComponentChanged, links.ComponentChanged}, nil
}

func (c *Core) registerIdentity(mgr *ctlruntime.Manager, scope func(string) bool) error {
	cfg := c.cfg
	oauth := atproto.OAuthOptions{Directory: identity.DefaultDirectory(), HTTPClient: &http.Client{Timeout: identityHTTPTimeout},
		States: &oauthclient.MemoryStateStore{}, Scopes: append([]string(nil), cfg.OAuthScopes...), QRScopes: atprotooauth.DIDKeyAssociatorOAuthScope}
	if key, err := loadOAuthCLIKey(cfg.StateDir); err == nil {
		oauth.CLIKey = key
		oauth.QROrigin = cmp.Or(strings.TrimSpace(os.Getenv("SOCIALWEB_QR_ORIGIN")), defaultQROrigin)
	}
	var mu sync.Mutex
	vaults := map[string]atproto.SecretStore{}
	ctl, err := marketidentity.Register(mgr, marketidentity.Options{Scope: scope, Endpoints: c.pdsEndpoint,
		Vault: func(workspace string) atproto.SecretStore {
			mu.Lock()
			defer mu.Unlock()
			if store, ok := vaults[workspace]; ok {
				return store
			}
			store, err := newIdentityVaultStore(c.store, workspace, cfg.CredentialClient, cfg.CredentialServer)
			if err != nil {
				return nil
			}
			vaults[workspace] = store
			return store
		},
		Resolver: &pdsclient.Resolver{Handlers: c.handlers, Timeout: identityResolverTimeout}, Bus: c.handlers, OAuth: oauth, Log: cfg.Log})
	if err != nil {
		return err
	}
	base := c.scope.base()
	for _, name := range []string{"bidder", "requester"} {
		c.oauthCallbacks.Register(name, func(ctx context.Context, _ http.ResponseWriter, r *http.Request) error {
			return ctl.CompleteBrowserAuthorization(ctx, base, name, r)
		})
	}
	return nil
}

func (c *Core) registerLeases(ctx context.Context, mgr *ctlruntime.Manager, scope, base func(string) bool) error {
	cfg := c.cfg
	portsFile := filepath.Join(cfg.StateDir, PortsDir, ports.PortsFile)
	networksFile := filepath.Join(cfg.StateDir, "networks.json")
	watch := func(paths ...string) <-chan struct{} {
		ch, err := local.Watch(ctx, paths...)
		if err != nil {
			cfg.Log.Warn("the lease controllers do not follow host file changes", "paths", strings.Join(paths, ", "), "err", err.Error())
			return nil
		}
		return ch
	}
	if _, err := portlease.Register(mgr, portlease.Options{Ports: c.ports, Changes: watch(portsFile), Scope: scope, Log: cfg.Log}); err != nil {
		return err
	}
	subnets, err := subnetallocator.Open(subnetallocator.Config{LeasePath: networksFile, AppInstance: cfg.InstallAccount})
	if err != nil {
		cfg.Log.Warn("the SubnetLease controller is not wired: no subnet allocator", "err", err.Error())
	} else if _, err := subnetlease.Register(mgr, subnetlease.Options{Subnets: subnets, Changes: watch(networksFile), Scope: scope,
		Log: cfg.Log}); err != nil {
		return err
	}
	if _, err := cluster.Register(mgr, cluster.Options{Ports: c.ports, Front: apiserverfront.Client{Path: cfg.FrontControl},
		Changes: watch(cfg.SlotsFile, portsFile), Scope: base}); err != nil {
		return err
	}
	if _, err := clusterroll.Register(mgr, clusterroll.Options{Rolls: local.SlotFile{Path: cfg.SlotsFile}, Changes: watch(cfg.SlotsFile),
		Scope: base}); err != nil {
		return err
	}
	return nil
}

func (c *Core) registerStatus(mgr *ctlruntime.Manager, scope func(string) bool) error {
	base := c.scope.base()
	plc := plcidentity.Options{Scope: scope}
	if url := c.plcBaseURL(); url != "" {
		directory, err := statusstate.NewHTTPPLCClient(url, c.busOrNetworkClient())
		if err != nil {
			return err
		}
		plc.Directory = directory
	}
	vault := openBaoMetadata(c.store, base)
	_, accountErr := pdsaccount.Register(mgr, pdsaccount.Options{Bus: c.handlers, Scope: scope})
	_, sessionErr := atprotosession.Register(mgr, atprotosession.Options{Bus: c.handlers, Scope: scope})
	_, plcErr := plcidentity.Register(mgr, plc)
	_, relayErr := relaystatus.Register(mgr, relaystatus.Options{Source: statusstate.NewHTTPRelayHealthSource(c.relayEndpoint(base), c.busOrNetworkClient()),
		Scope: scope})
	_, credentialErr := vaultcredential.Register(mgr, vaultcredential.Options{Source: vault, Scope: scope})
	_, certificateErr := vaultcertificate.Register(mgr, vaultcertificate.Options{Source: vault, Scope: scope})
	return errors.Join(accountErr, sessionErr, plcErr, relayErr, credentialErr, certificateErr)
}

func (c *Core) registerHost(mgr *ctlruntime.Manager, clients *workspaceClients, scope func(string) bool) error {
	cfg := c.cfg
	authority := cfg.HostAuthority
	if authority == nil {
		authority = c.fileAuthority()
	}
	_, kcpErr := kcpinstallation.Register(mgr, kcpinstallation.Options{Log: cfg.Log, Authority: authority, Scope: scope})
	_, installErr := hostinstall.Register(mgr, hostinstall.Options{Log: cfg.Log, Authority: authority, Scope: scope})
	_, releaseErr := hostrelease.Register(mgr, hostrelease.Options{Log: cfg.Log, Authority: authority, Scope: scope})
	_, childErr := childapplication.Register(mgr, childapplication.Options{Log: cfg.Log, Workspaces: workspaceTree{clients: clients},
		Objects: clients, Scope: scope})
	if err := errors.Join(kcpErr, installErr, releaseErr, childErr); err != nil {
		return err
	}
	if cfg.Postgres != nil {
		if _, err := postgres.Register(mgr, postgres.Options{Log: cfg.Log, Host: cfg.Postgres, Scope: scope}); err != nil {
			return err
		}
	}
	if cfg.OpenBao != nil {
		servers, err := openbaoserver.Register(mgr, openbaoserver.Options{Servers: cfg.OpenBao, Grants: guestGrants{clients: clients}, Scope: scope,
			Log: cfg.Log})
		if err != nil {
			return err
		}
		if _, err := guestidentitygrant.Register(mgr, guestidentitygrant.Options{Outcomes: servers, Events: servers.GrantEvents(), Scope: scope,
			Log: cfg.Log}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) fileAuthority() *hostlifecycle.FileAuthority {
	cfg := c.cfg
	return hostlifecycle.NewFileAuthority(hostlifecycle.FileAuthorityOptions{
		StateDir:     cfg.StateDir,
		Account:      cfg.InstallAccount,
		FrontControl: cfg.FrontControl,
		Record: func(account string) (hostlifecycle.InstallRecord, error) {
			rec, err := install.ReadRecord(cfg.InstallStateRoot, account)
			if err != nil {
				return hostlifecycle.InstallRecord{}, err
			}
			return hostlifecycle.InstallRecord{StateDir: rec.StateDir, Steps: rec.Steps}, nil
		},
		ActiveSlot: func(context.Context) (string, error) {
			slots, err := kcpserver.ReadSlots(cfg.SlotsFile)
			if err != nil {
				return "", err
			}
			return slots.Active, nil
		},
		CoreVersion: func(ctx context.Context, stateDir, link string) (string, error) {
			asset, err := kcpserver.ReadAsset(ctx, filepath.Join(stateDir, "bin", link, install.CoreName))
			if err != nil {
				return "", err
			}
			return cmp.Or(asset.Version, asset.SHA256), nil
		},
	})
}

func (c *Core) registerNetwork(mgr *ctlruntime.Manager, clients *workspaceClients, g *supervise.Group) error {
	cfg := c.cfg
	log := cfg.Log
	remoteBao := vaultctl.Remote(c.handlers, log)
	if err := repos.Register(mgr, c.repoOptions()); err != nil {
		return fmt.Errorf("componentcore: the repo controllers: %w", err)
	}
	if err := policyctl.Register(mgr, policyctl.Options{Bus: c.handlers, PLCURL: cfg.PLCBaseURL, HTTP: c.networkClient(networkReadTimeout), Log: log, Disabled: cfg.DisablePolicy,
		Actions: func(workspace string) (map[string]policycontroller.ActionHandler, error) {
			return policyActionHandlers(c.store, workspace)
		}}); err != nil {
		return fmt.Errorf("componentcore: the policy controllers: %w", err)
	}
	if _, err := vaultctl.Register(mgr, c.handlers, vaultctl.Options{Remote: remoteBao, Log: log}); err != nil {
		return fmt.Errorf("componentcore: the vault controller: %w", err)
	}
	if _, err := remoteopenbaoctl.Register(mgr, remoteopenbaoctl.Options{Remote: remoteBao, Log: log}); err != nil {
		return fmt.Errorf("componentcore: the RemoteOpenBao controller: %w", err)
	}
	run, err := eventsctl.Register(g.Context(), mgr, eventsctl.Options{Bus: c.handlers, Dynamic: eventstream.DynamicFactory(cfg.DynamicFactory),
		Resolver: &pdsclient.Resolver{Handlers: c.handlers, Timeout: pdsClientTimeout}, PLCURL: c.plcBaseURL(), HTTPClient: c.networkClient(networkReadTimeout), StateDir: cfg.StateDir,
		RecordWatchChildren: recordWatchChildren{clients: clients}, RecordWatchRepos: recordWatchRepos{clients: clients},
		RecordWatchValues: recordWatchValues(clients), Log: log})
	if err != nil {
		return fmt.Errorf("componentcore: the event controllers: %w", err)
	}
	g.Go("events", run)
	return nil
}

func (c *Core) networkClient(timeout time.Duration) *http.Client {
	egress := component.Egress{Hosts: c.hosts, TLS: c.tls, Log: c.cfg.Log}
	if n := c.cfg.Network; n != nil {
		if hosts := participant.NetworkHosts(*n, c.hosts); hosts != nil {
			egress.Hosts = hosts
		}
		if pool, err := x509.SystemCertPool(); err == nil && pool.AppendCertsFromPEM([]byte(n.CABundle)) {
			egress.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
		}
	}
	return egress.Client(timeout)
}

func (c *Core) busOrNetworkClient() *http.Client {
	external := c.networkClient(networkReadTimeout).Transport
	if external == nil {
		external = http.DefaultTransport
	}
	inProcess := c.handlers.RoundTripper()
	return &http.Client{Timeout: networkReadTimeout, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := c.handlers.HandlerForHost(hostport.Normalize(req.URL.Host)); ok {
			return inProcess.RoundTrip(req)
		}
		return external.RoundTrip(req)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const PublicPLCDirectoryURL = "https://plc.directory"

func (c *Core) plcBaseURL() string {
	if url := strings.TrimSpace(c.cfg.PLCBaseURL); url != "" {
		return url
	}
	if c.cfg.Network == nil {
		return PublicPLCDirectoryURL
	}
	return "http://" + kcp.ServiceFQDN(compose.NetworkPLCName, "default", c.cfg.Workspaces[0], kcp.DefaultServiceDomain)
}

func (c *Core) managerRestConfig() (*rest.Config, error) {
	if c.cfg.Kubeconfig != "" {
		return kcpserver.FrontRestConfig(c.cfg.Kubeconfig)
	}
	if c.cfg.KCP == nil {
		return nil, ErrNoKubeconfig
	}
	client, err := c.cfg.KCP.Client(c.cfg.Workspaces[0])
	if err != nil {
		return nil, err
	}
	restCfg := client.RESTConfig()
	if restCfg == nil {
		return nil, errors.New("componentcore: the kcp client has no rest config")
	}
	return restCfg, nil
}

func (c *Core) relayEndpoint(base string) statusstate.RelayEndpointFunc {
	return func(ref string) (string, bool) {
		ref = strings.TrimSpace(ref)
		ref = cmp.Or(ref, compose.ComponentKindRelay+"/"+compose.NetworkRelayName)
		if strings.Contains(ref, "://") {
			return ref, true
		}
		kind, name := compose.ComponentKindRelay, ref
		if before, after, ok := strings.Cut(ref, "/"); ok {
			kind, name = before, after
		}
		kind, name = strings.TrimSpace(kind), strings.TrimSpace(name)
		kind = cmp.Or(kind, compose.ComponentKindRelay)
		name = cmp.Or(name, compose.NetworkRelayName)
		if endpoint, ok := c.registry.Endpoint(base, kind, name, "http"); ok {
			if endpoint.URL != "" {
				return endpoint.URL, true
			}
			if endpoint.Address != "" {
				return "http://" + endpoint.Address, true
			}
		}
		for _, url := range c.cfg.Relays {
			if url = strings.TrimSpace(url); url != "" {
				return url, true
			}
		}
		return "http://" + kcp.ServiceFQDN(strings.ToLower(kind), "default", base, kcp.DefaultServiceDomain), true
	}
}

func openBaoMetadata(store *kcpstore.Store, workspace string) statusstate.VaultSource {
	if store == nil {
		return nil
	}
	return &statusstate.OpenBaoMetadataSource{Open: func(ctx context.Context, server, mount string) (statusstate.MetadataReader, error) {
		server = strings.TrimSpace(server)
		if server == "" {
			return nil, errors.New("componentcore: the vault status object names no OpenBaoServer")
		}
		obj, err := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, openbaoctl.GVR).Get(ctx, ref.New(workspace, "", server))
		if err != nil {
			return nil, fmt.Errorf("componentcore: reading OpenBaoServer %s: %w", server, err)
		}
		if client := openbao.ClientForMount(obj.Status.Clients, mount); client != "" {
			srv, err := openbao.ServerOf(workspace, server, obj.Status, client)
			if err != nil {
				return nil, err
			}
			return openbao.Open(ctx, srv)
		}
		access, err := vaultmain.RootAccessOf(*obj)
		if err != nil {
			return nil, err
		}
		return access.Client(ctx)
	}}
}
