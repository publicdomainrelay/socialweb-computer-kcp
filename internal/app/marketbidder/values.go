package marketbidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	oidcRotationPath = "oidc-rotation/"
	GuestOIDCKey     = "guest"
	guestOIDCKeyPath = "identity/oidc/key/" + GuestOIDCKey
)

type BidderKcp struct {
	Server    string
	Workspace string
}

var ErrFirecrackerInPod = errors.New("marketbidder: spec.firecracker: the bidder component runs no VM runtime command, so the firecracker provider cannot run in it; " +
	"use spec.kcp (the ComputeVM controller of the bidder's own core boots its guests)")

func providerValues(v *manifests.MarketBidder, spec v1alpha1.MarketBidderSpec, kcp BidderKcp) error {
	if spec.Firecracker != nil {
		return ErrFirecrackerInPod
	}
	k := spec.Kcp
	if k == nil {
		return errors.New("marketbidder: the MarketBidder names no provider; spec.kcp is the bidder's provider")
	}
	if kcp.Server == "" {
		return errors.New("marketbidder: spec.kcp: this instance records no kcp URL for the bidder's compute-provider-kcp")
	}
	handle := cmp.Or(k.Handle, spec.Identity.Handle)
	args, err := compose.Fragment([]string{"--compute-provider-kcp", "--compute-provider-kcp-server", kcp.Server,
		"--compute-provider-kcp-workspace", kcp.Workspace, "--compute-provider-kcp-handle", handle})
	if err != nil {
		return err
	}
	v.ProviderArgs = args
	v.Kcp = manifests.KcpRef{Server: kcp.Server, Workspace: kcp.Workspace}
	v.Guest.Handle = handle
	return nil
}

func BidderReads(v *manifests.MarketBidder) error {
	read, err := compose.ListFragment(v.CABundle)
	if err != nil {
		return err
	}
	v.Read = read
	return nil
}

func GuestReads(v *manifests.MarketBidder) error {
	read, err := compose.ListFragment(v.CABundle)
	if err != nil {
		return err
	}
	v.Guest.Read = read
	return nil
}

func BidderValues(spec v1alpha1.MarketBidderSpec, stateDir, guestStateDir string, kcp BidderKcp) (manifests.MarketBidder, error) {
	n := spec.Network
	if n == nil {
		return manifests.MarketBidder{}, errors.New("marketbidder: the MarketBidder names no network value; its composer or its profile sets spec.network")
	}
	caFile, err := compose.NetworkCAFile(stateDir, n)
	if err != nil {
		return manifests.MarketBidder{}, err
	}
	tlsPort := 0
	if n.GuestTLSURL() != "" {
		p, err := compose.PortOf(n.GuestTLSURL())
		if err != nil {
			return manifests.MarketBidder{}, fmt.Errorf("marketbidder: network guestTlsUrl: %w", err)
		}
		tlsPort = p
	} else if spec.Profile == BidderProfileNetwork {
		return manifests.MarketBidder{}, errors.New("marketbidder: the network value names no guestTlsUrl; a market network bidder hands guests the dispatcher's TLS front")
	}
	v := manifests.MarketBidder{StateDir: stateDir, Handle: spec.Identity.Handle, PdsUrl: spec.Identity.PdsURL,
		PlcDirectoryUrl: n.PLCDirectoryURL, IngressProxyHost: n.IngressProxyHost,
		GuestTLSPort: tlsPort, CABundle: caFile, Policy: spec.Policy}
	if err := providerValues(&v, spec, kcp); err != nil {
		return manifests.MarketBidder{}, err
	}
	v.Policy = cmp.Or(v.Policy, DefaultPolicy)
	trust, err := compose.ListFragment(caFile)
	if err != nil {
		return manifests.MarketBidder{}, err
	}
	v.Trust = trust
	if err := BidderReads(&v); err != nil {
		return manifests.MarketBidder{}, err
	}
	var extra []string
	if v.GuestTLSPort != 0 {
		extra = append(extra, "--guest-tls-port", strconv.Itoa(v.GuestTLSPort))
	}
	if n.QrOrigin != "" {
		extra = append(extra, "--qr-origin", n.QrOrigin)
	}
	if hosts := compose.NetworkHostsArg(n.Hosts); hosts != "" {
		extra = append(extra, "--network-hosts", hosts)
		if v.Guest.ExtraArgs, err = compose.Fragment([]string{"--network-hosts", hosts}); err != nil {
			return manifests.MarketBidder{}, err
		}
	}
	v.Guest.StateDir, v.Guest.VaultClient = guestStateDir, GuestVaultClient
	if err := GuestReads(&v); err != nil {
		return manifests.MarketBidder{}, err
	}
	opts, err := compose.OptionArgs(BidderApp, spec.Options)
	if err != nil {
		return manifests.MarketBidder{}, err
	}
	extra = append(extra, compose.BidderQuotaArgs(spec.Quota)...)
	extra = append(extra, opts...)
	if v.ExtraArgs, err = compose.Fragment(extra); err != nil {
		return manifests.MarketBidder{}, err
	}
	return v, nil
}

func BidderPds(v manifests.MarketBidder, stateDir string, n *v1alpha1.MarketNetworkValue) (manifests.RepoPds, error) {
	p := manifests.RepoPds{StateDir: stateDir, Identity: BidderIdentity, KcpServer: v.Kcp.Server, KcpWorkspace: v.Kcp.Workspace,
		VaultServer: v.Vault.Server, VaultClient: compose.PdsVaultClient, VaultKcpServer: v.Vault.KcpServer, VaultMount: BidderVaultClient,
		VaultMountClient: BidderVaultClient, VaultWorkspace: v.Kcp.Workspace,
		PlcDirectoryUrl: n.PLCDirectoryURL, IngressProxyHost: n.IngressProxyHost, Trust: v.Trust, Read: v.Read, AssociateService: compose.BidderAssociate}
	var err error
	if p.WatchEventSources, err = compose.RefFragment(compose.WatchEventSources(compose.SourceInput{Network: n})); err != nil {
		return p, err
	}
	if hosts := compose.NetworkHostsArg(n.Hosts); hosts != "" {
		p.ExtraArgs, err = compose.Fragment([]string{"--network-hosts", hosts})
	}
	return p, err
}

func (p *pass) infraValues() *manifests.MarketBidder {
	o, c := p.o, p.c
	stateRoot := c.StateRoot()
	values := &manifests.MarketBidder{StateDir: stateRoot, Policy: o.Spec.Policy}
	if o.Spec.Kcp != nil {
		guestStateDir := compose.ComponentCRStateDir(stateRoot, compose.ComponentKindGuestControlPlane, GuestComponent)
		if built, err := BidderValues(o.Spec, stateRoot, guestStateDir, BidderKcp{Server: c.KcpServer(), Workspace: c.Workspace()}); err == nil {
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

func rotationOf(data map[string]any) int64 {
	switch v := data["rotation"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func (p *pass) rotateSigningKey(ctx context.Context, mount string) error {
	o, c, r := p.o, p.c, p.r
	want := int64(o.Spec.SigningKeyRotation)
	if want <= 0 || p.ctl.rotatedAt(r) >= want {
		return nil
	}
	kv, err := c.KV(ctx, r, BidderVaultServer, mount+"/"+oidcRotationPath+"*")
	if err != nil {
		return err
	}
	data, err := compose.ReadKV(ctx, kv, mount, oidcRotationPath+GuestOIDCKey)
	if err != nil {
		return err
	}
	if held := rotationOf(data); held >= want {
		p.ctl.setRotated(r, held)
		return nil
	}
	keys, err := bus.Resolve[contractvault.Keys](ctx, c.Bus(), r.Ref(), compose.VaultRefIn(BidderVaultServer, r.Workspace),
		bus.Capability{Name: contractvault.CapabilityWrite, Scope: []string{guestOIDCKeyPath}})
	if err != nil {
		return err
	}
	if err := keys.RotateKey(ctx, mount, GuestOIDCKey); err != nil {
		var re *openbao.ResponseError
		if errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusBadRequest) {
			c.Log().Info("no OIDC signing key to rotate yet; the server makes it from its declaration", "name", o.Name, "key", GuestOIDCKey)
			p.ctl.setRotated(r, want)
			return nil
		}
		return err
	}
	if err := kv.Write(ctx, mount, oidcRotationPath+GuestOIDCKey, map[string]any{"rotation": want}); err != nil {
		return err
	}
	p.ctl.setRotated(r, want)
	c.Log().Info("rotated the bidder's OIDC signing key in its OpenBao; the issuer URL stays and the old key stays published for the key's verification ttl",
		"name", o.Name, "key", GuestOIDCKey, "rotation", want)
	return nil
}

func (p *pass) guestComponents(ctx context.Context) []v1alpha1.ApplicationComponent {
	var out []v1alpha1.ApplicationComponent
	for _, k := range []struct {
		kind     string
		gvr      schema.GroupVersionResource
		selector string
	}{{compose.KindComputeVM, computevm.GVR, ""}, {compose.KindHostProcess, compose.HostProcessGVR, computevm.LabelComputeVM}} {
		ri, err := p.c.Client(p.r.Workspace, k.gvr)
		if err != nil {
			continue
		}
		list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: k.selector})
		if err != nil {
			continue
		}
		for _, o := range list.Items {
			phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
			out = append(out, v1alpha1.ApplicationComponent{Kind: k.kind, Name: o.GetName(), Workspace: p.r.Workspace, Phase: phase})
		}
	}
	return out
}
