package embedded

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	componentlink "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/componentlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/contractevent"
	eventsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/exposurelocal"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketintents"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/repos"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vaultmemory"
	contractexposure "github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	sandboxembedded "github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/embedded"
)

const (
	DefaultStateDir = "socialweb"

	DefaultProviderWorkspace = "root:socialweb-provider"

	DefaultOrigin = "http://127.0.0.1"

	RootWorkspace = "root"

	AppName = "app"

	IdentityName = "requester"

	FieldManager = "socialweb-embedded"

	PLCDirectoryURL = "https://plc.directory"

	SnapshotVersion = 1

	ApplyStallTimeout = 10 * time.Minute

	CloseTimeout = 15 * time.Second
)

type PolicySandbox = sandboxembedded.Host

var ErrNoPolicySandbox = sandboxembedded.ErrNoHost

var providerSets = []string{"provider", "hostprocesses", "applications", "bidder"}

var providerBindings = []string{"applications", "computevms", "events", "exposures", "hostprocesses", "pds", "policy"}

type Config struct {
	Rest *rest.Config

	StateDir string

	ProviderWorkspace string

	Origin string

	IdentityMode v1alpha1.MarketIdentityMode

	Handle string

	DID string

	PDSURL string

	OAuthClientID string

	OAuthRedirectURI string

	ForceApply bool

	Restore []byte

	OnSnapshot func([]byte)

	OnRestoreError func(error)

	PolicySandbox PolicySandbox

	ProviderIdentities []byte

	BeforeRequester func(ctx context.Context) error

	Log *slog.Logger
}

type Snapshot struct {
	Version int                  `json:"version"`
	Seq     uint64               `json:"seq"`
	Vault   vaultmemory.Snapshot `json:"vault"`
}

type Requester struct {
	manager *ctlruntime.Manager

	vault *vaultmemory.Controller

	exposure *exposurelocal.Controller

	identity *marketidentity.Controller

	bus *bus.Bus

	snapMu sync.Mutex

	seq uint64

	wake chan struct{}

	ready chan struct{}

	readyOnce sync.Once

	cancel context.CancelFunc

	done chan struct{}

	rest *rest.Config

	providerWorkspace string
}

func Start(parent context.Context, cfg Config) (*Requester, error) {
	if cfg.Rest == nil {
		return nil, errors.New("embedded: a kcp rest config is required")
	}
	cfg.StateDir = cmp.Or(cfg.StateDir, DefaultStateDir)
	cfg.ProviderWorkspace = cmp.Or(cfg.ProviderWorkspace, DefaultProviderWorkspace)
	cfg.Origin = cmp.Or(cfg.Origin, DefaultOrigin)
	cfg.IdentityMode = cmp.Or(cfg.IdentityMode, v1alpha1.MarketIdentityModeOAuthBrowser)
	cfg.PolicySandbox = presentPolicySandbox(cfg.PolicySandbox)
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	admin := rest.CopyConfig(cfg.Rest)
	admin.Impersonate = rest.ImpersonationConfig{UserName: "system:kcp:admin", Groups: []string{"system:kcp:admin"}}
	cfg.Rest = admin

	identities, err := ParseProviderIdentities(cfg.ProviderIdentities)
	if err != nil {
		return nil, err
	}
	if cfg.ForceApply || !providerReady(parent, cfg.Rest) {
		if err := applyProvider(parent, cfg, identities, log); err != nil {
			return nil, err
		}
	} else if err := restoreProviderIdentities(parent, cfg.Rest, cfg.ProviderWorkspace, identities, log); err != nil {
		return nil, err
	}
	if cfg.BeforeRequester != nil {
		if err := applyRequesterBindings(parent, cfg, log); err != nil {
			return nil, err
		}
		if err := cfg.BeforeRequester(parent); err != nil {
			return nil, fmt.Errorf("embedded: the app's BeforeRequester hook: %w", err)
		}
		log.Info("embedded: the app's BeforeRequester hook ran")
	}

	ctx, cancel := context.WithCancel(parent)
	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()
	b := bus.New(ctx)
	composer, err := compose.New(compose.Config{Rest: cfg.Rest, Workspace: RootWorkspace, Bus: b, Log: log})
	if err != nil {
		return nil, fmt.Errorf("embedded: composer: %w", err)
	}
	mgr, err := ctlruntime.NewManager(ctlruntime.Config{
		Rest:            cfg.Rest,
		ExportWorkspace: cfg.ProviderWorkspace,
		AddToScheme:     schemes,
		Workspaces:      inRoot,
		Log:             log,
	})
	if err != nil {
		return nil, fmt.Errorf("embedded: manager: %w", err)
	}
	r := &Requester{manager: mgr, bus: b, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), rest: cfg.Rest, providerWorkspace: cfg.ProviderWorkspace}
	if err := r.register(ctx, cfg, composer, log); err != nil {
		return nil, err
	}
	runEvents, err := r.registerEvents(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	started = true
	go func() {
		defer close(r.done)
		if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("embedded: the controller manager exited", "err", err)
		}
	}()
	go func() {
		if err := runEvents(ctx); err != nil && ctx.Err() == nil {
			log.Error("embedded: the event streams exited", "err", err)
		}
	}()
	if err := r.createRoot(ctx, cfg); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Requester) register(ctx context.Context, cfg Config, composer *compose.Composer, log *slog.Logger) error {
	vaultCtl, err := vaultmemory.Register(r.manager, r.bus, vaultmemory.Options{Scope: inRoot})
	if err != nil {
		return err
	}
	r.vault = vaultCtl
	r.restore(ctx, cfg, log)
	exposureCtl, err := exposurelocal.Register(r.manager, exposurelocal.Options{Origin: cfg.Origin, Handlers: r.bus, Scope: inRoot, Log: log})
	if err != nil {
		return err
	}
	r.exposure = exposureCtl
	directory := Directory()
	secrets := func(ws string) atproto.SecretStore { return vaultmemory.NewSecrets(vaultCtl.Provider().Store(ws)) }
	callbacks := oauthcallback.NewRegistry()
	identityCtl, err := marketidentity.Register(r.manager, marketidentity.Options{
		Scope:      inRoot,
		Vault:      secrets,
		Bus:        r.bus,
		Log:        log,
		HTTPClient: http.DefaultClient,
		OAuth: atproto.OAuthOptions{
			ClientID:    cfg.OAuthClientID,
			RedirectURI: cfg.OAuthRedirectURI,
			Scopes:      oauth.RequesterOAuthScope,
			StateRef:    &commonv1.SecretRef{Mount: IdentityName, Path: "oauth/" + IdentityName},
			Directory:   directory,
			HTTPClient:  http.DefaultClient,
		},
	})
	if err != nil {
		return err
	}
	r.identity = identityCtl
	if err := r.registerReady(); err != nil {
		return err
	}
	callbacks.Register(IdentityName, func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
		return identityCtl.CompleteBrowserAuthorization(ctx, RootWorkspace, IdentityName, req)
	})
	if err := repos.Register(r.manager, repos.Options{
		Bus: r.bus,
		Log: log,
		Dynamic: func(workspace string) (*kube.Client, error) {
			return kube.ForWorkspace(cfg.Rest, workspace)
		},
		Network: reporemote.NewNetwork(reporemote.NetworkConfig{Directory: directory, HTTP: http.DefaultClient}),
		Pds:     func(string, string) (*atpds.Server, bool) { return nil, false },
		Secrets: func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error) {
			return secrets(workspace).Get(ctx, ref)
		},
		Scope: inRoot,
	}); err != nil {
		return fmt.Errorf("embedded: repo controllers: %w", err)
	}
	if _, err := componentlink.Register(r.manager, componentlink.Options{
		Policy: contractexposure.NewLinkPolicy(r.bus), Registry: component.NewRegistry(), Providers: r.bus, Scope: inRoot, Log: log,
	}); err != nil {
		return err
	}
	runner, opener := policySandbox(cfg.PolicySandbox)
	if err := policy.Register(r.manager, policy.Options{Bus: r.bus, Sandbox: runner, OpenSandbox: opener, Log: log}); err != nil {
		return fmt.Errorf("embedded: policy controllers: %w", err)
	}
	clients := workspaceClients(cfg.Rest)
	if _, err := marketintents.Register(r.manager, marketintents.Options{Bus: r.bus, Scope: inRoot, Log: log, Clients: clients}); err != nil {
		return err
	}
	if _, err := contractevent.Register(r.manager, contractevent.Options{Bus: r.bus, Scope: inRoot, Log: log, Clients: clients}); err != nil {
		return err
	}
	if _, err := infra.Register(r.manager, infra.Options{Bus: r.bus, Scope: inRoot, Log: log,
		Dynamic: func(workspace string) (*kube.Client, error) { return kube.ForWorkspace(cfg.Rest, workspace) }}); err != nil {
		return err
	}
	if err := marketrequester.Register(r.manager, composer); err != nil {
		return err
	}
	return registerRequesterRunner(ctx, r.manager, r.bus, callbacks, cfg.StateDir, inRoot, log)
}

func (r *Requester) registerEvents(ctx context.Context, cfg Config, log *slog.Logger) (func(context.Context) error, error) {
	watchChildren, watchRepos, watchValues := eventsctl.RecordWatch(workspaceClients(cfg.Rest))
	return eventsctl.Register(ctx, r.manager, eventsctl.Options{
		Bus: r.bus, Dynamic: kube.NewFactory(cfg.Rest), Resolver: &pdsclient.Resolver{Handlers: r.bus},
		PLCURL: PLCDirectoryURL, HTTPClient: http.DefaultClient, StateDir: cfg.StateDir,
		RecordWatchChildren: watchChildren, RecordWatchRepos: watchRepos, RecordWatchValues: watchValues, Scope: inRoot, Log: log,
	})
}

func (r *Requester) restore(ctx context.Context, cfg Config, log *slog.Logger) {
	persist := true
	if len(cfg.Restore) > 0 {
		snapshot, err := ParseSnapshot(cfg.Restore)
		if err != nil {
			persist = false
			log.Error("embedded: the saved state could not be read; not saving over it", "err", err.Error())
			if cfg.OnRestoreError != nil {
				cfg.OnRestoreError(err)
			}
		} else {
			r.vault.Restore(snapshot.Vault)
			r.seq = snapshot.Seq
		}
	}
	if cfg.OnSnapshot == nil || !persist {
		return
	}
	r.wake = make(chan struct{}, 1)
	r.vault.Provider().SetOnChange(func() {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	})
	go r.deliver(ctx, cfg.OnSnapshot, log)
}

func (r *Requester) deliver(ctx context.Context, fn func([]byte), log *slog.Logger) {
	emit := func() {
		raw, err := r.encode()
		if err != nil {
			log.Warn("embedded: encoding the snapshot", "err", err.Error())
			return
		}
		fn(raw)
	}
	for {
		select {
		case <-r.wake:
			emit()
		case <-ctx.Done():
			select {
			case <-r.wake:
				emit()
			default:
			}
			return
		}
	}
}

func (r *Requester) encode() ([]byte, error) {
	r.snapMu.Lock()
	defer r.snapMu.Unlock()
	r.seq++
	return json.Marshal(Snapshot{Version: SnapshotVersion, Seq: r.seq, Vault: r.vault.Snapshot()})
}

func ParseSnapshot(raw []byte) (Snapshot, error) {
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("embedded: decoding the snapshot: %w", err)
	}
	if snapshot.Version != SnapshotVersion {
		return Snapshot{}, fmt.Errorf("embedded: unsupported snapshot version %d (want %d)", snapshot.Version, SnapshotVersion)
	}
	return snapshot, nil
}

func (r *Requester) createRoot(ctx context.Context, cfg Config) error {
	kc, err := kube.ForWorkspace(cfg.Rest, RootWorkspace)
	if err != nil {
		return err
	}
	resource := kc.Resource(v1alpha1.MarketRequesterGVR)
	comms := &v1alpha1.MarketComms{Mode: v1alpha1.MarketCommsModeFirehose}
	provider := policySandboxProvider(cfg.PolicySandbox)
	components := policySandboxComponents(provider)
	if cur, err := kube.Get[v1alpha1.MarketRequester](ctx, resource, AppName); err == nil {
		if cur.Spec.Comms != nil && cur.Spec.Comms.Mode == comms.Mode && cur.Spec.SecretsFallback == v1alpha1.SecretsFallbackSealed &&
			componentsPolicySandbox(cur.Spec.Components) == provider {
			return nil
		}
		spec := map[string]any{"comms": comms, "secretsFallback": v1alpha1.SecretsFallbackSealed}
		if components != nil {
			spec["components"] = components
		}
		patch, err := json.Marshal(map[string]any{"spec": spec})
		if err != nil {
			return err
		}
		_, err = kube.Patch[v1alpha1.MarketRequester](ctx, resource, AppName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager})
		return err
	}
	obj := &v1alpha1.MarketRequester{Spec: v1alpha1.MarketRequesterSpec{
		Profile: marketrequester.RequesterProfileDefault,
		Identity: v1alpha1.ApplicationIdentity{
			Mode:             cfg.IdentityMode,
			Handle:           cfg.Handle,
			OAuthClientID:    cfg.OAuthClientID,
			OAuthRedirectURI: cfg.OAuthRedirectURI,
			Did:              cfg.DID,
			PdsURL:           cfg.PDSURL,
			Association:      v1alpha1.AssociationSkip,
		},
		Network:         &v1alpha1.MarketNetworkValue{PLCDirectoryURL: PLCDirectoryURL},
		Comms:           comms,
		SecretsFallback: v1alpha1.SecretsFallbackSealed,
		Components:      components,
	}}
	obj.APIVersion = v1alpha1.GroupVersion.String()
	obj.Kind = "MarketRequester"
	obj.Name = AppName
	_, err = kube.Apply[*v1alpha1.MarketRequester](ctx, resource, obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func presentPolicySandbox(host PolicySandbox) PolicySandbox {
	if host == nil {
		return nil
	}
	switch v := reflect.ValueOf(host); v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return nil
		}
	}
	return host
}

var policyEngineKey = compose.ComponentKey(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent)

func policySandboxComponents(provider string) v1alpha1.ComponentOverrides {
	if provider == "" {
		return nil
	}
	return v1alpha1.ComponentOverrides{policyEngineKey: {Settings: &v1alpha1.ComponentSettings{
		PolicyEngine: &v1alpha1.PolicyEngineSettings{Sandbox: &v1alpha1.PolicyEngineSandboxSettings{Provider: provider}},
	}}}
}

func componentsPolicySandbox(components v1alpha1.ComponentOverrides) string {
	settings := components[policyEngineKey].Settings
	if settings == nil || settings.PolicyEngine == nil || settings.PolicyEngine.Sandbox == nil {
		return ""
	}
	return settings.PolicyEngine.Sandbox.Provider
}

func (r *Requester) Close() error {
	if r == nil {
		return nil
	}
	if r.exposure != nil {
		r.exposure.Close()
	}
	if r.cancel != nil {
		r.cancel()
	}
	select {
	case <-r.done:
	case <-time.After(CloseTimeout):
	}
	return nil
}

func (r *Requester) CompleteOAuth(ctx context.Context, req *http.Request) error {
	if r == nil || r.identity == nil {
		return errors.New("embedded: the identity controller is not running")
	}
	return r.identity.CompleteBrowserAuthorization(ctx, RootWorkspace, IdentityName, req)
}

func (r *Requester) Lookup(host, path string) (http.Handler, bool) {
	if r == nil || r.exposure == nil {
		return nil, false
	}
	return r.exposure.Lookup(host, path)
}

func (r *Requester) Snapshot() ([]byte, error) {
	if r == nil || r.vault == nil {
		return nil, errors.New("embedded: the vault is not running")
	}
	return r.encode()
}

func Directory() identity.Directory {
	client := &http.Client{Timeout: 10 * time.Second}
	return &identity.BaseDirectory{
		PLCURL:     identity.DefaultPLCURL,
		HTTPClient: *client,
		PLCClient:  client,
	}
}

func inRoot(ws string) bool { return ws == RootWorkspace || strings.HasPrefix(ws, RootWorkspace+":") }

var schemes = []func(*kruntime.Scheme) error{
	v1alpha1.AddToScheme,
	exposurev1alpha1.AddToScheme,
	eventsv1alpha1.AddToScheme,
	policyv1alpha1.AddToScheme,
	pdsv1alpha1.AddToScheme,
}

func workspaceClients(base *rest.Config) func(string) (ctrlclient.Client, error) {
	scheme := kruntime.NewScheme()
	for _, add := range schemes {
		_ = add(scheme)
	}
	return func(ws string) (ctrlclient.Client, error) {
		cfg := rest.CopyConfig(base)
		cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + ws
		return ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	}
}

func providerReady(ctx context.Context, restCfg *rest.Config) bool {
	kc, err := kube.ForWorkspace(restCfg, RootWorkspace)
	if err != nil {
		return false
	}
	items, err := kube.List[map[string]any](ctx, kc.Resource(kcpgvr.APIBindings), metav1.ListOptions{})
	if err != nil {
		return false
	}
	ready := map[string]bool{}
	for _, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		if name, _ := meta["name"].(string); name != "" {
			ready[name] = bindingReady(item)
		}
	}
	for _, name := range providerBindings {
		if !ready[name] {
			return false
		}
	}
	return true
}

func bindingReady(item map[string]any) bool {
	status, _ := item["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Ready" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func applyProvider(parent context.Context, cfg Config, identities ProviderIdentities, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(parent, ApplyStallTimeout)
	defer cancel()
	applier, err := kcpapply.New(cfg.Rest, kcpapply.Options{Base: RootWorkspace, FieldManager: FieldManager, WaitTimeout: 3 * time.Minute, Log: log})
	if err != nil {
		return fmt.Errorf("embedded: manifest applier: %w", err)
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	restored := false
	for _, name := range providerSets {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("embedded: deploy/sets.yaml names no %s set", name)
		}
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return fmt.Errorf("embedded: decoding values for %s: %w", name, err)
		}
		start := time.Now()
		for _, part := range splitAfterPhase(set, providerWorkspacePhase) {
			if _, err := applier.Apply(ctx, part, values); err != nil {
				return fmt.Errorf("embedded: applying the %s set: %w", name, err)
			}
			if name == providerSets[0] && endsWithPhase(part, providerWorkspacePhase) {
				if err := restoreProviderIdentities(ctx, cfg.Rest, cfg.ProviderWorkspace, identities, log); err != nil {
					return err
				}
				restored = true
			}
		}
		if name == providerSets[0] && !restored && len(identities.Identities) > 0 {
			return fmt.Errorf("embedded: the %s set has no %s phase, so the provider identities cannot be restored before its APIExports", name, providerWorkspacePhase)
		}
		log.Info("embedded: provider set applied", "set", name, "seconds", time.Since(start).Seconds())
	}
	return nil
}
