package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
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

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	GatewayRootName         = "gateway"
	RootVaultAsset          = "bao"
	rootVaultAppRole        = "approle"
	rootVaultPKIMount       = "pki"
	rootVaultKVMount        = "secret"
	rootVaultPKIMaxTTL      = 8760 * time.Hour
	ProfileGatewayProd      = "prod"
	ProfileGatewayNetwork   = "market-network"
	GatewaySet              = "gateway"
	GatewayApp              = "gateway"
	WebAppComponent         = "socialweb-computer-k8s"
	WebAppWebDir            = "socialweb-computer-k8s/web"
	WebAppVaultClient       = "socialweb-computer-k8s"
	WebAppVaultMount        = "socialweb-computer-k8s"
	RequesterVaultMount     = "requester"
	GatewayRequesterPolicy  = "gateway-requester"
	GatewayPdsPolicy        = "gateway-repo-key"
	GatewayPdsGrant         = "socialweb-computer-k8s-pds"
	RequesterAssociate      = "requester_associate"
	NodeGuestCurlHome       = "/root"
	GatewayAudienceOption   = "gateway-audience"
	GatewayNodePolicy       = "node-policy"
	GatewayNodePolicyArgs   = "node-policy-args"
	GatewayNodePollOption   = "node-poll-interval-ms"
	GatewayPostSignInOption = "post-sign-in-url"
)

var GatewayRequiredDownloads = []string{"socialweb-meshd-x86_64", "socialweb-meshd-aarch64"}

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

func GatewayRequesterIdentity(g v1alpha1.GatewaySpec) v1alpha1.ApplicationIdentity {
	id := g.Identity
	if id.Mode == "" {
		id.Mode = v1alpha1.MarketIdentityMode(optionValue(g.Options, "atproto-identity-mode"))
	}
	if id.Handle == "" {
		id.Handle = optionValue(g.Options, "atproto-handle")
	}
	if id.PdsURL == "" {
		id.PdsURL = optionValue(g.Options, "atproto-pds-url")
	}
	if id.Association == "" && optionValue(g.Options, "skip-association") == "true" {
		id.Association = v1alpha1.AssociationSkip
	}
	return id
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

func gatewayNodeMarket(o v1alpha1.Gateway, workspace string) *v1alpha1.GatewayNodeMarket {
	m := o.Spec.MarketNetwork
	if m == nil {
		return nil
	}
	out := &v1alpha1.GatewayNodeMarket{OwnerSessionsFile: m.OwnerSessionsFile, UserDataBaseFile: m.NodeUserDataBaseFile,
		HomeWorkspace: workspace, ContractWorkspace: workspace,
		NodePolicy: optionValue(o.Spec.Options, GatewayNodePolicy), NodePolicyArgs: optionValue(o.Spec.Options, GatewayNodePolicyArgs),
		ExtraBidderDIDs: append([]string(nil), o.Spec.BidderDids...)}
	if ms := optionInt(o.Spec.Options, GatewayNodePollOption); ms > 0 {
		out.PollIntervalSeconds = int32((ms + 999) / 1000)
	}
	return out
}

func (ctl *Controller) gatewayDesired(ctx context.Context, o v1alpha1.Gateway, plan accountPlan, id *v1alpha1.MarketIdentity, ws, stateRoot, downloadsDir, clusterCADir, workspace, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *commonv1.ObjectRef) ([]desiredComponent, error) {
	suspend := o.Spec.Suspend
	network := o.Spec.Network
	ident := componentIdentity(id, o.Spec.Identity.Handle)
	vault := ComponentRefFor("OpenBaoServer", vaultlayout.RootServerName)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	if network != nil && network.QrOrigin != "" {
		if h := hostOf(network.QrOrigin); h != "" && !slices.Contains(hostnames, h) {
			hostnames = append(hostnames, h)
		}
	}
	var clusterProvider *v1alpha1.GatewayClusterProvider
	if o.Spec.ClusterProvider != nil {
		cp := *o.Spec.ClusterProvider
		clusterProvider = &cp
	}
	var storage *v1alpha1.ComponentStorage
	if downloadsDir != "" || clusterCADir != "" {
		storage = &v1alpha1.ComponentStorage{BlobsDir: downloadsDir, CertsDir: clusterCADir}
	}
	var tlsSpec *v1alpha1.ComponentTLS
	if network != nil {
		tlsSpec = &v1alpha1.ComponentTLS{Enabled: true, Hostnames: hostnames, CABundle: network.CABundle}
	}
	publicOrigin := optionValue(o.Spec.Options, publicOriginOption)
	audience := optionValue(o.Spec.Options, GatewayAudienceOption)
	if audience == "" {
		audience = ident.Did
	}
	postSignInURL := optionValue(o.Spec.Options, GatewayPostSignInOption)
	if postSignInURL == "" && publicOrigin != "" {
		postSignInURL = strings.TrimRight(publicOrigin, "/") + "/"
	}
	var requiredDownloads []string
	if downloadsDir != "" && (publicOrigin != "" || (tlsSpec != nil && len(hostnames) > 0)) {
		requiredDownloads = append([]string(nil), GatewayRequiredDownloads...)
	}
	mode, requesterOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := requesterComponentFields(o.Spec.Options, plan.Mode)
	var pds desiredComponent
	if plan.Local {
		pdsSpec, err := appPdsSpec(stateRoot, network, pdsHandle, vault, hostnames, suspend, pdsPublicURL, requireIngress, ingressRef)
		if err != nil {
			return nil, err
		}
		pds = desiredComponent{Kind: ComponentKindPds, Name: PdsComponent, Spec: pdsSpec}
	}
	var requesterPdsRef *commonv1.ObjectRef
	requesterPdsDID := ""
	pdsAccessToken := plan.accessToken()
	if plan.Local {
		requesterPdsRef = ComponentRefFor(ComponentKindPds, PdsComponent)
		pdsAccessToken = pdsAccessTokenFor("requester")
	} else {
		requesterPdsRef, requesterPdsDID = plan.PdsRef, plan.PdsDID
	}
	requester := desiredComponent{Kind: ComponentKindRequester, Name: RequesterComponent, Spec: v1alpha1.RequesterSpec{
		ComponentSpec:     NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindRequester, RequesterComponent), network, suspend),
		Identity:          ident,
		PdsRef:            requesterPdsRef,
		PdsDID:            requesterPdsDID,
		PDSAccessToken:    pdsAccessToken,
		PolicyEngineRef:   ComponentRefFor(ComponentKindPolicyEngine, PolicyEngineComponent),
		VaultRef:          vault,
		Hostnames:         hostnames,
		Mode:              mode,
		PublicOrigin:      requesterOrigin,
		MarketExposureRef: ctl.requesterMarketExposure(ctx, ws),
		OperatorDID:       requesterOperatorDID(id),
		OAuthClientID:     oauthClientID,
		OAuthRedirectURI:  oauthRedirectURI,
		GuestSSHKeysFile:  guestSSHKeysFile,
		BidderDIDs:        append([]string(nil), o.Spec.BidderDids...),
		AcceptCopies:      int32(optionInt(o.Spec.Options, "accept-copies")),
		Comms:             o.Spec.Comms,
	}}
	gatewayBase := NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindGatewayAPI, GatewayAPIComponent), network, suspend)
	gatewayBase.TLS = tlsSpec
	kubeconfigCAFile := optionValue(o.Spec.Options, "kcp-public-ca-file")
	if kubeconfigCAFile == "" && o.Spec.Profile != ProfileGatewayProd {
		kubeconfigCAFile = filepath.Join(stateRoot, "tls", "tls.crt")
	}
	var gatewayPdsRef *commonv1.ObjectRef
	if plan.Local {
		gatewayPdsRef = ComponentRefFor(ComponentKindPds, PdsComponent)
	}
	gatewayAPI := desiredComponent{Kind: ComponentKindGatewayAPI, Name: GatewayAPIComponent, Spec: v1alpha1.GatewayAPISpec{
		ComponentSpec:     gatewayBase,
		Identity:          ident,
		PdsRef:            gatewayPdsRef,
		RequesterRef:      ComponentRefFor(ComponentKindRequester, RequesterComponent),
		VaultRef:          vault,
		Hostnames:         hostnames,
		Storage:           storage,
		ClusterProvider:   clusterProvider,
		ClusterStateDir:   filepath.Join(stateRoot, "clusterprovider"),
		KubeconfigCAFile:  kubeconfigCAFile,
		PublicOrigin:      publicOrigin,
		Audience:          audience,
		RequiredDownloads: requiredDownloads,
		PostSignInURL:     postSignInURL,
		NodeMarket:        gatewayNodeMarket(o, ws),
	}}
	out := []desiredComponent{policyEngineDesired(suspend), requester, gatewayAPI}
	if plan.Local {
		out = append([]desiredComponent{pds}, out...)
		out = append(out, pdsExposures(ws, PdsComponent, network, hostnames, suspend, ingress)...)
	}
	out = append(out, httpServiceExposures(ws, ComponentKindRequester, RequesterComponent, "http", network, hostnames, true, true, suspend)...)
	out = append(out, httpServiceExposures(ws, ComponentKindGatewayAPI, GatewayAPIComponent, "http", network, hostnames, true, true, suspend)...)
	return applyComponentOverrides(o.Spec.Suspend, o.Spec.Components, out)
}

func (ctl *Controller) reconcileGateway(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.Gateway
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	ws := ctl.c.workspace() + ":" + ComponentWorkspace
	pdsAccountHandle := pdsHandle(ws, "gateway", "")
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile, Components: []v1alpha1.ApplicationComponent{}}
	if u.GetDeletionTimestamp() != nil {
		records, err := ctl.releaseComponentRecords(ctx, ws, owner)
		if err != nil {
			return err
		}
		if records > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d AtprotoRecord(s) leaving the repo before the Pds stops", records)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		left, err := ctl.stopComponents(ctx, owner)
		if err != nil {
			return err
		}
		if left == 0 {
			if left, err = ctl.deleteVaults(ctx, owner); err != nil {
				return err
			}
		}
		if left > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		delete(ctl.applied, string(o.UID))
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
	plan, err := ctl.planAccount(ctx, ws, GatewayRequesterIdentity(o.Spec), "requester",
		accountVault{Server: vaultlayout.RootServerName, Client: RequesterVaultClient, Mount: RequesterVaultMount},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		setOverrideErrorIfAny(&st, o.Generation, err)
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
	stateRoot := ctl.c.Layout.StateDir
	ingressSpec := AppIngressOrDefault(o.Spec.Profile, o.Spec.Ingress, o.Spec.Network)
	requireIngress, ingressRef, ingressConfig, ingressErr := pdsAppIngress(ingressSpec, o.Spec.Network, o.Spec.Profile == ProfileGatewayProd)
	pdsPublicURL, _ := ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, requireIngress)
	downloadsDir, clusterCADir := "", ""
	if ctl.c.WebApp != nil {
		downloadsDir, clusterCADir = ctl.c.WebApp.DownloadsDir, ctl.c.WebApp.ClusterCADir
	}
	if o.Spec.Suspend {
		id, _ := ctl.identityIn(ctx, ws, RequesterIdentity)
		desired, err := ctl.gatewayDesired(ctx, o, plan, id, ctl.c.workspace(), stateRoot, downloadsDir, clusterCADir, ws, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			setOverrideErrorIfAny(&st, o.Generation, err)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if err := ctl.ensureDesired(ctx, u, desired); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		_ = ctl.pruneDesired(ctx, owner, desired)
		snap, err := ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		st.Components = append(st.Components, snap.Components...)
		st.Listeners = snap.Listeners
		if id != nil {
			st.Did = id.Status.Did
		}
		fillComponentStatus(&st, o.Generation, o.Spec.Suspend, o.Spec.Components, desired, snap, nil, ctl.componentDrift(string(o.UID)))
		st.Phase, st.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	if !vaultReady {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the root vault to be Ready and synced"
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	awaitingOperator := ""
	if ctl.applied[string(o.UID)] != o.Generation {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its workspace, its records and its requester identity"
		ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		values := &manifests.Gateway{}
		if o.Spec.Network != nil {
			if pds, err := GatewayPds(*values, stateRoot, o.Spec.Network); err == nil {
				values.Pds = pds
			}
		}
		if err := ctl.applyInfra(ctx, GatewaySet, o.Spec.Profile, owner, values, "association-watch"); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its workspace: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Message = "its account password: " + err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if err := ctl.ensureIdentitySpec(ctx, ws, owner, RequesterIdentity, OperatorWatched(plan.Identity, o.Spec.Operator, RequesterAssociate)); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the requester's MarketIdentity: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the requester's account password: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if plan.Local {
			if err := ctl.ensureRepoKey(ctx, vaultlayout.RootServerName, RequesterVaultClient, RequesterKeysPath()); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the requester's repo key: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
		}
		ctl.applied[string(o.UID)] = o.Generation
	}
	id, err := ctl.identityIn(ctx, ws, RequesterIdentity)
	if apierrors.IsNotFound(err) {
		delete(ctl.applied, string(o.UID))
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's MarketIdentity"
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	if err != nil {
		return err
	}
	if !plan.Local {
		switch id.Status.Phase {
		case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, "the requester's MarketIdentity: "+id.Status.Message
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		case v1alpha1.MarketIdentityPhaseReady:
		default:
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's existing-account MarketIdentity to sign in"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if strings.TrimSpace(id.Status.Did) == "" {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's existing-account MarketIdentity to report a did"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
	}
	desired, err := ctl.gatewayDesired(ctx, o, plan, id, ctl.c.workspace(), stateRoot, downloadsDir, clusterCADir, ws, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		setOverrideErrorIfAny(&st, o.Generation, err)
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	allDesired := desired
	pds, hasPds := desiredPds(desired)
	if hasPds {
		first := pdsApplySet(desired, PdsComponent, componentRefName(ingressRef))
		if requireIngress && pdsPublicURL == "" {
			if applied, err := ctl.pdsPublicURLApplied(ctx, ctl.c.workspace(), PdsComponent); err != nil {
				return err
			} else if applied {
				first = withoutPds(first)
			}
		}
		if err := ctl.ensureDesired(ctx, u, first); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		snap, err := ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		if !snap.ready(ComponentKindPds, PdsComponent) {
			st.Components = append(st.Components, snap.Components...)
			st.Listeners = snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's app Pds to be ready"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if requireIngress {
			if ingressErr != nil {
				setPdsIngressCondition(&st, o.Generation, true, ingressErr)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the app Pds ingress: "+ingressErr.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			if err := ctl.pdsIngressReady(snap, PdsComponent, ingressRef); err != nil {
				setPdsIngressCondition(&st, o.Generation, true, err)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds ingress: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			pdsPublicURL, err = ctl.pdsIngressURL(ctx, ctl.c.workspace(), PdsComponent)
			if err != nil {
				setPdsIngressCondition(&st, o.Generation, true, err)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = pdsIngressPhase(err), "the app Pds ingress: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
			setPdsIngressCondition(&st, o.Generation, true, nil)
		} else {
			pdsPublicURL, _ = ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, false)
			setPdsIngressCondition(&st, o.Generation, false, nil)
		}
		if pdsPublicURL == "" {
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds public URL"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if desired, err = ctl.gatewayDesired(ctx, o, plan, id, ctl.c.workspace(), stateRoot, downloadsDir, clusterCADir, ws, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			setOverrideErrorIfAny(&st, o.Generation, err)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		allDesired = desired
		pds, _ = desiredPds(desired)
		if err := ctl.ensureDesired(ctx, u, []desiredComponent{pds}); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if applied, err := ctl.pdsApplied(ctx, owner); err != nil {
			return err
		} else if !applied {
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds to apply its public URL"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		endpoint := ctl.pdsEndpointIn(ctx, ctl.c.workspace(), PdsComponent)
		if endpoint == "" {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's app Pds endpoint"
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		caPEM := ""
		if o.Spec.Network != nil {
			caPEM = o.Spec.Network.CABundle
		}
		sess, err := ctl.ensurePdsAccountInVault(ctx, vaultlayout.RootServerName, RequesterVaultClient, pdsSessionPath("requester"), endpoint, caPEM, pdsAccountHandle)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds account: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		ctl.ensureAppPdsStatus(ctx, pdsAccountHandle, sess, vaultlayout.RootServerName+":"+pdsSessionPath("requester"), o.Spec.Network != nil)
		changed, err := ctl.syncRequesterRepoKey(stateRoot, sess.Did)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds signing key: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if changed {
			if err := ctl.RestartComponentIn(ctx, ctl.c.workspace(), ComponentKindRequester, RequesterComponent); err != nil && !apierrors.IsNotFound(err) {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "restarting the requester: "+err.Error()
				return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
			}
		}
		if err := ctl.recordPdsIdentity(ctx, ws, RequesterIdentity, pdsAccountHandle, pdsPublicURL, sess.Did, id.Spec.Association); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds identity: "+err.Error()
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		if id, err = ctl.identityIn(ctx, ws, RequesterIdentity); err != nil {
			return err
		}
		if desired, err = ctl.gatewayDesired(ctx, o, plan, id, ctl.c.workspace(), stateRoot, downloadsDir, clusterCADir, ws, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			setOverrideErrorIfAny(&st, o.Generation, err)
			return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
		}
		allDesired = desired
		desired = withoutPds(desired)
	}
	if err := ctl.ensureDesired(ctx, u, desired); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	if err := ctl.ensureMarketStreams(ctx, ws, "requester", o.Spec.Comms, o.Spec.Network, oauthReceiveOnly(plan.Mode)); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		setOverrideErrorIfAny(&st, o.Generation, err)
		return ctl.writeStatus(ctx, GatewayGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, allDesired); err != nil {
		return err
	}
	if id, err = ctl.identityIn(ctx, ws, RequesterIdentity); err != nil {
		return err
	}
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	st.Components = append(st.Components, snap.Components...)
	st.Listeners = snap.Listeners
	st.Did = id.Status.Did
	fillComponentStatus(&st, o.Generation, o.Spec.Suspend, o.Spec.Components, allDesired, snap, nil, ctl.componentDrift(string(o.UID)))
	ctl.ensureAccountRepo(ctx, ws, RequesterIdentity, plan, id.Status.Did, id.Status.PdsURL)
	if a := id.Status.PendingAction; a != nil && id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingScan {
		awaitingOperator = "waiting for the operator's claim record (badgeBlueKeys keyId " + id.Status.Did + " with the nonce n of " + a.URL + ")"
	}
	if m := o.Spec.MarketNetwork; o.Spec.Profile == ProfileGatewayNetwork && m != nil && m.OwnerSessionsFile != "" {
		if err := ctl.associateGatewayOperator(ctx, id, m.OwnerSessionsFile, o.Spec.Network); err != nil {
			ctl.c.log().Warn("associating the requester's DID with the market network operator account", "err", err.Error())
		}
		if err := ctl.reconcileOperatorRecords(ctx, ws, o.Spec.Network, func() (ownerSession, error) { return firstOwnerSession(m.OwnerSessionsFile) }); err != nil {
			ctl.c.log().Warn("writing the declared records of the market network operator account", "err", err.Error())
		}
	}
	if ctl.c.OnComponents != nil {
		ctl.c.OnComponents(owner, snap.Listeners)
	}
	associationMessage := associationWait(plan, id, RequesterAssociate)
	switch {
	case awaitingOperator != "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, awaitingOperator
	case associationMessage != "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, associationMessage
	case snap.satisfied(allDesired) && st.Did != "":
		st = ready(st, "the root vault is Ready and every component is ready")
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for every component to be ready and the requester's MarketIdentity for a did"
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
		if ca, err = NetworkCAFile(stateDir, n); err != nil {
			return p, err
		}
		p.PlcDirectoryUrl, p.IngressProxyHost = n.PLCDirectoryURL, n.IngressProxyHost
		if p.WatchRelayUrls, err = listFragment(append(append([]string{}, n.RelayURLs...), n.FirehoseURLs...)...); err != nil {
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
		caFile, err := NetworkCAFile(stateDir, n)
		if err != nil {
			return manifests.Gateway{}, err
		}
		if caFile != "" {
			v.TLS, trust = manifests.TLSTrust, []string{caFile}
			if o.Spec.Profile == ProfileGatewayNetwork {
				q, err := json.Marshal(caFile)
				if err != nil {
					return manifests.Gateway{}, err
				}
				v.ExtraProjected = "  - {name: network-ca, file: {path: " + string(q) + "}}\n"
				v.Requester.ExtraProjected = v.ExtraProjected
				requesterExtra = append(requesterExtra, "--guest-curl-home", NodeGuestCurlHome)
			}
		}
		for _, kv := range [][2]string{
			{"--plc-directory-url", n.PLCDirectoryURL},
			{"--ingress-proxy-host", n.IngressProxyHost},
			{"--relay-url", strings.Join(n.RelayURLs, ",")},
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
		if n.GuestTLSURL() != "" {
			p, err := portOf(n.GuestTLSURL())
			if err != nil {
				return manifests.Gateway{}, fmt.Errorf("appctl: network guestTlsUrl: %w", err)
			}
			requesterExtra = append(requesterExtra, "--guest-tls-port", strconv.Itoa(p))
		}
	}
	if len(o.Spec.BidderDids) > 0 {
		extra = append(extra, "--node-extra-bidder-dids", strings.Join(o.Spec.BidderDids, ","))
		requesterExtra = append(requesterExtra, "--bidder-dids", strings.Join(o.Spec.BidderDids, ","))
	}
	opts, err := OptionArgs(GatewayApp, o.Spec.Options)
	if err != nil {
		return manifests.Gateway{}, err
	}
	extra = append(extra, opts...)
	ropts, err := RequesterOptionArgs(GatewayApp, o.Spec.Options)
	if err != nil {
		return manifests.Gateway{}, err
	}
	requesterExtra = append(requesterExtra, ropts...)
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
