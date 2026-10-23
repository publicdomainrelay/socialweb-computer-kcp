package appctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	BidderSet             = "market-bidder"
	BidderComponent       = "bidder"
	BidderApp             = "market-bidder"
	BidderIdentity        = "bidder"
	PdsComponent          = "pds"
	PdsVaultClient        = "pds"
	DefaultPolicy         = "open"
	BidderProfileProdQr   = "prod-qr"
	BidderProfileNetwork  = "market-network"
	BidderProfileDevLocal = "dev-local"
	GuestComponent        = "guest-kcp"
	GuestVaultClient      = "guest-kcp"
	GuestAccount          = "guest-kcp"
	PolicyEngineComponent = "policy-engine"
	GatewayAPIComponent   = "gateway-api"
	SshServerComponent    = "ssh-server"

	BidderDIDsAnnotation = "apps.socialweb.computer/bidder-dids"
)

var BidderProfiles = []string{BidderProfileProdQr, BidderProfileNetwork, BidderProfileDevLocal}

type desiredComponent struct {
	Kind string
	Name string
	Spec any
}

type componentSnapshot struct {
	Views      []ComponentView
	Components []v1alpha1.ApplicationComponent
	Listeners  []v1alpha1.ApplicationListener
}

func (ctl *Controller) componentSnapshot(ctx context.Context, owner string) (componentSnapshot, error) {
	views, err := ctl.ListComponents(ctx, owner)
	if err != nil {
		return componentSnapshot{}, err
	}
	snap := componentSnapshot{Views: views}
	for _, v := range views {
		if isExposureKind(v.Kind) {
			snap.Listeners = append(snap.Listeners, v.Listeners()...)
			continue
		}
		snap.Components = append(snap.Components, v.ApplicationComponent())
		snap.Listeners = append(snap.Listeners, v.Listeners()...)
	}
	sort.Slice(snap.Listeners, func(i, j int) bool {
		if snap.Listeners[i].Component != snap.Listeners[j].Component {
			return snap.Listeners[i].Component < snap.Listeners[j].Component
		}
		return snap.Listeners[i].Name < snap.Listeners[j].Name
	})
	return snap, nil
}

func (s componentSnapshot) view(kind, name string) (ComponentView, bool) {
	for _, v := range s.Views {
		if v.Kind == kind && v.Name == name {
			return v, true
		}
	}
	return ComponentView{}, false
}

func (s componentSnapshot) ready(kind, name string) bool {
	v, ok := s.view(kind, name)
	return ok && v.Status.Ready
}

func (s componentSnapshot) complete(desired []desiredComponent) bool {
	if len(desired) == 0 {
		return false
	}
	for _, d := range desired {
		if isExposureKind(d.Kind) {
			continue
		}
		if !s.ready(d.Kind, d.Name) {
			return false
		}
	}
	return true
}

func desiredKeys(desired []desiredComponent) []string {
	out := make([]string, 0, len(desired))
	for _, d := range desired {
		out = append(out, ComponentKey(d.Kind, d.Name))
	}
	return out
}

func desiredSpecPtr(spec any) any {
	if spec == nil {
		return nil
	}
	v := reflect.ValueOf(spec)
	if v.Kind() == reflect.Pointer {
		return spec
	}
	out := reflect.New(v.Type())
	out.Elem().Set(v)
	return out.Interface()
}

func (ctl *Controller) ensureDesired(ctx context.Context, root *unstructured.Unstructured, desired []desiredComponent) error {
	for _, d := range desired {
		if err := ctl.ensureComponent(ctx, root, d.Kind, d.Name, desiredSpecPtr(d.Spec)); err != nil {
			return fmt.Errorf("%s %s: %w", d.Kind, d.Name, err)
		}
	}
	return ctl.ensureDesiredLinks(ctx, root, desired)
}

func (ctl *Controller) pruneDesired(ctx context.Context, owner string, desired []desiredComponent) error {
	if _, err := ctl.DeleteComponentsExcept(ctx, owner, desiredKeys(desired)); err != nil {
		return err
	}
	return ctl.pruneDesiredLinks(ctx, owner, desired)
}

func desiredPds(desired []desiredComponent) (desiredComponent, bool) {
	for _, d := range desired {
		if d.Kind == ComponentKindPds && d.Name == PdsComponent {
			return d, true
		}
	}
	return desiredComponent{}, false
}

func withoutPds(desired []desiredComponent) []desiredComponent {
	out := make([]desiredComponent, 0, len(desired))
	for _, d := range desired {
		if d.Kind == ComponentKindPds && d.Name == PdsComponent {
			continue
		}
		out = append(out, d)
	}
	return out
}

func (ctl *Controller) applyInfra(ctx context.Context, setName, profile, owner string, values any, include ...string) error {
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(setName)
	if !ok {
		return fmt.Errorf("appctl: deploy/sets.yaml names no %s set", setName)
	}
	out := set
	out.Phases = nil
	for _, p := range set.Phases {
		base, only, scoped := strings.Cut(p.Name, "@")
		if !scoped {
			out.Phases = append(out.Phases, p)
			continue
		}
		if only == profile && slices.Contains(include, base) {
			out.Phases = append(out.Phases, p)
		}
	}
	ap, err := kcpapply.New(ctl.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, FieldManager: FieldManager, Base: ctl.c.workspace(),
		Labels: map[string]string{OwnerLabel: owner}, Log: ctl.c.log()})
	if err != nil {
		return err
	}
	_, err = ap.Apply(ctx, out, values)
	return err
}

func componentIdentity(id *v1alpha1.MarketIdentity, handle string) *v1alpha1.ComponentIdentity {
	out := &v1alpha1.ComponentIdentity{Handle: handle}
	if id != nil {
		out.MarketIdentityRef = ComponentRefFor("MarketIdentity", id.Name)
		out.Did = id.Status.Did
		if out.Handle == "" {
			out.Handle = id.Status.Handle
		}
	}
	return out
}

func componentServe(host string, port int32) *v1alpha1.ComponentServe {
	if host == "" {
		host = "127.0.0.1"
	}
	return &v1alpha1.ComponentServe{Host: host, Port: port}
}

func policyEngineDesired(suspend bool) desiredComponent {
	return desiredComponent{Kind: ComponentKindPolicyEngine, Name: PolicyEngineComponent,
		Spec: policyv1alpha1.PolicyEngineSpec{Suspend: suspend, BundledActionsEnabled: true}}
}

func componentRank(kind string) int {
	switch kind {
	case ComponentKindService, ComponentKindExposure:
		return 0
	case ComponentKindGatewayAPI, ComponentKindSshServer, ComponentKindBidder:
		return 1
	case ComponentKindRequester, ComponentKindGuestControlPlane, ComponentKindPolicyEngine:
		return 2
	case ComponentKindIngressProxy, ComponentKindDispatcherTLS:
		return 3
	case ComponentKindPlc, ComponentKindRelay:
		return 4
	case ComponentKindPds:
		return 5
	}
	return 6
}

func (ctl *Controller) componentsLeft(ctx context.Context, views []ComponentView) (int, error) {
	left := 0
	for _, v := range views {
		_, found, err := ctl.GetComponent(ctx, v.Kind, v.Name)
		if err != nil {
			return left, err
		}
		if found {
			left++
		}
	}
	return left, nil
}

func (ctl *Controller) stopComponents(ctx context.Context, owner string) (int, error) {
	views, err := ctl.ListComponents(ctx, owner)
	if err != nil {
		return 0, err
	}
	sort.SliceStable(views, func(i, j int) bool {
		if ri, rj := componentRank(views[i].Kind), componentRank(views[j].Kind); ri != rj {
			return ri < rj
		}
		if views[i].Kind != views[j].Kind {
			return views[i].Kind < views[j].Kind
		}
		return views[i].Name < views[j].Name
	})
	for start := 0; start < len(views); {
		rank := componentRank(views[start].Kind)
		end := start
		for end < len(views) && componentRank(views[end].Kind) == rank {
			end++
		}
		group := views[start:end]
		for _, v := range group {
			if err := ctl.DeleteComponent(ctx, v.Kind, v.Name); err != nil {
				left, _ := ctl.componentsLeft(ctx, views)
				return left, err
			}
		}
		deadline := time.Now().Add(ComponentStopWait)
		for {
			left, err := ctl.componentsLeft(ctx, group)
			if err != nil {
				return left, err
			}
			if left == 0 {
				break
			}
			if time.Now().After(deadline) {
				total, err := ctl.componentsLeft(ctx, views)
				return total, err
			}
			if !sleep(ctx, RetryAfter) {
				return left, ctx.Err()
			}
		}
		start = end
	}
	if err := ctl.deleteLinks(ctx, owner); err != nil {
		ctl.c.log().Warn("deleting the ComponentLinks of a stopped application", "owner", owner, "err", err.Error())
	}
	return ctl.componentsLeft(ctx, views)
}

func (ctl *Controller) releaseComponentRecords(ctx context.Context, recordsWs, owner string) (int, error) {
	ri, err := ctl.client(recordsWs, AtprotoRecordGVR)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || (err == nil && len(list.Items) == 0) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pdsUp := ctl.componentReady(ctx, ComponentKindPds, PdsComponent)
	for i := range list.Items {
		o := &list.Items[i]
		switch {
		case o.GetDeletionTimestamp() == nil:
			err = ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{})
		case !pdsUp:
			_, err = ri.Patch(ctx, o.GetName(), "merge", []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return len(list.Items), err
		}
	}
	return len(list.Items), nil
}

func (ctl *Controller) componentReady(ctx context.Context, kind, name string) bool {
	v, found, err := ctl.GetComponent(ctx, kind, name)
	return err == nil && found && v.Status.Ready
}

func policyTriggerNote(log *slog.Logger, name string) {
	log.Warn("no ComputeVM PolicyTrigger action handler is registered; ComputeVM triggers stay fail-closed",
		"marketbidder", name)
}

type SignInGuard func(spec v1alpha1.MarketBidderSpec, id *v1alpha1.MarketIdentity) error

const MarketRootNameArg = "--market-root-name"

func withRootName(extra *string, name string) error {
	f, err := fragment([]string{MarketRootNameArg, name})
	if err != nil {
		return err
	}
	*extra += f
	return nil
}

func fragment(items []string) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, it := range items {
		q, err := json.Marshal(it)
		if err != nil {
			return "", err
		}
		b.WriteString(",")
		b.Write(q)
	}
	return b.String(), nil
}

func portOf(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	_, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, fmt.Errorf("%s names no port", rawURL)
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("%s names no port", rawURL)
	}
	return n, nil
}

func listFragment(items ...string) (string, error) {
	var parts []string
	for _, it := range items {
		if it == "" {
			continue
		}
		q, err := json.Marshal(it)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(q))
	}
	return strings.Join(parts, ", "), nil
}

func BidderReads(v *manifests.MarketBidder) error {
	read, err := listFragment(v.CABundle)
	if err != nil {
		return err
	}
	v.Read = read
	return nil
}

func GuestReads(v *manifests.MarketBidder) error {
	read, err := listFragment(v.CABundle)
	if err != nil {
		return err
	}
	v.Guest.Read = read
	return nil
}

func BidderValues(spec v1alpha1.MarketBidderSpec, stateDir, guestStateDir string, kcp BidderKcp) (manifests.MarketBidder, error) {
	n := spec.Network
	if n == nil {
		return manifests.MarketBidder{}, errors.New("appctl: the MarketBidder names no network value; its composer or its profile sets spec.network")
	}
	tlsPort := 0
	if n.GuestTlsUrl != "" {
		p, err := portOf(n.GuestTlsUrl)
		if err != nil {
			return manifests.MarketBidder{}, fmt.Errorf("appctl: network guestTlsUrl: %w", err)
		}
		tlsPort = p
	} else if spec.Profile == BidderProfileNetwork {
		return manifests.MarketBidder{}, errors.New("appctl: the network value names no guestTlsUrl; a market network bidder hands guests the dispatcher's TLS front")
	}
	v := manifests.MarketBidder{StateDir: stateDir, Handle: spec.Identity.Handle, PdsUrl: spec.Identity.PdsUrl,
		PlcDirectoryUrl: n.PlcDirectoryUrl, IngressProxyHost: n.IngressProxyHost, RelayUrls: strings.Join(n.RelayUrls, ","),
		GuestTLSPort: tlsPort, CABundle: n.CaBundleRef, Policy: spec.Policy}
	if err := providerValues(&v, spec, kcp); err != nil {
		return manifests.MarketBidder{}, err
	}
	if v.Policy == "" {
		v.Policy = DefaultPolicy
	}
	trust, err := listFragment(n.CaBundleRef)
	if err != nil {
		return manifests.MarketBidder{}, err
	}
	v.Trust = trust
	if err := BidderReads(&v); err != nil {
		return manifests.MarketBidder{}, err
	}
	var extra []string
	if v.RelayUrls != "" {
		extra = append(extra, "--relay-url", v.RelayUrls)
	}
	if v.GuestTLSPort != 0 {
		extra = append(extra, "--guest-tls-port", strconv.Itoa(v.GuestTLSPort))
	}
	if len(n.FirehoseUrls) > 0 {
		extra = append(extra, "--firehose-url", strings.Join(n.FirehoseUrls, ","))
	}
	if n.QrOrigin != "" {
		extra = append(extra, "--qr-origin", n.QrOrigin)
	}
	if hosts := NetworkHostsArg(n.Hosts); hosts != "" {
		extra = append(extra, "--network-hosts", hosts)
		if v.Guest.ExtraArgs, err = fragment([]string{"--network-hosts", hosts}); err != nil {
			return manifests.MarketBidder{}, err
		}
	}
	v.Guest.StateDir, v.Guest.VaultClient = guestStateDir, GuestVaultClient
	if err := GuestReads(&v); err != nil {
		return manifests.MarketBidder{}, err
	}
	opts, err := OptionArgs(BidderApp, spec.Options)
	if err != nil {
		return manifests.MarketBidder{}, err
	}
	extra = append(extra, BidderQuotaArgs(spec.Quota)...)
	extra = append(extra, opts...)
	if v.ExtraArgs, err = fragment(extra); err != nil {
		return manifests.MarketBidder{}, err
	}
	return v, nil
}

func (ctl *Controller) bidderInfraValues(o v1alpha1.MarketBidder, stateRoot string) *manifests.MarketBidder {
	values := &manifests.MarketBidder{StateDir: stateRoot, Policy: o.Spec.Policy}
	server, _, _ := strings.Cut(ctl.restCfg.Host, "/clusters/")
	if o.Spec.Kcp != nil {
		guestStateDir := ComponentCRStateDir(stateRoot, ComponentKindGuestControlPlane, GuestComponent)
		if built, err := BidderValues(o.Spec, stateRoot, guestStateDir, BidderKcp{Server: server, Workspace: ctl.c.workspace()}); err == nil {
			values = &built
		}
	}
	if o.Spec.Network != nil {
		if pds, err := BidderPds(*values, stateRoot, o.Spec.Network); err == nil {
			values.Pds = pds
		}
	}
	return values
}

func BidderPds(v manifests.MarketBidder, stateDir string, n *v1alpha1.MarketNetworkValue) (manifests.RepoPds, error) {
	p := manifests.RepoPds{StateDir: stateDir, Identity: BidderIdentity, KcpServer: v.Kcp.Server, KcpWorkspace: v.Kcp.Workspace,
		VaultServer: v.Vault.Server, VaultClient: PdsVaultClient, VaultKcpServer: v.Vault.KcpServer, VaultMount: BidderVaultClient,
		VaultMountClient: BidderVaultClient, VaultWorkspace: v.Kcp.Workspace,
		PlcDirectoryUrl: n.PlcDirectoryUrl, IngressProxyHost: n.IngressProxyHost, Trust: v.Trust, Read: v.Read, AssociateService: BidderAssociate}
	var err error
	if p.WatchRelayUrls, err = listFragment(append(append([]string{}, n.RelayUrls...), n.FirehoseUrls...)...); err != nil {
		return p, err
	}
	if hosts := NetworkHostsArg(n.Hosts); hosts != "" {
		p.ExtraArgs, err = fragment([]string{"--network-hosts", hosts})
	}
	return p, err
}

func ready(status v1alpha1.ApplicationStatus, msg string) v1alpha1.ApplicationStatus {
	status.Phase, status.Message = v1alpha1.ApplicationPhaseReady, msg
	return status
}

func (ctl *Controller) bidderMarketURL(ctx context.Context, ws string) string {
	ri, err := ctl.client(ws, ExposureGVR)
	if err != nil {
		return ""
	}
	name := serviceNameFor(ComponentKindBidder, BidderComponent) + ExposureIngressSuffix
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	url, _, _ := unstructured.NestedString(u.Object, "status", "url")
	return strings.TrimRight(strings.TrimSpace(url), "/")
}

func bidderDesired(o v1alpha1.MarketBidder, plan accountPlan, id *v1alpha1.MarketIdentity, ws, stateRoot string, bidderDIDs []string, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *v1alpha1.ComponentRef) ([]desiredComponent, error) {
	suspend := o.Spec.Suspend
	network := o.Spec.Network
	ident := componentIdentity(id, o.Spec.Identity.Handle)
	vault := ComponentRefFor("OpenBaoServer", BidderVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	var pds desiredComponent
	if plan.Local {
		pdsSpec, err := appPdsSpec(stateRoot, network, pdsHandle, vault, hostnames, suspend, pdsPublicURL, requireIngress, ingressRef)
		if err != nil {
			return nil, err
		}
		pds = desiredComponent{Kind: ComponentKindPds, Name: PdsComponent, Spec: pdsSpec}
	}
	var guestPdsRef *v1alpha1.ComponentRef
	if plan.Local {
		guestPdsRef = ComponentRefFor(ComponentKindPds, PdsComponent)
	}
	guest := desiredComponent{Kind: ComponentKindGuestControlPlane, Name: GuestComponent, Spec: v1alpha1.GuestControlPlaneSpec{
		ComponentSpec: NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindGuestControlPlane, GuestComponent), network, suspend),
		Identity:      ident,
		PdsRef:        guestPdsRef,
		VaultRef:      vault,
		Hostnames:     hostnames,
	}}
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
	var pdsAccessTokenPathValue string
	if plan.Local {
		pdsAccessTokenPathValue = pdsAccessTokenPath("bidder")
	} else {
		pdsAccessTokenPathValue = plan.accessTokenPath()
	}
	bidder := desiredComponent{Kind: ComponentKindBidder, Name: BidderComponent, Spec: v1alpha1.BidderSpec{
		ComponentSpec:      NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindBidder, BidderComponent), network, suspend),
		MarketURL:          plan.MarketURL,
		Identity:           ident,
		PdsRef:             plan.componentPdsRef(),
		PDSAccessTokenPath: pdsAccessTokenPathValue,
		PolicyEngineRef:    ComponentRefFor(ComponentKindPolicyEngine, PolicyEngineComponent),
		VaultRef:           vault,
		Hostnames:          hostnames,
		GuestImageSource:   image,
		Firecracker:        firecracker,
		Kcp:                kcp,
		Operator:           o.Spec.Operator,
		Policy:             o.Spec.Policy,
		PolicyArgs:         optionValue(o.Spec.Options, "policy-args"),
		BidderDIDs:         append([]string(nil), bidderDIDs...),
		Comms:              o.Spec.Comms,
	}}
	out := []desiredComponent{policyEngineDesired(suspend), guest, bidder}
	if plan.Local {
		out = append([]desiredComponent{pds}, out...)
	}
	out = append(out, httpServiceExposures(ws, ComponentKindBidder, BidderComponent, "market", network, hostnames, false, true, suspend)...)
	out = append(out, httpServiceExposures(ws, ComponentKindGuestControlPlane, GuestComponent, "http", network, hostnames, false, true, suspend)...)
	if plan.Local {
		out = append(out, pdsExposures(ws, PdsComponent, network, hostnames, suspend, ingress)...)
	}
	return out, nil
}

func (ctl *Controller) reconcileBidderAccount(ctx context.Context, u *unstructured.Unstructured, o v1alpha1.MarketBidder,
	plan accountPlan, id *v1alpha1.MarketIdentity, owner, stateRoot string, bidderDIDs []string) error {
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	plan.MarketURL = ctl.bidderMarketURL(ctx, ctl.c.workspace())
	switch id.Status.Phase {
	case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, "its MarketIdentity: "+id.Status.Message
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	case v1alpha1.MarketIdentityPhaseReady:
	default:
		switch plan.Mode {
		case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
			desired, err := bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, "", "", nil, false, nil)
			if err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
				return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
			}
			if err := ctl.ensureDesired(ctx, u, desired); err != nil {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
				return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
			}
			_ = ctl.pruneDesired(ctx, owner, desired)
			if err := ctl.ensureOAuthIdentityEndpoints(ctx, owner, ctl.c.workspace(), BidderIdentity, serviceNameFor(ComponentKindBidder, BidderComponent)+ExposureIngressSuffix, "bidder"); err != nil {
				return err
			}
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its existing-account MarketIdentity to sign in"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if strings.TrimSpace(id.Status.Did) == "" {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its existing-account MarketIdentity to report a did"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if plan.Mode == v1alpha1.MarketIdentityModePdsAccount && strings.TrimSpace(id.Status.PdsUrl) == "" {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, "the existing-account MarketIdentity reports no pdsUrl; it fails closed"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	desired, err := bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, "", "", nil, false, nil)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err := ctl.ensureDesired(ctx, u, desired); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, desired); err != nil {
		return err
	}
	ctl.ensureAccountRepo(ctx, ctl.c.workspace(), BidderIdentity, plan, id.Status.Did, id.Status.PdsUrl)
	if err := ctl.rotateSigningKey(ctx, o); err != nil {
		ctl.c.log().Warn("rotating the bidder's OIDC signing key", "name", o.Name, "err", err.Error())
	}
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	st.Components, st.Listeners = snap.Components, snap.Listeners
	st.Did = id.Status.Did
	if snap.complete(desired) {
		st = ready(st, "the existing account is signed in and every component is ready")
	} else {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for every component to be ready"
	}
	return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
}

func (ctl *Controller) reconcileBidder(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.MarketBidder
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	bidderDIDs := annotationList(u.GetAnnotations()[BidderDIDsAnnotation])
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	if u.GetDeletionTimestamp() != nil {
		vms, err := ctl.deleteComputeVMs(ctx)
		if err != nil {
			return err
		}
		if vms > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d ComputeVM(s) releasing", vms)
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		records, err := ctl.releaseComponentRecords(ctx, ctl.c.workspace(), owner)
		if err != nil {
			return err
		}
		if records > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d AtprotoRecord(s) leaving the repo before the Pds stops", records)
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
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
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		delete(ctl.applied, string(o.UID))
		return ctl.setFinalizer(ctx, MarketBidderGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, MarketBidderGVR, u, true); err != nil {
			return err
		}
	}
	plan, err := ctl.planAccount(ctx, ctl.c.workspace(), o.Spec.Identity, "bidder",
		accountVault{Server: BidderVaultServer, Client: BidderVaultClient, Mount: BidderVaultClient},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	plan.MarketURL = ctl.bidderMarketURL(ctx, ctl.c.workspace())
	if o.Spec.Identity.Mode == "" {
		o.Spec.Identity.Mode = plan.Mode
	}
	stateRoot := ctl.c.Layout.StateDir
	pdsAccountHandle := pdsHandle(ctl.c.workspace(), "bidder", o.Spec.Identity.Handle)
	ingressSpec := AppIngressOrDefault(o.Spec.Profile, o.Spec.Ingress, o.Spec.Network)
	requireIngress, ingressRef, ingressConfig, ingressErr := pdsAppIngress(ingressSpec, o.Spec.Network, o.Spec.Profile == BidderProfileProdQr)
	pdsPublicURL, _ := ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, requireIngress)
	if o.Spec.Suspend {
		id, _ := ctl.identityIn(ctx, ctl.c.workspace(), BidderIdentity)
		desired, err := bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.ensureDesired(ctx, u, desired); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		_ = ctl.pruneDesired(ctx, owner, desired)
		snap, err := ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		st.Components, st.Listeners = snap.Components, snap.Listeners
		if id != nil {
			st.Did = id.Status.Did
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhasePending, "suspended"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if ctl.applied[string(o.UID)] != o.Generation {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its own OpenBaoServer and workspace"
		ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		values := ctl.bidderInfraValues(o, stateRoot)
		associationPhases := []string{"association-watch"}
		if !plan.usesAssociation() {
			associationPhases = nil
		}
		if err := ctl.applyInfra(ctx, BidderSet, o.Spec.Profile, owner, values, associationPhases...); err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Message = "its account password: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.ensureIdentitySpec(ctx, ctl.c.workspace(), owner, BidderIdentity, OperatorWatched(plan.Identity, o.Spec.Operator)); err != nil {
			st.Message = "its MarketIdentity: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if plan.Local {
			if _, err := ctl.bidderVault(ctx, o.Spec, ComponentCRStateDir(stateRoot, ComponentKindPds, PdsComponent)); err != nil {
				st.Message = "its vault and its PDS account: " + err.Error()
				return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
			}
			if err := ctl.ensureRepoKey(ctx, BidderVaultServer, BidderVaultClient, identityKeysPath); err != nil {
				st.Message = "its repository key: " + err.Error()
				return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
			}
		} else if _, _, _, err := ctl.vaultRefs(ctx, BidderVaultServer, BidderVaultClient); err != nil {
			st.Message = "its vault: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Message = "its account password: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		ctl.applied[string(o.UID)] = o.Generation
		policyTriggerNote(ctl.c.log(), o.Name)
	}
	id, err := ctl.identityIn(ctx, ctl.c.workspace(), BidderIdentity)
	if apierrors.IsNotFound(err) {
		delete(ctl.applied, string(o.UID))
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its MarketIdentity"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err != nil {
		return err
	}
	if !plan.Local {
		return ctl.reconcileBidderAccount(ctx, u, o, plan, id, owner, stateRoot, bidderDIDs)
	}
	desired, err := bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
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
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	if !snap.ready(ComponentKindPds, PdsComponent) {
		st.Components, st.Listeners = snap.Components, snap.Listeners
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the bidder's app Pds to be ready"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if requireIngress {
		if ingressErr != nil {
			setPdsIngressCondition(&st, o.Generation, true, ingressErr)
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the bidder's app Pds ingress: "+ingressErr.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.pdsIngressReady(snap, PdsComponent, ingressRef); err != nil {
			setPdsIngressCondition(&st, o.Generation, true, err)
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the bidder's app Pds ingress: "+err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		pdsPublicURL, err = ctl.pdsIngressURL(ctx, ctl.c.workspace(), PdsComponent)
		if err != nil {
			setPdsIngressCondition(&st, o.Generation, true, err)
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = pdsIngressPhase(err), "the bidder's app Pds ingress: "+err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		setPdsIngressCondition(&st, o.Generation, true, nil)
	} else {
		pdsPublicURL, _ = ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, false)
		setPdsIngressCondition(&st, o.Generation, false, nil)
	}
	if pdsPublicURL == "" {
		st.Components, st.Listeners = snap.Components, snap.Listeners
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the bidder's app Pds public URL"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if desired, err = bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	pds, _ := desiredPds(desired)
	if err := ctl.ensureDesired(ctx, u, []desiredComponent{pds}); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if applied, err := ctl.pdsApplied(ctx, owner); err != nil {
		return err
	} else if !applied {
		st.Components, st.Listeners = snap.Components, snap.Listeners
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the bidder's app Pds to apply its public URL"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	endpoint := ctl.pdsEndpointIn(ctx, ctl.c.workspace(), PdsComponent)
	if endpoint == "" {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the bidder's app Pds endpoint"
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	caFile := ""
	if o.Spec.Network != nil {
		caFile = o.Spec.Network.CaBundleRef
	}
	sess, err := ctl.ensurePdsAccountInVault(ctx, BidderVaultServer, BidderVaultClient, pdsSessionPath("bidder"), endpoint, caFile, pdsAccountHandle)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds account: "+err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	ctl.ensureAppPdsStatus(ctx, pdsAccountHandle, sess, BidderVaultServer+":"+pdsSessionPath("bidder"), o.Spec.Network != nil)
	if err := ctl.recordPdsIdentity(ctx, ctl.c.workspace(), BidderIdentity, pdsAccountHandle, pdsPublicURL, sess.Did, o.Spec.Identity.Association); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds identity: "+err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if id, err = ctl.identityIn(ctx, ctl.c.workspace(), BidderIdentity); err != nil {
		return err
	}
	if desired, err = bidderDesired(o, plan, id, ctl.c.workspace(), stateRoot, bidderDIDs, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err := ctl.ensureDesired(ctx, u, withoutPds(desired)); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err := ctl.ensureMarketStreams(ctx, ctl.c.workspace(), "bidder", o.Spec.Comms, o.Spec.Network); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, desired); err != nil {
		return err
	}
	if id, err = ctl.identityIn(ctx, ctl.c.workspace(), BidderIdentity); err != nil {
		return err
	}
	st.Did = id.Status.Did
	if err := ctl.rotateSigningKey(ctx, o); err != nil {
		ctl.c.log().Warn("rotating the bidder's OIDC signing key", "name", o.Name, "err", err.Error())
	}
	if refused, err := ctl.guardSignIn(ctx, o, owner); err != nil || refused != "" {
		if err != nil {
			return err
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, refused
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	snap, err = ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	if err := ctl.ensureOAuthIdentityEndpoints(ctx, owner, ctl.c.workspace(), BidderIdentity, serviceNameFor(ComponentKindBidder, BidderComponent)+ExposureIngressSuffix, "bidder"); err != nil {
		return err
	}
	st.Components, st.Listeners = snap.Components, snap.Listeners
	st.Did = id.Status.Did
	if o.Spec.Kcp != nil {
		st.Components = append(st.Components, ctl.guestComponents(ctx)...)
	}
	if o.Spec.Profile == BidderProfileNetwork {
		if err := ctl.associateNetworkOperator(ctx, o.Spec); err != nil {
			ctl.c.log().Warn("associating the bidder's DID with the market network operator account", "err", err.Error())
		}
		if err := ctl.reconcileOperatorRecords(ctx, ctl.c.workspace(), o.Spec.Network, func() (ownerSession, error) {
			password, _, err := ctl.bidderPassword(ctx)
			return ownerSession{Service: o.Spec.Identity.PdsUrl, Identifier: o.Spec.Identity.Handle, Password: password}, err
		}); err != nil {
			ctl.c.log().Warn("writing the declared records of the market network operator account", "err", err.Error())
		}
	}
	switch {
	case snap.complete(desired) && st.Did != "":
		st = ready(st, "every component is ready")
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for every component to be ready and the bidder's MarketIdentity for a did"
	}
	return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
}

func (ctl *Controller) bidderIdentity(ctx context.Context) (*v1alpha1.MarketIdentity, error) {
	ri, err := ctl.client(ctl.c.workspace(), MarketIdentityGVR)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, BidderIdentity, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var id v1alpha1.MarketIdentity
	if err := fromUnstructured(u, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

func (ctl *Controller) guardSignIn(ctx context.Context, o v1alpha1.MarketBidder, owner string) (string, error) {
	if ctl.c.SignInGuard == nil {
		return "", nil
	}
	id, err := ctl.bidderIdentity(ctx)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	refusal := ctl.c.SignInGuard(o.Spec, id)
	if refusal == nil {
		return "", nil
	}
	ctl.c.log().Error("bidder_account_refused", "name", o.Name, "handle", o.Spec.Identity.Handle, "signedIn", id.Status.Handle,
		"did", id.Status.Did, "reason", refusal.Error())
	if ref := id.Status.SessionRef; ref != nil {
		if client, _, _, err := ctl.vaultClient(ctx, BidderVaultServer, BidderVaultClient); err == nil {
			if err := client.Delete(ctx, ref.Mount+"/metadata/"+ref.Path); err != nil {
				ctl.c.log().Warn("removing the refused sign-in session", "path", ref.Path, "err", err.Error())
			}
			client.RevokeSelf(context.Background())
		} else {
			ctl.c.log().Warn("opening the bidder's vault to remove the refused session", "err", err.Error())
		}
	}
	if _, err := ctl.DeleteComponents(ctx, owner); err != nil {
		return "", err
	}
	if ri, err := ctl.client(ctl.c.workspace(), MarketIdentityGVR); err == nil {
		if err := ri.Delete(ctx, BidderIdentity, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	delete(ctl.applied, string(o.UID))
	return "the sign-in was refused and the bidder restarts with a fresh QR: " + refusal.Error(), nil
}
