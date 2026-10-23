package tlsreload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

var ErrNoServerName = errors.New("tlsreload: the connection names no server to verify")

func TrustPool(ctx context.Context, trust vault.Trust, log *slog.Logger) (*Pool, func()) {
	p := &Pool{log: logOrDiscard(log)}
	return p, p.Follow(ctx, trust)
}

func EmptyPool(log *slog.Logger) *Pool {
	return &Pool{log: logOrDiscard(log)}
}

func SecretPool(read func(ctx context.Context) ([]byte, error), renew time.Duration, log *slog.Logger) *Pool {
	return NewPool(func(ctx context.Context) (*x509.CertPool, time.Time, error) {
		data, err := read(ctx)
		if err != nil {
			return nil, time.Time{}, err
		}
		pool, err := parsePool(data)
		if err != nil {
			return nil, time.Time{}, err
		}
		return pool, time.Now().Add(renew), nil
	}, log)
}

func (p *Pool) Follow(ctx context.Context, trust vault.Trust) func() {
	if trust == nil {
		return func() {}
	}
	cancel := trust.Watch(func(bundle []byte) { p.feed(bundle) })
	if bundle, err := trust.CABundle(ctx); err != nil {
		p.log.Warn("no CA bundle is published yet; the pool fills when it is", "err", err)
	} else {
		p.feed(bundle)
	}
	return cancel
}

func (p *Pool) feed(bundle []byte) {
	if len(bundle) == 0 {
		return
	}
	if err := p.Set(bundle); err != nil {
		p.log.Warn("keeping the last CA pool", "err", err)
		return
	}
	p.log.Info("loaded the published CA pool")
}

func (p *Pool) ClientConfig(base *tls.Config) *tls.Config {
	return p.clientConfig(base, false)
}

func (p *Pool) PublicClientConfig(base *tls.Config) *tls.Config {
	return p.clientConfig(base, true)
}

func (p *Pool) clientConfig(base *tls.Config, system bool) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if base != nil {
		cfg = base.Clone()
	}
	cfg.RootCAs = nil
	cfg.InsecureSkipVerify = true
	inner := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if err := p.verify(cs, system); err != nil {
			return err
		}
		if inner != nil {
			return inner(cs)
		}
		return nil
	}
	return cfg
}

func (p *Pool) verify(cs tls.ConnectionState, system bool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tlsreload: the peer sent no certificate")
	}
	if cs.ServerName == "" {
		return ErrNoServerName
	}
	intermediates := x509.NewCertPool()
	for _, cert := range cs.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: intermediates}
	if system {
		if _, err := cs.PeerCertificates[0].Verify(opts); err == nil {
			return nil
		}
	}
	roots, err := p.Current(context.Background())
	if err != nil {
		return err
	}
	opts.Roots = roots
	_, err = cs.PeerCertificates[0].Verify(opts)
	return err
}

type Pools struct {
	mu sync.Mutex

	entries map[string]poolEntry
}

type poolEntry struct {
	fingerprint string

	pool *Pool

	stop func()
}

func (ps *Pools) Get(name, fingerprint string, open func() (*Pool, func(), error)) (*Pool, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if e, ok := ps.entries[name]; ok && e.fingerprint == fingerprint {
		return e.pool, nil
	}
	pool, stop, err := open()
	if err != nil {
		return nil, err
	}
	if ps.entries == nil {
		ps.entries = map[string]poolEntry{}
	}
	if e, ok := ps.entries[name]; ok && e.stop != nil {
		e.stop()
	}
	ps.entries[name] = poolEntry{fingerprint: fingerprint, pool: pool, stop: stop}
	return pool, nil
}

func (ps *Pools) Drop(name string) {
	ps.mu.Lock()
	e, ok := ps.entries[name]
	delete(ps.entries, name)
	ps.mu.Unlock()
	if ok && e.stop != nil {
		e.stop()
	}
}
