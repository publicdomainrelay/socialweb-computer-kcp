package networkdial

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	DefaultDialTimeout      = 10 * time.Second
	DefaultKeepAlive        = 30 * time.Second
	DefaultHTTPTimeout      = 30 * time.Second
	DefaultIdleConnTimeout  = 90 * time.Second
	DefaultHandshakeTimeout = 15 * time.Second
	DefaultMaxIdleConns     = 100
)

type DialOptions struct {
	Hosts          Resolver
	TLSConfig      *tls.Config
	AllowSystemDNS bool
}

type Dialer struct {
	resolver       Resolver
	tlsConfig      *tls.Config
	dialer         net.Dialer
	allowSystemDNS bool
}

func NewDialer(hosts Resolver, tlsConfig *tls.Config) (*Dialer, error) {
	return NewDialerWithOptions(DialOptions{Hosts: hosts, TLSConfig: tlsConfig})
}

func NewDialerWithOptions(opts DialOptions) (*Dialer, error) {
	if opts.Hosts == nil {
		return nil, ErrNoResolver
	}
	return &Dialer{
		resolver:  opts.Hosts,
		tlsConfig: cloneTLSConfig(opts.TLSConfig),
		dialer: net.Dialer{
			Timeout:   DefaultDialTimeout,
			KeepAlive: DefaultKeepAlive,
		},
		allowSystemDNS: opts.AllowSystemDNS,
	}, nil
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || d.resolver == nil {
		return nil, ErrNoResolver
	}
	target, err := d.address(address)
	if err != nil {
		return nil, err
	}
	conn, err := d.dialer.DialContext(ctx, network, target)
	if err != nil {
		return nil, fmt.Errorf("networkdial: dialing %s for %s: %w", target, address, err)
	}
	return conn, nil
}

func (d *Dialer) Resolve(address string) (string, error) {
	if d == nil || d.resolver == nil {
		return "", ErrNoResolver
	}
	return d.address(address)
}

func (d *Dialer) TLSConfig() *tls.Config {
	if d == nil {
		return cloneTLSConfig(nil)
	}
	return cloneTLSConfig(d.tlsConfig)
}

func (d *Dialer) address(address string) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("networkdial: %q is not host:port: %w", address, err)
	}
	if mapped, ok := d.resolver.Address(address); ok {
		return mapped, nil
	}
	if net.ParseIP(host) != nil {
		return address, nil
	}
	if d.allowSystemDNS {
		return address, nil
	}
	return "", fmt.Errorf("%w: %w: %s", ErrUnknownHost, ErrSystemDNSBlocked, address)
}

func NewHTTPTransport(hosts Resolver, tlsConfig *tls.Config) (*http.Transport, error) {
	return NewHTTPTransportWithOptions(DialOptions{Hosts: hosts, TLSConfig: tlsConfig})
}

func NewHTTPTransportWithOptions(opts DialOptions) (*http.Transport, error) {
	d, err := NewDialerWithOptions(opts)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSClientConfig:       d.TLSConfig(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          DefaultMaxIdleConns,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       DefaultIdleConnTimeout,
		TLSHandshakeTimeout:   DefaultHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
	}, nil
}

func NewHTTPClient(hosts Resolver, tlsConfig *tls.Config) (*http.Client, error) {
	return NewHTTPClientWithOptions(DialOptions{Hosts: hosts, TLSConfig: tlsConfig})
}

func NewHTTPClientWithOptions(opts DialOptions) (*http.Client, error) {
	tr, err := NewHTTPTransportWithOptions(opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: tr,
		Timeout:   DefaultHTTPTimeout,
	}, nil
}

func NewNetworkDialer(hosts Resolver, caBundlePEM []byte) (*Dialer, error) {
	cfg, err := TLSConfigFromCA(caBundlePEM, "")
	if err != nil {
		return nil, err
	}
	return NewDialerWithOptions(DialOptions{Hosts: hosts, TLSConfig: cfg})
}

func NewNetworkHTTPClient(hosts Resolver, caBundlePEM []byte) (*http.Client, error) {
	cfg, err := TLSConfigFromCA(caBundlePEM, "")
	if err != nil {
		return nil, err
	}
	return NewHTTPClientWithOptions(DialOptions{Hosts: hosts, TLSConfig: cfg})
}

func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	out := cfg.Clone()
	if out.MinVersion == 0 {
		out.MinVersion = tls.VersionTLS12
	}
	return out
}
