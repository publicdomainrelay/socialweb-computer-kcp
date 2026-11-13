package marketbidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	vaultctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
)

const (
	Name = "marketbidder"

	Kind = string(v1alpha1.ApplicationKindMarketBidder)

	BidderSet         = "market-bidder"
	BidderComponent   = "bidder"
	BidderApp         = "market-bidder"
	BidderIdentity    = "bidder"
	BidderAccount     = "bidder"
	BidderVaultServer = "bidder"
	BidderVaultClient = "bidder"
	DefaultPolicy     = v1alpha1.BidderDefaultPolicy
	IrohIngress       = "ingress-iroh"

	IrohExposureSuffix = "-iroh"

	DumbpipeExecutable = "dumbpipe"

	BidderProfileProdQr   = "prod-qr"
	BidderProfileNetwork  = "market-network"
	BidderProfileDevLocal = "dev-local"
	GuestComponent        = "guest-kcp"
	GuestVaultClient      = "guest-kcp"
	GuestAccount          = "guest-kcp"
	MarketRootNameArg     = "--market-root-name"

	GuestDownloadsExecutable = "socialweb-guest-downloads"

	identityKeysPath = "identity/bidder/keys"
)

var BidderProfiles = []string{BidderProfileProdQr, BidderProfileNetwork, BidderProfileDevLocal}

type SignInGuard func(spec v1alpha1.MarketBidderSpec, id *v1alpha1.MarketIdentity) error

type Controller struct {
	c *compose.Composer

	guard SignInGuard

	mu sync.Mutex

	rotated map[string]int64
}

func Register(m *ctlruntime.Manager, c *compose.Composer, guard SignInGuard) error {
	ctl := &Controller{c: c, guard: guard, rotated: map[string]int64{}}
	c.ProvideTrust(schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind})
	opts := append([]ctlruntime.Option{ctlruntime.Export(compose.ExportApplications), ctlruntime.Finalizer(compose.Finalizer)}, c.Watches(Kind)...)
	opts = append(opts, compose.ComputeVMWatch(c, compose.ExportComputeVMs, Kind),
		compose.DrainWatch[*v1alpha1.AtprotoRecord](c, compose.ExportMarketIdentities, Kind),
		compose.OperatorRecordWatch(c, compose.ExportMarketIdentities, Kind), compose.VaultBindWatch(c, Kind))
	if err := ctlruntime.For[*v1alpha1.MarketBidder](m, Name, ctl, opts...); err != nil {
		return fmt.Errorf("marketbidder: registering the MarketBidder controller: %w", err)
	}
	return nil
}

func (ctl *Controller) rotatedAt(r compose.Root) int64 {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return ctl.rotated[string(r.UID)]
}

func (ctl *Controller) setRotated(r compose.Root, n int64) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	ctl.rotated[string(r.UID)] = n
}

func (ctl *Controller) forget(r compose.Root) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	delete(ctl.rotated, string(r.UID))
}

type pass struct {
	ctx context.Context

	ctl *Controller

	c *compose.Composer

	r compose.Root

	o *v1alpha1.MarketBidder

	st v1alpha1.ApplicationStatus

	identityPhase v1alpha1.MarketIdentityPhase
}

func (p *pass) log() *slog.Logger { return p.c.Log().With("marketbidder", p.o.Name) }

func (p *pass) write(phase v1alpha1.ApplicationPhase, msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = phase, msg
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.MarketBidderGVR, p.st)
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

func (p *pass) ready(msg string) (ctlruntime.Result, error) {
	p.st = compose.Ready(p.st, msg)
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.MarketBidderGVR, p.st)
}

func (p *pass) snapshot(ctx context.Context) (compose.Snapshot, error) {
	snap, err := p.c.Snapshot(ctx, p.r.Workspace, p.r.Owner())
	if err != nil {
		return snap, err
	}
	p.st.Components, p.st.Listeners = snap.Components, snap.Listeners
	return snap, nil
}

func (ctl *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.MarketBidder) (ctlruntime.Result, error) {
	c := ctl.c
	if !c.Mine(req) {
		return ctlruntime.Result{}, nil
	}
	r := compose.RootOf(req, Kind, o, o.Spec.Suspend)
	c.Observe(r)
	p := &pass{ctx: ctx, ctl: ctl, c: c, r: r, o: o, st: compose.NewStatus(o.Generation, o.Spec.Profile, o.Status)}
	if r.Deleting {
		return p.delete(ctx)
	}
	if c.Parked(r) {
		return ctlruntime.Result{}, nil
	}
	res, err := p.reconcile(ctx)
	if errors.Is(err, vaultctl.ErrUnbound) {
		p.log().Info("the bidder waits for its vault to bind; the bind wakes it", "err", err.Error())
		return p.wait("waiting for its vault to bind: " + err.Error())
	}
	if err == nil {
		if reason := compose.ParkReason("", p.identityPhase); reason != "" && !r.Suspend {
			c.Park(ctx, r, "", reason)
			return res, nil
		}
		c.Adopt(ctx, r)
		c.ApplyQuotas(ctx, r, o.Spec.Quota)
	}
	return res, err
}

func (p *pass) deleting(msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseDeleting, msg
	return ctlruntime.Result{Waiting: true}, p.c.WriteStatus(p.ctx, p.r, compose.MarketBidderGVR, p.st)
}

func (p *pass) delete(ctx context.Context) (ctlruntime.Result, error) {
	c, r := p.c, p.r
	vms, err := c.DeleteAll(ctx, r.Workspace, v1alpha1.ComputeVMGVR)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if vms > 0 {
		return p.deleting(fmt.Sprintf("%d ComputeVM(s) releasing", vms))
	}
	pdsUp := false
	if v, found, err := c.GetComponent(ctx, r.Workspace, compose.ComponentKindPds, compose.PdsComponent); err == nil && found && v.Status.Ready {
		pdsUp = true
	}
	if pdsUp {
		p.unassociate(ctx)
	}
	records, err := c.ReleaseRecords(ctx, r.Workspace, pdsUp)
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
		if left, err = c.DeleteVaults(ctx, r); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left > 0 {
		return p.deleting(fmt.Sprintf("%d component(s) stopping", left))
	}
	p.ctl.forget(r)
	c.Drop(r)
	return ctlruntime.Result{}, nil
}

func (p *pass) self() commonv1.ObjectRef {
	return compose.AppRef(compose.ComponentKindBidder, BidderComponent, p.r.Workspace)
}

func (p *pass) guest() commonv1.ObjectRef {
	return compose.AppRef(compose.ComponentKindGuestControlPlane, GuestComponent, p.r.Workspace)
}

func (p *pass) baseLinks(mount string) []compose.Link {
	self, ws := p.self(), p.r.Workspace
	return []compose.Link{
		compose.PolicyRunLink(self, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, ws), cmp.Or(strings.TrimSpace(p.o.Spec.Policy), DefaultPolicy)),
		compose.VaultLink(self, BidderVaultServer, true, mount+"/guests/*"),
		compose.ComputeVMVaultLink(ws, BidderVaultServer, mount+"/guests/*"),
		compose.RootVaultLink(p.r, BidderVaultServer, mount+"/identity/*", mount+"/oidc-rotation/*", guestOIDCKeyPath),
		compose.RefLink(self, p.guest(), compose.Cap(guestcp.CapabilityEvents)),
	}
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	return p.c.EnsureLinked(ctx, p.r, desired, desired)
}

func (p *pass) prune(ctx context.Context, desired []compose.Desired) error {
	_, err := p.c.DeleteComponentsExcept(ctx, p.r.Workspace, p.r.Owner(), compose.DesiredKeys(desired))
	return err
}

type ingress struct {
	config *v1alpha1.IngressConfig

	required bool

	ref *commonv1.ObjectRef

	err error
}

func defaultIngressProfile(profile string) bool {
	return profile == BidderProfileProdQr || profile == BidderProfileNetwork
}

func (p *pass) reconcile(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	plan, err := c.PlanAccount(ctx, ws, o.Spec.Identity, "bidder",
		compose.AccountVault{Server: BidderVaultServer, Client: BidderVaultClient, Mount: BidderVaultClient},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		return p.fail(err)
	}
	mount := plan.Vault.MountOr(BidderVaultClient)
	o.Spec.Identity.Mode = cmp.Or(o.Spec.Identity.Mode, plan.Mode)
	pdsHandle := compose.PdsHandle(ws, "bidder", o.Spec.Identity.Handle)
	in := ingress{}
	spec := compose.AppIngressOrDefault(defaultIngressProfile(o.Spec.Profile), o.Spec.Ingress, o.Spec.Network)
	in.required, in.ref, in.config, in.err = compose.PdsAppIngress(spec, o.Spec.Network, o.Spec.Profile == BidderProfileProdQr)
	pdsPublicURL, _ := c.PdsPublicURL(ctx, ws, compose.PdsComponent, in.required)
	if r.Suspend {
		return p.suspended(ctx, plan, pdsHandle, pdsPublicURL, in)
	}
	if err := c.ApplyVault(ctx, r, BidderVaultServer); err != nil {
		return ctlruntime.Result{}, err
	}
	links := p.baseLinks(mount)
	if err := c.ApplyLinks(ctx, r, "root", links); err != nil {
		return ctlruntime.Result{}, err
	}
	var phases []string
	if plan.UsesAssociation() {
		phases = []string{"association-watch"}
	}
	values := p.infraValues()
	intact, err := c.CheckWorkloads(ctx, r)(c.SetWorkloads(BidderSet, o.Spec.Profile, values, phases...))
	if err != nil {
		return p.retry("its set workloads", err)
	}
	if !c.Applied(r) {
		identity := compose.OperatorWatched(plan.Identity, o.Spec.Operator)
		digest, digestErr := compose.InfraDigest(BidderSet, o.Spec.Profile, values, identity, phases...)
		if intact && digestErr == nil && c.AppliedBefore(r, o.Status.AppliedSet, digest) {
			c.MarkApplied(r)
		} else {
			if _, err := p.write(v1alpha1.ApplicationPhaseProvisioning, "applying its own OpenBaoServer and workspace"); err != nil {
				return ctlruntime.Result{}, err
			}
			if err := c.ApplyInfra(ctx, r, BidderSet, o.Spec.Profile, values, phases...); err != nil {
				return p.retry("", err)
			}
			if err := c.ApplyIdentitySpec(ctx, r, ws, BidderIdentity, identity); err != nil {
				return p.retry("its MarketIdentity", err)
			}
			c.MarkApplied(r)
			if digestErr == nil {
				p.st.AppliedSet = &v1alpha1.AppliedSet{Generation: o.Generation, Digest: digest}
			}
		}
	}
	if _, _, err := c.VaultRefs(ctx, ws, BidderVaultServer, BidderVaultClient); err != nil {
		return p.wait("its vault: " + err.Error())
	}
	if ok, msg := c.LinksAccepted(ctx, r, links); !ok {
		return p.wait(msg)
	}
	if err := c.SeedPdsAccountPassword(ctx, r, plan); err != nil {
		return p.retry("its account password", err)
	}
	if plan.Local || o.Spec.Identity.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		if err := c.EnsureRepoKey(ctx, r, BidderVaultServer, mount, identityKeysPath); err != nil {
			return p.retry("its repository key", err)
		}
	}
	id, err := c.Identity(ctx, ws, BidderIdentity)
	if apierrors.IsNotFound(err) {
		if err := c.ApplyIdentitySpec(ctx, r, ws, BidderIdentity, compose.OperatorWatched(plan.Identity, o.Spec.Operator)); err != nil {
			return p.retry("its MarketIdentity", err)
		}
		return p.wait("its MarketIdentity was missing and is applied again")
	}
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !plan.Local {
		return p.account(ctx, plan, id, mount)
	}
	return p.local(ctx, plan, id, mount, pdsHandle, pdsPublicURL, in)
}

func (p *pass) desired(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, pdsHandle, pdsPublicURL string, in ingress) ([]compose.Desired, error) {
	c, o, ws := p.c, p.o, p.r.Workspace
	stateRoot := c.StateRoot()
	network := o.Spec.Network
	ident := compose.ComponentIdentity(id, o.Spec.Identity.Handle)
	vault := compose.ComponentRefFor(compose.KindOpenBaoServer, BidderVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	guestPdsRef := plan.ComponentPdsRef()
	guestSpec := v1alpha1.GuestControlPlaneSpec{
		ComponentSpec: p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindGuestControlPlane, GuestComponent), network),
		Identity:      ident,
		PdsRef:        guestPdsRef,
		VaultRef:      vault,
		Hostnames:     hostnames,
	}
	downloads, err := p.guestDownloadsDir()
	if err != nil {
		return nil, err
	}
	if downloads != "" {
		guestSpec.Storage = &v1alpha1.ComponentStorage{BlobsDir: downloads}
	}
	guest := compose.Desired{Kind: compose.ComponentKindGuestControlPlane, Name: GuestComponent, Spec: guestSpec}
	dumbpipe, err := p.dumbpipe()
	if err != nil {
		return nil, err
	}
	if dumbpipe == "" && p.c.Config().Record != nil && servesMarketInbound(o.Spec.Network.MergeComms(o.Spec.Comms)) {
		return nil, errors.New("marketbidder: the instance records no dumbpipe, so the offering would carry no iroh market endpoint " +
			"(https only on a test profile, none on prod-qr); run a release build or one from scripts/build-socialweb-computer.sh, or give --dumbpipe-path")
	}
	marketHTTPS := o.Spec.Profile != BidderProfileProdQr || compose.OptionValue(o.Spec.Options, "market-https") == "true"
	var image string
	var firecracker *v1alpha1.FirecrackerProvider
	var kcp *v1alpha1.KcpProvider
	if o.Spec.Kcp != nil {
		k := *o.Spec.Kcp
		kcp = &k
		image = k.GuestImageSource
	}
	if o.Spec.Firecracker != nil {
		f := *o.Spec.Firecracker
		f.VmruntimeCommand = append([]string(nil), f.VmruntimeCommand...)
		firecracker = &f
	}
	bidder := compose.Desired{Kind: compose.ComponentKindBidder, Name: BidderComponent, Spec: v1alpha1.BidderSpec{
		ComponentSpec:        p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindBidder, BidderComponent), network),
		MarketExposureRefs:   p.marketExposures(marketHTTPS, dumbpipe),
		Mode:                 string(plan.Mode),
		Identity:             ident,
		PdsRef:               plan.ComponentPdsRef(),
		PolicyEngineRef:      compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
		VaultRef:             vault,
		Hostnames:            hostnames,
		GuestImageSource:     image,
		Firecracker:          firecracker,
		Kcp:                  kcp,
		ComputeKinds:         append([]string(nil), o.Spec.ComputeKinds...),
		Operator:             o.Spec.Operator,
		Policy:               cmp.Or(strings.TrimSpace(o.Spec.Policy), DefaultPolicy),
		PolicyArgs:           compose.OptionValue(o.Spec.Options, "policy-args"),
		Association:          o.Spec.Association.DeepCopy(),
		GuestNetwork:         o.Spec.GuestNetwork,
		BidderDIDs:           append([]string(nil), o.Spec.BidderDids...),
		Comms:                o.Spec.Network.MergeComms(o.Spec.Comms),
		SuspendParticipation: o.Spec.Suspend,
		Storage:              guestSpec.Storage,
	}}
	out := []compose.Desired{compose.PolicyEngineDesired(), guest, bidder}
	if plan.Local {
		pdsSpec, err := compose.AppPdsSpec(stateRoot, "", false, network, pdsHandle, vault, hostnames, pdsPublicURL, in.required, in.ref)
		if err != nil {
			return nil, err
		}
		out = append([]compose.Desired{{Kind: compose.ComponentKindPds, Name: compose.PdsComponent, Spec: pdsSpec}}, out...)
	}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindBidder, BidderComponent, "market", network, hostnames, false, marketHTTPS)...)
	if dumbpipe != "" {
		out = append(out, compose.Desired{Kind: compose.ComponentKindIngressProxy, Name: IrohIngress, Spec: v1alpha1.IngressProxySpec{
			ComponentSpec: compose.NewComponentSpec(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindIngressProxy, IrohIngress), nil),
			Provider:      v1alpha1.IngressProviderIroh,
			Iroh:          &v1alpha1.IngressProxyIroh{DumbpipePath: dumbpipe},
		}}, compose.ExposureDesired(compose.ComponentKindBidder, BidderComponent, IrohExposureSuffix, exposurev1alpha1.ExposureModeIngress, "", "/",
			IrohIngress, exposurev1alpha1.ExposureTLS{}))
	}
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindGuestControlPlane, GuestComponent, "http", network, hostnames, true, true)...)
	if plan.Local {
		out = append(out, compose.PdsExposures(ws, compose.PdsComponent, network, hostnames, in.config)...)
	}
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func (p *pass) marketExposures(https bool, dumbpipe string) []commonv1.LocalObjectRef {
	name := compose.ServiceNameFor(compose.ComponentKindBidder, BidderComponent)
	var out []commonv1.LocalObjectRef
	if dumbpipe != "" {
		out = append(out, commonv1.LocalObjectRef{Name: name + IrohExposureSuffix})
	}
	if https {
		out = append(out, commonv1.LocalObjectRef{Name: name + compose.ExposureIngressSuffix})
	}
	return out
}

func servesMarketInbound(c *v1alpha1.MarketComms) bool {
	return c == nil || c.Mode == "" || c.Mode == v1alpha1.MarketCommsModeXRPC || c.Mode == v1alpha1.MarketCommsModeBoth
}

func (p *pass) dumbpipe() (string, error) {
	record := p.c.Config().Record
	if record == nil {
		return "", nil
	}
	rec, err := record()
	if err != nil {
		return "", fmt.Errorf("marketbidder: reading the instance record for dumbpipe: %w", err)
	}
	return rec.Executables[DumbpipeExecutable].Path, nil
}

func (p *pass) guestDownloadsDir() (string, error) {
	record := p.c.Config().Record
	if record == nil {
		return "", nil
	}
	rec, err := record()
	if err != nil {
		return "", fmt.Errorf("marketbidder: reading the instance record for the guest downloads: %w", err)
	}
	return rec.Executables[GuestDownloadsExecutable].Path, nil
}

func (p *pass) suspended(ctx context.Context, plan compose.AccountPlan, pdsHandle, pdsPublicURL string, in ingress) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	id, _ := c.Identity(ctx, r.Workspace, BidderIdentity)
	desired, err := p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, in)
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
	if did := strings.TrimSpace(p.st.Did); did != "" {
		local := ""
		if plan.Local {
			local = did
		}
		if err := p.streams(ctx, plan, local); err != nil {
			return p.retry("its market streams (releases of its running VMs still come in while it is suspended)", err)
		}
	}
	if o.Spec.Kcp != nil {
		p.st.Components = append(p.st.Components, p.guestComponents(ctx)...)
	}
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.SuspendedOnceApplied(p.st, snap, held)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.MarketBidderGVR, p.st)
}

func (p *pass) streams(ctx context.Context, plan compose.AccountPlan, localDID string) error {
	streams, err := p.c.ApplyMarketStreams(ctx, p.r, compose.Streams{Role: compose.BidderStreamRole, Comms: compose.OverrideComms(p.o.Spec.Components, compose.ComponentKindBidder, BidderComponent, p.o.Spec.Network.MergeComms(p.o.Spec.Comms)),
		Sources: compose.SourceInput{Network: p.o.Spec.Network, LocalDID: localDID}, ForceFirehose: compose.OAuthReceiveOnly(plan.Mode)})
	if err != nil {
		return err
	}
	return p.c.ApplyLinks(ctx, p.r, "events", streams.Links)
}

func (p *pass) oauthEndpoints(ctx context.Context, snap compose.Snapshot) error {
	return p.c.ApplyOAuthIdentityEndpoints(ctx, p.r, snap, p.r.Workspace, BidderIdentity,
		compose.ServiceNameFor(compose.ComponentKindBidder, BidderComponent)+compose.ExposureIngressSuffix, "bidder", p.o.Spec.Identity)
}

func (p *pass) account(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, mount string) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	if id.Status.Phase != v1alpha1.MarketIdentityPhaseAwaitingScan {
		p.identityPhase = id.Status.Phase
	}
	switch id.Status.Phase {
	case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
		return p.write(v1alpha1.ApplicationPhaseFailed, "its MarketIdentity: "+id.Status.Message)
	case v1alpha1.MarketIdentityPhaseReady:
	default:
		switch plan.Mode {
		case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
			desired, err := p.desired(ctx, plan, id, "", "", ingress{})
			if err != nil {
				return p.fail(err)
			}
			if err := p.ensure(ctx, desired); err != nil {
				return p.retry("", err)
			}
			if err := p.prune(ctx, desired); err != nil {
				return ctlruntime.Result{}, err
			}
			snap, err := p.snapshot(ctx)
			if err != nil {
				return ctlruntime.Result{}, err
			}
			if err := p.oauthEndpoints(ctx, snap); err != nil {
				return ctlruntime.Result{}, err
			}
		}
		return p.wait("waiting for its existing-account MarketIdentity to sign in")
	}
	if strings.TrimSpace(id.Status.Did) == "" {
		return p.wait("waiting for its existing-account MarketIdentity to report a did")
	}
	if plan.Mode == v1alpha1.MarketIdentityModePdsAccount && strings.TrimSpace(id.Status.PdsURL) == "" {
		return p.write(v1alpha1.ApplicationPhaseFailed, "the existing-account MarketIdentity reports no pdsUrl; it fails closed")
	}
	desired, err := p.desired(ctx, plan, id, "", "", ingress{})
	if err != nil {
		return p.fail(err)
	}
	if err := p.ensure(ctx, desired); err != nil {
		return p.retry("", err)
	}
	if err := p.streams(ctx, plan, ""); err != nil {
		return p.fail(err)
	}
	if err := p.prune(ctx, desired); err != nil {
		return ctlruntime.Result{}, err
	}
	repoLinks, err := c.AccountRepo(ctx, r, ws, BidderIdentity, plan, id.Status.Did, id.Status.PdsURL)
	if err != nil {
		p.log().Warn("the existing-account repo", "err", err.Error())
	}
	repoLinks = append(repoLinks, compose.RepoWriteLink(p.self(), compose.RepoRefIn(id.Status.Did, ws), compose.ScopeAll),
		compose.ServiceAuthLink(p.self(), compose.AppRef(compose.KindMarketIdentity, BidderIdentity, ws), id.Status.Did))
	if ref := plan.ComponentPdsRef(); ref != nil && strings.EqualFold(ref.Kind, compose.KindRemotePDS) && strings.TrimSpace(ref.Name) != "" {
		repoLinks = append(repoLinks, compose.ServiceAuthLink(p.guest(), compose.RemotePDSRefIn(strings.TrimSpace(ref.Name), ws), id.Status.Did))
	}
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	p.operatorSignIn(ctx, mount)
	if err := p.rotateSigningKey(ctx, mount); err != nil {
		p.log().Warn("rotating the bidder's OIDC signing key", "err", err.Error())
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = id.Status.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	if snap.Satisfied(desired) {
		return p.ready("the existing account is signed in and every component is ready")
	}
	return p.wait("waiting for every component to be ready")
}

func (p *pass) local(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, mount, pdsHandle, pdsPublicURL string, in ingress) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	desired, err := p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, in)
	if err != nil {
		return p.fail(err)
	}
	first := compose.PdsApplySet(desired, compose.PdsComponent, compose.ComponentRefName(in.ref))
	if in.required && pdsPublicURL == "" {
		if applied, err := c.PdsPublicURLApplied(ctx, ws, compose.PdsComponent); err != nil {
			return ctlruntime.Result{}, err
		} else if applied {
			first = compose.WithoutPds(first)
		}
	}
	if err := p.c.EnsureAdding(ctx, p.r, first); err != nil {
		return p.retry("", err)
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !snap.Ready(compose.ComponentKindPds, compose.PdsComponent) {
		return p.wait("waiting for the bidder's app Pds to be ready")
	}
	if in.required {
		if in.err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, in.err)
			return p.wait("the bidder's app Pds ingress: " + in.err.Error())
		}
		if err := snap.PdsIngressReady(compose.PdsComponent, in.ref); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return p.wait("waiting for the bidder's app Pds ingress: " + err.Error())
		}
		if pdsPublicURL, err = c.PdsIngressURL(ctx, ws, compose.PdsComponent); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return p.write(compose.PdsIngressPhase(err), "the bidder's app Pds ingress: "+err.Error())
		}
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, nil)
	} else {
		pdsPublicURL, _ = c.PdsPublicURL(ctx, ws, compose.PdsComponent, false)
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", false, nil)
	}
	if pdsPublicURL == "" {
		return p.wait("waiting for the bidder's app Pds public URL")
	}
	if desired, err = p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, in); err != nil {
		return p.fail(err)
	}
	pds, _ := compose.DesiredPds(desired)
	if err := p.c.EnsureAdding(ctx, p.r, []compose.Desired{pds}); err != nil {
		return p.retry("", err)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if !snap.PdsApplied(compose.PdsComponent) {
		return p.wait("waiting for the bidder's app Pds to apply its public URL")
	}
	account := compose.PdsAccount{Vault: BidderVaultServer, Mount: mount, Path: compose.PdsSessionPath("bidder"), Handle: pdsHandle}
	accountLinks := compose.PdsAccountLinks(r, account)
	if err := c.ApplyLinks(ctx, r, "account", compose.WithoutLinks(accountLinks, p.baseLinks(mount))); err != nil {
		return ctlruntime.Result{}, err
	}
	if ok, msg := c.LinksAccepted(ctx, r, accountLinks); !ok {
		return p.wait(msg)
	}
	did, err := c.EnsurePdsAccount(ctx, r, account)
	if err != nil {
		return p.retry("its app Pds account", err)
	}
	acct := compose.LocalAccount{DID: did, Handle: pdsHandle, SessionID: BidderVaultServer + ":" + account.Path, PLC: o.Spec.Network != nil}
	repo := compose.RepoRefIn(did, ws)
	repoLinks := append(compose.LocalAccountLinks(ws, acct), compose.RepoWriteLink(p.self(), repo, compose.ScopeAll),
		compose.RecordWriteLink(ws, r.Owner(), repo, compose.ScopeAll),
		compose.ServiceAuthLink(p.self(), compose.AppRef(compose.ComponentKindPds, compose.PdsComponent, ws), did),
		compose.ServiceAuthLink(p.guest(), compose.AppRef(compose.ComponentKindPds, compose.PdsComponent, ws), did))
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := c.ApplyLocalAccount(ctx, r, ws, acct); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := c.PruneAccountRepos(ctx, r, ws, BidderIdentity, compose.RepoRefIn(did, ws).Name); err != nil {
		return ctlruntime.Result{}, err
	}
	identity := compose.OperatorWatched(plan.Identity, o.Spec.Operator)
	identity.Did, identity.PdsURL, identity.Handle = did, pdsPublicURL, pdsHandle
	if err := c.ApplyIdentitySpec(ctx, r, ws, BidderIdentity, identity); err != nil {
		return p.retry("its app Pds identity", err)
	}
	if id, err = c.Identity(ctx, ws, BidderIdentity); err != nil {
		return ctlruntime.Result{}, err
	}
	if desired, err = p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, in); err != nil {
		return p.fail(err)
	}
	if err := p.c.EnsureLinked(ctx, p.r, desired, compose.WithoutPds(desired)); err != nil {
		return p.retry("", err)
	}
	if err := p.streams(ctx, plan, did); err != nil {
		return p.fail(err)
	}
	if err := p.prune(ctx, desired); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = id.Status.Did
	if err := p.rotateSigningKey(ctx, mount); err != nil {
		p.log().Warn("rotating the bidder's OIDC signing key", "err", err.Error())
	}
	if refused, err := p.guardSignIn(ctx, id, mount); err != nil || refused != "" {
		if err != nil {
			return ctlruntime.Result{}, err
		}
		return p.wait(refused)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := p.oauthEndpoints(ctx, snap); err != nil {
		return ctlruntime.Result{}, err
	}
	if o.Spec.Kcp != nil {
		p.st.Components = append(p.st.Components, p.guestComponents(ctx)...)
	}
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	p.associate(ctx, plan, id)
	if o.Spec.Profile == BidderProfileNetwork {
		p.operator(ctx, mount)
	}
	p.operatorSignIn(ctx, mount)
	if snap.Satisfied(desired) && p.st.Did != "" {
		return p.ready("every component is ready")
	}
	return p.wait("waiting for every component to be ready and the bidder's MarketIdentity for a did")
}

func (p *pass) operatorSession() bool {
	return p.o.Spec.Profile == BidderProfileNetwork || compose.OperatorSignInSet(p.o.Spec.Options)
}

func (p *pass) principal(ctx context.Context) string {
	if operator := strings.TrimSpace(p.o.Spec.Operator); identlink.IsDID(operator) {
		return operator
	}
	return p.c.IdentityDid(ctx, p.r.Workspace, compose.OperatorRepoName)
}

func (p *pass) borrowedHandle(ctx context.Context) []string {
	o := p.o
	handle := strings.TrimSpace(o.Spec.Identity.Handle)
	if handle == "" {
		return nil
	}
	operatorHandle := ""
	if op, err := p.c.Identity(ctx, p.r.Workspace, compose.OperatorRepoName); err == nil {
		operatorHandle = op.Status.Handle
	}
	for _, name := range []string{o.Spec.Operator, compose.OptionValue(o.Spec.Options, compose.OperatorSignInOption), operatorHandle} {
		if strings.EqualFold(strings.TrimSpace(name), handle) {
			return compose.BorrowedHandle(handle)
		}
	}
	if o.Spec.Profile == BidderProfileNetwork {
		return compose.BorrowedHandle(handle)
	}
	return nil
}

func (p *pass) associate(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity) {
	c, r := p.c, p.r
	agent := strings.TrimSpace(id.Status.Did)
	if !plan.Local || plan.Identity.Association == v1alpha1.AssociationSkip || agent == "" {
		return
	}
	if p.operatorSession() {
		if err := c.ApplyOperatorPair(ctx, r, r.Workspace, BidderIdentity, agent); err != nil {
			p.log().Warn("bidder_operator_pair", "agent", agent, "err", err.Error())
		}
	} else if err := c.DeleteOperatorPair(ctx, r.Workspace); err != nil {
		p.log().Warn("bidder_operator_pair_delete", "agent", agent, "err", err.Error())
	}
	principal := p.principal(ctx)
	if principal == "" {
		return
	}
	if err := c.UpdateAgentOf(ctx, r, r.Workspace, agent, compose.AgentOfEntries(principal), p.borrowedHandle(ctx)); err != nil {
		p.log().Warn("bidder_agent_of", "agent", agent, "principal", principal, "err", err.Error())
	}
}

func (p *pass) unassociate(ctx context.Context) {
	c, r := p.c, p.r
	agent, principal := c.IdentityDid(ctx, r.Workspace, BidderIdentity), p.principal(ctx)
	if agent == "" || principal == "" {
		return
	}
	if err := c.UpdateAgentOf(ctx, r, r.Workspace, agent, nil, compose.AgentOfEntries(principal)); err != nil {
		p.log().Warn("bidder_agent_of_remove", "agent", agent, "principal", principal, "err", err.Error())
	}
}

func (p *pass) operator(ctx context.Context, mount string) {
	c, r, o := p.c, p.r, p.o
	if o.Spec.Network == nil || o.Spec.Identity.Handle == "" || o.Spec.Identity.PdsURL == "" {
		return
	}
	op := compose.Operator{PdsURL: o.Spec.Identity.PdsURL, CARef: c.PublishNetworkTrust(r, o.Spec.Network), Identifier: o.Spec.Identity.Handle,
		Vault: BidderVaultServer, Mount: mount, Create: true}
	if err := c.ApplyLinks(ctx, r, "operator", compose.OperatorLinks(r, op)); err != nil {
		p.log().Warn("the operator account links", "err", err.Error())
		return
	}
	if compose.OperatorSignInSet(o.Spec.Options) {
		return
	}
	if err := c.ApplyOperatorRepo(ctx, r, r.Workspace, op); err != nil {
		p.log().Warn("composing the repo the market network operator account's declared records write through", "err", err.Error())
	}
}

func (p *pass) operatorSignIn(ctx context.Context, mount string) {
	c, r, o := p.c, p.r, p.o
	if !compose.OperatorSignInSet(o.Spec.Options) {
		return
	}
	var caRef *commonv1.ObjectRef
	if compose.OptionValue(o.Spec.Options, compose.OperatorSignInModeOption) == string(v1alpha1.MarketIdentityModePdsAccount) {
		caRef = c.PublishNetworkTrust(r, o.Spec.Network)
	}
	s, err := compose.OperatorSignInOf(o.Spec.Options, BidderVaultServer, mount, caRef)
	if err != nil {
		p.log().Warn("bidder_operator_signin_refused", "err", err.Error())
		return
	}
	did, err := c.ApplyOperatorSignIn(ctx, r, s)
	switch {
	case err != nil:
		p.log().Warn("bidder_operator_signin", "account", s.Account, "mode", string(s.Mode), "err", err.Error())
	case did == "":
		p.log().Info("bidder_operator_signin_waiting", "account", s.Account, "mode", string(s.Mode), "identity", compose.OperatorRepoName)
	}
}

func (p *pass) guardSignIn(ctx context.Context, id *v1alpha1.MarketIdentity, mount string) (string, error) {
	c, r, o := p.c, p.r, p.o
	if p.ctl.guard == nil || id == nil {
		return "", nil
	}
	refusal := p.ctl.guard(o.Spec, id)
	if refusal == nil {
		return "", nil
	}
	p.log().Error("bidder_account_refused", "handle", o.Spec.Identity.Handle, "signedIn", id.Status.Handle,
		"did", id.Status.Did, "reason", refusal.Error())
	if ref := id.Status.SessionRef; ref != nil {
		refMount := cmp.Or(strings.Trim(ref.Mount, "/"), mount)
		if kv, err := c.KV(ctx, r, BidderVaultServer, refMount+"/identity/*"); err == nil {
			if err := kv.Delete(ctx, refMount, ref.Path); err != nil {
				p.log().Warn("removing the refused sign-in session", "path", ref.Path, "err", err.Error())
			}
		} else {
			p.log().Warn("opening the bidder's vault to remove the refused session", "err", err.Error())
		}
	}
	if _, err := c.DeleteComponentsExcept(ctx, r.Workspace, r.Owner(), nil); err != nil {
		return "", err
	}
	ri, err := c.Client(r.Workspace, compose.MarketIdentityGVR)
	if err != nil {
		return "", err
	}
	if err := ri.Delete(ctx, BidderIdentity, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	c.Forget(r)
	return "the sign-in was refused and the bidder restarts with a fresh QR: " + refusal.Error(), nil
}
