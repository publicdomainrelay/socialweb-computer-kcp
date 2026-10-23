package vaultlayout

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitybundle"
)

type VaultName string

const (
	Root    VaultName = "root"
	Cluster VaultName = "cluster"
)

type MountKind string

const (
	MountKV  MountKind = "kv-v2"
	MountPKI MountKind = "pki"
)

const (
	MountKVRoot    = "secret/"
	MountPKIRoot   = "pki/"
	MountKVCluster = "secret/"
)

const (
	PathKCPAdmin           = "secret/data/socialweb/kcp/admin"
	PathKCPAdminTokenStore = "secret/data/socialweb/kcp/admin-token-store"
	PathKCPClientCA        = "secret/data/socialweb/kcp/client-ca"
	PathContainerTLS       = "secret/data/socialweb/container/tls"

	PathClusterIdentity      = "secret/data/k3s/identity"
	PathClusterInstanceToken = "secret/data/k3s/instance-token"
	PathClusterPostgres      = "secret/data/k3s/postgres"
	PathClusterSSH           = "secret/data/k3s/ssh"

	PathDoorClaimsPrefix = "secret/data/socialweb-door/claims/"
)

const DoorClaimSecretsIngressKey = "secrets-ingress"

func DoorClaimSecretsIngressPath(claim string) string {
	return PathDoorClaimsPrefix + claim + "/" + DoorClaimSecretsIngressKey
}

const (
	RootServerName   = "root-vault"
	RootClient       = "gateway"
	PolicyRootWriter = "root-writer"
)

const (
	PolicyKCPRuntime         = "kcp-runtime"
	PolicyKCPAdminTokenStore = "kcp-admin-token-store"
	PolicyKCPCA              = "kcp-ca"
	PolicyContainerTLS       = "container-tls"
	PolicyKCPClientIssue     = "kcp-client-issue"
	PolicyIdentityBundle     = "identity-bundle"
	PolicyInstanceToken      = "instance-token"
	PolicyPostgres           = "postgres"
	PolicySSH                = "ssh"
)

const (
	RoleGateway         = "gateway"
	RoleClusterProvider = "clusterprovider"
	RoleContainerTLS    = "container-tls"
	RoleClusterIdentity = "cluster-identity"
	RoleClusterData     = "cluster-data"
	RoleClusterNode     = "cluster-node"
	RoleKCPClient       = "kcp-client"
)

type SecretClassName string

const (
	ClassKCPAdminKubeconfig SecretClassName = "kcp-admin-kubeconfig"
	ClassKCPAdminTokenStore SecretClassName = "kcp-admin-token-store"
	ClassKCPClientCA        SecretClassName = "kcp-client-ca"
	ClassContainerTLS       SecretClassName = "container-tls-certificate"
	ClassIdentityBundle     SecretClassName = "identity-bundle"
	ClassInstanceToken      SecretClassName = "instance-token"
	ClassPostgresCredential SecretClassName = "postgres-credential"
	ClassPerClusterSSHKey   SecretClassName = "per-cluster-ssh-key"
)

type Mount struct {
	Vault       VaultName
	Path        string
	Kind        MountKind
	MaxLeaseTTL time.Duration
}

type Policy struct {
	Vault VaultName
	Name  string
	HCL   string
}

type AppRole struct {
	Vault       VaultName
	Name        string
	Policies    []string
	TokenTTL    time.Duration
	TokenMaxTTL time.Duration
	SecretIDTTL time.Duration
}

type PKIRole struct {
	Vault            VaultName
	Mount            string
	Name             string
	AllowedDomains   []string
	AllowSubdomains  bool
	AllowBareDomains bool
	AllowIPSANs      bool
	EnforceHostnames bool
	RequireCN        bool
	ClientAuth       bool
	ServerAuth       bool
	KeyUsage         []string
	KeyType          string
	KeyBits          int
	TTL              time.Duration
	MaxTTL           time.Duration
}

type SecretClass struct {
	Name  SecretClassName
	Vault VaultName
}

type Secret struct {
	Vault    VaultName
	Path     string
	Class    SecretClassName
	Consumer string
	Keys     []string
}

type Layout struct {
	Mounts        []Mount
	Policies      []Policy
	AppRoles      []AppRole
	PKIRoles      []PKIRole
	Secrets       []Secret
	SecretClasses []SecretClass
}

func readPolicy(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "path \"%s\" {\n  capabilities = [\"read\"]\n}\n", p)
	}
	return b.String()
}

func issuePolicy(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "path \"%s\" {\n  capabilities = [\"create\", \"update\"]\n}\n", p)
	}
	return b.String()
}

func Default() Layout {
	return Layout{
		Mounts: []Mount{
			{Vault: Root, Path: MountKVRoot, Kind: MountKV},
			{Vault: Root, Path: MountPKIRoot, Kind: MountPKI, MaxLeaseTTL: 8760 * time.Hour},
			{Vault: Cluster, Path: MountKVCluster, Kind: MountKV},
		},
		Policies: []Policy{
			{Vault: Root, Name: PolicyKCPRuntime, HCL: readPolicy(PathKCPAdmin)},
			{Vault: Root, Name: PolicyKCPAdminTokenStore, HCL: readPolicy(PathKCPAdminTokenStore)},
			{Vault: Root, Name: PolicyKCPCA, HCL: readPolicy(PathKCPClientCA)},
			{Vault: Root, Name: PolicyContainerTLS, HCL: readPolicy(PathContainerTLS)},
			{Vault: Root, Name: PolicyKCPClientIssue, HCL: issuePolicy(MountPKIRoot + "issue/" + RoleKCPClient)},

			{Vault: Cluster, Name: PolicyIdentityBundle, HCL: readPolicy(PathClusterIdentity)},
			{Vault: Cluster, Name: PolicyInstanceToken, HCL: readPolicy(PathClusterInstanceToken)},
			{Vault: Cluster, Name: PolicyPostgres, HCL: readPolicy(PathClusterPostgres)},
			{Vault: Cluster, Name: PolicySSH, HCL: readPolicy(PathClusterSSH)},
		},
		AppRoles: []AppRole{
			{
				Vault:       Root,
				Name:        RoleGateway,
				Policies:    []string{PolicyKCPRuntime, PolicyKCPClientIssue},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
			{
				Vault:       Root,
				Name:        RoleClusterProvider,
				Policies:    []string{PolicyKCPAdminTokenStore, PolicyKCPCA},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
			{
				Vault:       Root,
				Name:        RoleContainerTLS,
				Policies:    []string{PolicyContainerTLS},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
			{
				Vault:       Cluster,
				Name:        RoleClusterIdentity,
				Policies:    []string{PolicyIdentityBundle, PolicyInstanceToken},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
			{
				Vault:       Cluster,
				Name:        RoleClusterData,
				Policies:    []string{PolicyPostgres},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
			{
				Vault:       Cluster,
				Name:        RoleClusterNode,
				Policies:    []string{PolicySSH},
				TokenTTL:    time.Hour,
				TokenMaxTTL: 4 * time.Hour,
				SecretIDTTL: 30 * time.Minute,
			},
		},
		PKIRoles: []PKIRole{
			{
				Vault:            Root,
				Mount:            MountPKIRoot,
				Name:             RoleKCPClient,
				AllowedDomains:   []string{"socialweb.local"},
				AllowSubdomains:  true,
				AllowBareDomains: false,
				AllowIPSANs:      false,
				EnforceHostnames: true,
				RequireCN:        true,
				ClientAuth:       true,
				ServerAuth:       false,
				KeyUsage:         []string{"DigitalSignature", "KeyEncipherment"},
				KeyType:          "ecdsa",
				KeyBits:          256,
				TTL:              168 * time.Hour,
				MaxTTL:           720 * time.Hour,
			},
		},
		Secrets: []Secret{
			{
				Vault:    Root,
				Path:     PathKCPAdmin,
				Class:    ClassKCPAdminKubeconfig,
				Consumer: RoleGateway,
				Keys:     []string{"kubeconfig"},
			},
			{
				Vault:    Root,
				Path:     PathKCPAdminTokenStore,
				Class:    ClassKCPAdminTokenStore,
				Consumer: RoleClusterProvider,
				Keys:     []string{"token-hash"},
			},
			{
				Vault:    Root,
				Path:     PathKCPClientCA,
				Class:    ClassKCPClientCA,
				Consumer: RoleClusterProvider,
				Keys:     []string{"ca.crt"},
			},
			{
				Vault:    Root,
				Path:     PathContainerTLS,
				Class:    ClassContainerTLS,
				Consumer: RoleContainerTLS,
				Keys:     []string{"tls.crt", "tls.key"},
			},
			{
				Vault:    Cluster,
				Path:     PathClusterIdentity,
				Class:    ClassIdentityBundle,
				Consumer: RoleClusterIdentity,
				Keys:     identitybundle.Paths,
			},
			{
				Vault:    Cluster,
				Path:     PathClusterInstanceToken,
				Class:    ClassInstanceToken,
				Consumer: RoleClusterIdentity,
				Keys:     []string{"token"},
			},
			{
				Vault:    Cluster,
				Path:     PathClusterPostgres,
				Class:    ClassPostgresCredential,
				Consumer: RoleClusterData,
				Keys:     []string{"username", "password", "database"},
			},
			{
				Vault:    Cluster,
				Path:     PathClusterSSH,
				Class:    ClassPerClusterSSHKey,
				Consumer: RoleClusterNode,
				Keys:     []string{"id_ed25519", "id_ed25519.pub"},
			},
		},
		SecretClasses: []SecretClass{
			{Name: ClassKCPAdminKubeconfig, Vault: Root},
			{Name: ClassKCPAdminTokenStore, Vault: Root},
			{Name: ClassKCPClientCA, Vault: Root},
			{Name: ClassContainerTLS, Vault: Root},
			{Name: ClassIdentityBundle, Vault: Cluster},
			{Name: ClassInstanceToken, Vault: Cluster},
			{Name: ClassPostgresCredential, Vault: Cluster},
			{Name: ClassPerClusterSSHKey, Vault: Cluster},
		},
	}
}

func policyPaths(hcl string) []string {
	var out []string
	for line := range strings.SplitSeq(hcl, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "path \"") {
			continue
		}
		rest := strings.TrimPrefix(line, "path \"")
		if before, _, ok := strings.Cut(rest, "\""); ok {
			out = append(out, before)
		}
	}
	return out
}

func providedBy(mounts map[string]Mount, path string) bool {
	for _, m := range mounts {
		if strings.HasPrefix(path, m.Path) {
			return true
		}
	}
	return false
}

func Validate(l Layout) error {
	var errs []error

	mounts := map[VaultName]map[string]Mount{}
	for _, m := range l.Mounts {
		if mounts[m.Vault] == nil {
			mounts[m.Vault] = map[string]Mount{}
		}
		mounts[m.Vault][m.Path] = m
	}

	policies := map[VaultName]map[string]Policy{}
	for _, p := range l.Policies {
		if policies[p.Vault] == nil {
			policies[p.Vault] = map[string]Policy{}
		}
		if _, dup := policies[p.Vault][p.Name]; dup {
			errs = append(errs, fmt.Errorf("policy %s/%s is declared twice", p.Vault, p.Name))
		}
		policies[p.Vault][p.Name] = p
		for _, path := range policyPaths(p.HCL) {
			if !providedBy(mounts[p.Vault], path) {
				errs = append(errs, fmt.Errorf(
					"policy %s/%s references path %q, which no mount in the %s vault provides",
					p.Vault, p.Name, path, p.Vault))
			}
		}
	}

	roles := map[VaultName]map[string]AppRole{}
	for _, r := range l.AppRoles {
		if roles[r.Vault] == nil {
			roles[r.Vault] = map[string]AppRole{}
		}
		if _, dup := roles[r.Vault][r.Name]; dup {
			errs = append(errs, fmt.Errorf("role %s/%s is declared twice", r.Vault, r.Name))
		}
		roles[r.Vault][r.Name] = r
		if len(r.Policies) == 0 {
			errs = append(errs, fmt.Errorf("role %s/%s names no policy", r.Vault, r.Name))
		}
		for _, name := range r.Policies {
			if _, ok := policies[r.Vault][name]; !ok {
				errs = append(errs, fmt.Errorf(
					"role %s/%s references policy %q, which does not exist in the %s vault",
					r.Vault, r.Name, name, r.Vault))
			}
		}
	}

	for _, r := range l.PKIRoles {
		m, ok := mounts[r.Vault][r.Mount]
		if !ok {
			errs = append(errs, fmt.Errorf(
				"PKI role %s/%s names mount %q, which the %s vault does not have",
				r.Vault, r.Name, r.Mount, r.Vault))
			continue
		}
		if m.Kind != MountPKI {
			errs = append(errs, fmt.Errorf(
				"PKI role %s/%s names mount %q, which is %s and not pki",
				r.Vault, r.Name, r.Mount, m.Kind))
		}
		if r.MaxTTL > m.MaxLeaseTTL {
			errs = append(errs, fmt.Errorf(
				"PKI role %s/%s has max TTL %s, above its mount %q max lease TTL %s",
				r.Vault, r.Name, r.MaxTTL, r.Mount, m.MaxLeaseTTL))
		}
		if !r.ClientAuth && !r.ServerAuth {
			errs = append(errs, fmt.Errorf(
				"PKI role %s/%s grants neither client nor server auth", r.Vault, r.Name))
		}
	}

	declared := map[SecretClassName]SecretClass{}
	for _, c := range l.SecretClasses {
		if _, dup := declared[c.Name]; dup {
			errs = append(errs, fmt.Errorf("secret class %q is declared twice", c.Name))
		}
		declared[c.Name] = c
	}

	counts := map[SecretClassName]int{}
	for _, s := range l.Secrets {
		c, ok := declared[s.Class]
		if !ok {
			errs = append(errs, fmt.Errorf(
				"secret %s/%s has class %q, which is not declared", s.Vault, s.Path, s.Class))
			continue
		}
		counts[s.Class]++
		if c.Vault != s.Vault {
			errs = append(errs, fmt.Errorf(
				"secret %s/%s has class %q, whose home is the %s vault, not the %s vault",
				s.Vault, s.Path, s.Class, c.Vault, s.Vault))
		}
		if _, ok := roles[s.Vault][s.Consumer]; !ok {
			errs = append(errs, fmt.Errorf(
				"secret %s/%s names consumer %q, which is not a role in the %s vault",
				s.Vault, s.Path, s.Consumer, s.Vault))
		}
		if !providedBy(mounts[s.Vault], s.Path) {
			errs = append(errs, fmt.Errorf(
				"secret %s/%s is not under any mount in the %s vault", s.Vault, s.Path, s.Vault))
		}
		if s.Class == ClassIdentityBundle && len(s.Keys) != len(identitybundle.Paths) {
			errs = append(errs, fmt.Errorf(
				"secret %s/%s is the identity bundle and holds %d keys, want %d",
				s.Vault, s.Path, len(s.Keys), len(identitybundle.Paths)))
		}
	}

	for _, c := range l.SecretClasses {
		switch {
		case counts[c.Name] == 0:
			errs = append(errs, fmt.Errorf(
				"secret class %q is assigned to no secret, so it has no vault", c.Name))
		case counts[c.Name] > 1:
			errs = append(errs, fmt.Errorf(
				"secret class %q is assigned to %d secrets, so it has more than one vault",
				c.Name, counts[c.Name]))
		}
	}

	return errors.Join(errs...)
}
