package clusterprovider

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identitybundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	VaultPKIPath = "pki"

	VaultPKIRoleTenant = "tenant-superuser"

	VaultPKIRoleKubeconfig = "kubeconfig-user"

	VaultPKIRoleKubeconfigNoExpiry = "kubeconfig-user-noexpiry"

	NoExpiryOrganizationalUnit = "socialweb.computer:no-expiry"
)

func VaultAddr(port int) string {
	return fmt.Sprintf("https://127.0.0.1:%d", port)
}

func vaultCA(dataDir string) (*x509.CertPool, error) {
	path := clusterstack.OpenBaoTLSCAPath(dataDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: reading the cluster vault's TLS CA at %s: %w", path, err)
	}
	pool, err := openbao.CertPoolFromPEM(raw)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the cluster vault's TLS CA at %s: %w", path, err)
	}
	return pool, nil
}

type vaultAdmin struct {
	addr   string
	client *openbao.Client
	http   *http.Client
}

func newVaultAdmin(addr string, client *openbao.Client, pool *x509.CertPool) *vaultAdmin {
	httpClient := http.DefaultClient
	if pool != nil {
		httpClient, _ = httpclient.TLSClient(httpclient.TLSOptions{Pool: pool, IdleConnTimeout: openbao.IdleConnTimeout})
	}
	return &vaultAdmin{addr: strings.TrimRight(addr, "/"), client: client, http: httpClient}
}

func (v *vaultAdmin) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	if v.client != nil {
		if err := v.client.EnsureToken(ctx); err != nil {
			return 0, nil, fmt.Errorf("clusterprovider: renewing the vault token before %s /v1/%s: %w", method, path, err)
		}
	}
	status, data, err := v.attempt(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if v.client != nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if rerr := v.client.RefreshToken(ctx); rerr == nil {
			return v.attempt(ctx, method, path, body)
		}
	}
	return status, data, nil
}

func (v *vaultAdmin) attempt(ctx context.Context, method, path string, body any) (int, []byte, error) {
	req, err := httpjson.NewRequest(ctx, method, v.addr+"/v1/"+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("clusterprovider: building the vault request %s /v1/%s: %w", method, path, err)
	}
	token := ""
	if v.client != nil {
		token = v.client.Token()
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := httpjson.Exchange(ctx, v.http, req, httpjson.Unlimited)
	if err != nil {
		return 0, nil, fmt.Errorf("clusterprovider: the vault request %s /v1/%s: %w", method, path, err)
	}
	return resp.Status, resp.Body, nil
}

func vaultTTL(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func vaultKeyType(kind string) string {
	switch strings.ToLower(kind) {
	case "ecdsa", "ec":
		return "ec"
	case "rsa":
		return "rsa"
	case "ed25519":
		return "ed25519"
	default:
		return kind
	}
}

func ImportClientCA(ctx context.Context, dataDir string, client *openbao.Client) error {
	tlsDir := clusterstack.TLSDir(dataDir)
	certPEM, err := os.ReadFile(filepath.Join(tlsDir, clientCACertName))
	if err != nil {
		return fmt.Errorf("clusterprovider: reading the cluster's client CA for import into the vault: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(tlsDir, clientCAKeyName))
	if err != nil {
		return fmt.Errorf("clusterprovider: reading the cluster's client CA key for import into the vault: %w", err)
	}
	bundle := strings.TrimRight(string(certPEM), "\n") + "\n" + strings.TrimRight(string(keyPEM), "\n") + "\n"
	if err := client.Write(ctx, VaultPKIPath+"/config/ca", map[string]any{"pem_bundle": bundle}); err != nil {
		return fmt.Errorf("clusterprovider: importing the cluster's client CA into the vault: %w", err)
	}
	if err := client.Write(ctx, VaultPKIPath+"/issuer/default", map[string]any{"leaf_not_after_behavior": "truncate", "usage": "read-only,issuing-certificates"}); err != nil {
		return fmt.Errorf("clusterprovider: setting the client CA issuer to truncate leaf lifetimes: %w", err)
	}
	return nil
}

func WriteBundleToVault(ctx context.Context, client *openbao.Client, b *identitybundle.Bundle) error {
	data := make(map[string]any, len(b.Entries))
	for _, e := range b.Entries {
		data[e.Path] = base64.StdEncoding.EncodeToString(e.Bytes)
	}
	if err := client.PutKV(ctx, vaultlayout.PathClusterIdentity, data); err != nil {
		return fmt.Errorf("clusterprovider: writing the identity bundle into the cluster vault: %w", err)
	}
	return nil
}

func ReadBundleFromVault(ctx context.Context, client *openbao.Client) (*identitybundle.Bundle, error) {
	data, err := client.GetKV(ctx, vaultlayout.PathClusterIdentity)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: reading the identity bundle from the cluster vault: %w", err)
	}
	b := &identitybundle.Bundle{Entries: make([]identitybundle.Entry, 0, len(identitybundle.Paths))}
	for _, path := range identitybundle.Paths {
		value, ok := data[path].(string)
		if !ok {
			return nil, fmt.Errorf(
				"clusterprovider: the identity bundle in the cluster vault holds no %q, so it is "+
					"not the 13-entry bundle k3s authored", path)
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("clusterprovider: the identity bundle entry %q in the cluster vault is not base64: %v", path, err)
		}
		b.Entries = append(b.Entries, identitybundle.Entry{Path: path, Bytes: raw, ModTime: time.Now()})
	}
	return b, nil
}

type CustodyOptions struct {
	StateDir string

	Cluster string

	VaultPort int

	PostgresDSN string
}

func CustodyClusterVault(ctx context.Context, o CustodyOptions) error {
	if o.VaultPort <= 0 {
		return fmt.Errorf(
			"clusterprovider: identity custody for %s was asked for with no vault port, so the "+
				"bundle k3s just authored would be persisted to the file tree and NOT to the vault. "+
				"The vault is only custodied for a stack that leases one (PurposesFor)", o.Cluster)
	}
	dataDir, err := ClusterDataDir(o.StateDir, o.Cluster)
	if err != nil {
		return err
	}
	client, _, err := clusterVault(ctx, dataDir)
	if err != nil {
		return err
	}
	if err := ImportClientCA(ctx, dataDir, client); err != nil {
		return err
	}
	b, err := PersistedBundle(o.StateDir, o.Cluster)
	if err != nil {
		return err
	}
	if err := WriteBundleToVault(ctx, client, b); err != nil {
		return err
	}
	if token, err := os.ReadFile(TokenPath(dataDir)); err == nil {
		if err := client.PutKV(ctx, vaultlayout.PathClusterInstanceToken, map[string]any{
			"token": base64.StdEncoding.EncodeToString(token),
		}); err != nil {
			return fmt.Errorf("clusterprovider: writing the instance token into the cluster vault: %w", err)
		}
	}
	if o.PostgresDSN != "" {
		if err := writePostgresToVault(ctx, client, o.PostgresDSN); err != nil {
			return err
		}
	}
	return nil
}

func writePostgresToVault(ctx context.Context, client *openbao.Client, dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("clusterprovider: the postgres DSN %q for the vault: %w", dsn, err)
	}
	username := ""
	password := ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	database := strings.TrimPrefix(u.Path, "/")
	if username == "" || database == "" {
		return fmt.Errorf("clusterprovider: the postgres DSN %q carries no complete username and database", dsn)
	}
	if err := client.PutKV(ctx, vaultlayout.PathClusterPostgres, map[string]any{
		"username": username,
		"password": password,
		"database": database,
		"socket":   u.Query().Get("host"),
	}); err != nil {
		return fmt.Errorf("clusterprovider: writing the postgres credential into the cluster vault: %w", err)
	}
	return nil
}

var ErrNoVaultBundle = errors.New(
	"clusterprovider: the cluster vault holds no identity bundle to materialise, so this wake has " +
		"no vault to restore from and the file tree is the store it falls back to")

func SeedBundleFromVault(ctx context.Context, stateDir, cluster string) error {
	dataDir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return err
	}
	if _, err := os.Stat(VaultAccessPath(dataDir)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s names no way to the cluster vault", ErrNoVaultBundle, VaultAccessPath(dataDir))
	}
	client, a, err := clusterVault(ctx, dataDir)
	if err != nil {
		return fmt.Errorf(
			"clusterprovider: this cluster's vault is unreachable, so a bundle it may hold cannot be "+
				"restored and falling back to the file tree would let k3s author NEW CAs under the same name: %w", err)
	}
	b, err := ReadBundleFromVault(ctx, client)
	if err != nil {
		if errors.Is(err, openbao.ErrNotFound) {
			return fmt.Errorf("%w: the cluster vault at %s holds no %s", ErrNoVaultBundle, a.Address,
				vaultlayout.PathClusterIdentity)
		}
		return err
	}
	root, err := bundleRoot(stateDir, cluster)
	if err != nil {
		return err
	}
	if err := b.Seed(root); err != nil {
		return fmt.Errorf("clusterprovider: seeding the identity bundle for %s from the vault into %s: %w", cluster, root, err)
	}
	dir, err := BundleDir(stateDir, cluster)
	if err != nil {
		return err
	}
	if err := b.Seed(dir); err != nil {
		return fmt.Errorf("clusterprovider: materialising the identity bundle for %s from the vault into %s: %w", cluster, dir, err)
	}
	return nil
}
