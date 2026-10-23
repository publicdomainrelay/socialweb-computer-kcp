package networkdial

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

type TLSConfigOptions struct {
	CABundle     []byte
	ServerName   string
	Certificates []tls.Certificate
	MinVersion   uint16
}

func NewTLSConfig(opts TLSConfigOptions) (*tls.Config, error) {
	cfg := &tls.Config{
		ServerName:   normalizeHost(opts.ServerName),
		Certificates: append([]tls.Certificate(nil), opts.Certificates...),
		MinVersion:   opts.MinVersion,
	}
	cfg.MinVersion = cmp.Or(cfg.MinVersion, tls.VersionTLS12)
	if len(opts.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CABundle) {
			return nil, ErrInvalidCABundle
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func TLSConfigFromCA(caBundlePEM []byte, serverName string) (*tls.Config, error) {
	if len(caBundlePEM) == 0 {
		return nil, ErrNoCABundle
	}
	cfg, err := NewTLSConfig(TLSConfigOptions{CABundle: caBundlePEM, ServerName: serverName})
	if err != nil {
		return nil, fmt.Errorf("networkdial: building the network TLS config: %w", err)
	}
	return cfg, nil
}
