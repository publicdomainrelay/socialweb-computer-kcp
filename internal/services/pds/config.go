package pds

import (
	"crypto/tls"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
)

const Kind = "Pds"

const (
	RepoKeyDir        = "repo-keys"
	SessionSecretFile = "session-secret"
	RotationKeyFile   = "plc-rotation.key"
)

type Deps struct {
	component.Instance
	Credentials component.CredentialResolver
	TLS         *tls.Config
	Egress      component.Egress
}

func filePath(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"file://", "file:"} {
		if after, ok := strings.CutPrefix(raw, prefix); ok {
			return after
		}
	}
	return raw
}

func keyFileFor(certFile string) string {
	certFile = strings.TrimSpace(certFile)
	if certFile == "" {
		return ""
	}
	ext := filepath.Ext(certFile)
	switch strings.ToLower(ext) {
	case ".crt", ".cert", ".pem":
		return strings.TrimSuffix(certFile, ext) + ".key"
	}
	return certFile + ".key"
}

func (s *Service) buildTLSConfig() (*tls.Config, error) {
	if s.spec.TLS == nil || !s.spec.TLS.Enabled {
		return nil, nil
	}
	var tlsConfig *tls.Config
	if s.certFile == "" {
		if s.deps.TLS == nil {
			return nil, errors.New("pds: tls is enabled but no certificate is configured")
		}
		tlsConfig = s.deps.TLS.Clone()
	} else {
		reloader, err := tlsreload.New(s.certFile, []string{s.keyFile, s.certFile}, s.deps.Log)
		if err != nil {
			if s.deps.TLS == nil {
				return nil, fmt.Errorf("pds: loading the tls certificate: %w", err)
			}
			tlsConfig = s.deps.TLS.Clone()
		} else {
			tlsConfig = reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
		}
	}
	if s.caBundle != "" {
		pool, err := tlsreload.PoolFromBundle(s.caBundle, s.deps.Log)
		if err != nil {
			return nil, fmt.Errorf("pds: the tls ca bundle: %w", err)
		}
		if tlsConfig.ClientCAs == nil {
			tlsConfig = pool.ServerConfig(tlsConfig, tlsConfig.ClientAuth)
		}
	}
	return tlsConfig, nil
}
