package marketnetwork

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/jsonx"
)

const (
	Name = "marketnetwork"

	Kind = "MarketNetwork"

	NetworkSet          = "market-network"
	NetworkProfile      = "market-network"
	BidderAccountDomain = "bob.test"
	NetworkServiceState = "state"
	NetworkPortsFile    = "ports.json"
	NetworkPortFirst    = 30000
	NetworkPortLast     = 39999
	FrontRebindAfter    = 30 * time.Second

	NetworkAlicePdsName      = "alice"
	NetworkBobPdsName        = "bob"
	NetworkDispatcherTLSName = "dispatcher-tls"

	NetworkPlcHost        = "plc.default.global.svc.kcp.local"
	NetworkRelayHost      = v1alpha1.NetworkRelayHost
	NetworkIngressHost    = "dispatcher.default.relay.svc.kcp.local"
	NetworkDispatcherHost = v1alpha1.NetworkDispatcherHost
	NetworkAlicePdsHost   = "pds.default.alice.svc.kcp.local"
	NetworkBobPdsHost     = "pds.default.bob.svc.kcp.local"
	NetworkPortPlc        = "Global/plc"
	NetworkPortRelay      = "Relay/relay"
	NetworkPortIngress    = "IngressProxy/http"
	NetworkPortDispatcher = "DispatcherTLS/tls"
	NetworkPortAlicePds   = "AlicePds/http"
	NetworkPortBobPds     = "BobPds/http"

	ConditionServicesReady  = "ServicesReady"
	ConditionVerifierPassed = "VerifierPassed"
	ConditionOneKcp         = "OneKcp"

	VerifierProbeTimeout = 5 * time.Second

	BidderIdentity    = "bidder"
	RequesterIdentity = "requester"
)

var networkPortNames = []string{NetworkPortPlc, NetworkPortRelay, NetworkPortIngress, NetworkPortDispatcher, NetworkPortAlicePds, NetworkPortBobPds}

type Controller struct {
	c *compose.Composer

	mu sync.Mutex

	pkiPEM []byte

	peers string

	value *v1alpha1.MarketNetworkValue

	valueUID string

	unservedMu sync.Mutex

	unserved map[string]bool

	roots sync.Map
}

func Register(m *ctlruntime.Manager, c *compose.Composer) error {
	ctl := &Controller{c: c}
	c.ProvideTrust(schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind})
	opts := append([]ctlruntime.Option{ctlruntime.Export(compose.ExportApplications), ctlruntime.Finalizer(compose.Finalizer)}, c.Watches(Kind)...)
	opts = append(opts, childRootWatch[*v1alpha1.MarketBidder](ctl), childRootWatch[*v1alpha1.MarketRequester](ctl),
		childRootWatch[*v1alpha1.SshGateway](ctl), childRootWatch[*v1alpha1.Gateway](ctl))
	if err := ctlruntime.For[*v1alpha1.MarketNetwork](m, Name, ctl, opts...); err != nil {
		return fmt.Errorf("marketnetwork: registering the MarketNetwork controller: %w", err)
	}
	return nil
}

func childRootWatch[T ctlruntime.Object](ctl *Controller) ctlruntime.Option {
	return ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj T) []ctlruntime.Request {
		if obj.GetName() != compose.ChildRootName || !strings.HasPrefix(src.Workspace, ctl.c.Workspace()+":") {
			return nil
		}
		var out []ctlruntime.Request
		ctl.roots.Range(func(_, v any) bool {
			out = append(out, v.(ctlruntime.Request))
			return true
		})
		return out
	})
}

func (ctl *Controller) log() *slog.Logger { return ctl.c.Log().With("root", Name) }

type pass struct {
	ctx context.Context

	ctl *Controller

	c *compose.Composer

	r compose.Root

	o *v1alpha1.MarketNetwork

	st v1alpha1.MarketNetworkStatus
}

func (p *pass) write(phase v1alpha1.ApplicationPhase, msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = phase, msg
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.MarketNetworkGVR, p.st)
}

func (p *pass) retry(phase v1alpha1.ApplicationPhase, what string, err error) (ctlruntime.Result, error) {
	msg := err.Error()
	if what != "" {
		msg = what + ": " + msg
	}
	if _, werr := p.write(phase, msg); werr != nil {
		return ctlruntime.Result{}, errors.Join(err, werr)
	}
	return ctlruntime.Result{}, err
}

func (p *pass) waiting(msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseDeleting, msg
	return ctlruntime.Result{Waiting: true}, p.c.WriteStatus(p.ctx, p.r, compose.MarketNetworkGVR, p.st)
}

func (ctl *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.MarketNetwork) (ctlruntime.Result, error) {
	c := ctl.c
	if !c.Mine(req) {
		return ctlruntime.Result{}, nil
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	r := compose.RootOf(req, Kind, o, o.Spec.Suspend)
	c.Observe(r)
	ctl.roots.Store(string(r.UID), ctlruntime.Request{Cluster: req.Cluster, Name: req.Name})
	p := &pass{ctx: ctx, ctl: ctl, c: c, r: r, o: o, st: v1alpha1.MarketNetworkStatus{ObservedGeneration: o.Generation, Network: o.Status.Network,
		BidderPdsURL: o.Status.BidderPdsURL, Apps: o.Status.Apps, Conditions: append([]metav1.Condition(nil), o.Status.Conditions...)}}
	if r.Deleting {
		return p.delete(ctx)
	}
	res, err := p.reconcile(ctx)
	if err == nil {
		c.Adopt(ctx, r)
		c.ApplyQuotas(ctx, r, nil)
	}
	return res, err
}

func (p *pass) delete(ctx context.Context) (ctlruntime.Result, error) {
	if err := p.ctl.teardownAll(ctx, p.r, nil); err != nil {
		res, werr := p.waiting(err.Error())
		return res, errors.Join(err, werr)
	}
	left, err := p.c.StopComponents(ctx, p.r, "")
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if left > 0 {
		return p.waiting(fmt.Sprintf("%d component(s) stopping", left))
	}
	vaults, err := p.c.DeleteVaults(ctx, p.r)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if vaults > 0 {
		return p.waiting("the OpenBaoServer " + NetworkVaultServer + " is stopping")
	}
	if p.ctl.valueUID == string(p.r.UID) {
		p.ctl.value, p.ctl.valueUID = nil, ""
	}
	p.ctl.pkiPEM = nil
	p.ctl.roots.Delete(string(p.r.UID))
	p.c.Drop(p.r)
	p.ctl.log().Info("the market network is gone: children, components and its OpenBaoServer")
	return ctlruntime.Result{}, nil
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	return p.c.EnsureLinked(ctx, p.r, desired, desired)
}

func (p *pass) prune(ctx context.Context, desired []compose.Desired) error {
	_, err := p.c.DeleteComponentsExcept(ctx, p.r.Workspace, p.r.Owner(), compose.DesiredKeys(desired))
	return err
}

func (p *pass) snapshot(ctx context.Context) (compose.Snapshot, error) {
	snap, err := p.c.Snapshot(ctx, p.r.Workspace, p.r.Owner())
	if err != nil {
		return snap, err
	}
	p.st.Components, p.st.Listeners = snap.Components, snap.Listeners
	return snap, nil
}

func (p *pass) fill(desired []compose.Desired, snap compose.Snapshot) {
	compose.FillComponentStatus(&p.st.Conditions, &p.st.ComponentSummary, &p.st.ComponentOverrides, p.o.Generation, p.r.Suspend,
		p.o.Spec.Components, desired, snap, nil)
}

func (p *pass) reconcile(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o, ctl := p.c, p.r, p.o, p.ctl
	if o.Spec.Profile != NetworkProfile {
		return p.write(v1alpha1.ApplicationPhaseFailed, fmt.Sprintf("profile %q: the built MarketNetwork profile is %s", o.Spec.Profile, NetworkProfile))
	}
	ws := r.Workspace
	stateRoot := c.StateRoot()
	if err := c.ApplyVault(ctx, r, NetworkVaultServer); err != nil {
		return ctlruntime.Result{}, err
	}
	if !c.Applied(r) {
		if _, err := p.write(v1alpha1.ApplicationPhaseProvisioning, "applying the network's OpenBaoServer"); err != nil {
			return ctlruntime.Result{}, err
		}
		if err := c.ApplySetPhases(ctx, r, NetworkSet, &manifests.MarketNetwork{}, "vault"); err != nil {
			return p.retry(v1alpha1.ApplicationPhaseProvisioning, "the network's OpenBaoServer", err)
		}
		c.MarkApplied(r)
		ctl.log().Info("applied the market network's OpenBaoServer", "set", NetworkSet)
	}
	if err := ctl.vaultReady(ctx, ws); err != nil {
		return p.write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's OpenBaoServer: "+err.Error())
	}
	if err := c.ApplyLinks(ctx, r, "root", vaultLinks(r)); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := EnsureGuestTLS(filepath.Join(NetworkStateDir(stateRoot), GuestTLSDir)); err != nil {
		return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	leases, err := ctl.portLeases()
	if err != nil {
		return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	bundle, err := ctl.caBundle(ctx, r)
	if err != nil {
		return p.retry(v1alpha1.ApplicationPhaseProvisioning, "the network CA bundle", err)
	}
	c.PublishTrust(r.Ref(), []byte(bundle))
	value := networkValueFromLeases(leases, bundle)
	if ctl.value != nil && ctl.valueUID == string(r.UID) {
		value = *ctl.value
	}
	netIngress := compose.NetworkIngressConfig(o.Spec.Ingress, ws)
	value.Ingress = netIngress
	if err := compose.NetworkIngressSelection(netIngress); err != nil {
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, NetworkAlicePdsName, true, err)
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, NetworkBobPdsName, true, err)
		return p.write(v1alpha1.ApplicationPhaseFailed, "the network Pds ingress: "+err.Error())
	}
	for _, name := range []string{NetworkAlicePdsName, NetworkBobPdsName} {
		if err := compose.EnsurePDSKeyFiles(compose.ComponentCRStateDir(NetworkStateDir(stateRoot), compose.ComponentKindPds, name)); err != nil {
			return p.write(v1alpha1.ApplicationPhaseFailed, "the "+name+" PDS keys: "+err.Error())
		}
	}
	alicePublicURL, aliceIngressErr := c.PdsPublicURL(ctx, ws, NetworkAlicePdsName, true)
	bobPublicURL, bobIngressErr := c.PdsPublicURL(ctx, ws, NetworkBobPdsName, true)
	for _, item := range []struct {
		name string
		err  error
	}{{NetworkAlicePdsName, aliceIngressErr}, {NetworkBobPdsName, bobIngressErr}} {
		if item.err == nil || compose.PdsIngressReason(item.err) == compose.PdsIngressReasonNotReady {
			continue
		}
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, item.name, true, item.err)
		return p.write(v1alpha1.ApplicationPhaseFailed, "the "+item.name+" PDS ingress: "+item.err.Error())
	}
	compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, NetworkAlicePdsName, true, aliceIngressErr)
	compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, NetworkBobPdsName, true, bobIngressErr)
	desired, err := networkDesired(o, ws, stateRoot, leases, value, alicePublicURL, bobPublicURL)
	if err != nil {
		compose.SetOverrideErrorIfAny(&p.st.Conditions, &p.st.ComponentSummary, o.Generation, err)
		return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
	}
	if err := c.ApplyRelayStatus(ctx, r, compose.NetworkRelayName); err != nil {
		return ctlruntime.Result{}, err
	}
	applyDesired := desired
	if alicePublicURL == "" || bobPublicURL == "" {
		appliedAlice, err := c.PdsPublicURLApplied(ctx, ws, NetworkAlicePdsName)
		if err != nil {
			return ctlruntime.Result{}, err
		}
		appliedBob, err := c.PdsPublicURLApplied(ctx, ws, NetworkBobPdsName)
		if err != nil {
			return ctlruntime.Result{}, err
		}
		if appliedAlice && appliedBob {
			applyDesired = compose.WithoutPdsNamed(desired, NetworkAlicePdsName, NetworkBobPdsName)
		}
	}
	if r.Suspend {
		return p.suspended(ctx, desired, applyDesired)
	}
	if err := p.c.EnsureLinked(ctx, p.r, desired, applyDesired); err != nil {
		return p.retry(v1alpha1.ApplicationPhaseProvisioning, "", err)
	}
	if err := p.prune(ctx, desired); err != nil {
		return ctlruntime.Result{}, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	p.fill(desired, snap)
	if !snap.Satisfied(desired) {
		var waiting []string
		for _, d := range desired {
			if !compose.IsExposureKind(d.Kind) && !snap.Ready(d.Kind, d.Name) {
				waiting = append(waiting, compose.ComponentKey(d.Kind, d.Name))
			}
		}
		conds.Set(&p.st.Conditions, o.Generation, ConditionServicesReady, false, "Waiting", strings.Join(waiting, "; "))
		return p.write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's components to report ready")
	}
	if alicePublicURL == "" || bobPublicURL == "" {
		return p.write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's PDS ingress URLs")
	}
	published, err := networkValueFromExposures(snap, bundle)
	if err != nil {
		if ctl.value == nil || ctl.valueUID != string(r.UID) {
			conds.Set(&p.st.Conditions, o.Generation, ConditionServicesReady, false, "Endpoints", err.Error())
			return p.write(v1alpha1.ApplicationPhaseProvisioning, "the network's Exposure endpoints: "+err.Error())
		}
		published = *ctl.value
	} else {
		published.Ingress = netIngress
		if ctl.value == nil || ctl.valueUID != string(r.UID) || !jsonx.Equal(*ctl.value, published) {
			ctl.value, ctl.valueUID = &published, string(r.UID)
			if desired, err = networkDesired(o, ws, stateRoot, leases, published, alicePublicURL, bobPublicURL); err != nil {
				compose.SetOverrideErrorIfAny(&p.st.Conditions, &p.st.ComponentSummary, o.Generation, err)
				return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
			}
			if err := p.ensure(ctx, desired); err != nil {
				return p.retry(v1alpha1.ApplicationPhaseProvisioning, "", err)
			}
			if err := p.prune(ctx, desired); err != nil {
				return ctlruntime.Result{}, err
			}
		}
	}
	published.Ingress = netIngress
	p.st.Network = &published
	if o.Status.Network == nil || !jsonx.Equal(*o.Status.Network, published) {
		p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseProvisioning, "publishing the network endpoints before the child applications start"
		if err := c.WriteStatus(ctx, r, compose.MarketNetworkGVR, p.st); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	pdsURL, _ := snap.ExposureURL(compose.ServiceNameFor(compose.ComponentKindPds, NetworkBobPdsName) + compose.ExposureLocalSuffix)
	aliceURL, _ := snap.ExposureURL(compose.ServiceNameFor(compose.ComponentKindPds, NetworkAlicePdsName) + compose.ExposureLocalSuffix)
	if pdsURL == "" {
		pdsURL = networkValueHostURL(published, NetworkBobPdsHost)
	}
	if aliceURL == "" {
		aliceURL = networkValueHostURL(published, NetworkAlicePdsHost)
	}
	if pdsURL == "" || aliceURL == "" {
		return p.write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's PDS Exposure URLs")
	}
	p.st.BidderPdsURL = pdsURL
	var keep []string
	for _, app := range o.Spec.Apps {
		if ch, err := ctl.child(app); err == nil {
			keep = append(keep, ch.id)
		}
	}
	if err := ctl.teardownAll(ctx, r, keep); err != nil {
		ctl.log().Warn("removing children the spec no longer names", "err", err.Error())
	}
	apps := ctl.ensureApps(ctx, r, o, published, pdsURL, aliceURL, false)
	unserved := ctl.takeUnserved()
	ctl.declareOperatorVouches(ctx, r, o.Spec.Apps)
	p.st.Apps = apps
	names := make([]string, 0, len(o.Spec.Apps))
	for _, a := range o.Spec.Apps {
		names = append(names, a.Name)
	}
	if err := ctl.grantPeers(ctx, r, names); err != nil {
		ctl.log().Warn("granting the child applications reads of each other's MarketIdentity objects", "err", err.Error())
	}
	conds.Set(&p.st.Conditions, o.Generation, ConditionServicesReady, true, "Ready",
		fmt.Sprintf("%d component(s) are ready and report their endpoints", len(snap.Views)))
	if wrong := ctl.oneKcp(ws, apps); len(wrong) > 0 {
		conds.Set(&p.st.Conditions, o.Generation, ConditionOneKcp, false, "Apart", strings.Join(wrong, "; "))
	} else {
		conds.Set(&p.st.Conditions, o.Generation, ConditionOneKcp, true, "Workspaces", "every child application runs in its own workspace of the network's kcp")
	}
	if err := ctl.verify(ctx, snap, apps); err != nil {
		conds.Set(&p.st.Conditions, o.Generation, ConditionVerifierPassed, false, "Probe", err.Error())
		if unserved != nil {
			return p.retry(v1alpha1.ApplicationPhaseProvisioning, "the network verifier: "+err.Error(), unserved)
		}
		return p.write(v1alpha1.ApplicationPhaseProvisioning, "the network verifier: "+err.Error())
	}
	conds.Set(&p.st.Conditions, o.Generation, ConditionVerifierPassed, true, "Verified",
		"the PLC, relay, PDS, ingress and child application endpoints answer")
	for _, t := range []string{ConditionServicesReady, ConditionOneKcp, ConditionVerifierPassed} {
		if cond := meta.FindStatusCondition(p.st.Conditions, t); cond == nil || cond.Status != metav1.ConditionTrue {
			return p.write(v1alpha1.ApplicationPhaseProvisioning, "the child applications are ready; waiting for condition "+t)
		}
	}
	return p.write(v1alpha1.ApplicationPhaseReady, fmt.Sprintf("%d component(s) and %d child application(s), each in its own workspace of one kcp, are ready", len(snap.Views), len(apps)))
}

func (p *pass) suspended(ctx context.Context, desired, applyDesired []compose.Desired) (ctlruntime.Result, error) {
	c, r, o, ctl := p.c, p.r, p.o, p.ctl
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	held := compose.Existing(snap, applyDesired)
	if err := p.ensure(ctx, held); err != nil {
		return p.retry(v1alpha1.ApplicationPhaseSuspended, "", err)
	}
	p.fill(compose.Existing(snap, desired), snap)
	value := o.Status.Network
	if ctl.value != nil && ctl.valueUID == string(r.UID) {
		value = ctl.value
	}
	if value != nil {
		pdsURL, aliceURL := o.Status.BidderPdsURL, networkValueHostURL(*value, NetworkAlicePdsHost)
		if u, ok := snap.ExposureURL(compose.ServiceNameFor(compose.ComponentKindPds, NetworkAlicePdsName) + compose.ExposureLocalSuffix); ok {
			aliceURL = u
		}
		p.st.Apps = ctl.ensureApps(ctx, r, o, *value, pdsURL, aliceURL, true)
	}
	conds.Set(&p.st.Conditions, o.Generation, ConditionVerifierPassed, false, compose.ReasonSuspended, "the network is suspended")
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.MarketNetworkGVR, p.suspendStatus())
}

func (p *pass) suspendStatus() v1alpha1.MarketNetworkStatus {
	st := p.st
	st.Phase, st.Message = v1alpha1.ApplicationPhaseSuspended, compose.SuspendedMessage
	return st
}

func (ctl *Controller) ensureApps(ctx context.Context, r compose.Root, o *v1alpha1.MarketNetwork, value v1alpha1.MarketNetworkValue,
	pdsURL, aliceURL string, placedOnly bool) []v1alpha1.NetworkAppStatus {
	var bidderDids []string
	known := map[string]string{}
	for _, a := range o.Status.Apps {
		known[a.Name] = a.Did
		if a.Kind == v1alpha1.ApplicationKindMarketBidder && a.Did != "" {
			bidderDids = append(bidderDids, a.Did)
		}
	}
	apps := make([]v1alpha1.NetworkAppStatus, len(o.Spec.Apps))
	var wg sync.WaitGroup
	for i, app := range o.Spec.Apps {
		wg.Go(func() {
			apps[i] = ctl.ensureApp(ctx, r, app, value, o.Spec.Comms, pdsURL, aliceURL, bidderDids, o.Spec.Suspend || app.Suspend, placedOnly)
			apps[i].Did = cmp.Or(apps[i].Did, known[app.Name])
		})
	}
	wg.Wait()
	return apps
}
