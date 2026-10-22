package clusterprovider

import (
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	VaultClientPolicy = "clusterprovider"

	VaultGatewayClient       = "gateway-kubeconfig"
	VaultGatewayClientPolicy = "gateway-kubeconfig"
	vaultAppRoleMount        = "approle"
)

func durationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func clusterProviderRules() []v1alpha1.OpenBaoPolicyRule {
	cu := []string{"create", "update"}
	cru := []string{"create", "read", "update"}
	return []v1alpha1.OpenBaoPolicyRule{
		{Path: strings.TrimSuffix(vaultlayout.MountKVCluster, "/") + "/data/k3s/*", Capabilities: cru},
		{Path: vaultlayout.NodesKVMount + "/data/*", Capabilities: cu},
		{Path: clusterlink.Mount + "/data/*", Capabilities: cru},
		{Path: VaultPKIPath + "/config/ca", Capabilities: cu},
		{Path: VaultPKIPath + "/issuer/*", Capabilities: cu},
		{Path: VaultPKIPath + "/issue/" + VaultPKIRoleTenant, Capabilities: cu},
		{Path: VaultPKIPath + "/issue/" + vaultcert.PKIRoleNode, Capabilities: cu},
	}
}

func gatewayKubeconfigRules() []v1alpha1.OpenBaoPolicyRule {
	cu := []string{"create", "update"}
	return []v1alpha1.OpenBaoPolicyRule{
		{Path: VaultPKIPath + "/config/ca", Capabilities: cu},
		{Path: VaultPKIPath + "/issuer/*", Capabilities: cu},
		{Path: VaultPKIPath + "/issue/" + VaultPKIRoleKubeconfig, Capabilities: cu},
		{Path: VaultPKIPath + "/issue/" + VaultPKIRoleKubeconfigNoExpiry, Capabilities: cu},
		{Path: VaultPKIPath + "/revoke", Capabilities: cu},
		{Path: VaultPKIPath + "/certs", Capabilities: []string{"list"}},
		{Path: VaultPKIPath + "/certs/", Capabilities: []string{"list"}},
		{Path: VaultPKIPath + "/certs/revoked", Capabilities: []string{"list"}},
		{Path: VaultPKIPath + "/certs/revoked/", Capabilities: []string{"list"}},
		{Path: VaultPKIPath + "/cert/*", Capabilities: []string{"read"}},
	}
}

func layoutPKIRole(r vaultlayout.PKIRole) v1alpha1.OpenBaoPKIRole {
	return v1alpha1.OpenBaoPKIRole{Mount: VaultPKIPath, Name: r.Name, AllowedDomains: r.AllowedDomains, AllowSubdomains: r.AllowSubdomains,
		AllowBareDomains: r.AllowBareDomains, AllowIPSANs: r.AllowIPSANs, EnforceHostnames: r.EnforceHostnames, RequireCN: r.RequireCN,
		ClientFlag: r.ClientAuth, ServerFlag: r.ServerAuth, KeyType: vaultKeyType(r.KeyType), KeyBits: int32(r.KeyBits),
		KeyUsage: r.KeyUsage, TTL: durationString(r.TTL), MaxTTL: durationString(r.MaxTTL), NoStore: true}
}

func staticClusterDeclared() *v1alpha1.OpenBaoServerDeclared {
	layout := vaultlayout.Default()
	d := &v1alpha1.OpenBaoServerDeclared{}
	for _, m := range layout.Mounts {
		if m.Vault == vaultlayout.Cluster {
			d.SecretMounts = append(d.SecretMounts, v1alpha1.OpenBaoSecretMount{Path: strings.Trim(m.Path, "/"), Type: string(m.Kind)})
		}
	}
	d.SecretMounts = append(d.SecretMounts,
		v1alpha1.OpenBaoSecretMount{Path: VaultPKIPath, Type: string(vaultlayout.MountPKI)},
		v1alpha1.OpenBaoSecretMount{Path: vaultlayout.NodesKVMount, Type: string(vaultlayout.MountKV)},
		v1alpha1.OpenBaoSecretMount{Path: clusterlink.Mount, Type: string(vaultlayout.MountKV)})
	d.AuthMounts = append(d.AuthMounts, v1alpha1.OpenBaoAuthMount{Path: vaultAppRoleMount, Type: "approle"})
	for _, p := range layout.Policies {
		if p.Vault == vaultlayout.Cluster {
			d.Policies = append(d.Policies, v1alpha1.OpenBaoPolicy{Name: p.Name, HCL: p.HCL})
		}
	}
	d.Policies = append(d.Policies,
		v1alpha1.OpenBaoPolicy{Name: vaultlayout.PolicyESO, HCL: vaultlayout.ESOCertPolicyHCL()},
		v1alpha1.OpenBaoPolicy{Name: VaultClientPolicy, Rules: clusterProviderRules()},
		v1alpha1.OpenBaoPolicy{Name: VaultGatewayClientPolicy, Rules: gatewayKubeconfigRules()})
	d.CertRoles = append(d.CertRoles, v1alpha1.OpenBaoCertRole{Mount: vaultlayout.CertAuthPath, Name: vaultlayout.RoleESO,
		CommonNames: []string{vaultlayout.RoleESO}, CAFromPKI: VaultPKIPath, Policies: []string{vaultlayout.PolicyESO},
		TokenTTL: vaultcert.NodeTokenTTL, TokenMaxTTL: vaultcert.NodeTokenMax, NoDefaultPolicy: true})
	for _, r := range layout.AppRoles {
		if r.Vault == vaultlayout.Cluster {
			d.AppRoles = append(d.AppRoles, v1alpha1.OpenBaoAppRole{Mount: vaultAppRoleMount, Name: r.Name, Policies: r.Policies,
				TokenTTL: durationString(r.TokenTTL), TokenMaxTTL: durationString(r.TokenMaxTTL), SecretIDTTL: durationString(r.SecretIDTTL)})
		}
	}
	for _, r := range layout.PKIRoles {
		d.PKIRoles = append(d.PKIRoles, layoutPKIRole(r))
	}
	d.PKIRoles = append(d.PKIRoles,
		v1alpha1.OpenBaoPKIRole{Mount: VaultPKIPath, Name: VaultPKIRoleTenant, AllowAnyName: true, ClientFlag: true, RequireCN: true,
			KeyType: "ec", KeyBits: 256, Organization: []string{SuperuserOrganization}, TTL: "8760h", MaxTTL: "87600h", NoStore: true},
		v1alpha1.OpenBaoPKIRole{Mount: VaultPKIPath, Name: vaultcert.PKIRoleNode, AllowAnyName: true, ClientFlag: true, RequireCN: true,
			KeyType: "ec", KeyBits: 256, TTL: durationString(vaultcert.NodeCertTTL), MaxTTL: durationString(vaultcert.NodeMaxTTL), NoStore: true},
		v1alpha1.OpenBaoPKIRole{Mount: VaultPKIPath, Name: VaultPKIRoleKubeconfig, AllowAnyName: true, EnforceHostnames: false,
			ClientFlag: true, ServerFlag: false, RequireCN: true, KeyType: "ec", KeyBits: 256, KeyUsage: []string{"DigitalSignature"},
			Organization: []string{kcpclient.OwnersGroup}, CNValidations: []string{"disabled"}, TTL: "8h", MaxTTL: "24h"},
		v1alpha1.OpenBaoPKIRole{Mount: VaultPKIPath, Name: VaultPKIRoleKubeconfigNoExpiry, AllowAnyName: true, EnforceHostnames: false,
			ClientFlag: true, ServerFlag: false, RequireCN: true, KeyType: "ec", KeyBits: 256, KeyUsage: []string{"DigitalSignature"},
			Organization: []string{kcpclient.OwnersGroup}, OrganizationalUnit: []string{NoExpiryOrganizationalUnit},
			CNValidations: []string{"disabled"}})
	d.PKITidy = append(d.PKITidy, v1alpha1.OpenBaoPKITidy{Mount: VaultPKIPath})
	return d
}

func ClusterVaultDeclared(prev *v1alpha1.OpenBaoServerDeclared) *v1alpha1.OpenBaoServerDeclared {
	d := staticClusterDeclared()
	setVaultNodes(d, vaultNodes(prev))
	return d
}

func grantUsable(g *v1alpha1.NodeClaimGrant) bool {
	return g != nil && g.Issuer != "" && g.Subject != "" && g.Audience != ""
}

func vaultNodes(d *v1alpha1.OpenBaoServerDeclared) map[string]*v1alpha1.NodeClaimGrant {
	out := map[string]*v1alpha1.NodeClaimGrant{}
	if d == nil {
		return out
	}
	issuers := map[string]string{}
	for _, m := range d.AuthMounts {
		if vaultlayout.IsNodeJWTMount(m.Path) && m.JWT != nil {
			issuers[m.Path] = m.JWT.DiscoveryURL
		}
	}
	for _, p := range d.Policies {
		if node, ok := vaultlayout.NodeFromJWTPolicy(p.Name); ok {
			out[node] = nil
		}
	}
	for _, r := range d.JWTRoles {
		if _, ok := out[r.Name]; !ok || issuers[r.Mount] == "" || len(r.BoundAudiences) == 0 {
			continue
		}
		out[r.Name] = &v1alpha1.NodeClaimGrant{Issuer: issuers[r.Mount], Subject: r.BoundSubject, Audience: r.BoundAudiences[0]}
	}
	return out
}

func setVaultNodes(d *v1alpha1.OpenBaoServerDeclared, nodes map[string]*v1alpha1.NodeClaimGrant) {
	keep := d.Policies[:0]
	for _, p := range d.Policies {
		if _, ok := vaultlayout.NodeFromJWTPolicy(p.Name); !ok {
			keep = append(keep, p)
		}
	}
	d.Policies = keep
	auths := d.AuthMounts[:0]
	for _, m := range d.AuthMounts {
		if !vaultlayout.IsNodeJWTMount(m.Path) {
			auths = append(auths, m)
		}
	}
	d.AuthMounts = auths
	d.JWTRoles = nil
	kv := d.KV[:0]
	for _, s := range d.KV {
		if s.Mount != vaultlayout.NodesKVMount {
			kv = append(kv, s)
		}
	}
	d.KV = kv

	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	mounts := map[string]string{}
	for _, n := range names {
		d.Policies = append(d.Policies, v1alpha1.OpenBaoPolicy{Name: vaultlayout.NodeJWTPolicyName(n), HCL: vaultlayout.NodeJWTPolicyHCL(n)})
		g := nodes[n]
		if !grantUsable(g) {
			continue
		}
		mount := vaultlayout.NodeJWTMount(g.Issuer)
		mounts[mount] = g.Issuer
		d.JWTRoles = append(d.JWTRoles, v1alpha1.OpenBaoJWTRole{Mount: mount, Name: vaultlayout.NodeJWTRoleName(n),
			RoleType: vaultlayout.NodeJWTRoleType, BoundSubject: g.Subject, BoundAudiences: []string{g.Audience},
			UserClaim: vaultlayout.NodeJWTUserClaim, Policies: []string{vaultlayout.NodeJWTPolicyName(n)}, TokenTTL: vaultlayout.NodeJWTTokenTTL})
	}
	mountNames := make([]string, 0, len(mounts))
	for m := range mounts {
		mountNames = append(mountNames, m)
	}
	sort.Strings(mountNames)
	for _, m := range mountNames {
		d.AuthMounts = append(d.AuthMounts, v1alpha1.OpenBaoAuthMount{Path: m, Type: "jwt",
			JWT: &v1alpha1.OpenBaoJWTConfig{DiscoveryURL: mounts[m], BoundIssuer: mounts[m]}})
	}
	d.KV = append(d.KV, v1alpha1.OpenBaoKVScope{Mount: vaultlayout.NodesKVMount, Keys: names})
}
