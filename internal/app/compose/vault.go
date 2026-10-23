package compose

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	RepoKeyField  = "repoKeyHex"
	passwordBytes = 24
)

func (c *Composer) ApplyVault(ctx context.Context, r Root, name string) error {
	obj := &v1alpha1.Vault{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.VaultSpec{BackendRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindOpenBaoServer, Name: name}}}
	return c.ApplyTyped(ctx, r, r.Workspace, VaultGVR, v1alpha1.GroupVersion.String(), KindVault, obj)
}

func RootVaultLink(r Root, vaultName string, prefixes ...string) Link {
	return VaultLink(r.Ref(), vaultName, true, prefixes...)
}

func (c *Composer) KV(ctx context.Context, r Root, vaultName string, scope ...string) (contractvault.KV, error) {
	return bus.Resolve[contractvault.KV](ctx, c.cfg.Bus, r.Ref(), VaultRefIn(vaultName, r.Workspace),
		bus.Capability{Name: contractvault.CapabilityWrite, Scope: cleanPrefixes(scope)})
}

func readKV(ctx context.Context, kv contractvault.KV, mount, path string) (map[string]any, error) {
	data, err := kv.Read(ctx, mount, path, 0)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

func ReadKV(ctx context.Context, kv contractvault.KV, mount, path string) (map[string]any, error) {
	return readKV(ctx, kv, mount, path)
}

func NewPassword() (string, error) {
	buf := make([]byte, passwordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

var secp256k1N, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

func NewRepoKeyHex() (string, error) {
	for {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		k := new(big.Int).SetBytes(buf)
		if k.Sign() > 0 && k.Cmp(secp256k1N) < 0 {
			return hex.EncodeToString(buf), nil
		}
	}
}

func (c *Composer) EnsureRepoKey(ctx context.Context, r Root, vaultName, mount, keysPath string) error {
	kv, err := c.KV(ctx, r, vaultName, mount+"/identity/*")
	if err != nil {
		return err
	}
	if err := c.ApplyVaultCredential(ctx, r, vaultName, mount, keysPath); err != nil {
		return err
	}
	data, err := readKV(ctx, kv, mount, keysPath)
	if err != nil {
		return err
	}
	if k, _ := data[RepoKeyField].(string); k != "" {
		return nil
	}
	key, err := NewRepoKeyHex()
	if err != nil {
		return err
	}
	data[RepoKeyField] = key
	if err := kv.Write(ctx, mount, keysPath, data); err != nil {
		return err
	}
	c.log.Info("the application's repo key is in its own vault", "vault", vaultName, "path", keysPath)
	return nil
}

func (c *Composer) Password(ctx context.Context, r Root, vaultName, mount, path string) (string, error) {
	kv, err := c.KV(ctx, r, vaultName, mount+"/identity/*")
	if err != nil {
		return "", err
	}
	data, err := readKV(ctx, kv, mount, path)
	if err != nil {
		return "", err
	}
	if password, _ := data["password"].(string); password != "" {
		return password, nil
	}
	password, err := NewPassword()
	if err != nil {
		return "", err
	}
	data["password"] = password
	if err := kv.Write(ctx, mount, path, data); err != nil {
		return "", err
	}
	return password, nil
}

func (c *Composer) VaultRefs(ctx context.Context, ws, server, clientName string) (v1alpha1.OpenBaoServerStatus, v1alpha1.OpenBaoServerClientRefs, error) {
	o, err := Get[v1alpha1.OpenBaoServer](ctx, c, ws, OpenBaoServerGVR, server)
	if err != nil {
		return v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	st := o.Status
	if st.Address == "" || st.Refs == nil {
		return st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no address yet (phase %s)", server, st.Phase)
	}
	for _, x := range st.Clients {
		if x.Name == clientName && x.ClientCert != "" {
			return st, x, nil
		}
	}
	return st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no client %s yet", server, clientName)
}

func (c *Composer) KVMount(ctx context.Context, ws, server, clientName, fallback string) string {
	_, refs, err := c.VaultRefs(ctx, ws, server, clientName)
	if err != nil || strings.TrimSpace(refs.KVMount) == "" {
		return fallback
	}
	return strings.Trim(refs.KVMount, "/")
}

func ServerClient(ctx context.Context, r kube.Resource, server, clientName string) (*openbao.Client, v1alpha1.OpenBaoServerStatus, v1alpha1.OpenBaoServerClientRefs, error) {
	o, err := kube.Get[v1alpha1.OpenBaoServer](ctx, r, server)
	if err != nil {
		return nil, v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	st := o.Status
	if st.Address == "" || st.Refs == nil {
		return nil, st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no address yet (phase %s)", server, st.Phase)
	}
	var cl v1alpha1.OpenBaoServerClientRefs
	for _, x := range st.Clients {
		if x.Name == clientName {
			cl = x
		}
	}
	if cl.ClientCert == "" {
		return nil, st, cl, fmt.Errorf("the OpenBaoServer %s reports no client %s yet", server, clientName)
	}
	caPEM, err := os.ReadFile(openbaoctl.FileRef(st.Refs.CA))
	if err != nil {
		return nil, st, cl, err
	}
	pool, err := openbao.CertPoolFromPEM(caPEM)
	if err != nil {
		return nil, st, cl, err
	}
	cert, err := tls.LoadX509KeyPair(openbaoctl.FileRef(cl.ClientCert), openbaoctl.FileRef(cl.ClientKey))
	if err != nil {
		return nil, st, cl, err
	}
	client := openbao.New(st.Address, "", openbao.WithCACertPool(pool), openbao.WithClientCertificate(cert))
	logged, err := client.LoginCert(ctx, cl.CertAuthMount, cl.CertAuthRole)
	if err != nil {
		return nil, st, cl, err
	}
	return logged, st, cl, nil
}

func (c *Composer) ServerClient(ctx context.Context, ws, server, clientName string) (*openbao.Client, v1alpha1.OpenBaoServerClientRefs, error) {
	ri, err := c.Client(ws, OpenBaoServerGVR)
	if err != nil {
		return nil, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	client, _, refs, err := ServerClient(ctx, ri, server, clientName)
	return client, refs, err
}

func (c *Composer) DeleteVaults(ctx context.Context, r Root) (int, error) {
	vaults, err := c.DeleteOwned(ctx, r.Workspace, VaultGVR, r.Owner())
	if err != nil {
		return vaults, err
	}
	servers, err := c.DeleteOwned(ctx, r.Workspace, OpenBaoServerGVR, r.Owner())
	return vaults + servers, err
}
