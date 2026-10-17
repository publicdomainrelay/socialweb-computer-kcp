package appctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

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

func IdentitySpecOf(id v1alpha1.ApplicationIdentity) v1alpha1.MarketIdentitySpec {
	s := v1alpha1.MarketIdentitySpec{Mode: id.Mode, Handle: id.Handle, PdsUrl: id.PdsUrl, Association: id.Association}
	if s.Mode == "" {
		s.Mode = v1alpha1.MarketIdentityModeLocalRepo
	}
	if s.Association == "" {
		s.Association = v1alpha1.AssociationRequireApproval
	}
	return s
}

func OperatorWatched(s v1alpha1.MarketIdentitySpec, operator string) v1alpha1.MarketIdentitySpec {
	if s.Mode == v1alpha1.MarketIdentityModeLocalRepo && s.Association != v1alpha1.AssociationSkip {
		s.AssociateWith, s.AssociationWatch = operator, AssociationWatch
	}
	return s
}

func (ctl *Controller) ensureIdentity(ctx context.Context, name string, id v1alpha1.ApplicationIdentity) error {
	return ctl.ensureIdentitySpec(ctx, ctl.c.workspace(), name, IdentitySpecOf(id))
}

func (ctl *Controller) ensureIdentitySpec(ctx context.Context, ws, name string, spec v1alpha1.MarketIdentitySpec) error {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return err
	}
	u, err := toUnstructured(&v1alpha1.MarketIdentity{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}, "MarketIdentity")
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

func (ctl *Controller) reconcileRequester(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.MarketRequester
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	ws := ctl.c.workspace() + ":" + ComponentWorkspace
	st := v1alpha1.ApplicationStatus{ObservedGeneration: o.Generation, Profile: o.Spec.Profile}
	if u.GetDeletionTimestamp() != nil {
		left, err := ctl.deletePods(ctx, ws, owner, nil)
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
		ctl.forgetPlaced(owner)
		return ctl.setFinalizer(ctx, MarketRequesterGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, MarketRequesterGVR, u, true); err != nil {
			return err
		}
	}
	if ctl.applied[string(o.UID)] != o.Generation || ctl.lostComponent(ctx, owner) != "" {
		values, err := RequesterValues(o.Spec, ComponentStateDir(ctl.c.Layout.StateDir, RequesterComponent))
		if err == nil {
			err = withRootName(&values.ExtraArgs, o.Name)
		}
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its schemas, its own OpenBaoServer and its workspace"
		ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		if _, err := ctl.applyProfile(ctx, RequesterSet, o.Spec.Profile, owner, false, &values); err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if err := ctl.ensureIdentity(ctx, RequesterIdentity, o.Spec.Identity); err != nil {
			st.Message = "its MarketIdentity: " + err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if values.Kcp, values.Vault, err = ctl.requesterKcp(ctx, values.StateDir); err != nil {
			st.Message = "its vault and its kcp account: " + err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		st.Message = "applying the " + o.Spec.Profile + " components"
		ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		pods, err := ctl.applyProfile(ctx, RequesterSet, o.Spec.Profile, owner, true, &values)
		if err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
		}
		if _, err := ctl.deletePods(ctx, ws, owner, pods); err != nil {
			return err
		}
		ctl.applied[string(o.UID)] = o.Generation
		ctl.c.log().Info("applied a MarketRequester's components", "name", o.Name, "profile", o.Spec.Profile, "denopods", pods)
	}
	pods, err := ctl.ownedPods(ctx, ws, owner)
	if err != nil {
		return err
	}
	allReady := len(pods) > 0
	for i := range pods {
		v := podViewOf(ws, &pods[i])
		st.Components = append(st.Components, v.component())
		st.Listeners = append(st.Listeners, v.Listeners...)
		allReady = allReady && v.Ready
	}
	st.Did = ctl.identityDid(ctx)
	switch {
	case allReady && st.Did != "":
		st = ready(st, "the requester is signed in and its components are ready")
	case st.Did == "":
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester's MarketIdentity to sign in"
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the requester DenoPod to report ready"
	}
	return ctl.writeStatus(ctx, MarketRequesterGVR, o.Name, st)
}
