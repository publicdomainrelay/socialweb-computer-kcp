package tenantcredentialmain

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterkcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
)

const caCertName = "ca.crt"

const clientCertName = "tls.crt"

const clientKeyName = "tls.key"

func Main(ctx context.Context, inv foldmain.Invocation) int {
	if err := run(ctx, inv); err != nil {
		fmt.Fprintln(inv.Stderr, "socialweb-tenantcredential:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, inv foldmain.Invocation) error {
	cfg, err := load(inv.Env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.OutDir, 0o700); err != nil {
		return fmt.Errorf("creating the out dir %s: %w", cfg.OutDir, err)
	}
	if err := refuseExisting(cfg.OutDir); err != nil {
		return err
	}
	restCfg, err := clusterkcp.MintTenantCredentialForClusterAs(
		ctx, cfg.StateDir, cfg.Cluster, cfg.Server, cfg.CommonName, cfg.Organizations, cfg.ValidFor)
	if err != nil {
		return err
	}
	leaf, err := identityOf(restCfg)
	if err != nil {
		return err
	}
	if err := checkIdentity(leaf, cfg.CommonName, cfg.Organizations); err != nil {
		return err
	}
	paths, err := writeCredential(cfg.OutDir, restCfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(inv.Stdout, "socialweb-tenantcredential: cluster=%s commonName=%s organizations=%s validUntil=%s "+
		"ca=%s cert=%s key=%s\n", cfg.Cluster, cfg.CommonName, strings.Join(cfg.Organizations, ","),
		leaf.NotAfter.UTC().Format(time.RFC3339), paths[0], paths[1], paths[2])
	return nil
}

func refuseExisting(dir string) error {
	var found []string
	for _, name := range []string{caCertName, clientCertName, clientKeyName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf(
			"these files already exist and this tool will not overwrite a credential: %s. "+
				"Overwriting would replace the credential some runner or controller is already "+
				"authenticating with, and that silent replacement is the failure invariant 20 "+
				"exists to prevent. Move the existing files aside, or name a different out dir",
			strings.Join(found, ", "))
	}
	return nil
}

func identityOf(cfg *rest.Config) (*x509.Certificate, error) {
	if len(cfg.TLSClientConfig.CertData) == 0 {
		return nil, errors.New("the minting path returned a credential with no client " +
			"certificate, so there is no identity to compare against what was asked for and " +
			"nothing to write to tls.crt")
	}
	for block, rest := pem.Decode(cfg.TLSClientConfig.CertData); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the minted client certificate does not parse: %w", err)
		}
		return cert, nil
	}
	return nil, errors.New("the minted client certificate holds no CERTIFICATE PEM block")
}

func checkIdentity(leaf *x509.Certificate, commonName string, organizations []string) error {
	if leaf.Subject.CommonName != commonName {
		return fmt.Errorf(
			"the minted credential authenticates as %q and %s asked for %q. The certificate is the "+
				"artifact: an operator whose RBAC binds %q would be handed a credential for %q, and "+
				"every rule keyed on it would miss. This tool refuses rather than write a credential "+
				"whose identity is not the one that was asked for",
			leaf.Subject.CommonName, envCommonName, commonName, commonName, leaf.Subject.CommonName)
	}
	if !slices.Equal(leaf.Subject.Organization, organizations) {
		return fmt.Errorf(
			"the minted credential carries the organizations %v and %s asked for %v. The apiserver "+
				"reads the groups off the Organizations, so a mismatch is a credential whose groups "+
				"the operator's RBAC does not bind: this tool refuses rather than write it",
			leaf.Subject.Organization, envOrganizations, organizations)
	}
	return nil
}

func writeCredential(dir string, cfg *rest.Config) ([3]string, error) {
	files := [3]struct {
		name string

		data []byte

		why string
	}{
		{caCertName, cfg.CAData,
			"the cluster's server CA, which is what verifies the apiserver's own certificate"},
		{clientCertName, cfg.TLSClientConfig.CertData, "the minted client certificate"},
		{clientKeyName, cfg.TLSClientConfig.KeyData, "the minted client key"},
	}
	var missing []string
	for _, f := range files {
		if len(f.data) == 0 {
			missing = append(missing, fmt.Sprintf("%s (%s)", f.name, f.why))
		}
	}
	if len(missing) > 0 {
		return [3]string{}, fmt.Errorf(
			"the minted credential is missing %s. The three files are a set and a partial one is "+
				"refused rather than written: ca.crt without tls.crt has nothing to present, and "+
				"tls.key without tls.crt is a private key that authenticates nothing, so a reader "+
				"of the directory would find a credential that looks complete and is not",
			strings.Join(missing, " and "))
	}

	var paths [3]string
	written := make([]string, 0, len(files))
	for i, f := range files {
		path, err := writeOne(dir, f.name, f.data)
		if err != nil {
			for _, done := range written {
				_ = os.Remove(done)
			}
			return [3]string{}, err
		}
		paths[i] = path
		written = append(written, path)
	}
	return paths, nil
}

func writeOne(dir, name string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating a temporary file for %s in %s: %w", name, dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("setting %s to mode 0600: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("closing %s: %w", tmpName, err)
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("renaming %s into place as %s: %w", tmpName, final, err)
	}
	return final, nil
}
