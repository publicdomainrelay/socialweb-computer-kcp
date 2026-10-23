package servetls

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
)

func KeyFileFor(certFile string) string {
	switch {
	case certFile == "":
		return ""
	case strings.HasSuffix(certFile, ".crt"):
		return strings.TrimSuffix(certFile, ".crt") + ".key"
	case strings.HasSuffix(certFile, ".pem"):
		return strings.TrimSuffix(certFile, ".pem") + ".key"
	default:
		return certFile + ".key"
	}
}

func KeyFilesFor(certFile string) []string {
	candidates := []string{KeyFileFor(certFile)}
	if before, ok := strings.CutSuffix(certFile, ".pem"); ok {
		candidates = append(candidates, before+"-key.pem")
	}
	return append(candidates, certFile)
}

func RefPath(ref string, storage *v1alpha1.ComponentStorage) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if filepath.IsAbs(ref) || storage == nil || storage.CertsDir == "" {
		return ref
	}
	return filepath.Join(storage.CertsDir, ref)
}

func CertificateFiles(certRef string, storage *v1alpha1.ComponentStorage, hostnames []string) (string, string) {
	certFile := RefPath(certRef, storage)
	if certFile == "" && storage != nil && storage.CertsDir != "" && len(hostnames) > 0 {
		name := hostport.HostnameOnly(hostnames[0])
		if name != "" && name != "*" {
			certFile = filepath.Join(storage.CertsDir, name+".crt")
		}
	}
	return certFile, KeyFileFor(certFile)
}

func Config(certFile string, fallback *tls.Config, log *slog.Logger) (*tls.Config, error) {
	if certFile != "" {
		reloader, err := tlsreload.New(certFile, KeyFilesFor(certFile), log)
		if err != nil {
			return nil, err
		}
		return reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}), nil
	}
	if fallback != nil {
		tlsConfig := fallback.Clone()
		if len(tlsConfig.NextProtos) == 0 {
			tlsConfig.NextProtos = []string{"http/1.1"}
		}
		return tlsConfig, nil
	}
	return nil, errors.New("a TLS certificate is required")
}
