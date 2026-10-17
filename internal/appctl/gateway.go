package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	GatewayRootName        = "gateway"
	RootVaultAsset         = "bao"
	rootVaultAppRole       = "approle"
	rootVaultPKIMount      = "pki"
	rootVaultKVMount       = "secret"
	rootVaultPKIMaxTTL     = 8760 * time.Hour
	ProfileGatewayProd     = "prod"
	ProfileGatewayNetwork  = "market-network"
	GatewaySet             = "gateway"
	GatewayApp             = "gateway"
	WebAppComponent        = "socialweb-computer-k8s"
	WebAppWebDir           = "socialweb-computer-k8s/web"
	WebAppVaultClient      = "socialweb-computer-k8s"
	WebAppVaultMount       = "socialweb-computer-k8s"
	RequesterVaultMount    = "requester"
	GatewayRequesterPolicy = "gateway-requester"
	GatewayPdsPolicy       = "gateway-repo-key"
	GatewayPdsGrant        = "socialweb-computer-k8s-pds"
	RequesterAssociate     = "requester_associate"
	NodeGuestCurlHome      = "/root"
)

type WebApp struct {
	FrontKubeconfig string
	ClusterCADir    string
	DownloadsDir    string
}

func duration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func keyType(kind string) string {
	switch strings.ToLower(kind) {
	case "ecdsa", "ec":
		return "ec"
	}
	return kind
}

func RootVaultWriterRules() []v1alpha1.OpenBaoPolicyRule {
	cu := []string{"create", "update"}
	return []v1alpha1.OpenBaoPolicyRule{
		{Path: rootVaultPKIMount + "/root/generate/internal", Capabilities: cu},
		{Path: rootVaultPKIMount + "/cert/ca", Capabilities: []string{"read"}},
		{Path: rootVaultKVMount + "/data/socialweb/*", Capabilities: []string{"create", "read", "update"}},
	}
}

func RootVaultDeclared() *v1alpha1.OpenBaoServerDeclared {
	layout := vaultlayout.Default()
	d := &v1alpha1.OpenBaoServerDeclared{}
	for _, m := range layout.Mounts {
		if m.Vault != vaultlayout.Root {
			continue
		}
		sm := v1alpha1.OpenBaoSecretMount{Path: strings.Trim(m.Path, "/"), Type: string(m.Kind)}
		if m.Kind == vaultlayout.MountPKI {
			ttl := m.MaxLeaseTTL
			if ttl <= 0 {
				ttl = rootVaultPKIMaxTTL
			}
			sm.MaxLeaseTTL = ttl.String()
		}
		d.SecretMounts = append(d.SecretMounts, sm)
	}
	d.AuthMounts = append(d.AuthMounts, v1alpha1.OpenBaoAuthMount{Path: rootVaultAppRole, Type: "approle"})
	for _, p := range layout.Policies {
		if p.Vault == vaultlayout.Root {
			d.Policies = append(d.Policies, v1alpha1.OpenBaoPolicy{Name: p.Name, HCL: p.HCL})
		}
	}
	d.Policies = append(d.Policies, v1alpha1.OpenBaoPolicy{Name: vaultlayout.PolicyRootWriter, Rules: RootVaultWriterRules()})
	for _, r := range layout.AppRoles {
		if r.Vault == vaultlayout.Root {
			d.AppRoles = append(d.AppRoles, v1alpha1.OpenBaoAppRole{Mount: rootVaultAppRole, Name: r.Name, Policies: r.Policies,
				TokenTTL: duration(r.TokenTTL), TokenMaxTTL: duration(r.TokenMaxTTL), SecretIDTTL: duration(r.SecretIDTTL)})
		}
	}
	for _, r := range layout.PKIRoles {
		if r.Vault != vaultlayout.Root {
			continue
		}
		d.PKIRoles = append(d.PKIRoles, v1alpha1.OpenBaoPKIRole{Mount: strings.Trim(r.Mount, "/"), Name: r.Name,
			AllowedDomains: r.AllowedDomains, AllowSubdomains: r.AllowSubdomains, AllowBareDomains: r.AllowBareDomains,
			AllowIPSANs: r.AllowIPSANs, EnforceHostnames: r.EnforceHostnames, RequireCN: r.RequireCN, ClientFlag: r.ClientAuth,
			ServerFlag: r.ServerAuth, KeyType: keyType(r.KeyType), KeyBits: int32(r.KeyBits), KeyUsage: r.KeyUsage,
			TTL: duration(r.TTL), MaxTTL: duration(r.MaxTTL), NoStore: true})
	}
	for _, m := range d.SecretMounts {
		if m.Type == string(vaultlayout.MountPKI) {
			d.PKITidy = append(d.PKITidy, v1alpha1.OpenBaoPKITidy{Mount: m.Path})
		}
	}
	return d
}

func RequesterVaultRules() []v1alpha1.OpenBaoPolicyRule {
	return []v1alpha1.OpenBaoPolicyRule{
		{Path: WebAppVaultMount + "/data/contracts/*", Capabilities: []string{"read"}},
	}
}

func RequesterKeysPath() string {
	return "identity/" + RequesterIdentity + "/keys"
}

func RootVaultSpec(g v1alpha1.GatewaySpec) (v1alpha1.OpenBaoServerSpec, error) {
	declared := RootVaultDeclared()
	declared.Policies = append(declared.Policies, v1alpha1.OpenBaoPolicy{Name: GatewayRequesterPolicy, Rules: RequesterVaultRules()},
		v1alpha1.OpenBaoPolicy{Name: GatewayPdsPolicy, Rules: []v1alpha1.OpenBaoPolicyRule{
			{Path: RequesterVaultMount + "/data/" + RequesterKeysPath(), Capabilities: []string{"read"}}}})
	declared.Policies = append(declared.Policies, AtprotoPolicies()...)
	plugin, err := AtprotoPlugin(g.Options, g.Network, GatewayAtprotoScope())
	if err != nil {
		return v1alpha1.OpenBaoServerSpec{}, err
	}
	return v1alpha1.OpenBaoServerSpec{Asset: &v1alpha1.HostProcessAsset{Name: RootVaultAsset},
		Clients: []v1alpha1.OpenBaoServerClient{{Name: vaultlayout.RootClient, Policies: []string{vaultlayout.PolicyRootWriter}},
			{Name: WebAppVaultClient, KVMount: WebAppVaultMount},
			{Name: RequesterVaultClient, KVMount: RequesterVaultMount, Policies: []string{GatewayRequesterPolicy}},
			{Name: PdsVaultClient, Policies: []string{GatewayPdsPolicy}}},
		Declared: declared, Plugins: []v1alpha1.OpenBaoPlugin{plugin}}, nil
}

func GatewayRequesterIdentity(g v1alpha1.GatewaySpec) v1alpha1.MarketIdentitySpec {
	return OperatorWatched(IdentitySpecOf(v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo}), g.Operator)
}

func sameSpec(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func (ctl *Controller) ensureRootVault(ctx context.Context, owner string, g v1alpha1.GatewaySpec) (*v1alpha1.OpenBaoServer, error) {
	ri, err := ctl.client(ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return nil, err
	}
	want, err := RootVaultSpec(g)
	if err != nil {
		return nil, err
	}
	for range 5 {
		u, err := ri.Get(ctx, vaultlayout.RootServerName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			o := v1alpha1.OpenBaoServer{ObjectMeta: metav1.ObjectMeta{Name: vaultlayout.RootServerName,
				Labels: map[string]string{OwnerLabel: owner}}, Spec: want}
			nu, err := toUnstructured(&o, "OpenBaoServer")
			if err != nil {
				return nil, err
			}
			if u, err = ri.Create(ctx, nu, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return nil, err
			}
			ctl.c.log().Info("declared the gateway's root vault", "openbaoserver", vaultlayout.RootServerName)
		} else if err != nil {
			return nil, err
		}
		var o v1alpha1.OpenBaoServer
		if err := fromUnstructured(u, &o); err != nil {
			return nil, err
		}
		if sameSpec(o.Spec, want) && o.Labels[OwnerLabel] == owner {
			return &o, nil
		}
		o.Spec = want
		if o.Labels == nil {
			o.Labels = map[string]string{}
		}
		o.Labels[OwnerLabel] = owner
		nu, err := toUnstructured(&o, "OpenBaoServer")
		if err != nil {
			return nil, err
		}
		if _, err := ri.Update(ctx, nu, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return nil, err
		}
		ctl.c.log().Info("the gateway's root vault declaration changed", "openbaoserver", vaultlayout.RootServerName)
	}
	return nil, fmt.Errorf("appctl: the OpenBaoServer %s stayed in conflict", vaultlayout.RootServerName)
}

func RootVaultSynced(o *v1alpha1.OpenBaoServer) bool {
	if o == nil || o.Status.ObservedGeneration != o.Generation || o.Status.Phase != v1alpha1.OpenBaoServerPhaseReady {
		return false
	}
	c := meta.FindStatusCondition(o.Status.Conditions, openbaoctl.ConditionSynced)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == o.Generation
}

func (ctl *Controller) reconcileGateway(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.Gateway
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile, Components: []v1alpha1.ApplicationComponent{}}
	if u.GetDeletionTimestamp() != nil {
		ws := ctl.c.workspace() + ":" + ComponentWorkspace
		records, err := ctl.releaseRecords(ctx, ws, ws, owner)
		if err != nil {
			return err
		}
		if records > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d AtprotoRecord(s) leaving the repo before the pds DenoPod stops", records)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		left, err := ctl.deletePods(ctx, ws, owner, nil)
		if err != nil {
			return err
		}
		if left == 0 {
			if err := ctl.revokeWebApp(ctx); err != nil {
				return err
			}
			if left, err = ctl.deleteVaults(ctx, owner); err != nil {
				return err
			}
		}
		if left > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		return ctl.setFinalizer(ctx, GatewayGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, GatewayGVR, u, true); err != nil {
			return err
		}
	}
	switch o.Spec.Profile {
	case ProfileGatewayProd, ProfileGatewayNetwork:
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, fmt.Sprintf("profile %q: want %s or %s", o.Spec.Profile, ProfileGatewayProd, ProfileGatewayNetwork)
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	vault, err := ctl.ensureRootVault(ctx, owner, o.Spec)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its root vault: "+err.Error()
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	vaultReady := RootVaultSynced(vault)
	st.Components = append(st.Components, v1alpha1.ApplicationComponent{Kind: "OpenBaoServer", Name: vault.Name,
		Workspace: ctl.c.workspace(), Phase: string(vault.Status.Phase), Ready: vaultReady})
	var core []v1alpha1.ApplicationListener
	if ctl.c.Listeners != nil {
		core = ctl.c.Listeners(owner)
	}
	st.Listeners = append(st.Listeners, core...)
	webAppReady := true
	awaitingOperator := ""
	webAppVault, haveWebAppVault := WebAppVaultOf(vault)
	requesterVault, haveRequesterVault := RequesterVaultOf(vault)
	haveVault := haveWebAppVault && haveRequesterVault
	if ctl.c.WebApp != nil && !haveVault {
		webAppReady = false
	}
	if ctl.c.WebApp != nil && haveVault {
		ws := ctl.c.workspace() + ":" + ComponentWorkspace
		if ctl.applied[string(o.UID)] != o.Generation || ctl.lostComponent(ctx, owner) != "" {
			values, err := GatewayValues(o, ctl.c.workspace(), ComponentStateDir(ctl.c.Layout.StateDir, WebAppComponent),
				ComponentStateDir(ctl.c.Layout.StateDir, RequesterComponent), *ctl.c.WebApp, webAppVault, requesterVault)
			if err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			if _, err := ctl.applyProfile(ctx, GatewaySet, o.Spec.Profile, owner, false, &values); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its workspace: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			if err := ctl.ensureIdentitySpec(ctx, ws, RequesterIdentity, GatewayRequesterIdentity(o.Spec)); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the requester's MarketIdentity: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			server, err := ctl.webAppIdentity(ctx, o, owner, *ctl.c.WebApp)
			if err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the web app's kcp identity: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			values.KcpServer = server
			if err := ctl.ensureRepoKey(ctx, vaultlayout.RootServerName, RequesterVaultClient, RequesterKeysPath()); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the requester's repo key: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			if values.Pds, err = GatewayPds(values, ComponentStateDir(ctl.c.Layout.StateDir, PdsComponent), o.Spec.Network); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			pods, err := ctl.applyProfile(ctx, GatewaySet, o.Spec.Profile, owner, true, &values)
			if err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its components: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			if _, err := ctl.deletePods(ctx, ws, owner, pods); err != nil {
				return err
			}
			ctl.applied[string(o.UID)] = o.Generation
			ctl.c.log().Info("applied the Gateway's components", "name", o.Name, "profile", o.Spec.Profile, "denopods", pods)
		}
		pods, err := ctl.ownedPods(ctx, ws, owner)
		if err != nil {
			return err
		}
		webAppReady = len(pods) > 0
		var components []v1alpha1.ApplicationListener
		for i := range pods {
			v := podViewOf(ws, &pods[i])
			st.Components = append(st.Components, v.component())
			components = append(components, v.Listeners...)
			webAppReady = webAppReady && v.Ready && len(v.Listeners) > 0
		}
		st.Listeners = append(st.Listeners, components...)
		if id, err := ctl.identityIn(ctx, ws, RequesterIdentity); err == nil {
			st.Did = id.Status.Did
			if a := id.Status.PendingAction; a != nil && id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingScan {
				awaitingOperator = "waiting for the operator's claim record (badgeBlueKeys keyId " + id.Status.Did + " with the nonce n of " + a.Url + ")"
			}
			if m := o.Spec.MarketNetwork; o.Spec.Profile == ProfileGatewayNetwork && m != nil && m.OwnerSessionsFile != "" {
				if err := ctl.associateGatewayOperator(ctx, id, m.OwnerSessionsFile, o.Spec.Network); err != nil {
					ctl.c.log().Warn("associating the requester's DID with the market network operator account", "err", err.Error())
				}
			}
		}
		if ctl.c.OnComponents != nil {
			ctl.c.OnComponents(owner, components)
		}
	}
	switch {
	case !vaultReady:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the root vault to be Ready and synced"
	case ctl.c.Listeners != nil && len(core) == 0:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the root vault is Ready; waiting for the core's members to report their listeners"
	case !webAppReady && awaitingOperator != "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, awaitingOperator
	case !webAppReady:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the "+WebAppComponent+" DenoPod to be ready and report its listener"
	default:
		st = ready(st, "the root vault is Ready and every member and component reported its listener")
	}
	return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
}

func GatewayPds(v manifests.Gateway, stateDir string, n *v1alpha1.MarketNetworkValue) (manifests.RepoPds, error) {
	p := manifests.RepoPds{StateDir: stateDir, Identity: RequesterIdentity, KcpServer: v.KcpServer, KcpWorkspace: v.IdentityWorkspace,
		VaultServer: v.Requester.Vault.Server, VaultClient: PdsVaultClient, VaultKcpServer: v.KcpServer + "/clusters/" + v.GatewayWorkspace,
		VaultMount: RequesterVaultMount, VaultMountClient: RequesterVaultClient, VaultWorkspace: v.GatewayWorkspace}
	p.AssociateService = RequesterAssociate
	extra := []string{"--associate-service-id", RequesterAssociate}
	var ca string
	var err error
	if n != nil {
		p.PlcDirectoryUrl, p.IngressProxyHost, ca = n.PlcDirectoryUrl, n.IngressProxyHost, n.CaBundleRef
		if p.WatchRelayUrls, err = listFragment(append(append([]string{}, n.RelayUrls...), n.FirehoseUrls...)...); err != nil {
			return p, err
		}
		if hosts := NetworkHostsArg(n.Hosts); hosts != "" {
			extra = append(extra, "--network-hosts", hosts)
		}
	}
	if p.Trust, err = listFragment(ca); err != nil {
		return p, err
	}
	if p.Read, err = listFragment(ca); err != nil {
		return p, err
	}
	p.ExtraArgs, err = fragment(extra)
	return p, err
}

func listJSON(items []string) (string, error) {
	if items == nil {
		items = []string{}
	}
	b, err := json.Marshal(items)
	return string(b), err
}

func clientVaultOf(o *v1alpha1.OpenBaoServer, client string) (manifests.GatewayVault, bool) {
	if o == nil || !RootVaultSynced(o) || o.Status.Refs == nil || o.Status.Refs.CA == "" {
		return manifests.GatewayVault{}, false
	}
	for _, c := range o.Status.Clients {
		if c.Name == client && c.ClientCert != "" {
			return manifests.GatewayVault{Server: o.Name, Client: c.Name}, true
		}
	}
	return manifests.GatewayVault{}, false
}

func WebAppVaultOf(o *v1alpha1.OpenBaoServer) (manifests.GatewayVault, bool) {
	return clientVaultOf(o, WebAppVaultClient)
}

func RequesterVaultOf(o *v1alpha1.OpenBaoServer) (manifests.GatewayVault, bool) {
	return clientVaultOf(o, RequesterVaultClient)
}

func GatewayValues(o v1alpha1.Gateway, workspace, stateDir, requesterStateDir string, w WebApp, vault, requesterVault manifests.GatewayVault) (manifests.Gateway, error) {
	if !filepath.IsAbs(w.FrontKubeconfig) {
		return manifests.Gateway{}, fmt.Errorf("appctl: the %s web app needs an absolute kcp front kubeconfig", WebAppComponent)
	}
	v := manifests.Gateway{StateDir: stateDir, WebDir: manifests.UnderTSRoot(WebAppWebDir),
		GatewayWorkspace: workspace, GatewayName: o.Name, TLS: manifests.TLSNone, Vault: vault,
		IdentityWorkspace: workspace + ":" + ComponentWorkspace,
		Requester:         manifests.GatewayRequester{StateDir: requesterStateDir, Vault: requesterVault}}
	var trust, read []string
	var extra, requesterExtra []string
	if n := o.Spec.Network; n != nil {
		if n.CaBundleRef != "" {
			v.TLS, trust = manifests.TLSTrust, []string{n.CaBundleRef}
			if o.Spec.Profile == ProfileGatewayNetwork {
				q, err := json.Marshal(n.CaBundleRef)
				if err != nil {
					return manifests.Gateway{}, err
				}
				v.ExtraProjected = "  - {name: network-ca, file: {path: " + string(q) + "}}\n"
				v.Requester.ExtraProjected = v.ExtraProjected
				requesterExtra = append(requesterExtra, "--guest-curl-home", NodeGuestCurlHome)
			}
		}
		for _, kv := range [][2]string{
			{"--plc-directory-url", n.PlcDirectoryUrl},
			{"--ingress-proxy-host", n.IngressProxyHost},
			{"--relay-url", strings.Join(n.RelayUrls, ",")},
			{"--network-hosts", NetworkHostsArg(n.Hosts)},
		} {
			if kv[1] != "" {
				extra = append(extra, kv[0], kv[1])
				requesterExtra = append(requesterExtra, kv[0], kv[1])
			}
		}
		if n.QrOrigin != "" {
			requesterExtra = append(requesterExtra, "--qr-origin", n.QrOrigin)
		}
	}
	if len(o.Spec.BidderDids) > 0 {
		extra = append(extra, "--node-extra-bidder-dids", strings.Join(o.Spec.BidderDids, ","))
	}
	opts, err := OptionArgs(GatewayApp, o.Spec.Options)
	if err != nil {
		return manifests.Gateway{}, err
	}
	extra = append(extra, opts...)
	if w.DownloadsDir != "" {
		if !filepath.IsAbs(w.DownloadsDir) {
			return manifests.Gateway{}, fmt.Errorf("appctl: the %s downloads dir %q is not absolute", WebAppComponent, w.DownloadsDir)
		}
		extra = append(extra, "--downloads-dir", w.DownloadsDir)
		read = append(read, w.DownloadsDir)
	}
	if o.Spec.Profile == ProfileGatewayNetwork {
		extra = append(extra, "--test-node-download-host", guestnet.HostAddress)
	}
	if w.ClusterCADir != "" {
		if !filepath.IsAbs(w.ClusterCADir) {
			return manifests.Gateway{}, fmt.Errorf("appctl: the cluster CA dir %q is not absolute", w.ClusterCADir)
		}
		extra = append(extra, "--cluster-ca-dir", w.ClusterCADir)
		read = append(read, w.ClusterCADir)
	}
	if len(read) > 0 {
		if v.ExtraRead, err = fragment(read); err != nil {
			return manifests.Gateway{}, err
		}
	}
	if v.Trust, err = listJSON(trust); err != nil {
		return manifests.Gateway{}, err
	}
	if v.ExtraArgs, err = fragment(extra); err != nil {
		return manifests.Gateway{}, err
	}
	if m := o.Spec.MarketNetwork; m != nil {
		if o.Spec.Profile != ProfileGatewayNetwork {
			return manifests.Gateway{}, fmt.Errorf("appctl: spec.marketNetwork is a value of the %s profile only, not of %s", ProfileGatewayNetwork, o.Spec.Profile)
		}
		var args, read []string
		for _, file := range []string{m.OwnerSessionsFile, m.NodeUserDataBaseFile} {
			if file != "" && !filepath.IsAbs(file) {
				return manifests.Gateway{}, fmt.Errorf("appctl: spec.marketNetwork names %q, not an absolute path", file)
			}
		}
		if m.NodeUserDataBaseFile != "" {
			args, read = append(args, "--test-node-user-data-base-file", m.NodeUserDataBaseFile), append(read, m.NodeUserDataBaseFile)
		}
		if len(m.NodeGuestHostAliases) > 0 {
			requesterExtra = append(requesterExtra, "--guest-host-aliases", strings.Join(m.NodeGuestHostAliases, ","))
		}
		if v.TestArgs, err = fragment(args); err != nil {
			return manifests.Gateway{}, err
		}
		if v.TestRead, err = fragment(read); err != nil {
			return manifests.Gateway{}, err
		}
	}
	if v.Requester.ExtraArgs, err = fragment(requesterExtra); err != nil {
		return manifests.Gateway{}, err
	}
	return v, nil
}
