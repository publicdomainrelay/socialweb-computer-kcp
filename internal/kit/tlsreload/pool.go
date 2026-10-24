package tlsreload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

const LoadWait = 10 * time.Second

type Pool struct {
	log *slog.Logger

	mu     sync.Mutex
	source *lease.Source[*x509.CertPool]
	path   string
	stamp  stamp
	pool   *x509.CertPool
	loaded chan struct{}
}

func NewPool(fetch lease.Fetch[*x509.CertPool], log *slog.Logger) *Pool {
	p := &Pool{log: logOrDiscard(log)}
	if fetch != nil {
		p.source = lease.New(fetch)
	}
	return p
}

func PEMPool(data []byte, log *slog.Logger) (*Pool, error) {
	pool, err := parsePool(data)
	if err != nil {
		return nil, err
	}
	return &Pool{log: logOrDiscard(log), pool: pool}, nil
}

func FilePool(path string, log *slog.Logger) (*Pool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("tlsreload: no CA bundle file")
	}
	st := statFile(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsreload: reading the CA bundle %s: %w", path, err)
	}
	pool, err := parsePool(data)
	if err != nil {
		return nil, fmt.Errorf("tlsreload: the CA bundle %s: %w", path, err)
	}
	return &Pool{log: logOrDiscard(log), path: path, stamp: st, pool: pool}, nil
}

func PoolFromBundle(value string, log *slog.Logger) (*Pool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("tlsreload: the CA bundle is empty")
	}
	if strings.Contains(value, "-----BEGIN") {
		return PEMPool([]byte(value), log)
	}
	return FilePool(strings.TrimPrefix(value, "file:"), log)
}

func (p *Pool) Set(data []byte) error {
	pool, err := parsePool(data)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.pool = pool
	p.source = nil
	p.path = ""
	p.stamp = stamp{}
	loaded := p.loadedLocked()
	select {
	case <-loaded:
	default:
		close(loaded)
	}
	p.mu.Unlock()
	return nil
}

func (p *Pool) loadedLocked() chan struct{} {
	if p.loaded == nil {
		p.loaded = make(chan struct{})
	}
	return p.loaded
}

func (p *Pool) Current(ctx context.Context) (*x509.CertPool, error) {
	p.mu.Lock()
	source := p.source
	if source == nil && p.path != "" {
		p.refreshLocked()
	}
	pool := p.pool
	var loaded chan struct{}
	if source == nil && pool == nil {
		loaded = p.loadedLocked()
	}
	p.mu.Unlock()
	if source != nil {
		fetched, err := source.Get(ctx)
		if err != nil {
			return nil, fmt.Errorf("tlsreload: fetching the CA pool: %w", err)
		}
		pool = fetched
	}
	if loaded != nil {
		pool = p.awaitLoaded(ctx, loaded)
	}
	if pool == nil {
		return nil, errors.New("tlsreload: no CA pool is loaded")
	}
	return pool, nil
}

func (p *Pool) awaitLoaded(ctx context.Context, loaded <-chan struct{}) *x509.CertPool {
	timer := time.NewTimer(LoadWait)
	defer timer.Stop()
	select {
	case <-loaded:
	case <-ctx.Done():
	case <-timer.C:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pool
}

func (p *Pool) ServerConfig(base *tls.Config, auth tls.ClientAuthType) *tls.Config {
	if base == nil {
		base = &tls.Config{}
	}
	cfg := base.Clone()
	cfg.ClientAuth = auth
	inner := base.GetConfigForClient
	plain := base.Clone()
	plain.GetConfigForClient = nil
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		next := plain
		if inner != nil {
			got, err := inner(hello)
			if err != nil {
				return nil, err
			}
			if got != nil {
				next = got
			}
		}
		pool, err := p.Current(hello.Context())
		if err != nil {
			return nil, err
		}
		out := next.Clone()
		out.GetConfigForClient = nil
		out.ClientCAs = pool
		out.ClientAuth = auth
		return out, nil
	}
	return cfg
}

func (p *Pool) DialTLSContext(base *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &tls.Config{}
	}
	base = base.Clone()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		roots, err := p.Current(ctx)
		if err != nil {
			return nil, err
		}
		cfg := base.Clone()
		cfg.RootCAs = roots
		if cfg.ServerName == "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			cfg.ServerName = host
		}
		if len(cfg.NextProtos) == 0 {
			cfg.NextProtos = []string{"http/1.1"}
		}
		dialer := &tls.Dialer{Config: cfg}
		return dialer.DialContext(ctx, network, addr)
	}
}

func (p *Pool) refreshLocked() {
	st := statFile(p.path)
	if sameStamps([]stamp{st}, []stamp{p.stamp}) {
		return
	}
	data, err := os.ReadFile(p.path)
	var pool *x509.CertPool
	if err == nil {
		pool, err = parsePool(data)
	}
	if err != nil {
		p.log.Warn("keeping the last CA pool", "path", p.path, "err", err)
	} else {
		p.pool = pool
		p.log.Info("reloaded the CA pool", "path", p.path)
	}
	p.stamp = st
}

func statFile(path string) stamp {
	st, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{mod: st.ModTime(), size: st.Size(), ok: true}
}

func parsePool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if pool.AppendCertsFromPEM(data) {
		return pool, nil
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("tlsreload: the CA bundle holds no certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("tlsreload: parsing the CA certificate: %w", err)
	}
	return nil, errors.New("tlsreload: the CA bundle holds no certificate")
}

func logOrDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return log
}
