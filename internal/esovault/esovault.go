package esovault

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eso"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

type VaultStore struct {
	Cluster string

	DataDir string

	VaultPort int

	Client *openbao.Client

	ManifestDir string

	Namespace string
}

func StoreVaultURL(vaultPort int) string {
	return "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(vaultPort))
}

func ApplyVaultStore(ctx context.Context, c *kube.Client, o VaultStore) (string, error) {
	if o.DataDir == "" || o.VaultPort == 0 || o.Client == nil || o.Namespace == "" {
		return "", errors.New("esovault: the vault-backed store needs the cluster's data dir, its vault's " +
			"leased port, a client logged in to it, and the tenant namespace the ExternalSecret is written into")
	}
	if err := eso.InstallOperator(ctx, c, nil); err != nil {
		return "", fmt.Errorf("%w. The External Secrets controller runs beside the control plane and "+
			"its process-group step installs the CRDs before it starts, so a tenant that serves none "+
			"of them is one whose group came up without that step", err)
	}
	vaultURL := StoreVaultURL(o.VaultPort)
	vaultTLS, err := vaultcert.EnsureServedVaultTLS(ctx, o.DataDir, o.VaultPort, "127.0.0.1")
	if err != nil {
		return "", fmt.Errorf("esovault: the store for %s would be told to dial %s, and the vault that is "+
			"running cannot present that host: %w", o.Cluster, vaultURL, err)
	}
	if err := o.Client.PutKV(ctx, vaultlayout.PathESODemo, map[string]any{"demo": "openbao:" + o.Cluster}); err != nil {
		return "", fmt.Errorf("esovault: writing the demo value the tenant's ExternalSecret reads out of "+
			"the cluster vault for %s: %w", o.Cluster, err)
	}
	cert, err := currentOrNewESOCertificate(ctx, c, o, vaultURL, vaultTLS.CAPEM)
	if err != nil {
		return "", fmt.Errorf("esovault: providing the store's client certificate for %s: %w", o.Cluster, err)
	}
	if err := eso.Apply(ctx, c, eso.Options{
		ManifestDir:   o.ManifestDir,
		Namespace:     o.Namespace,
		VaultServer:   vaultURL,
		ServerCAPEM:   vaultTLS.CAPEM,
		ClientCertPEM: cert.CertPEM,
		ClientKeyPEM:  cert.KeyPEM,
	}); err != nil {
		return "", err
	}
	return vaultURL, nil
}

func currentOrNewESOCertificate(ctx context.Context, c *kube.Client, o VaultStore, vaultURL string, caPEM []byte) (vaultcert.ClientCertificate, error) {
	heldCert, heldKey, found, err := eso.CurrentClientCertificate(ctx, c, o.Namespace)
	if err != nil {
		return vaultcert.ClientCertificate{}, err
	}
	if found && len(heldCert) > 0 && len(heldKey) > 0 {
		fresh, freshErr := vaultcert.ESOCertificateFresh(heldCert, heldKey, time.Now())
		if freshErr == nil && fresh {
			return vaultcert.ClientCertificate{
				Metadata:    vaultcert.ClientMetadata{Identity: vaultlayout.RoleESO, Role: vaultlayout.RoleESO, VaultAddr: vaultURL},
				CertPEM:     heldCert,
				KeyPEM:      heldKey,
				ServerCAPEM: caPEM,
			}, nil
		}
	}
	return vaultcert.IssueESOCertificate(ctx, o.Client, vaultURL, caPEM)
}
