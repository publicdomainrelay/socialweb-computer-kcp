package appctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
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
)

var BidderProfiles = []string{BidderProfileProdQr, BidderProfileNetwork, BidderProfileDevLocal}

func BidderIdentityMode(profile string) (v1alpha1.MarketIdentityMode, error) {
	switch profile {
	case BidderProfileProdQr, BidderProfileNetwork, BidderProfileDevLocal:
		return v1alpha1.MarketIdentityModeLocalRepo, nil
	}
	return "", fmt.Errorf("appctl: MarketBidder profile %q: want one of %s", profile, strings.Join(BidderProfiles, ", "))
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

func (ctl *Controller) reconcileBidder(ctx context.Context, u *unstructured.Unstructured) error {
	var o v1alpha1.MarketBidder
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	ws := ctl.c.workspace() + ":" + ComponentWorkspace
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
		records, err := ctl.releaseRecords(ctx, ctl.c.workspace(), ws, owner)
		if err != nil {
			return err
		}
		if records > 0 {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d AtprotoRecord(s) leaving the repo before the pds DenoPod stops", records)
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
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
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		delete(ctl.applied, string(o.UID))
		ctl.forgetPlaced(owner)
		return ctl.setFinalizer(ctx, MarketBidderGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := ctl.setFinalizer(ctx, MarketBidderGVR, u, true); err != nil {
			return err
		}
	}
	stateDir := ComponentStateDir(ctl.c.Layout.StateDir, BidderComponent)
	guestStateDir := ComponentStateDir(ctl.c.Layout.StateDir, GuestComponent)
	mode, err := BidderIdentityMode(o.Spec.Profile)
	if err != nil {
		st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
	}
	if o.Spec.Identity.Mode == "" {
		o.Spec.Identity.Mode = mode
	}
	if ctl.applied[string(o.UID)] != o.Generation || ctl.lostComponent(ctx, owner) != "" {
		var kcp BidderKcp
		if rec, err := ctl.c.Record(); err == nil {
			kcp = BidderKcpOf(rec)
		}
		values, err := BidderValues(o.Spec, stateDir, guestStateDir, kcp)
		if err == nil {
			err = withRootName(&values.ExtraArgs, o.Name)
		}
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseFailed, err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "applying its own OpenBaoServer and workspace"
		ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		if _, err := ctl.applyProfile(ctx, BidderSet, o.Spec.Profile, owner, false, &values); err != nil {
			st.Message = err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if err := ctl.ensureIdentitySpec(ctx, ctl.c.workspace(), BidderIdentity, OperatorWatched(IdentitySpecOf(o.Spec.Identity), o.Spec.Operator)); err != nil {
			st.Message = "its MarketIdentity: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		vault, err := ctl.bidderVault(ctx, o.Spec, values.StateDir)
		if err != nil {
			st.Message = "its vault and its PDS account: " + err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		values.Vault = vault
		if err := BidderReads(&values); err != nil {
			return err
		}
		if values.Pds, err = BidderPds(values, ComponentStateDir(ctl.c.Layout.StateDir, PdsComponent), o.Spec.Network); err != nil {
			return err
		}
		st.Message = "applying the " + o.Spec.Profile + " components"
		ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		pods, err := ctl.applyProfile(ctx, BidderSet, o.Spec.Profile, owner, true, &values)
		if err != nil {
			st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, err.Error()
			return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
		}
		if _, err := ctl.deletePods(ctx, ws, owner, pods); err != nil {
			return err
		}
		ctl.applied[string(o.UID)] = o.Generation
		ctl.c.log().Info("applied a MarketBidder's components", "name", o.Name, "profile", o.Spec.Profile, "denopods", pods,
			"plc", o.Spec.Network.PlcDirectoryUrl)
	}
	if err := ctl.rotateSigningKey(ctx, o); err != nil {
		ctl.c.log().Warn("rotating the bidder's OIDC signing key", "name", o.Name, "err", err.Error())
	}
	if refused, err := ctl.guardSignIn(ctx, o, ws, owner); err != nil || refused != "" {
		if err != nil {
			return err
		}
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, refused
		return ctl.writeStatus(ctx, MarketBidderGVR, o.Name, st)
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
		if v.Name == BidderComponent {
			if did, _ := ctl.runStatus(v.RunID)["did"].(string); did != "" {
				st.Did = did
			}
		}
	}
	if o.Spec.Kcp != nil {
		st.Components = append(st.Components, ctl.guestComponents(ctx)...)
	}
	if o.Spec.Profile == BidderProfileNetwork {
		if err := ctl.associateNetworkOperator(ctx, o.Spec); err != nil {
			ctl.c.log().Warn("associating the bidder's DID with the market network operator account", "err", err.Error())
		}
	}
	switch {
	case allReady && st.Did != "":
		st = ready(st, "every component is ready")
	default:
		st.Phase, st.Message = v1alpha1.ApplicationPhaseProvisioning, "waiting for the components to report ready and the bidder its did"
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

func (ctl *Controller) guardSignIn(ctx context.Context, o v1alpha1.MarketBidder, ws, owner string) (string, error) {
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
	if _, err := ctl.deletePods(ctx, ws, owner, nil); err != nil {
		return "", err
	}
	if ri, err := ctl.client(ctl.c.workspace(), MarketIdentityGVR); err == nil {
		if err := ri.Delete(ctx, BidderIdentity, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	delete(ctl.applied, string(o.UID))
	ctl.forgetPlaced(owner)
	return "the sign-in was refused and the bidder restarts with a fresh QR: " + refusal.Error(), nil
}
