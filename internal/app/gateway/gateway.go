package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	Name = "gateway"

	Kind = string(v1alpha1.ApplicationKindGateway)

	GatewayRootName         = "gateway"
	ProfileGatewayProd      = "prod"
	ProfileGatewayNetwork   = "market-network"
	GatewaySet              = "gateway"
	GatewayApp              = "gateway"
	WebAppComponent         = "socialweb-computer-k8s"
	WebAppVaultClient       = "socialweb-computer-k8s"
	WebAppVaultMount        = "socialweb-computer-k8s"
	RequesterVaultMount     = "requester"
	GatewayRequesterPolicy  = "gateway-requester"
	GatewayPdsPolicy        = "gateway-repo-key"
	GatewayPdsGrant         = "socialweb-computer-k8s-pds"
	RequesterAssociate      = compose.RequesterAssociate
	NodeGuestCurlHome       = "/root"
	GatewayAudienceOption   = "gateway-audience"
	GatewayNodePolicy       = "node-policy"
	GatewayNodePolicyArgs   = "node-policy-args"
	GatewayPostSignInOption = "post-sign-in-url"
	GatewayAPIComponent     = "gateway-api"
)

var GatewayRequiredDownloads = []string{"socialweb-meshd-x86_64", "socialweb-meshd-aarch64"}

type Controller struct {
	c *compose.Composer

	web *WebApp
}

func Register(m *ctlruntime.Manager, c *compose.Composer, web *WebApp) error {
	ctl := &Controller{c: c, web: web}
	c.ProvideTrust(schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind})
	opts := append([]ctlruntime.Option{ctlruntime.Export(compose.ExportApplications), ctlruntime.Finalizer(compose.Finalizer)}, c.Watches(Kind)...)
	opts = append(opts, compose.DrainWatch[*v1alpha1.AtprotoRecord](c, compose.ExportMarketIdentities, Kind))
	if err := ctlruntime.For[*v1alpha1.Gateway](m, Name, ctl, opts...); err != nil {
		return fmt.Errorf("gateway: registering the Gateway controller: %w", err)
	}
	return nil
}

type pass struct {
	ctx context.Context

	ctl *Controller

	c *compose.Composer

	r compose.Root

	o *v1alpha1.Gateway

	ws string

	st v1alpha1.ApplicationStatus

	vault []v1alpha1.ApplicationComponent
}

func (p *pass) write(phase v1alpha1.ApplicationPhase, msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = phase, msg
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.GatewayGVR, p.st)
}

func (p *pass) fail(err error) (ctlruntime.Result, error) {
	compose.SetOverrideErrorIfAny(&p.st.Conditions, &p.st.ComponentSummary, p.o.Generation, err)
	return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
}

func (p *pass) retry(what string, err error) (ctlruntime.Result, error) {
	msg := err.Error()
	if what != "" {
		msg = what + ": " + msg
	}
	if _, werr := p.write(v1alpha1.ApplicationPhaseProvisioning, msg); werr != nil {
		return ctlruntime.Result{}, errors.Join(err, werr)
	}
	return ctlruntime.Result{}, err
}

func (p *pass) wait(msg string) (ctlruntime.Result, error) {
	return p.write(v1alpha1.ApplicationPhaseProvisioning, msg)
}

func (p *pass) snapshot(ctx context.Context) (compose.Snapshot, error) {
	snap, err := p.c.Snapshot(ctx, p.r.Workspace, p.r.Owner())
	if err != nil {
		return snap, err
	}
	p.st.Components = append(slices.Clone(p.vault), snap.Components...)
	p.st.Listeners = snap.Listeners
	return snap, nil
}

func (ctl *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.Gateway) (ctlruntime.Result, error) {
	c := ctl.c
	if !c.Mine(req) {
		return ctlruntime.Result{}, nil
	}
	r := compose.RootOf(req, Kind, o, o.Spec.Suspend)
	c.Observe(r)
	st := compose.NewStatus(o.Generation, o.Spec.Profile, o.Status)
	st.Components = []v1alpha1.ApplicationComponent{}
	p := &pass{ctx: ctx, ctl: ctl, c: c, r: r, o: o, ws: r.Workspace + ":" + compose.ComponentWorkspace, st: st}
	if r.Deleting {
		return p.delete(ctx)
	}
	res, err := p.reconcile(ctx)
	if err == nil {
		c.Adopt(ctx, r)
		c.ApplyQuotas(ctx, r, o.Spec.Quota)
	}
	return res, err
}

func (p *pass) deleting(msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseDeleting, msg
	return ctlruntime.Result{Waiting: true}, p.c.WriteStatus(p.ctx, p.r, compose.GatewayGVR, p.st)
}

func (p *pass) delete(ctx context.Context) (ctlruntime.Result, error) {
	c, r := p.c, p.r
	pdsUp := false
	if v, ok, err := c.GetComponent(ctx, r.Workspace, compose.ComponentKindPds, compose.PdsComponent); err == nil && ok && v.Status.Ready {
		pdsUp = true
	}
	records, err := c.ReleaseRecords(ctx, p.ws, pdsUp)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if records > 0 {
		return p.deleting(fmt.Sprintf("%d AtprotoRecord(s) leaving the repo before the Pds stops", records))
	}
	left, err := c.StopComponents(ctx, r, "")
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if left == 0 {
		if err := c.DeleteLinks(ctx, r, p.ws); err != nil {
			return ctlruntime.Result{}, err
		}
		if left, err = c.DeleteVaults(ctx, r); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left > 0 {
		return p.deleting(fmt.Sprintf("%d component(s) stopping", left))
	}
	c.Drop(r)
	return ctlruntime.Result{}, nil
}

func GatewayRequesterIdentity(g v1alpha1.GatewaySpec) v1alpha1.ApplicationIdentity {
	id := g.Identity
	if id.Mode == "" {
		id.Mode = v1alpha1.MarketIdentityMode(compose.OptionValue(g.Options, "atproto-identity-mode"))
	}
	if id.Handle == "" {
		id.Handle = compose.OptionValue(g.Options, "atproto-handle")
	}
	if id.PdsURL == "" {
		id.PdsURL = compose.OptionValue(g.Options, "atproto-pds-url")
	}
	if id.Association == "" && compose.OptionValue(g.Options, "skip-association") == "true" {
		id.Association = v1alpha1.AssociationSkip
	}
	id.ApprovedAssociations = slices.Clone(g.Identity.ApprovedAssociations)
	return id
}

func gatewayNodeMarket(o *v1alpha1.Gateway, workspace, contracts string) *v1alpha1.GatewayNodeMarket {
	m := o.Spec.MarketNetwork
	if m == nil {
		m = &v1alpha1.GatewayMarketNetwork{}
	}
	out := &v1alpha1.GatewayNodeMarket{OwnerSessionsFile: m.OwnerSessionsFile, UserDataBaseFile: m.NodeUserDataBaseFile,
		HomeWorkspace: workspace, ContractWorkspace: contracts,
		NodePolicy: compose.OptionValue(o.Spec.Options, GatewayNodePolicy), NodePolicyArgs: compose.OptionValue(o.Spec.Options, GatewayNodePolicyArgs),
		ExtraBidderDIDs: slices.Clone(o.Spec.BidderDids)}
	return out
}

func GatewayPds(v manifests.Gateway, stateDir string, n *v1alpha1.MarketNetworkValue) (manifests.RepoPds, error) {
	p := manifests.RepoPds{StateDir: stateDir, Identity: marketrequester.RequesterIdentity, KcpServer: v.KcpServer, KcpWorkspace: v.IdentityWorkspace,
		VaultServer: v.Requester.Vault.Server, VaultClient: compose.PdsVaultClient, VaultKcpServer: v.KcpServer + "/clusters/" + v.GatewayWorkspace,
		VaultMount: RequesterVaultMount, VaultMountClient: marketrequester.RequesterVaultClient, VaultWorkspace: v.GatewayWorkspace}
	p.AssociateService = RequesterAssociate
	extra := []string{"--associate-service-id", RequesterAssociate}
	var ca string
	var err error
	if n != nil {
		if ca, err = compose.NetworkCAFile(stateDir, n); err != nil {
			return p, err
		}
		p.PlcDirectoryUrl, p.IngressProxyHost = n.PLCDirectoryURL, n.IngressProxyHost
		if p.WatchEventSources, err = compose.RefFragment(compose.WatchEventSources(compose.SourceInput{Network: n})); err != nil {
			return p, err
		}
		if hosts := compose.NetworkHostsArg(n.Hosts); hosts != "" {
			extra = append(extra, "--network-hosts", hosts)
		}
	}
	if p.Trust, err = compose.ListFragment(ca); err != nil {
		return p, err
	}
	if p.Read, err = compose.ListFragment(ca); err != nil {
		return p, err
	}
	p.ExtraArgs, err = compose.Fragment(extra)
	return p, err
}

func (p *pass) identitySpec(plan compose.AccountPlan) v1alpha1.MarketIdentitySpec {
	return compose.OperatorWatched(plan.Identity, p.o.Spec.Operator, RequesterAssociate)
}

func (p *pass) self(kind, name string) commonv1.ObjectRef {
	return compose.AppRef(kind, name, p.r.Workspace)
}

func (p *pass) baseLinks() []compose.Link {
	requester := p.self(compose.ComponentKindRequester, marketrequester.RequesterComponent)
	api := p.self(compose.ComponentKindGatewayAPI, GatewayAPIComponent)
	vault := vaultlayout.RootServerName
	return []compose.Link{
		compose.PolicyRunLink(requester, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, p.r.Workspace), marketrequester.Workflows...),
		compose.VaultLink(requester, vault, false, RequesterVaultMount+"/*", WebAppVaultMount+"/contracts/*"),
		compose.VaultLink(requester, vault, true, RequesterVaultMount+"/contracts/*"),
		compose.VaultLink(api, vault, false, WebAppVaultMount+"/*"),
		compose.VaultLink(api, vault, true, WebAppVaultMount+"/*"),
		compose.VaultPKILink(api, vault, "pki/*"),
		compose.RootVaultLink(p.r, vault, RequesterVaultMount+"/identity/*"),
		compose.NodeClaimVaultLink(p.r.Workspace, vault, WebAppVaultMount+"/contracts/*"),
	}
}

func (p *pass) reconcile(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o, ws := p.c, p.r, p.o, p.ws
	pdsAccountHandle := compose.PdsHandle(ws, "gateway", "")
	switch o.Spec.Profile {
	case ProfileGatewayProd, ProfileGatewayNetwork:
	default:
		return p.write(v1alpha1.ApplicationPhaseFailed, fmt.Sprintf("profile %q: want %s or %s", o.Spec.Profile, ProfileGatewayProd, ProfileGatewayNetwork))
	}
	plan, err := c.PlanAccount(ctx, ws, GatewayRequesterIdentity(o.Spec), "requester",
		compose.AccountVault{Server: vaultlayout.RootServerName, Client: marketrequester.RequesterVaultClient, Mount: RequesterVaultMount},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		return p.fail(err)
	}
	vault, err := p.rootVault(ctx, r.Suspend)
	if err != nil {
		return p.retry("its root vault", err)
	}
	vaultReady := vault != nil && openbaoctl.Synced(vault)
	if vault != nil {
		p.vault = []v1alpha1.ApplicationComponent{{Kind: compose.KindOpenBaoServer, Name: vault.Name, Workspace: r.Workspace,
			Phase: string(vault.Status.Phase), Ready: vaultReady}}
		p.st.Components = slices.Clone(p.vault)
	}
	ingressSpec := compose.AppIngressOrDefault(o.Spec.Profile == ProfileGatewayProd, o.Spec.Ingress, o.Spec.Network)
	requireIngress, ingressRef, ingressConfig, ingressErr := compose.PdsAppIngress(ingressSpec, o.Spec.Network, o.Spec.Profile == ProfileGatewayProd)
	pdsPublicURL, _ := c.PdsPublicURL(ctx, r.Workspace, compose.PdsComponent, requireIngress)
	in := desiredInput{pdsHandle: pdsAccountHandle, pdsPublicURL: pdsPublicURL, ingress: ingressConfig, requireIngress: requireIngress, ingressRef: ingressRef}
	if r.Suspend {
		return p.suspended(ctx, plan, in)
	}
	if !vaultReady {
		return p.wait("waiting for the root vault to be Ready and synced")
	}
	links := p.baseLinks()
	if err := c.ApplyLinks(ctx, r, "root", links); err != nil {
		return ctlruntime.Result{}, err
	}
	if !c.Applied(r) {
		if _, err := p.write(v1alpha1.ApplicationPhaseProvisioning, "applying its workspace, its records and its requester identity"); err != nil {
			return ctlruntime.Result{}, err
		}
		values := &manifests.Gateway{}
		if o.Spec.Network != nil {
			if pds, err := GatewayPds(*values, c.StateRoot(), o.Spec.Network); err == nil {
				values.Pds = pds
			}
		}
		if err := c.ApplyInfra(ctx, r, GatewaySet, o.Spec.Profile, values, "association-watch"); err != nil {
			return p.retry("its workspace", err)
		}
		if err := c.ApplyIdentitySpec(ctx, r, ws, marketrequester.RequesterIdentity, p.identitySpec(plan)); err != nil {
			return p.retry("the requester's MarketIdentity", err)
		}
		c.MarkApplied(r)
	}
	if ok, msg := c.LinksAccepted(ctx, r, links); !ok {
		return p.wait(msg)
	}
	if err := c.SeedPdsAccountPassword(ctx, r, plan); err != nil {
		return p.retry("the requester's account password", err)
	}
	if plan.Local {
		if err := c.EnsureRepoKey(ctx, r, vaultlayout.RootServerName, RequesterVaultMount, RequesterKeysPath()); err != nil {
			return p.retry("the requester's repo key", err)
		}
	}
	id, err := c.Identity(ctx, ws, marketrequester.RequesterIdentity)
	if apierrors.IsNotFound(err) {
		c.Forget(r)
		return p.wait("waiting for the requester's MarketIdentity")
	}
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !plan.Local {
		switch id.Status.Phase {
		case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
			return p.write(v1alpha1.ApplicationPhaseFailed, "the requester's MarketIdentity: "+id.Status.Message)
		case v1alpha1.MarketIdentityPhaseReady:
		default:
			return p.wait("waiting for the requester's existing-account MarketIdentity to sign in")
		}
		if strings.TrimSpace(id.Status.Did) == "" {
			return p.wait("waiting for the requester's existing-account MarketIdentity to report a did")
		}
	}
	return p.run(ctx, plan, id, in, ingressErr)
}

type desiredInput struct {
	pdsHandle string

	pdsPublicURL string

	ingress *v1alpha1.IngressConfig

	requireIngress bool

	ingressRef *commonv1.ObjectRef
}

func (p *pass) desired(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, in desiredInput) ([]compose.Desired, error) {
	c, o := p.c, p.o
	ws := p.r.Workspace
	stateRoot := c.StateRoot()
	network := o.Spec.Network
	ident := compose.ComponentIdentity(id, o.Spec.Identity.Handle)
	vault := compose.ComponentRefFor(compose.KindOpenBaoServer, vaultlayout.RootServerName)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	if network != nil && network.QrOrigin != "" {
		if h := compose.HostOf(network.QrOrigin); h != "" && !slices.Contains(hostnames, h) {
			hostnames = append(hostnames, h)
		}
	}
	var clusterProvider *v1alpha1.GatewayClusterProvider
	if o.Spec.ClusterProvider != nil {
		cp := *o.Spec.ClusterProvider
		clusterProvider = &cp
	}
	downloadsDir, clusterCADir := "", ""
	if p.ctl.web != nil {
		downloadsDir, clusterCADir = p.ctl.web.DownloadsDir, p.ctl.web.ClusterCADir
	}
	var storage *v1alpha1.ComponentStorage
	if downloadsDir != "" || clusterCADir != "" {
		storage = &v1alpha1.ComponentStorage{BlobsDir: downloadsDir, CertsDir: clusterCADir}
	}
	var tlsSpec *v1alpha1.ComponentTLS
	if network != nil {
		tlsSpec = &v1alpha1.ComponentTLS{Enabled: true, Hostnames: hostnames, CABundle: network.CABundle}
	}
	publicOrigin := compose.OptionValue(o.Spec.Options, compose.PublicOriginOption)
	audience := cmp.Or(compose.OptionValue(o.Spec.Options, GatewayAudienceOption), ident.Did)
	postSignInURL := compose.OptionValue(o.Spec.Options, GatewayPostSignInOption)
	if postSignInURL == "" && publicOrigin != "" {
		postSignInURL = strings.TrimRight(publicOrigin, "/") + "/"
	}
	var requiredDownloads []string
	if downloadsDir != "" && (publicOrigin != "" || (tlsSpec != nil && len(hostnames) > 0)) {
		requiredDownloads = slices.Clone(GatewayRequiredDownloads)
	}
	mode, requesterOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := compose.RequesterComponentFields(o.Spec.Options, plan.Mode)
	var pds compose.Desired
	if plan.Local {
		pdsSpec, err := compose.AppPdsSpec(stateRoot, network, in.pdsHandle, vault, hostnames, in.pdsPublicURL, in.requireIngress, in.ingressRef)
		if err != nil {
			return nil, err
		}
		pds = compose.Desired{Kind: compose.ComponentKindPds, Name: compose.PdsComponent, Spec: pdsSpec}
	}
	requesterSpec := v1alpha1.RequesterSpec{
		ComponentSpec:        p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindRequester, marketrequester.RequesterComponent), network),
		Identity:             ident,
		PolicyEngineRef:      compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
		VaultRef:             vault,
		Hostnames:            hostnames,
		Mode:                 mode,
		PublicOrigin:         requesterOrigin,
		MarketExposureRef:    &commonv1.LocalObjectRef{Name: compose.ServiceNameFor(compose.ComponentKindRequester, marketrequester.RequesterComponent) + compose.ExposureIngressSuffix},
		OperatorDID:          compose.OperatorDID(o.Spec.Identity.ApprovedAssociations, id),
		OAuthClientID:        oauthClientID,
		OAuthRedirectURI:     oauthRedirectURI,
		GuestSSHKeysFile:     guestSSHKeysFile,
		BidderDIDs:           slices.Clone(o.Spec.BidderDids),
		AcceptCopies:         int32(compose.OptionInt(o.Spec.Options, "accept-copies")),
		Comms:                o.Spec.Comms,
		SuspendParticipation: o.Spec.Suspend,
	}
	if plan.Local {
		requesterSpec.PdsRef = compose.ComponentRefFor(compose.ComponentKindPds, compose.PdsComponent)
	} else {
		requesterSpec.PdsRef, requesterSpec.PdsDID, requesterSpec.PDSAccessToken = plan.PdsRef, plan.PdsDID, plan.AccessToken()
	}
	gatewayBase := compose.NewComponentSpec(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindGatewayAPI, GatewayAPIComponent), network)
	gatewayBase.TLS = tlsSpec
	kubeconfigCAFile := compose.OptionValue(o.Spec.Options, "kcp-public-ca-file")
	if kubeconfigCAFile == "" && o.Spec.Profile != ProfileGatewayProd {
		kubeconfigCAFile = filepath.Join(stateRoot, "tls", "tls.crt")
	}
	var gatewayPdsRef *commonv1.ObjectRef
	if plan.Local {
		gatewayPdsRef = compose.ComponentRefFor(compose.ComponentKindPds, compose.PdsComponent)
	}
	gatewayAPI := compose.Desired{Kind: compose.ComponentKindGatewayAPI, Name: GatewayAPIComponent, Spec: v1alpha1.GatewayAPISpec{
		ComponentSpec:     gatewayBase,
		Identity:          ident,
		PdsRef:            gatewayPdsRef,
		RequesterRef:      compose.ComponentRefFor(compose.ComponentKindRequester, marketrequester.RequesterComponent),
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
		NodeMarket:        gatewayNodeMarket(o, p.ws, p.r.Workspace),
	}}
	requester := compose.Desired{Kind: compose.ComponentKindRequester, Name: marketrequester.RequesterComponent, Spec: requesterSpec}
	out := []compose.Desired{compose.PolicyEngineDesired(), requester, gatewayAPI}
	if plan.Local {
		out = append([]compose.Desired{pds}, out...)
		out = append(out, compose.PdsExposures(ws, compose.PdsComponent, network, hostnames, in.ingress)...)
	}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRequester, marketrequester.RequesterComponent, "http", network, hostnames, true, true)...)
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindGatewayAPI, GatewayAPIComponent, "http", network, hostnames, true, true)...)
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	return p.c.EnsureLinked(ctx, p.r, desired, desired)
}

func (p *pass) suspended(ctx context.Context, plan compose.AccountPlan, in desiredInput) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	id, _ := c.Identity(ctx, p.ws, marketrequester.RequesterIdentity)
	desired, err := p.desired(ctx, plan, id, in)
	if err != nil {
		return p.fail(err)
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	held := compose.Existing(snap, desired)
	if err := p.ensure(ctx, held); err != nil {
		return p.retry("", err)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if id != nil {
		p.st.Did = id.Status.Did
	}
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.SuspendedOnceApplied(p.st, snap, held)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.GatewayGVR, p.st)
}

func (p *pass) localAccount(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, in desiredInput,
	desired []compose.Desired, ingressErr error) ([]compose.Desired, *v1alpha1.MarketIdentity, string, bool, error) {
	c, r, o := p.c, p.r, p.o
	rws := r.Workspace
	first := compose.PdsApplySet(desired, compose.PdsComponent, compose.ComponentRefName(in.ingressRef))
	if in.requireIngress && in.pdsPublicURL == "" {
		if applied, err := c.PdsPublicURLApplied(ctx, rws, compose.PdsComponent); err != nil {
			return nil, nil, "", false, err
		} else if applied {
			first = compose.WithoutPds(first)
		}
	}
	if err := p.c.EnsureAdding(ctx, p.r, first); err != nil {
		return nil, nil, "", false, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return nil, nil, "", false, err
	}
	if !snap.Ready(compose.ComponentKindPds, compose.PdsComponent) {
		return nil, nil, "waiting for the requester's app Pds to be ready", false, nil
	}
	if in.requireIngress {
		if ingressErr != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, ingressErr)
			return nil, nil, "the app Pds ingress: " + ingressErr.Error(), false, nil
		}
		if err := snap.PdsIngressReady(compose.PdsComponent, in.ingressRef); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return nil, nil, "waiting for the app Pds ingress: " + err.Error(), false, nil
		}
		if in.pdsPublicURL, err = c.PdsIngressURL(ctx, rws, compose.PdsComponent); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return nil, nil, "the app Pds ingress: " + err.Error(), compose.PdsIngressPhase(err) == v1alpha1.ApplicationPhaseFailed, nil
		}
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, nil)
	} else {
		in.pdsPublicURL, _ = c.PdsPublicURL(ctx, rws, compose.PdsComponent, false)
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", false, nil)
	}
	if in.pdsPublicURL == "" {
		return nil, nil, "waiting for the app Pds public URL", false, nil
	}
	if desired, err = p.desired(ctx, plan, id, in); err != nil {
		return nil, nil, "", true, err
	}
	pds, _ := compose.DesiredPds(desired)
	if err := p.c.EnsureAdding(ctx, p.r, []compose.Desired{pds}); err != nil {
		return nil, nil, "", false, err
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return nil, nil, "", false, err
	}
	if !snap.PdsApplied(compose.PdsComponent) {
		return nil, nil, "waiting for the app Pds to apply its public URL", false, nil
	}
	account := compose.PdsAccount{Vault: vaultlayout.RootServerName, Mount: RequesterVaultMount, Path: compose.PdsSessionPath("requester"), Handle: in.pdsHandle}
	accountLinks := compose.PdsAccountLinks(r, account)
	if err := c.ApplyLinks(ctx, r, "account", accountLinks); err != nil {
		return nil, nil, "", false, err
	}
	if ok, msg := c.LinksAccepted(ctx, r, accountLinks); !ok {
		return nil, nil, msg, false, nil
	}
	did, err := c.EnsurePdsAccount(ctx, r, account)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("its app Pds account: %w", err)
	}
	acct := compose.LocalAccount{DID: did, Handle: in.pdsHandle, SessionID: vaultlayout.RootServerName + ":" + account.Path, PLC: o.Spec.Network != nil}
	repo := compose.RepoRefIn(did, rws)
	repoLinks := append(compose.LocalAccountLinks(rws, acct),
		compose.RepoWriteLink(p.self(compose.ComponentKindRequester, marketrequester.RequesterComponent), repo, compose.ScopeAll),
		compose.RecordWriteLink(p.ws, r.Owner(), repo, compose.ScopeAll))
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks, p.ws); err != nil {
		return nil, nil, "", false, err
	}
	if err := c.ApplyLocalAccount(ctx, r, rws, acct); err != nil {
		return nil, nil, "", false, err
	}
	if err := c.ApplyAssociationRecord(ctx, r, p.ws, repo, marketrequester.RequesterIdentity, RequesterAssociate); err != nil {
		return nil, nil, "", false, err
	}
	requesterDir := compose.ComponentCRStateDir(c.StateRoot(), compose.ComponentKindRequester, marketrequester.RequesterComponent)
	changed, err := compose.SyncRequesterRepoKey(c.StateRoot(), did, requesterDir)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("its app Pds signing key: %w", err)
	}
	if changed {
		if err := c.RestartComponent(ctx, r, rws, compose.ComponentKindRequester, marketrequester.RequesterComponent); err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, "", false, fmt.Errorf("restarting the requester: %w", err)
		}
	}
	identity := p.identitySpec(plan)
	identity.Did, identity.PdsURL, identity.Handle = did, in.pdsPublicURL, in.pdsHandle
	if err := c.ApplyIdentitySpec(ctx, r, p.ws, marketrequester.RequesterIdentity, identity); err != nil {
		return nil, nil, "", false, fmt.Errorf("its app Pds identity: %w", err)
	}
	if id, err = c.Identity(ctx, p.ws, marketrequester.RequesterIdentity); err != nil {
		return nil, nil, "", false, err
	}
	if desired, err = p.desired(ctx, plan, id, in); err != nil {
		return nil, nil, "", true, err
	}
	return desired, id, "", false, nil
}

func (p *pass) run(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, in desiredInput, ingressErr error) (ctlruntime.Result, error) {
	c, r, o, ws := p.c, p.r, p.o, p.ws
	desired, err := p.desired(ctx, plan, id, in)
	if err != nil {
		return p.fail(err)
	}
	all := desired
	if _, hasPds := compose.DesiredPds(desired); hasPds {
		next, nextID, msg, failed, err := p.localAccount(ctx, plan, id, in, desired, ingressErr)
		switch {
		case err != nil && failed:
			return p.fail(err)
		case err != nil:
			return p.retry("", err)
		case msg != "" && failed:
			return p.write(v1alpha1.ApplicationPhaseFailed, msg)
		case msg != "":
			return p.wait(msg)
		}
		all, id, desired = next, nextID, compose.WithoutPds(next)
	}
	if err := p.c.EnsureLinked(ctx, p.r, all, desired); err != nil {
		return p.retry("", err)
	}
	streamLinks, err := c.ApplyMarketStreams(ctx, r, compose.Streams{Role: "requester", Workspace: ws,
		Comms:   compose.OverrideComms(o.Spec.Components, compose.ComponentKindRequester, marketrequester.RequesterComponent, o.Spec.Comms),
		Sources: compose.SourceInput{Network: o.Spec.Network}, ForceFirehose: compose.OAuthReceiveOnly(plan.Mode)})
	if err != nil {
		return p.fail(err)
	}
	if err := c.ApplyLinks(ctx, r, "events", streamLinks, ws); err != nil {
		return ctlruntime.Result{}, err
	}
	if _, err := c.DeleteComponentsExcept(ctx, r.Workspace, r.Owner(), compose.DesiredKeys(all)); err != nil {
		return ctlruntime.Result{}, err
	}
	if id, err = c.Identity(ctx, ws, marketrequester.RequesterIdentity); err != nil {
		return ctlruntime.Result{}, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = id.Status.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, all, snap, nil)
	if !plan.Local {
		repoLinks, err := c.AccountRepo(ctx, r, ws, marketrequester.RequesterIdentity, plan, id.Status.Did, id.Status.PdsURL)
		if err != nil {
			c.Log().Warn("the requester's existing-account repo", "root", r.Owner(), "err", err.Error())
		}
		repoLinks = append(repoLinks, compose.RepoWriteLink(p.self(compose.ComponentKindRequester, marketrequester.RequesterComponent),
			compose.RepoRefIn(id.Status.Did, ws), compose.ScopeAll))
		if err := c.ApplyLinks(ctx, r, "repo", repoLinks, ws); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	awaitingOperator := ""
	if a := id.Status.PendingAction; a != nil && id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingScan {
		awaitingOperator = "waiting for the operator's claim record (badgeBlueKeys keyId " + id.Status.Did + " with the nonce n of " + a.URL + ")"
	}
	if m := o.Spec.MarketNetwork; o.Spec.Profile == ProfileGatewayNetwork && m != nil && m.OwnerSessionsFile != "" {
		p.operatorWrites(ctx, id, m.OwnerSessionsFile)
	}
	if fn := c.Config().OnComponents; fn != nil {
		fn(r.Owner(), snap.Listeners)
	}
	switch msg := compose.AssociationWait(plan, id, RequesterAssociate); {
	case awaitingOperator != "":
		return p.wait(awaitingOperator)
	case msg != "":
		return p.wait(msg)
	case snap.Satisfied(all) && p.st.Did != "":
		p.st = compose.Ready(p.st, "the root vault is Ready and every component is ready")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.GatewayGVR, p.st)
	}
	return p.wait("waiting for every component to be ready and the requester's MarketIdentity for a did")
}

func (p *pass) operatorWrites(ctx context.Context, id *v1alpha1.MarketIdentity, sessionsFile string) {
	c, r, o := p.c, p.r, p.o
	log := c.Log().With("root", r.Owner())
	_, s, err := compose.FirstOwnerSession(sessionsFile)
	if err != nil {
		log.Warn("reading the market network operator session", "err", err.Error())
		return
	}
	op := compose.Operator{PdsURL: s.Service, CARef: c.PublishNetworkTrust(r, o.Spec.Network), Identifier: s.Identifier, Password: s.Password,
		Vault: vaultlayout.RootServerName, Mount: RequesterVaultMount}
	if err := c.ApplyLinks(ctx, r, "operator", compose.OperatorLinks(r, op)); err != nil {
		log.Warn("the market network operator links", "err", err.Error())
		return
	}
	if compose.ClaimOpen(id) && o.Spec.Network != nil {
		operator, err := c.WriteClaim(ctx, r, op, id, "requester-", RequesterAssociate)
		if err != nil {
			log.Warn("associating the requester's DID with the market network operator account", "err", err.Error())
		} else if operator != "" {
			log.Info("the market network operator account wrote its claim record naming the gateway requester's own DID", "operator", operator,
				"handle", s.Identifier, "requesterDid", id.Status.Did)
		}
	}
	if o.Spec.Network != nil {
		if err := c.ApplyOperatorRepo(ctx, r, p.ws, op); err != nil {
			log.Warn("composing the repo the market network operator account's declared records write through", "err", err.Error())
		}
	}
}
