package vaultcert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/rootvault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const RootVaultClientCommonName = "socialweb-rootvault-client"

var ErrNoRootVaultTLSStateDir = errors.New(
	"vaultcert: the root vault's TLS material is derived from the state dir, and an empty one " +
		"writes the CA, the server certificate and the client certificate into the process's " +
		"current directory, which is not a path any consumer is told to read")

type RootVaultTLS struct {
	CAPath         string
	ServerCertFile string
	ServerKeyFile  string
	ClientCertFile string
	ClientKeyFile  string
	CAPEM          []byte
}

func EnsureRootVaultTLS(stateDir string, hosts ...string) (RootVaultTLS, error) {
	if stateDir == "" {
		return RootVaultTLS{}, ErrNoRootVaultTLSStateDir
	}
	if len(hosts) == 0 {
		return RootVaultTLS{}, fmt.Errorf("%w (none were given)", ErrNoVaultTLSHost)
	}
	dns, ips, err := splitHosts(hosts)
	if err != nil {
		return RootVaultTLS{}, err
	}
	out := RootVaultTLS{
		CAPath:         rootvault.TLSCAPath(stateDir),
		ServerCertFile: rootvault.ServerCertPath(stateDir),
		ServerKeyFile:  rootvault.ServerKeyPath(stateDir),
		ClientCertFile: rootvault.ClientCertPath(stateDir),
		ClientKeyFile:  rootvault.ClientKeyPath(stateDir),
	}
	if reuse, ok := reuseRootVaultTLS(out, dns, ips); ok {
		return reuse, nil
	}
	return generateRootVaultTLS(out, dns, ips)
}

func LoadRootVaultTLS(stateDir string) (RootVaultTLS, error) {
	if stateDir == "" {
		return RootVaultTLS{}, ErrNoRootVaultTLSStateDir
	}
	out := RootVaultTLS{
		CAPath:         rootvault.TLSCAPath(stateDir),
		ServerCertFile: rootvault.ServerCertPath(stateDir),
		ServerKeyFile:  rootvault.ServerKeyPath(stateDir),
		ClientCertFile: rootvault.ClientCertPath(stateDir),
		ClientKeyFile:  rootvault.ClientKeyPath(stateDir),
	}
	caPEM, err := os.ReadFile(out.CAPath)
	if err != nil {
		return RootVaultTLS{}, fmt.Errorf("vaultcert: reading the root vault's TLS CA at %s: %w", out.CAPath, err)
	}
	out.CAPEM = caPEM
	return out, nil
}

func (v RootVaultTLS) CertPool() (*x509.CertPool, error) {
	if len(v.CAPEM) == 0 {
		return nil, openbao.ErrNoCA
	}
	return openbao.CertPoolFromPEM(v.CAPEM)
}

func (v RootVaultTLS) ClientTLSConfig() (*tls.Config, error) {
	pool, err := v.CertPool()
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(v.ClientCertFile, v.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("vaultcert: the client certificate and key the root vault's listener "+
			"demands at %s and %s: %w", v.ClientCertFile, v.ClientKeyFile, err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func reuseRootVaultTLS(v RootVaultTLS, dns []string, ips []netip.Addr) (RootVaultTLS, bool) {
	caPEM, err := os.ReadFile(v.CAPath)
	if err != nil {
		return RootVaultTLS{}, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return RootVaultTLS{}, false
	}
	serverLeaf, ok := usableLeaf(v.ServerCertFile, v.ServerKeyFile, pool, x509.ExtKeyUsageServerAuth)
	if !ok || !coversHosts(serverLeaf, dns, ips) {
		return RootVaultTLS{}, false
	}
	if _, ok := usableLeaf(v.ClientCertFile, v.ClientKeyFile, pool, x509.ExtKeyUsageClientAuth); !ok {
		return RootVaultTLS{}, false
	}
	v.CAPEM = caPEM
	return v, true
}

func usableLeaf(certFile, keyFile string, pool *x509.CertPool, eku x509.ExtKeyUsage) (*x509.Certificate, bool) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, false
	}
	if time.Until(leaf.NotAfter) < vaultTLSRenewBefore {
		return nil, false
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{eku},
	}); err != nil {
		return nil, false
	}
	return leaf, true
}

func generateRootVaultTLS(v RootVaultTLS, dns []string, ips []netip.Addr) (RootVaultTLS, error) {
	now := time.Now()
	caKey, caCert, caDER, err := newVaultTLSCA(now)
	if err != nil {
		return RootVaultTLS{}, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	serverPEM, serverKeyPEM, err := signLeaf(now, caKey, caCert, vaultTLSServerCommonName,
		x509.ExtKeyUsageServerAuth, dns, ips)
	if err != nil {
		return RootVaultTLS{}, err
	}
	clientPEM, clientKeyPEM, err := signLeaf(now, caKey, caCert, RootVaultClientCommonName,
		x509.ExtKeyUsageClientAuth, nil, nil)
	if err != nil {
		return RootVaultTLS{}, err
	}

	dir := filepath.Dir(v.CAPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return RootVaultTLS{}, fmt.Errorf("vaultcert: creating the root vault's TLS directory %s: %w", dir, err)
	}
	for _, f := range []struct {
		path string
		data []byte
	}{
		{v.CAPath, caPEM},
		{v.ServerCertFile, append(append([]byte{}, serverPEM...), caPEM...)},
		{v.ServerKeyFile, serverKeyPEM},
		{v.ClientCertFile, append(append([]byte{}, clientPEM...), caPEM...)},
		{v.ClientKeyFile, clientKeyPEM},
	} {
		if err := os.WriteFile(f.path, f.data, vaultTLSFileMode); err != nil {
			return RootVaultTLS{}, fmt.Errorf("vaultcert: writing %s: %w", f.path, err)
		}
	}
	v.CAPEM = caPEM
	return v, nil
}
