package clusterprovider

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	clientCACertName = "client-ca.crt"
	clientCAKeyName  = "client-ca.key"
	serverCACertName = "server-ca.crt"
)

const SuperuserName = "system:admin"

const SuperuserOrganization = "system:masters"

const DefaultCredentialValidity = 10 * 365 * 24 * time.Hour

const credentialSerialBits = 128

var ErrNoCredentialDataDir = errors.New(
	"clusterprovider: the tenant credential is minted from the cluster's own ClientCA, which " +
		"lives in that cluster's data dir; with no data dir the CA paths resolve against the " +
		"working directory, and the only CA that could be signed there belongs to some other " +
		"cluster or to no cluster at all")

var ErrNoCredentialCommonName = errors.New(
	"clusterprovider: a client certificate with an empty CommonName authenticates to the " +
		"apiserver as the empty username, which no RBAC rule and no ValidatingAdmissionPolicy " +
		"carve-out admits; the credential would be refused at the first apply with a 403 that " +
		"reads as a broken grant rather than as a malformed certificate")

var ErrNoCredentialOrganization = errors.New(
	"clusterprovider: a client certificate with no organization carries no group. The apiserver " +
		"reads the username off the CommonName and the groups off the Organizations, so a " +
		"credential with no organization is an unprivileged user, and every object the tenant " +
		"grant exists to apply is refused")

var ErrNoCredentialServer = errors.New(
	"clusterprovider: the tenant credential needs the https URL of the apiserver it will " +
		"authenticate to. That URL's host is the name the apiserver's own certificate must carry, " +
		"so an empty or hostless one is a credential whose server identity nothing checks")

var ErrCredentialServerNotHTTPS = errors.New(
	"clusterprovider: the tenant credential's server URL must be https. The apiserver serves " +
		"nothing else, and a plaintext URL is either a dial that cannot connect or an identity " +
		"crossing the network in the clear")

var ErrNoClientCA = errors.New(
	"clusterprovider: the cluster's ClientCA files are not readable under <DataDir>/server/tls, " +
		"so no credential can be minted from them. This is a refusal and NOT a reason to reach " +
		"for a wider credential: the only other usable credential on this host is k3s's own admin " +
		"kubeconfig, which is shared state and is not this provider's to hand out")

var ErrClientCAUnusable = errors.New(
	"clusterprovider: the files under <DataDir>/server/tls are present and are not a usable " +
		"signing pair. A re-seed that wrote something else here leaves a cluster whose every " +
		"minted credential the apiserver refuses as an unknown authority")

var ErrClientCAKeyMismatch = errors.New(
	"clusterprovider: the ClientCA key is not the key of the ClientCA certificate. A re-seed " +
		"that replaced one and not the other still signs, and the certificate it produces is one " +
		"no apiserver trusts, so the failure would surface as an authentication error at the " +
		"first apply, hundreds of lines from the pair that caused it")

var ErrNoServerCA = errors.New(
	"clusterprovider: the cluster's server CA is absent or holds no CA certificate, so nothing " +
		"the apiserver presents for this URL could ever verify. A credential built without it " +
		"could only be used with server verification switched off, and that is not a state this " +
		"provider will construct")

var ErrCredentialSign = errors.New(
	"clusterprovider: the tenant credential could not be signed")

var ErrCredentialDoesNotChain = errors.New(
	"clusterprovider: the freshly signed certificate does not verify against the very CA it was " +
		"signed with, so the credential is refused here rather than by the apiserver as an " +
		"authentication error at the first apply")

var ErrCredentialSourceUnknown = errors.New(
	"clusterprovider: the tenant credential's source is one of cluster-ca, which signs it from " +
		"the cluster's own ClientCA on disk, or vault-if-sealed, which reaches for the cluster's " +
		"vault when that cluster has left unseal material beside it. A caller that names neither " +
		"has left the choice to a default, and the default is what this seam exists to remove: on " +
		"a market node the vault is not between the provider and the tenant apiserver at all, so " +
		"the seal's mere presence would put a second TLS dependency on that path and its failure " +
		"would read as a broken CA rather than as a source nothing should have consulted")

type CredentialSource string

const (
	CredentialSourceClusterCA CredentialSource = "cluster-ca"

	CredentialSourceVaultIfSealed CredentialSource = "vault-if-sealed"
)

type Credential struct {
	Host string

	CAData []byte

	CertData []byte

	KeyData []byte
}

type CredentialOptions struct {
	DataDir string

	Server string

	CommonName string

	Organizations []string

	ValidFor time.Duration
}

func MintTenantCredential(o CredentialOptions) (*Credential, error) {
	if o.DataDir == "" {
		return nil, ErrNoCredentialDataDir
	}
	if o.CommonName == "" {
		return nil, ErrNoCredentialCommonName
	}
	if len(o.Organizations) == 0 {
		return nil, fmt.Errorf("%w (none were given)", ErrNoCredentialOrganization)
	}
	if slices.Contains(o.Organizations, "") {
		return nil, fmt.Errorf("%w (one of the %d given is empty)",
			ErrNoCredentialOrganization, len(o.Organizations))
	}
	server, err := credentialServer(o.Server)
	if err != nil {
		return nil, err
	}
	validFor := o.ValidFor
	validFor = opt.Positive(validFor, DefaultCredentialValidity)

	tlsDir := clusterstack.TLSDir(o.DataDir)
	caCert, caKey, caPool, err := loadClientCA(
		filepath.Join(tlsDir, clientCACertName),
		filepath.Join(tlsDir, clientCAKeyName),
	)
	if err != nil {
		return nil, err
	}
	serverCA, err := loadServerCA(filepath.Join(tlsDir, serverCACertName))
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: generating the tenant credential's key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), credentialSerialBits))
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: drawing the tenant credential's serial: %w", err)
	}
	serial.Add(serial, big.NewInt(1))

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   o.CommonName,
			Organization: o.Organizations,
		},
		NotBefore:             now,
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, key.Public(), caKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialSign, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: the certificate it produced does not parse: %w", ErrCredentialSign, err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     caPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialDoesNotChain, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: marshalling the tenant credential's key: %w", err)
	}

	return &Credential{
		Host:     server.String(),
		CAData:   serverCA,
		CertData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyData:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func MintTenantCredentialForCluster(ctx context.Context, stateDir, cluster, server string, source CredentialSource) (*Credential, error) {
	dataDir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return nil, err
	}
	return MintTenantCredentialForDataDir(ctx, dataDir, server, source)
}

func MintTenantCredentialForDataDir(ctx context.Context, dataDir, server string, source CredentialSource) (*Credential, error) {
	o := CredentialOptions{
		DataDir:       dataDir,
		Server:        server,
		CommonName:    SuperuserName,
		Organizations: []string{SuperuserOrganization},
	}
	switch source {
	case CredentialSourceClusterCA:
		return MintTenantCredential(o)
	case CredentialSourceVaultIfSealed:
		client, a, err := clusterVault(ctx, dataDir)
		if err == nil {
			return mintTenantCredentialFromVault(ctx, o, client, a.Address)
		}
		return MintTenantCredential(o)
	}
	return nil, fmt.Errorf("%w (got %q)", ErrCredentialSourceUnknown, source)
}

func MintTenantCredentialForClusterAs(ctx context.Context, stateDir, cluster, server, commonName string, organizations []string, validFor time.Duration) (*Credential, error) {
	dataDir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return nil, err
	}
	return MintTenantCredentialForDataDirAs(dataDir, server, commonName, organizations, validFor)
}

func MintTenantCredentialForDataDirAs(dataDir, server, commonName string, organizations []string, validFor time.Duration) (*Credential, error) {
	return MintTenantCredential(CredentialOptions{
		DataDir:       dataDir,
		Server:        server,
		CommonName:    commonName,
		Organizations: organizations,
		ValidFor:      validFor,
	})
}

func mintTenantCredentialFromVault(ctx context.Context, o CredentialOptions, client *openbao.Client, addr string) (*Credential, error) {
	if o.DataDir == "" {
		return nil, ErrNoCredentialDataDir
	}
	if o.CommonName == "" {
		return nil, ErrNoCredentialCommonName
	}
	if len(o.Organizations) == 0 {
		return nil, fmt.Errorf("%w (none were given)", ErrNoCredentialOrganization)
	}
	if slices.Contains(o.Organizations, "") {
		return nil, fmt.Errorf("%w (one of the %d given is empty)",
			ErrNoCredentialOrganization, len(o.Organizations))
	}
	server, err := credentialServer(o.Server)
	if err != nil {
		return nil, err
	}
	validFor := o.ValidFor
	validFor = opt.Positive(validFor, DefaultCredentialValidity)

	tlsDir := clusterstack.TLSDir(o.DataDir)
	serverCA, err := loadServerCA(filepath.Join(tlsDir, serverCACertName))
	if err != nil {
		return nil, err
	}
	caPath := filepath.Join(tlsDir, clientCACertName)
	caCertPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoClientCA, caPath, err)
	}
	caCert, err := parseCertificate(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrClientCAUnusable, caPath, err)
	}
	if !caCert.IsCA {
		return nil, fmt.Errorf("%w: %s is not a CA certificate", ErrClientCAUnusable, caPath)
	}
	if remaining := time.Until(caCert.NotAfter) - time.Hour; remaining < validFor {
		validFor = remaining
	}
	if validFor <= 0 {
		return nil, fmt.Errorf("%w: the client CA at %s expires at %s, so the vault can issue no "+
			"credential that outlives it", ErrCredentialSign, caPath, caCert.NotAfter.UTC())
	}

	issued, err := client.IssueCert(ctx, VaultPKIPath, VaultPKIRoleTenant, openbao.CertRequest{
		CommonName: o.CommonName,
		TTL:        vaultTTL(validFor),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: issuing the tenant credential from the vault at %s: %w",
			ErrCredentialSign, addr, err)
	}
	leaf, err := parseCertificate([]byte(issued.Certificate))
	if err != nil {
		return nil, fmt.Errorf("%w: the vault's certificate does not parse: %w", ErrCredentialSign, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("%w: %s holds no CERTIFICATE PEM block", ErrClientCAUnusable, caPath)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialDoesNotChain, err)
	}
	return &Credential{
		Host:     server.String(),
		CAData:   serverCA,
		CertData: []byte(issued.Certificate),
		KeyData:  []byte(issued.PrivateKey),
	}, nil
}

func credentialServer(server string) (*url.URL, error) {
	if server == "" {
		return nil, fmt.Errorf("%w (it is empty)", ErrNoCredentialServer)
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("%w (%q does not parse): %w", ErrNoCredentialServer, server, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%w (got %q)", ErrCredentialServerNotHTTPS, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%w (%q carries no host)", ErrNoCredentialServer, server)
	}
	return u, nil
}

func loadClientCA(certPath, keyPath string) (*x509.Certificate, crypto.Signer, *x509.CertPool, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %s: %w", ErrNoClientCA, certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %s: %w", ErrNoClientCA, keyPath, err)
	}
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %s: %w", ErrClientCAUnusable, certPath, err)
	}
	if !cert.IsCA {
		return nil, nil, nil, fmt.Errorf(
			"%w: %s is not a CA certificate, so anything it signs is a certificate no apiserver "+
				"resolves to a group", ErrClientCAUnusable, certPath)
	}
	key, err := parseSigner(keyPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %s: %w", ErrClientCAUnusable, keyPath, err)
	}
	if err := sameKey(certPath, keyPath, cert, key); err != nil {
		return nil, nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, nil, nil, fmt.Errorf("%w: %s holds no CERTIFICATE PEM block", ErrClientCAUnusable, certPath)
	}
	return cert, key, pool, nil
}

func loadServerCA(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoServerCA, path, err)
	}
	certs, err := parseCertificates(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoServerCA, path, err)
	}
	anyCA := false
	for _, c := range certs {
		if c.IsCA {
			anyCA = true
		}
	}
	if !anyCA {
		return nil, fmt.Errorf("%w: %s holds %d certificate(s) and not one is a CA",
			ErrNoServerCA, path, len(certs))
	}
	return raw, nil
}

func sameKey(certPath, keyPath string, cert *x509.Certificate, key crypto.Signer) error {
	want, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return fmt.Errorf("%w: the public key of %s: %w", ErrClientCAUnusable, certPath, err)
	}
	got, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return fmt.Errorf("%w: the public half of %s: %w", ErrClientCAUnusable, keyPath, err)
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf(
			"%w: %s is the certificate %q and %s holds the key of another certificate, so the "+
				"minted credential would be signed by a key that CA never published",
			ErrClientCAKeyMismatch, certPath, cert.Subject.CommonName, keyPath)
	}
	return nil
}

func parseSigner(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the file holds no PEM block")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		signer, ok := parsed.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("the PEM block holds a %T, which cannot sign a certificate", parsed)
		}
		return signer, nil
	}
	if parsed, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return parsed, nil
	}
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return parsed, nil
	}
	return nil, errors.New("the PEM block is not a PKCS#8, SEC 1 or PKCS#1 private key")
}

func parseCertificate(certPEM []byte) (*x509.Certificate, error) {
	certs, err := parseCertificates(certPEM)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, errors.New("the file holds no CERTIFICATE PEM block")
	}
	return certs[0], nil
}

func parseCertificates(certPEM []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for block, rest := pem.Decode(certPEM); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	return out, nil
}
