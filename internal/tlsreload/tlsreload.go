package tlsreload

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

type stamp struct {
	mod  time.Time
	size int64
	ok   bool
}

type Reloader struct {
	certFile string
	keyFiles []string
	watch    []string
	log      *slog.Logger

	mu     sync.Mutex
	cert   *tls.Certificate
	stamps []stamp
}

func New(certFile string, keyFiles []string, log *slog.Logger) (*Reloader, error) {
	if certFile == "" {
		return nil, errors.New("tlsreload: no certificate file")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Reloader{certFile: certFile, keyFiles: append([]string(nil), keyFiles...), log: log}
	r.watch = r.watchPaths()
	r.stamps = r.stampsNow()
	cert, err := loadPair(certFile, r.keyFiles)
	if err != nil {
		return nil, err
	}
	r.cert = cert
	return r, nil
}

func (r *Reloader) TLSConfig(base *tls.Config) *tls.Config {
	if base == nil {
		base = &tls.Config{}
	}
	cfg := base.Clone()
	cfg.Certificates = nil
	cfg.GetCertificate = r.GetCertificate
	return cfg
}

func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stamps := r.stampsNow(); !sameStamps(stamps, r.stamps) {
		cert, err := loadPair(r.certFile, r.keyFiles)
		if err != nil {
			r.log.Warn("keeping the last certificate", "cert", r.certFile, "err", err)
		} else {
			r.cert = cert
			r.log.Info("reloaded the certificate", "cert", r.certFile)
		}
		r.stamps = stamps
	}
	if r.cert == nil {
		return nil, fmt.Errorf("tlsreload: no certificate loaded from %s", r.certFile)
	}
	return r.cert, nil
}

func (r *Reloader) watchPaths() []string {
	out := []string{r.certFile}
	for _, path := range r.keyFiles {
		if path == "" || path == r.certFile {
			continue
		}
		dup := slices.Contains(out, path)
		if !dup {
			out = append(out, path)
		}
	}
	return out
}

func (r *Reloader) stampsNow() []stamp {
	out := make([]stamp, len(r.watch))
	for i, path := range r.watch {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		out[i] = stamp{mod: st.ModTime(), size: st.Size(), ok: true}
	}
	return out
}

func sameStamps(a, b []stamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ok != b[i].ok || a[i].size != b[i].size || !a[i].mod.Equal(b[i].mod) {
			return false
		}
	}
	return true
}

func loadPair(certFile string, keyFiles []string) (*tls.Certificate, error) {
	var firstErr error
	for _, keyFile := range keyFiles {
		if keyFile == "" {
			continue
		}
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err == nil {
			return &pair, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = errors.New("no key file")
	}
	return nil, fmt.Errorf("tlsreload: the certificate %s: %w", certFile, firstErr)
}
