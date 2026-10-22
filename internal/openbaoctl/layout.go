package openbaoctl

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/baosync"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultcert"
)

const (
	CertAuthMount  = "cert"
	CertAuthRole   = "socialweb-core"
	CorePolicy     = "socialweb-core"
	KVMount        = "secret"
	CoreTokenTTL   = "15m"
	CoreTokenMax   = "1h"
	AppRoleMount   = "approle"
	DefaultJWTRole = "jwt"

	DefaultKVMaxVersions   = 3
	DefaultTidyInterval    = 6 * time.Hour
	DefaultPKITidyInterval = "6h"
	DefaultPKITidyBuffer   = "72h"
)

var CorePolicyRules = []v1alpha1.OpenBaoPolicyRule{
	{Path: "sys/auth", Capabilities: []string{"read"}},
	{Path: "sys/auth/*", Capabilities: []string{"create", "read", "update", "delete", "sudo"}},
	{Path: "sys/mounts", Capabilities: []string{"read"}},
	{Path: "sys/mounts/*", Capabilities: []string{"create", "read", "update", "delete"}},
	{Path: "sys/policies/acl/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
	{Path: "auth/+/certs/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
	{Path: "auth/+/role/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
	{Path: "auth/+/config", Capabilities: []string{"create", "read", "update"}},
	{Path: "+/config", Capabilities: []string{"create", "read", "update"}},
	{Path: "+/config/auto-tidy", Capabilities: []string{"create", "read", "update"}},
	{Path: "auth/token/tidy", Capabilities: []string{"create", "update"}},
	{Path: "sys/leases/tidy", Capabilities: []string{"create", "update"}},
	{Path: "+/roles/*", Capabilities: []string{"create", "read", "update", "delete", "list"}},
	{Path: "+/root/generate/internal", Capabilities: []string{"create", "update"}},
	{Path: "+/metadata/*", Capabilities: []string{"read", "delete", "list"}},
}

var (
	clientNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	reservedMountNames = map[string]bool{KVMount: true, CertAuthMount: true, "sys": true, "auth": true, "identity": true, "cubbyhole": true}
)

func ClientPolicy(name string) string { return "client-" + name }

func clientPolicyRules(mount string) []v1alpha1.OpenBaoPolicyRule {
	return []v1alpha1.OpenBaoPolicyRule{
		{Path: mount + "/data/*", Capabilities: []string{"create", "read", "update", "patch", "delete"}},
		{Path: mount + "/metadata/*", Capabilities: []string{"read", "delete", "list"}},
	}
}

func PolicyHCL(rules []v1alpha1.OpenBaoPolicyRule) string {
	var b strings.Builder
	for _, r := range rules {
		caps := make([]string, len(r.Capabilities))
		for i, c := range r.Capabilities {
			caps[i] = `"` + c + `"`
		}
		fmt.Fprintf(&b, "path \"%s\" {\n  capabilities = [%s]\n}\n", r.Path, strings.Join(caps, ", "))
	}
	return b.String()
}

func declaredPolicies(d *v1alpha1.OpenBaoServerDeclared) map[string]bool {
	out := map[string]bool{}
	if d != nil {
		for _, p := range d.Policies {
			out[p.Name] = true
		}
	}
	return out
}

func checkClients(clients []v1alpha1.OpenBaoServerClient, declared *v1alpha1.OpenBaoServerDeclared) error {
	names, mounts := map[string]bool{}, map[string]bool{}
	policies := declaredPolicies(declared)
	for _, c := range clients {
		if !clientNamePattern.MatchString(c.Name) || c.Name == CertAuthRole {
			return fmt.Errorf("client name %q: lower case letters, digits and dashes, and never %s", c.Name, CertAuthRole)
		}
		if c.KVMount == "" && len(c.Policies) == 0 {
			return fmt.Errorf("client %s: names neither a kvMount of its own nor a declared policy, so its token could do nothing", c.Name)
		}
		if c.KVMount != "" && (!clientNamePattern.MatchString(c.KVMount) || reservedMountNames[c.KVMount]) {
			return fmt.Errorf("client %s: kvMount %q is not a mount of its own (never %s, %s or a system mount)", c.Name, c.KVMount, KVMount, CertAuthMount)
		}
		for _, p := range c.Policies {
			if !policies[p] {
				return fmt.Errorf("client %s: policy %q is not among spec.declared.policies", c.Name, p)
			}
		}
		if names[c.Name] || (c.KVMount != "" && mounts[c.KVMount]) {
			return fmt.Errorf("client %s: names and kvMounts are each used once", c.Name)
		}
		names[c.Name] = true
		if c.KVMount != "" {
			mounts[c.KVMount] = true
		}
	}
	return nil
}

func clientTokenPolicies(c v1alpha1.OpenBaoServerClient) []string {
	var out []string
	if c.KVMount != "" {
		out = append(out, ClientPolicy(c.Name))
	}
	return append(out, c.Policies...)
}

type layoutInput struct {
	caPEM        []byte
	clientsCAPEM []byte
	clients      []v1alpha1.OpenBaoServerClient
	declared     *v1alpha1.OpenBaoServerDeclared
	plugins      []placedPlugin
	tidyDue      bool
	accessors    map[string]string
	grants       []v1alpha1.GuestIdentityGrant
}

func or(v, dflt string) string {
	if v == "" {
		return dflt
	}
	return v
}

func put(m map[string]any, k string, v any) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case []string:
		if len(x) == 0 {
			return
		}
	case int32:
		if x == 0 {
			return
		}
		v = int(x)
	}
	m[k] = v
}

type caSource func(ctx context.Context, pkiMount string) (string, error)

func desired(ctx context.Context, in layoutInput, ca caSource) (baosync.Desired, []string, error) {
	var d baosync.Desired
	var pending []string
	d.AuthMounts = append(d.AuthMounts, baosync.AuthMount{Path: CertAuthMount, Type: baosync.AuthCert})
	d.SecretMounts = append(d.SecretMounts, baosync.SecretMount{Path: KVMount, Type: baosync.MountKV2})
	maxVersions := map[string]int32{}
	if in.declared != nil {
		for _, m := range in.declared.SecretMounts {
			if m.MaxVersions > 0 {
				maxVersions[strings.Trim(m.Path, "/")] = m.MaxVersions
			}
		}
	}
	kvConfig := func(mount string) baosync.Object {
		n := maxVersions[mount]
		if n == 0 {
			n = DefaultKVMaxVersions
		}
		return baosync.Object{Path: mount + "/config", Data: map[string]any{"max_versions": int(n)}}
	}
	d.Objects = append(d.Objects, kvConfig(KVMount))
	core := CorePolicyRules
	for _, p := range in.plugins {
		bp := p.plugin
		bp.ManagedRoles = atprotoPlugin(p.spec)
		d.Plugins = append(d.Plugins, bp)
		d.AuthMounts = append(d.AuthMounts, baosync.AuthMount{Path: strings.Trim(p.spec.MountPath, "/"), Type: p.plugin.Name})
		d.Objects = append(d.Objects, atprotoObjects(p)...)
	}
	if len(in.plugins) > 0 {
		core = append(append([]v1alpha1.OpenBaoPolicyRule(nil), CorePolicyRules...), PluginCorePolicyRules...)
	}
	if in.declared != nil && in.declared.IdentityOIDC != nil {
		core = append(append([]v1alpha1.OpenBaoPolicyRule(nil), core...), IdentityCoreRules...)
	}
	d.Policies = append(d.Policies, baosync.Policy{Name: CorePolicy, HCL: PolicyHCL(core)})
	d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + CertAuthMount + "/certs/" + CertAuthRole, Data: map[string]any{
		"certificate":          string(in.caPEM),
		"allowed_common_names": []string{vaultcert.RootVaultClientCommonName},
		"token_policies":       []string{CorePolicy},
		"token_ttl":            CoreTokenTTL,
		"token_max_ttl":        CoreTokenMax,
	}})
	for _, c := range in.clients {
		if c.KVMount != "" {
			d.SecretMounts = append(d.SecretMounts, baosync.SecretMount{Path: c.KVMount, Type: baosync.MountKV2})
			d.Objects = append(d.Objects, kvConfig(c.KVMount))
			d.Policies = append(d.Policies, baosync.Policy{Name: ClientPolicy(c.Name), HCL: PolicyHCL(clientPolicyRules(c.KVMount))})
		}
		d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + CertAuthMount + "/certs/" + c.Name, Data: map[string]any{
			"certificate":          string(in.clientsCAPEM),
			"allowed_common_names": []string{vaultcert.OpenBaoClientCommonName(c.Name)},
			"token_policies":       clientTokenPolicies(c),
			"token_ttl":            or(c.TokenTTL, CoreTokenTTL),
			"token_max_ttl":        or(c.TokenMaxTTL, CoreTokenMax),
		}})
	}
	decl := in.declared
	if decl == nil {
		return d, nil, nil
	}
	for _, m := range decl.SecretMounts {
		if strings.Trim(m.Path, "/") == KVMount && m.Type == baosync.MountKV2 {
			continue
		}
		d.SecretMounts = append(d.SecretMounts, baosync.SecretMount{Path: strings.Trim(m.Path, "/"), Type: m.Type, MaxLeaseTTL: m.MaxLeaseTTL})
		if m.Type == baosync.MountKV2 {
			d.Objects = append(d.Objects, kvConfig(strings.Trim(m.Path, "/")))
		}
	}
	for _, m := range decl.AuthMounts {
		p := strings.Trim(m.Path, "/")
		if p != CertAuthMount || m.Type != baosync.AuthCert {
			d.AuthMounts = append(d.AuthMounts, baosync.AuthMount{Path: p, Type: m.Type})
		}
		if m.JWT != nil {
			d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + p + "/config", Data: map[string]any{
				"oidc_discovery_url": m.JWT.DiscoveryURL,
				"bound_issuer":       or(m.JWT.BoundIssuer, m.JWT.DiscoveryURL),
			}})
		}
	}
	for _, p := range decl.Policies {
		if p.Name == CorePolicy {
			return d, nil, fmt.Errorf("spec.declared.policies: %s is the controller's own policy", CorePolicy)
		}
		hcl := p.HCL
		if hcl == "" {
			hcl = PolicyHCL(p.Rules)
		} else if len(p.Rules) > 0 {
			return d, nil, fmt.Errorf("spec.declared.policies: %s carries both rules and hcl", p.Name)
		}
		rendered, missing := renderAccessors(hcl, in.accessors)
		if len(missing) > 0 {
			pending = append(pending, fmt.Sprintf("policy %s waits for the accessor of the auth mount %s", p.Name, strings.Join(missing, ", ")))
			continue
		}
		d.Policies = append(d.Policies, baosync.Policy{Name: p.Name, HCL: rendered})
	}
	cas := map[string]string{}
	for _, r := range decl.CertRoles {
		mount := strings.Trim(r.Mount, "/")
		if mount == CertAuthMount && r.Name == CertAuthRole {
			return d, nil, fmt.Errorf("spec.declared.certRoles: %s is the controller's own role", CertAuthRole)
		}
		pem, ok := cas[r.CAFromPKI]
		if !ok {
			var err error
			pem, err = ca(ctx, r.CAFromPKI)
			if err != nil {
				return d, nil, err
			}
			cas[r.CAFromPKI] = pem
		}
		if pem == "" {
			pending = append(pending, fmt.Sprintf("cert role %s/%s waits for a CA in the %s mount", mount, r.Name, r.CAFromPKI))
			continue
		}
		data := map[string]any{"certificate": pem, "allowed_common_names": r.CommonNames, "token_policies": r.Policies,
			"token_no_default_policy": r.NoDefaultPolicy}
		put(data, "token_ttl", r.TokenTTL)
		put(data, "token_max_ttl", r.TokenMaxTTL)
		d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + mount + "/certs/" + r.Name, Data: data})
	}
	for _, r := range decl.JWTRoles {
		data := map[string]any{"role_type": or(r.RoleType, DefaultJWTRole),
			"bound_audiences": r.BoundAudiences, "user_claim": r.UserClaim, "token_policies": r.Policies}
		if r.UserClaim == "" || r.UserClaim == "sub" {
			data["bound_subject"] = r.BoundSubject
		} else {
			data["bound_claims"] = map[string]any{r.UserClaim: r.BoundSubject}
		}
		put(data, "token_ttl", r.TokenTTL)
		d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + strings.Trim(r.Mount, "/") + "/role/" + r.Name, Data: data})
	}
	for _, r := range decl.AppRoles {
		data := map[string]any{"token_policies": r.Policies}
		put(data, "token_ttl", r.TokenTTL)
		put(data, "token_max_ttl", r.TokenMaxTTL)
		put(data, "secret_id_ttl", r.SecretIDTTL)
		d.Objects = append(d.Objects, baosync.Object{Path: "auth/" + or(strings.Trim(r.Mount, "/"), AppRoleMount) + "/role/" + r.Name, Data: data})
	}
	for _, r := range decl.PKIRoles {
		data := map[string]any{"allow_subdomains": r.AllowSubdomains, "allow_bare_domains": r.AllowBareDomains,
			"allow_any_name": r.AllowAnyName, "allow_ip_sans": r.AllowIPSANs, "enforce_hostnames": r.EnforceHostnames,
			"require_cn": r.RequireCN, "client_flag": r.ClientFlag, "server_flag": r.ServerFlag, "no_store": r.NoStore}
		put(data, "allowed_domains", r.AllowedDomains)
		put(data, "key_type", r.KeyType)
		put(data, "key_bits", r.KeyBits)
		put(data, "key_usage", r.KeyUsage)
		put(data, "organization", r.Organization)
		put(data, "ou", r.OrganizationalUnit)
		put(data, "cn_validations", r.CNValidations)
		put(data, "ttl", r.TTL)
		put(data, "max_ttl", r.MaxTTL)
		d.Objects = append(d.Objects, baosync.Object{Path: strings.Trim(r.Mount, "/") + "/roles/" + r.Name, Data: data})
	}
	for _, t := range decl.PKITidy {
		d.Objects = append(d.Objects, baosync.Object{Path: strings.Trim(t.Mount, "/") + "/config/auto-tidy", Data: map[string]any{
			"enabled": true, "tidy_cert_store": true, "tidy_revoked_certs": true, "tidy_expired_issuers": false,
			"interval_duration": or(t.Interval, DefaultPKITidyInterval), "safety_buffer": or(t.SafetyBuffer, DefaultPKITidyBuffer)}})
	}
	for _, s := range decl.KV {
		keys := append([]string(nil), s.Keys...)
		sort.Strings(keys)
		var grace time.Duration
		if s.Grace != "" {
			g, err := time.ParseDuration(s.Grace)
			if err != nil {
				return d, nil, fmt.Errorf("spec.declared.kv %s/%s: grace %q: %w", s.Mount, s.Prefix, s.Grace, err)
			}
			grace = g
		}
		d.KV = append(d.KV, baosync.KVScope{Mount: strings.Trim(s.Mount, "/"), Prefix: s.Prefix, Keys: keys, Grace: grace})
	}
	return d, pending, nil
}

func pkiCA(c *openbao.Client) caSource {
	return func(ctx context.Context, mount string) (string, error) {
		if mount == "" {
			return "", errors.New("a cert role names no caFromPKI mount")
		}
		mounts, err := c.SecretMounts(ctx)
		if err != nil {
			return "", err
		}
		if m, ok := mounts[strings.Trim(mount, "/")]; !ok || m.Type != baosync.MountPKI {
			return "", nil
		}
		pem, err := c.CAPEM(ctx, mount)
		var re *openbao.ResponseError
		if err != nil && (errors.As(err, &re) && (re.Status == 404 || re.Status == 400) || strings.Contains(err.Error(), "holds no certificate")) {
			return "", nil
		}
		return pem, err
	}
}

func loginCore(ctx context.Context, c *openbao.Client) (*openbao.Client, error) {
	tok, err := c.LoginCert(ctx, CertAuthMount, CertAuthRole)
	if err != nil {
		return nil, err
	}
	return c.WithToken(tok), nil
}

func loginRefused(err error) bool {
	var re *openbao.ResponseError
	if !errors.As(err, &re) {
		return false
	}
	return re.Status == 400 || re.Status == 403 || re.Status == 404
}

func permissionDenied(err error) bool {
	var re *openbao.ResponseError
	return errors.As(err, &re) && re.Status == 403
}
