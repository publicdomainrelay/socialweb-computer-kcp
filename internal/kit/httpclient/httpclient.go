package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/networkdial"
)

var ErrNoCA = errors.New("httpclient: the CA bundle holds no certificate")

type Options struct {
	Timeout time.Duration

	Hosts networkdial.Resolver

	TLS *tls.Config

	ProxyFromEnv bool

	UserAgent string

	OnError func(error)
}

func External(opts Options) *http.Client {
	transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{
		Hosts:          opts.Hosts,
		TLSConfig:      opts.TLS,
		AllowSystemDNS: true,
	})
	if err != nil {
		if opts.OnError != nil {
			opts.OnError(err)
		}
		return &http.Client{Timeout: opts.Timeout, Transport: withUserAgent(nil, opts.UserAgent)}
	}
	if opts.ProxyFromEnv {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Timeout: opts.Timeout, Transport: withUserAgent(transport, opts.UserAgent)}
}

type TLSOptions struct {
	CAPEM []byte

	Pool *x509.CertPool

	CertPEM []byte

	KeyPEM []byte

	ServerName string

	Timeout time.Duration

	IdleConnTimeout time.Duration
}

func TLSConfig(opts TLSOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: opts.ServerName, RootCAs: opts.Pool}
	if len(opts.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CAPEM) {
			return nil, ErrNoCA
		}
		cfg.RootCAs = pool
	}
	if len(opts.CertPEM) > 0 || len(opts.KeyPEM) > 0 {
		pair, err := tls.X509KeyPair(opts.CertPEM, opts.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("httpclient: the client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

func TLSClient(opts TLSOptions) (*http.Client, error) {
	cfg, err := TLSConfig(opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: opts.Timeout, Transport: &http.Transport{IdleConnTimeout: opts.IdleConnTimeout, TLSClientConfig: cfg}}, nil
}

func withUserAgent(base http.RoundTripper, agent string) http.RoundTripper {
	if agent == "" {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return userAgentTransport{base: base, agent: agent}
}

type userAgentTransport struct {
	base http.RoundTripper

	agent string
}

func (t userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", t.agent)
	return t.base.RoundTrip(clone)
}
