package appctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	RequesterSet            = "market-requester"
	RequesterComponent      = "requester"
	RequesterApp            = "market-requester"
	RequesterVaultServer    = "requester"
	RequesterVaultClient    = "requester"
	RequesterAccount        = "requester"
	RequesterIdentity       = "requester"
	RequesterContract       = "contract"
	RequesterProfileNetwork = "market-network"
	RequesterProfileDefault = "default"
	guestRouteProbe         = "1.1.1.1:53"
)

var (
	MarketIdentityGVR  = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "marketidentities"}
	ComputeContractGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "computecontracts"}
)

func GuestHostIP() (string, error) {
	c, err := net.Dial("udp4", guestRouteProbe)
	if err != nil {
		return "", fmt.Errorf("appctl: the host address guests reach: %w", err)
	}
	defer c.Close()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP.IsLoopback() || a.IP.IsUnspecified() {
		return "", fmt.Errorf("appctl: the host has no routable address for guests (%v)", c.LocalAddr())
	}
	return a.IP.String(), nil
}

func hostOf(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func guestAliases(n *v1alpha1.MarketNetworkValue) ([]string, error) {
	var names []string
	for _, h := range []string{hostOf(n.IngressProxyHost), hostOf(n.GuestTlsUrl)} {
		if h != "" && net.ParseIP(h) == nil && !slices.Contains(names, h) {
			names = append(names, h)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	ip, err := GuestHostIP()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, h := range names {
		out = append(out, ip+" "+h)
	}
	return out, nil
}

func NetworkHostsArg(hosts []v1alpha1.NetworkHost) string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name+"="+h.Address)
	}
	return strings.Join(out, ",")
}

func RequesterValues(spec v1alpha1.MarketRequesterSpec, stateDir string) (manifests.MarketRequester, error) {
	v := manifests.MarketRequester{StateDir: stateDir}
	var extra []string
	if n := spec.Network; n != nil {
		v.CABundle = n.CaBundleRef
		for _, kv := range [][2]string{
			{"--plc-directory-url", n.PlcDirectoryUrl},
			{"--ingress-proxy-host", n.IngressProxyHost},
			{"--relay-url", strings.Join(n.RelayUrls, ",")},
			{"--firehose-url", strings.Join(n.FirehoseUrls, ",")},
			{"--qr-origin", n.QrOrigin},
		} {
			if kv[1] != "" {
				extra = append(extra, kv[0], kv[1])
			}
		}
		if n.GuestTlsUrl != "" {
			p, err := portOf(n.GuestTlsUrl)
			if err != nil {
				return manifests.MarketRequester{}, fmt.Errorf("appctl: network guestTlsUrl: %w", err)
			}
			extra = append(extra, "--guest-tls-port", strconv.Itoa(p))
		}
		if hosts := NetworkHostsArg(n.Hosts); hosts != "" {
			extra = append(extra, "--network-hosts", hosts)
		}
		if spec.Profile == RequesterProfileNetwork {
			aliases, err := guestAliases(n)
			if err != nil {
				return v, err
			}
			if len(aliases) > 0 {
				extra = append(extra, "--guest-host-aliases", strings.Join(aliases, ","))
			}
		}
	}
	if spec.Profile == RequesterProfileNetwork && (spec.Network == nil || v.CABundle == "") {
		return v, errors.New("appctl: the market-network profile needs the network value with its caBundleRef; the MarketNetwork composer copies it")
	}
	if len(spec.BidderDids) > 0 {
		extra = append(extra, "--bidder-dids", strings.Join(spec.BidderDids, ","))
	}
	opts, err := OptionArgs(RequesterApp, spec.Options)
	if err != nil {
		return v, err
	}
	extra = append(extra, opts...)
	if v.ExtraArgs, err = fragment(extra); err != nil {
		return v, err
	}
	return v, nil
}

func OperatorWatched(s v1alpha1.MarketIdentitySpec, operator string) v1alpha1.MarketIdentitySpec {
	if s.Mode == v1alpha1.MarketIdentityModeLocalRepo && s.Association != v1alpha1.AssociationSkip {
		s.AssociateWith, s.AssociationWatch = operator, AssociationWatch
	}
	return s
}

func (ctl *Controller) ensureIdentitySpec(ctx context.Context, ws, owner, name string, spec v1alpha1.MarketIdentitySpec) error {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return err
	}
	u, err := toUnstructured(&v1alpha1.MarketIdentity{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{OwnerLabel: owner}}, Spec: spec}, "MarketIdentity")
	if err != nil {
		return err
	}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (ctl *Controller) identityDid(ctx context.Context) string {
	return ctl.identityDidIn(ctx, ctl.c.workspace())
}

func (ctl *Controller) identityDidIn(ctx context.Context, ws string) string {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return ""
	}
	u, err := ri.Get(ctx, RequesterIdentity, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	did, _, _ := unstructured.NestedString(u.Object, "status", "did")
	return did
}

func (ctl *Controller) requesterKcp(ctx context.Context, stateDir string) (manifests.KcpRef, manifests.BidderVault, error) {
	if _, _, _, err := ctl.vaultRefs(ctx, RequesterVaultServer, RequesterVaultClient); err != nil {
		return manifests.KcpRef{}, manifests.BidderVault{}, err
	}
	vault := manifests.BidderVault{Server: RequesterVaultServer, Client: RequesterVaultClient}
	return manifests.KcpRef{Workspace: ctl.c.workspace()}, vault, nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

func (ctl *Controller) vaultRefs(ctx context.Context, server, clientName string) (string, v1alpha1.OpenBaoServerStatus, v1alpha1.OpenBaoServerClientRefs, error) {
	ri, err := ctl.client(ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return "", v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	u, err := ri.Get(ctx, server, metav1.GetOptions{})
	if err != nil {
		return "", v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	var o v1alpha1.OpenBaoServer
	if err := fromUnstructured(u, &o); err != nil {
		return "", o.Status, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	st := o.Status
	if st.Address == "" || st.Refs == nil {
		return "", st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no address yet (phase %s)", server, st.Phase)
	}
	for _, x := range st.Clients {
		if x.Name == clientName && x.ClientCert != "" {
			return st.Address, st, x, nil
		}
	}
	return "", st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no client %s yet", server, clientName)
}

func requesterDesired(o v1alpha1.MarketRequester, plan accountPlan, id *v1alpha1.MarketIdentity, ws, stateRoot, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *v1alpha1.ComponentRef) ([]desiredComponent, error) {
	suspend := o.Spec.Suspend
	network := o.Spec.Network
	ident := componentIdentity(id, o.Spec.Identity.Handle)
	vault := ComponentRefFor("OpenBaoServer", RequesterVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	local := plan.Local
	mode, publicOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := requesterComponentFields(o.Spec.Options, plan.Mode)
	requesterSpec := v1alpha1.RequesterSpec{
		ComponentSpec:    NewComponentSpec(ComponentCRStateDir(stateRoot, ComponentKindRequester, RequesterComponent), network, suspend),
		Identity:         ident,
		PolicyEngineRef:  ComponentRefFor(ComponentKindPolicyEngine, PolicyEngineComponent),
		VaultRef:         vault,
		Hostnames:        hostnames,
		Mode:             mode,
		PublicOrigin:     publicOrigin,
		OAuthClientID:    oauthClientID,
		OAuthRedirectURI: oauthRedirectURI,
		GuestSSHKeysFile: guestSSHKeysFile,
		AcceptCopies:     int32(optionInt(o.Spec.Options, "accept-copies")),
	}
	out := []desiredComponent{policyEngineDesired(suspend)}
	if local {
		pdsSpec, err := appPdsSpec(stateRoot, network, pdsHandle, vault, hostnames, suspend, pdsPublicURL, requireIngress, ingressRef)
		if err != nil {
			return nil, err
		}
		out = append(out, desiredComponent{Kind: ComponentKindPds, Name: PdsComponent, Spec: pdsSpec})
		requesterSpec.PdsRef = ComponentRefFor(ComponentKindPds, PdsComponent)
		requesterSpec.PDSAccessTokenPath = pdsAccessTokenPath("requester")
	} else {
		requesterSpec.PdsRef = plan.PdsRef
		requesterSpec.PDSAccessTokenPath = plan.accessTokenPath()
	}
	out = append(out, desiredComponent{Kind: ComponentKindRequester, Name: RequesterComponent, Spec: requesterSpec})
	out = append(out, httpServiceExposures(ws, ComponentKindRequester, RequesterComponent, "http", network, hostnames, false, true, suspend)...)
	if local {
		out = append(out, pdsExposures(ws, PdsComponent, network, hostnames, suspend, ingress)...)
	}
	return out, nil
}

func (ctl *Controller) reconcileRequesterAccount(ctx context.Context, u *unstructured.Unstructured, o v1alpha1.MarketRequester,
	plan accountPlan, id *v1alpha1.MarketIdentity, owner, stateRoot string) error {
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	switch id.Status.Phase {
	case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, "its MarketIdentity: "+id.Status.Message
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	case v1alpha1.MarketIdentityPhaseReady:
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its existing-account MarketIdentity to sign in"
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if strings.TrimSpace(id.Status.Did) == "" {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its existing-account MarketIdentity to report a did"
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if plan.Mode == v1alpha1.MarketIdentityModePdsAccount && strings.TrimSpace(id.Status.PdsUrl) == "" {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, "the existing-account MarketIdentity reports no pdsUrl; it fails closed"
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	desired, err := requesterDesired(o, plan, id, ctl.c.workspace(), stateRoot, "", "", nil, false, nil)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if err := ctl.ensureDesired(ctx, u, desired); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, desired); err != nil {
		return err
	}
	ctl.ensureAccountRepo(ctx, ctl.c.workspace(), RequesterIdentity, plan, id.Status.Did)
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
	return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
}

func (ctl *Controller) reconcileRequester(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.MarketRequester
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	if u.GetDeletionTimestamp() != nil {
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
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		delete(ctl.applied, string(o.UID))
		return ctl.setFinalizer(ctx, MarketRequesterGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, MarketRequesterGVR, u, true); err != nil {
			return err
		}
	}
	plan, err := ctl.planAccount(ctx, ctl.c.workspace(), o.Spec.Identity, "requester",
		accountVault{Server: RequesterVaultServer, Client: RequesterVaultClient, Mount: RequesterVaultClient},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	stateRoot := ctl.c.Layout.StateDir
	pdsAccountHandle := pdsHandle(ctl.c.workspace(), "requester", o.Spec.Identity.Handle)
	ingressSpec := AppIngressOrDefault(o.Spec.Profile, o.Spec.Ingress, o.Spec.Network)
	requireIngress, ingressRef, ingressConfig, ingressErr := pdsAppIngress(ingressSpec, o.Spec.Network, o.Spec.Profile == RequesterProfileDefault)
	pdsPublicURL, _ := ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, requireIngress)
	if o.Spec.Suspend {
		id, _ := ctl.identityIn(ctx, ctl.c.workspace(), RequesterIdentity)
		desired, err := requesterDesired(o, plan, id, ctl.c.workspace(), stateRoot, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if err := ctl.ensureDesired(ctx, u, desired); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
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
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if ctl.applied[string(o.UID)] != o.Generation {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its schemas, its own OpenBaoServer and its workspace"
		ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		if err := ctl.applyInfra(ctx, RequesterSet, o.Spec.Profile, owner, &manifests.MarketRequester{}); err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if err := ctl.ensureIdentitySpec(ctx, ctl.c.workspace(), owner, RequesterIdentity, plan.Identity); err != nil {
			st.Message = "its MarketIdentity: " + err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if _, _, err := ctl.requesterKcp(ctx, ComponentCRStateDir(stateRoot, ComponentKindRequester, RequesterComponent)); err != nil {
			st.Message = "its vault and its kcp account: " + err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if err := ctl.seedPdsAccountPassword(ctx, plan); err != nil {
			st.Message = "its account password: " + err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		ctl.applied[string(o.UID)] = o.Generation
	}
	id, err := ctl.identityIn(ctx, ctl.c.workspace(), RequesterIdentity)
	if apierrors.IsNotFound(err) {
		delete(ctl.applied, string(o.UID))
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for its MarketIdentity"
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if err != nil {
		return err
	}
	if !plan.Local {
		return ctl.reconcileRequesterAccount(ctx, u, o, plan, id, owner, stateRoot)
	}
	desired, err := requesterDesired(o, plan, id, ctl.c.workspace(), stateRoot, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
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
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		snap, err := ctl.componentSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		if !snap.ready(ComponentKindPds, PdsComponent) {
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's app Pds to be ready"
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if requireIngress {
			if ingressErr != nil {
				setPdsIngressCondition(&st, o.Generation, true, ingressErr)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "the app Pds ingress: "+ingressErr.Error()
				return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
			}
			if err := ctl.pdsIngressReady(snap, PdsComponent, ingressRef); err != nil {
				setPdsIngressCondition(&st, o.Generation, true, err)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds ingress: "+err.Error()
				return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
			}
			pdsPublicURL, err = ctl.pdsIngressURL(ctx, ctl.c.workspace(), PdsComponent)
			if err != nil {
				setPdsIngressCondition(&st, o.Generation, true, err)
				st.Components, st.Listeners = snap.Components, snap.Listeners
				st.Phase, st.Message = pdsIngressPhase(err), "the app Pds ingress: "+err.Error()
				return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
			}
			setPdsIngressCondition(&st, o.Generation, true, nil)
		} else {
			pdsPublicURL, _ = ctl.pdsPublicURLFor(ctx, ctl.c.workspace(), PdsComponent, false)
			setPdsIngressCondition(&st, o.Generation, false, nil)
		}
		if pdsPublicURL == "" {
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds public URL"
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if desired, err = requesterDesired(o, plan, id, ctl.c.workspace(), stateRoot, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		allDesired = desired
		pds, _ = desiredPds(desired)
		if err := ctl.ensureDesired(ctx, u, []desiredComponent{pds}); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if applied, err := ctl.pdsApplied(ctx, owner); err != nil {
			return err
		} else if !applied {
			st.Components, st.Listeners = snap.Components, snap.Listeners
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the app Pds to apply its public URL"
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		endpoint := ctl.pdsEndpointIn(ctx, ctl.c.workspace(), PdsComponent)
		if endpoint == "" {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's app Pds endpoint"
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		caFile := ""
		if o.Spec.Network != nil {
			caFile = o.Spec.Network.CaBundleRef
		}
		sess, err := ctl.ensurePdsAccountInVault(ctx, RequesterVaultServer, RequesterVaultClient, pdsSessionPath("requester"), endpoint, caFile, pdsAccountHandle)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds account: "+err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		ctl.ensureAppPdsStatus(ctx, pdsAccountHandle, sess, RequesterVaultServer+":"+pdsSessionPath("requester"), o.Spec.Network != nil)
		changed, err := ctl.syncRequesterRepoKey(stateRoot, sess.Did)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds signing key: "+err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if changed {
			if err := ctl.RestartComponentIn(ctx, ctl.c.workspace(), ComponentKindRequester, RequesterComponent); err != nil && !apierrors.IsNotFound(err) {
				st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "restarting the requester: "+err.Error()
				return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
			}
		}
		if err := ctl.recordPdsIdentity(ctx, ctl.c.workspace(), RequesterIdentity, pdsAccountHandle, endpoint, sess.Did, o.Spec.Identity.Association); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "its app Pds identity: "+err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if id, err = ctl.identityIn(ctx, ctl.c.workspace(), RequesterIdentity); err != nil {
			return err
		}
		if desired, err = requesterDesired(o, plan, id, ctl.c.workspace(), stateRoot, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef); err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		allDesired = desired
		desired = withoutPds(desired)
	}
	if err := ctl.ensureDesired(ctx, u, desired); err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
		return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
	}
	if err := ctl.pruneDesired(ctx, owner, allDesired); err != nil {
		return err
	}
	if id, err = ctl.identityIn(ctx, ctl.c.workspace(), RequesterIdentity); err != nil {
		return err
	}
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return err
	}
	st.Components, st.Listeners = snap.Components, snap.Listeners
	st.Did = id.Status.Did
	switch {
	case snap.complete(allDesired) && st.Did != "":
		st = ready(st, "the requester is signed in and its components are ready")
	case st.Did == "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's MarketIdentity to sign in"
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for every component to be ready"
	}
	return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
}
