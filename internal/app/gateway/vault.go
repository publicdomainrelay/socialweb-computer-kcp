package gateway

import (
	"context"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	RootVaultAsset     = "bao"
	rootVaultAppRole   = "approle"
	rootVaultPKIMount  = "pki"
	rootVaultKVMount   = "secret"
	rootVaultPKIMaxTTL = 8760 * time.Hour
)

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
			sm.MaxLeaseTTL = opt.Positive(m.MaxLeaseTTL, rootVaultPKIMaxTTL).String()
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
				TokenTTL: clock.DurationString(r.TokenTTL), TokenMaxTTL: clock.DurationString(r.TokenMaxTTL), SecretIDTTL: clock.DurationString(r.SecretIDTTL)})
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
			TTL: clock.DurationString(r.TTL), MaxTTL: clock.DurationString(r.MaxTTL), NoStore: true})
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
	return "identity/" + marketrequester.RequesterIdentity + "/keys"
}

func RootVaultSpec(g v1alpha1.GatewaySpec) (v1alpha1.OpenBaoServerSpec, error) {
	declared := RootVaultDeclared()
	declared.Policies = append(declared.Policies, v1alpha1.OpenBaoPolicy{Name: GatewayRequesterPolicy, Rules: RequesterVaultRules()},
		v1alpha1.OpenBaoPolicy{Name: GatewayPdsPolicy, Rules: []v1alpha1.OpenBaoPolicyRule{
			{Path: RequesterVaultMount + "/data/" + RequesterKeysPath(), Capabilities: []string{"read"}}}})
	declared.Policies = append(declared.Policies, compose.AtprotoPolicies()...)
	plugin, err := compose.AtprotoPlugin(g.Options, g.Network, compose.GatewayAtprotoScope())
	if err != nil {
		return v1alpha1.OpenBaoServerSpec{}, err
	}
	return v1alpha1.OpenBaoServerSpec{Asset: &v1alpha1.HostProcessAsset{Name: RootVaultAsset},
		Clients: []v1alpha1.OpenBaoServerClient{{Name: vaultlayout.RootClient, Policies: []string{vaultlayout.PolicyRootWriter}},
			{Name: WebAppVaultClient, KVMount: WebAppVaultMount},
			{Name: marketrequester.RequesterVaultClient, KVMount: RequesterVaultMount, Policies: []string{GatewayRequesterPolicy}},
			{Name: compose.PdsVaultClient, Policies: []string{GatewayPdsPolicy}}},
		Declared: declared, Plugins: []v1alpha1.OpenBaoPlugin{plugin}}, nil
}

func (p *pass) rootVault(ctx context.Context, onlyExisting bool) (*v1alpha1.OpenBaoServer, error) {
	c, r := p.c, p.r
	if onlyExisting {
		if _, err := c.Get(ctx, r.Workspace, compose.OpenBaoServerGVR, vaultlayout.RootServerName); apierrors.IsNotFound(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	want, err := RootVaultSpec(p.o.Spec)
	if err != nil {
		return nil, err
	}
	obj := &v1alpha1.OpenBaoServer{ObjectMeta: metav1.ObjectMeta{Name: vaultlayout.RootServerName}, Spec: want}
	if err := c.ApplyTyped(ctx, r, r.Workspace, compose.OpenBaoServerGVR, v1alpha1.GroupVersion.String(), compose.KindOpenBaoServer, obj); err != nil {
		return nil, err
	}
	if err := c.ApplyVault(ctx, r, vaultlayout.RootServerName); err != nil {
		return nil, err
	}
	u, err := c.Get(ctx, r.Workspace, compose.OpenBaoServerGVR, vaultlayout.RootServerName)
	if err != nil {
		return nil, err
	}
	var o v1alpha1.OpenBaoServer
	if err := unstruct.Into(u, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func clientVaultOf(o *v1alpha1.OpenBaoServer, client string) (manifests.GatewayVault, bool) {
	if o == nil || !openbaoctl.Synced(o) || o.Status.Refs == nil || o.Status.Refs.CA == "" {
		return manifests.GatewayVault{}, false
	}
	for _, cl := range o.Status.Clients {
		if cl.Name == client && cl.ClientCert != "" {
			return manifests.GatewayVault{Server: o.Name, Client: cl.Name}, true
		}
	}
	return manifests.GatewayVault{}, false
}

func WebAppVaultOf(o *v1alpha1.OpenBaoServer) (manifests.GatewayVault, bool) {
	return clientVaultOf(o, WebAppVaultClient)
}

func RequesterVaultOf(o *v1alpha1.OpenBaoServer) (manifests.GatewayVault, bool) {
	return clientVaultOf(o, marketrequester.RequesterVaultClient)
}
