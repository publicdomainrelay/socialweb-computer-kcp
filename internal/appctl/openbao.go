package appctl

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

const (
	NetworkDir         = "network"
	NetworkVaultServer = "network"
	NetworkVaultClient = "components"
	NetworkPKIMount    = "pki"
)

func NetworkStateDir(instanceState string) string { return filepath.Join(instanceState, NetworkDir) }

func writeSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type networkVault struct {
	address    string
	certMount  string
	certRole   string
	caPEM      []byte
	clientCert []byte
	clientKey  []byte
}

func readNetworkVault(ctx context.Context, ri dynamic.ResourceInterface) (networkVault, error) {
	u, err := ri.Get(ctx, NetworkVaultServer, metav1.GetOptions{})
	if err != nil {
		return networkVault{}, err
	}
	var o v1alpha1.OpenBaoServer
	if err := unstruct.Into(u, &o); err != nil {
		return networkVault{}, err
	}
	st := o.Status
	if st.Phase != v1alpha1.OpenBaoServerPhaseReady || st.Address == "" || st.Refs == nil {
		return networkVault{}, fmt.Errorf("the OpenBaoServer %s is %s, not Ready", NetworkVaultServer, st.Phase)
	}
	var c v1alpha1.OpenBaoServerClientRefs
	for _, x := range st.Clients {
		if x.Name == NetworkVaultClient {
			c = x
		}
	}
	if c.ClientCert == "" {
		return networkVault{}, fmt.Errorf("the OpenBaoServer %s reports no client %s yet", NetworkVaultServer, NetworkVaultClient)
	}
	v := networkVault{address: st.Address, certMount: c.CertAuthMount, certRole: c.CertAuthRole}
	for _, f := range []struct {
		dst *[]byte
		ref string
	}{{&v.caPEM, st.Refs.CA}, {&v.clientCert, c.ClientCert}, {&v.clientKey, c.ClientKey}} {
		if *f.dst, err = os.ReadFile(fileRef(f.ref)); err != nil {
			return networkVault{}, err
		}
	}
	return v, nil
}

func (v networkVault) client(token string) (*openbao.Client, error) {
	pool, err := openbao.CertPoolFromPEM(v.caPEM)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(v.clientCert, v.clientKey)
	if err != nil {
		return nil, err
	}
	return openbao.New(v.address, token, openbao.WithCACertPool(pool), openbao.WithClientCertificate(cert)), nil
}

func (n *networks) vault(ctx context.Context) (networkVault, error) {
	ri, err := n.ctl.client(n.ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return networkVault{}, err
	}
	return readNetworkVault(ctx, ri)
}

func (n *networks) deleteVault(ctx context.Context) (bool, error) {
	ri, err := n.ctl.client(n.ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return false, err
	}
	u, err := ri.Get(ctx, NetworkVaultServer, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if u.GetDeletionTimestamp() == nil {
		if err := ri.Delete(ctx, NetworkVaultServer, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

func pkiCA(ctx context.Context, v networkVault) ([]byte, error) {
	anon, err := v.client("")
	if err != nil {
		return nil, err
	}
	token, err := anon.LoginCert(ctx, v.certMount, v.certRole)
	if err != nil {
		return nil, err
	}
	c, err := v.client(token)
	if err != nil {
		return nil, err
	}
	defer c.RevokeSelf(context.WithoutCancel(ctx))
	pem, err := c.CAPEM(ctx, NetworkPKIMount)
	if err != nil {
		return nil, fmt.Errorf("the network's PKI root CA: %w", err)
	}
	return []byte(pem), nil
}
