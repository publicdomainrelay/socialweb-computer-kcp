package appctl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	GuestTLSDir     = "guest-tls"
	GuestTLSHost    = v1alpha1.GuestTLSHost
	guestTLSDays    = 30
	guestCAFile     = "ca.crt"
	guestCAKeyFile  = "ca.key"
	guestLeafFile   = GuestTLSHost + ".crt"
	guestLeafKey    = GuestTLSHost + ".key"
	keysDir         = "keys"
	hexKeyBytes     = 32
	certRenewBefore = 24 * time.Hour
)

func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
}

func certValid(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return false
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	return err == nil && time.Until(c.NotAfter) > certRenewBefore
}

func EnsureGuestTLS(dir string) error {
	if certValid(filepath.Join(dir, guestLeafFile)) && certValid(filepath.Join(dir, guestCAFile)) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	sn, err := serial()
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: "market network guest CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, guestTLSDays), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	if sn, err = serial(); err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: GuestTLSHost},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, guestTLSDays), DNSNames: []string{GuestTLSHost, "*." + GuestTLSHost},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{guestCAKeyFile: pemBlock("PRIVATE KEY", caKeyDER), guestLeafKey: pemBlock("PRIVATE KEY", leafKeyDER)} {
		if err := writeSecret(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{guestCAFile: pemBlock("CERTIFICATE", caDER), guestLeafFile: pemBlock("CERTIFICATE", leafDER)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func EnsureHexKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 2*hexKeyBytes {
		return string(b), nil
	}
	buf := make([]byte, hexKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	k := hex.EncodeToString(buf)
	return k, writeSecret(path, []byte(k))
}
