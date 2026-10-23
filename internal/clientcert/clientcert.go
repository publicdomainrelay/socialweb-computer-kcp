package clientcert

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kubeconfig"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

var ErrVaultUnavailable = gateway.ErrIssuerUnavailable

const (
	issueTimeout  = 10 * time.Second
	revokeTimeout = 30 * time.Second
	crlTimeout    = 5 * time.Second
	lifetimeSlack = 5 * time.Minute
)

const noExpiryNotAfter = "9999-12-31T23:59:59Z"

type Recorder interface {
	RecordCertificate(ctx context.Context, rec kcpclient.CertificateRecord) error
	MarkCertificateRevoked(ctx context.Context, did, cluster, serial string) error
}

type Issuer struct {
	StateDir string
	CAFile   string
	Log      *slog.Logger
	Recorder Recorder
}

func DataDir(stateDir, did, cluster string) (string, error) {
	key, err := kcpclient.ClusterKey(kcpclient.HomePath(did, cluster))
	if err != nil {
		return "", err
	}
	return clusterprovider.ClusterDataDir(stateDir, key)
}

func (i *Issuer) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

func (i *Issuer) Grant(ctx context.Context, in kcpclient.GrantInput) (string, error) {
	if in.Caller == "" || in.Cluster == "" || in.Server == "" {
		return "", errors.New("clientcert: a caller, a cluster and a server are required")
	}
	ttl := in.TTL
	ttl = cmp.Or(ttl, gateway.DefaultClientCertTTL)
	if in.NoExpiry {
		if in.TTL != 0 {
			return "", errors.New("clientcert: a lifetime and no expiry are exclusive")
		}
	} else if ttl < gateway.MinClientCertTTL || ttl > gateway.MaxClientCertTTL {
		return "", errors.New("clientcert: the lifetime is outside the allowed window")
	}
	dir, err := DataDir(i.StateDir, string(in.Caller), in.Cluster)
	if err != nil {
		return "", err
	}
	if certPEM, keyPEM, leaf, ok, err := signFromClientCA(dir, string(in.Caller), ttl, in.NoExpiry); err != nil {
		return "", err
	} else if ok {
		return i.recordAndBuild(ctx, in, leaf, leaf.SerialNumber.Text(16), certPEM, keyPEM, nil)
	}
	client, _, err := clusterprovider.GatewayVaultClient(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrVaultUnavailable, err)
	}
	cctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()
	role, req := clusterprovider.VaultPKIRoleKubeconfig, openbao.CertRequest{
		CommonName:        string(in.Caller),
		TTL:               strconv.FormatInt(int64(ttl/time.Second), 10) + "s",
		ExcludeCNFromSANs: true,
	}
	if in.NoExpiry {
		role, req.TTL, req.NotAfter = clusterprovider.VaultPKIRoleKubeconfigNoExpiry, "", noExpiryNotAfter
	}
	if err := clusterprovider.ImportClientCA(cctx, dir, client); err != nil {
		return "", fmt.Errorf("%w: %v", ErrVaultUnavailable, err)
	}
	cert, err := client.IssueCert(cctx, clusterprovider.VaultPKIPath, role, req)
	if err != nil {
		return "", fmt.Errorf("%w: issue: %v", ErrVaultUnavailable, err)
	}
	leaf, err := parseCertificate(cert.Certificate)
	if err != nil {
		return "", err
	}
	switch {
	case leaf.Subject.CommonName != string(in.Caller):
		return "", errors.New("clientcert: the issued certificate has the wrong common name")
	case !slices.Equal(leaf.Subject.Organization, []string{kcpclient.OwnersGroup}):
		return "", errors.New("clientcert: the issued certificate has the wrong organization")
	case !in.NoExpiry && leaf.NotAfter.Sub(leaf.NotBefore) > gateway.MaxClientCertTTL+lifetimeSlack:
		return "", errors.New("clientcert: the issued certificate lives too long")
	case in.NoExpiry != slices.Contains(leaf.Subject.OrganizationalUnit, clusterprovider.NoExpiryOrganizationalUnit):
		return "", errors.New("clientcert: the issued certificate has the wrong organizational unit")
	case !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth):
		return "", errors.New("clientcert: the issued certificate is not a client certificate")
	case cert.PrivateKey == "":
		return "", errors.New("clientcert: the issuer returned no private key")
	}
	return i.recordAndBuild(ctx, in, leaf, cert.Serial, cert.Certificate, cert.PrivateKey,
		func() { i.revokeSerial(ctx, client, cert.Serial) })
}

func (i *Issuer) recordAndBuild(ctx context.Context, in kcpclient.GrantInput, leaf *x509.Certificate, serial, certPEM, keyPEM string, revoke func()) (string, error) {
	if i.Recorder == nil {
		if revoke != nil {
			revoke()
		}
		return "", fmt.Errorf("%w: no recorder for certificates", ErrVaultUnavailable)
	}
	rec := kcpclient.CertificateRecord{DID: string(in.Caller), Cluster: in.Cluster, Serial: serial,
		NotAfter: leaf.NotAfter, NoExpiry: in.NoExpiry}
	if err := i.Recorder.RecordCertificate(ctx, rec); err != nil {
		if revoke != nil {
			revoke()
		}
		return "", fmt.Errorf("%w: record: %v", ErrVaultUnavailable, err)
	}
	var caPEM []byte
	var err error
	if i.CAFile != "" {
		if caPEM, err = os.ReadFile(i.CAFile); err != nil {
			return "", fmt.Errorf("clientcert: reading the CA file: %w", err)
		}
	}
	out, err := kubeconfig.BuildClientCert(kubeconfig.ClientCertInput{ClusterName: in.Cluster, Server: in.Server,
		CACertPEM: caPEM, CertPEM: []byte(certPEM), KeyPEM: []byte(keyPEM)})
	if err != nil {
		return "", err
	}
	msg := "clientcert: issued a kubeconfig certificate"
	if in.NoExpiry {
		msg = "clientcert: kubeconfig no-expiry certificate issued"
	}
	i.log().Info(msg, "did", string(in.Caller), "cluster", in.Cluster, "serial", serial,
		"notAfter", leaf.NotAfter.UTC().Format(time.RFC3339))
	return out, nil
}

func (i *Issuer) revokeSerial(ctx context.Context, client *openbao.Client, serial string) {
	if err := client.Write(ctx, "pki/revoke", map[string]any{"serial_number": serial}); err != nil {
		i.log().Warn("clientcert: the revoke of an unrecorded certificate failed", "serial", serial, "err", err.Error())
	}
}

func signFromClientCA(dir, did string, ttl time.Duration, noExpiry bool) (string, string, *x509.Certificate, bool, error) {
	tlsDir := filepath.Join(dir, "server", "tls")
	certPEM, err := os.ReadFile(filepath.Join(tlsDir, "client-ca.crt"))
	if err != nil {
		return "", "", nil, false, nil
	}
	keyPEM, err := os.ReadFile(filepath.Join(tlsDir, "client-ca.key"))
	if err != nil {
		return "", "", nil, false, nil
	}
	caBlock, _ := pem.Decode(certPEM)
	if caBlock == nil {
		return "", "", nil, false, fmt.Errorf("clientcert: %s holds no certificate", filepath.Join(tlsDir, "client-ca.crt"))
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return "", "", nil, false, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return "", "", nil, false, fmt.Errorf("clientcert: %s holds no key", filepath.Join(tlsDir, "client-ca.key"))
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if parseErr != nil {
			return "", "", nil, false, err
		}
		var ok bool
		if caKey, ok = parsed.(*ecdsa.PrivateKey); !ok {
			return "", "", nil, false, errors.New("clientcert: the client CA key is not ECDSA")
		}
	}
	if !caCert.PublicKey.(*ecdsa.PublicKey).Equal(&caKey.PublicKey) {
		return "", "", nil, false, errors.New("clientcert: the client CA key is not its certificate's key")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", nil, false, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", nil, false, err
	}
	now := time.Now()
	notAfter := now.Add(ttl)
	ou := []string(nil)
	if noExpiry {
		notAfter = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		ou = []string{clusterprovider.NoExpiryOrganizationalUnit}
	}
	tmpl := &x509.Certificate{SerialNumber: serial,
		Subject:   pkix.Name{CommonName: did, Organization: []string{kcpclient.OwnersGroup}, OrganizationalUnit: ou},
		NotBefore: now.Add(-time.Minute), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return "", "", nil, false, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return "", "", nil, false, err
	}
	kder, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return "", "", nil, false, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})), leaf, true, nil
}

func parseCertificate(pemText string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("clientcert: the issuer returned no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func (i *Issuer) Revoke(ctx context.Context, did, cluster string) (int, error) {
	dir, err := DataDir(i.StateDir, did, cluster)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	client, _, err := clusterprovider.GatewayVaultClient(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrVaultUnavailable, err)
	}
	serials, err := client.List(ctx, "pki/certs")
	if err != nil {
		return 0, fmt.Errorf("clientcert: listing certificates: %w", err)
	}
	count := 0
	now := time.Now()
	for _, serial := range serials {
		data, err := client.Read(ctx, "pki/cert/"+serial)
		if err != nil {
			return count, fmt.Errorf("clientcert: reading a certificate: %w", err)
		}
		text, _ := data["certificate"].(string)
		leaf, err := parseCertificate(text)
		if err != nil {
			continue
		}
		if leaf.Subject.CommonName != did || !slices.Equal(leaf.Subject.Organization, []string{kcpclient.OwnersGroup}) || !leaf.NotAfter.After(now) {
			continue
		}
		if err := client.Write(ctx, "pki/revoke", map[string]any{"serial_number": serial}); err != nil {
			return count, fmt.Errorf("clientcert: revoking a certificate: %w", err)
		}
		count++
		if i.Recorder != nil {
			if err := i.Recorder.MarkCertificateRevoked(ctx, did, cluster, serial); err != nil {
				i.log().Warn("clientcert: the record of a revoked certificate was not updated", "serial", serial, "err", err.Error())
			}
		}
	}
	i.log().Info("clientcert: revoked kubeconfig certificates", "did", did, "cluster", cluster, "count", count)
	return count, nil
}

func FetchRevoked(ctx context.Context, dataDir string) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(ctx, crlTimeout)
	defer cancel()
	client, _, err := clusterprovider.GatewayVaultClient(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	serials, err := client.List(ctx, "pki/certs/revoked")
	if err != nil {
		return nil, fmt.Errorf("clientcert: listing revoked certificates: %w", err)
	}
	out := make(map[string]struct{}, len(serials))
	for _, s := range serials {
		out[NormalizeSerial(s)] = struct{}{}
	}
	return out, nil
}

func NormalizeSerial(s string) string {
	return kcpclient.CertSerialKey(s)
}
