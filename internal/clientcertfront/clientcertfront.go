package clientcertfront

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsfront"
)

const (
	HeaderClientCertificate = "X-Socialweb-Client-Certificate"
	EdgeCertName            = "edge-client.crt"
	EdgeKeyName             = "edge-client.key"
	EdgeClientCN            = "socialweb-edge-client"
	EdgeClientLife          = 10 * 365 * 24 * time.Hour

	NoExpiryOrganizationalUnit = "socialweb.computer:no-expiry"

	trustTTL            = 30 * time.Second
	revokedTTL          = 2 * time.Second
	revokedStale        = 10 * time.Minute
	revokedRefresh      = 2 * time.Second
	revokedFetchTimeout = 5 * time.Second
	denyTTL             = 2 * time.Second
	readHeaderLimit     = 10 * time.Second
	shutdownTimeout     = 5 * time.Second
	upstreamDialWait    = 5 * time.Second
	lifetimeSlack       = 5 * time.Minute
)

var pathPattern = regexp.MustCompile(`^/clusters/root:([a-z0-9]+):([a-z0-9]([a-z0-9-]*[a-z0-9])?):clusters(/.*)?$`)

type Records interface {
	CertificateState(ctx context.Context, did, cluster, serial string) (kcpclient.CertificateState, error)
}

type Config struct {
	Address         string
	CertFile        string
	KeyFile         string
	EdgeDir         string
	ClusterStateDir string
	Deny            revocation.Deny
	Records         Records
	OnListen        func(net.Addr)
	Log             *slog.Logger
}

func EnsureEdgeClient(dir string) (string, string, error) {
	return tlsfront.EnsurePair(dir, EdgeCertName, EdgeKeyName, func() ([]byte, []byte, error) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		if err != nil {
			return nil, nil, err
		}
		now := time.Now()
		tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: EdgeClientCN},
			NotBefore: now.Add(-time.Minute), NotAfter: now.Add(EdgeClientLife), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
		if err != nil {
			return nil, nil, err
		}
		kder, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
	})
}

type trust struct {
	clientCA  *x509.CertPool
	front     string
	transport *http.Transport
	loaded    time.Time
}

type revokedEntry struct {
	serials map[string]struct{}
	fetched time.Time
}

type server struct {
	c          Config
	log        *slog.Logger
	edgePin    [32]byte
	mu         sync.Mutex
	trusts     map[string]*trust
	revoked    map[string]*revokedEntry
	refreshing map[string]bool
	denies     map[string]time.Time
	wake       chan struct{}
}

func Run(ctx context.Context, c Config) error {
	if c.Deny == nil {
		return errors.New("clientcertfront: no deny lookup")
	}
	if c.Records == nil {
		return errors.New("clientcertfront: no certificate records")
	}
	edgeCert, _, err := EnsureEdgeClient(c.EdgeDir)
	if err != nil {
		return fmt.Errorf("clientcertfront: the edge client certificate: %w", err)
	}
	edgeDER, err := readCertDER(edgeCert)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return fmt.Errorf("clientcertfront: the certificate %s and key %s: %w", c.CertFile, c.KeyFile, err)
	}
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &server{c: c, log: log.With("component", "client-cert-front"), edgePin: sha256.Sum256(edgeDER),
		trusts: map[string]*trust{}, revoked: map[string]*revokedEntry{}, refreshing: map[string]bool{},
		denies: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	ln, err := net.Listen("tcp", c.Address)
	if err != nil {
		return fmt.Errorf("clientcertfront: listening on %s: %w", c.Address, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: readHeaderLimit,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAnyClientCert,
			MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}}
	s.log.Info("listening", "addr", ln.Addr().String())
	if c.OnListen != nil {
		c.OnListen(ln.Addr())
	}
	go s.refreshLoop(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		srv.Shutdown(sctx)
		return nil
	}
}

func readCertDER(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("clientcertfront: %s holds no PEM certificate", path)
	}
	return block.Bytes, nil
}

func (s *server) refuse(w http.ResponseWriter, r *http.Request, status int, did, cluster, serial, reason string) {
	s.log.Warn("refused", "remote", r.RemoteAddr, "did", did, "cluster", cluster, "serial", serial, "status", status, "reason", reason)
	http.Error(w, http.StatusText(status), status)
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		s.refuse(w, r, http.StatusUnauthorized, "", "", "", "no peer certificate")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	user := peer
	header := r.Header.Get(HeaderClientCertificate)
	if sha256.Sum256(peer.Raw) == s.edgePin {
		if header == "" {
			s.refuse(w, r, http.StatusUnauthorized, "", "", "", "the edge sent no certificate")
			return
		}
		der, err := base64.StdEncoding.DecodeString(header)
		if err != nil {
			s.refuse(w, r, http.StatusBadRequest, "", "", "", "the edge certificate header is not base64")
			return
		}
		if user, err = x509.ParseCertificate(der); err != nil {
			s.refuse(w, r, http.StatusBadRequest, "", "", "", "the edge certificate header is not a certificate")
			return
		}
	} else if header != "" {
		s.refuse(w, r, http.StatusBadRequest, "", "", "", "a certificate header from a peer that is not the edge")
		return
	}
	serial := user.SerialNumber.Text(16)

	m := pathPattern.FindStringSubmatch(r.URL.Path)
	if m == nil {
		s.refuse(w, r, http.StatusNotFound, "", "", serial, "path")
		return
	}
	account, cluster, rest := m[1], m[2], m[4]
	if rest == "" {
		rest = "/"
	}

	cn := user.Subject.CommonName
	switch {
	case !strings.HasPrefix(cn, "did:"):
		s.refuse(w, r, http.StatusForbidden, cn, cluster, serial, "common name is not a DID")
		return
	case kcpclient.AccountHash(cn) != account:
		s.refuse(w, r, http.StatusForbidden, cn, cluster, serial, "another account")
		return
	case !slices.Equal(user.Subject.Organization, []string{kcpclient.OwnersGroup}):
		s.refuse(w, r, http.StatusForbidden, cn, cluster, serial, "organization")
		return
	case user.NotAfter.Sub(user.NotBefore) > gateway.MaxClientCertTTL+lifetimeSlack &&
		!slices.Contains(user.Subject.OrganizationalUnit, NoExpiryOrganizationalUnit):
		s.refuse(w, r, http.StatusForbidden, cn, cluster, serial, "lifetime")
		return
	}

	dir, err := clientcert.DataDir(s.c.ClusterStateDir, cn, cluster)
	if err != nil {
		s.refuse(w, r, http.StatusNotFound, cn, cluster, serial, "cluster key")
		return
	}
	t, err := s.trustFor(dir)
	if err != nil {
		s.refuse(w, r, http.StatusServiceUnavailable, cn, cluster, serial, "cluster not reachable: "+err.Error())
		return
	}
	if _, err := user.Verify(x509.VerifyOptions{Roots: t.clientCA, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		s.refuse(w, r, http.StatusUnauthorized, cn, cluster, serial, "chain: "+err.Error())
		return
	}
	revoked, err := s.revokedFor(r.Context(), dir)
	if err != nil {
		s.refuse(w, r, http.StatusServiceUnavailable, cn, cluster, serial, "revocation list: "+err.Error())
		return
	}
	if _, gone := revoked[clientcert.NormalizeSerial(serial)]; gone {
		s.refuse(w, r, http.StatusUnauthorized, cn, cluster, serial, "revoked in the PKI")
		return
	}
	record, err := s.c.Records.CertificateState(r.Context(), cn, cluster, serial)
	switch {
	case err != nil:
		s.refuse(w, r, http.StatusServiceUnavailable, cn, cluster, serial, "certificate records: "+err.Error())
		return
	case !record.Present:
		s.refuse(w, r, http.StatusUnauthorized, cn, cluster, serial, "no ClusterClientCertificate for this serial")
		return
	case record.Revoked:
		s.refuse(w, r, http.StatusUnauthorized, cn, cluster, serial, "ClusterClientCertificate revoked")
		return
	}
	if status, reason := s.admit(r.Context(), cn); status != 0 {
		s.refuse(w, r, status, cn, cluster, serial, reason)
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), user.NotAfter)
	defer cancel()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = t.front
			pr.Out.URL.Path = rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = t.front
			pr.Out.Header.Del("Authorization")
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Remote-") {
					delete(pr.Out.Header, k)
				}
			}
			pr.Out.Header.Del(HeaderClientCertificate)
			pr.Out.Header.Set("X-Remote-User", cn)
			pr.Out.Header.Set("X-Remote-Group", kcpclient.OwnersGroup)
		},
		FlushInterval: -1,
		Transport:     t.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.refuse(w, r, http.StatusServiceUnavailable, cn, cluster, serial, "upstream: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (s *server) admit(ctx context.Context, did string) (int, string) {
	s.mu.Lock()
	until, ok := s.denies[did]
	s.mu.Unlock()
	if ok && time.Now().Before(until) {
		return 0, ""
	}
	if _, err := revocation.Admit(ctx, s.c.Deny, did); err != nil {
		switch {
		case errors.Is(err, revocation.ErrRevoked):
			return http.StatusForbidden, "identity revoked"
		case errors.Is(err, revocation.ErrDenyRecordMissing):
			return http.StatusForbidden, "no deny record"
		}
		return http.StatusServiceUnavailable, "deny lookup: " + err.Error()
	}
	s.mu.Lock()
	s.denies[did] = time.Now().Add(denyTTL)
	s.mu.Unlock()
	return 0, ""
}

func (s *server) revokedFor(ctx context.Context, dir string) (map[string]struct{}, error) {
	s.mu.Lock()
	e := s.revoked[dir]
	s.mu.Unlock()
	if e != nil {
		age := time.Since(e.fetched)
		if age >= revokedStale {
			s.requestRefresh()
			return nil, errors.New("the revocation list has not refreshed for ten minutes")
		}
		if age >= revokedTTL {
			s.requestRefresh()
		}
		return e.serials, nil
	}
	fctx, cancel := context.WithTimeout(ctx, revokedFetchTimeout)
	defer cancel()
	serials, err := clientcert.FetchRevoked(fctx, dir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.revoked[dir] = &revokedEntry{serials: serials, fetched: time.Now()}
	s.mu.Unlock()
	return serials, nil
}

func (s *server) requestRefresh() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *server) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(revokedRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		s.refreshStale(ctx)
	}
}

func (s *server) refreshStale(ctx context.Context) {
	s.mu.Lock()
	var dirs []string
	for dir, e := range s.revoked {
		if time.Since(e.fetched) >= revokedTTL && !s.refreshing[dir] {
			s.refreshing[dir] = true
			dirs = append(dirs, dir)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, dir := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			s.refresh(ctx, dir)
		}(dir)
	}
	wg.Wait()
}

func (s *server) refresh(ctx context.Context, dir string) {
	defer func() {
		s.mu.Lock()
		delete(s.refreshing, dir)
		s.mu.Unlock()
	}()
	fctx, cancel := context.WithTimeout(ctx, revokedFetchTimeout)
	defer cancel()
	serials, err := clientcert.FetchRevoked(fctx, dir)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("the revocation list refresh failed; serving the last known list", "dir", dir, "err", err.Error())
		}
		return
	}
	s.mu.Lock()
	s.revoked[dir] = &revokedEntry{serials: serials, fetched: time.Now()}
	s.mu.Unlock()
}

type frontState struct {
	Address string `json:"address"`
}

func (s *server) trustFor(dir string) (*trust, error) {
	s.mu.Lock()
	t := s.trusts[dir]
	s.mu.Unlock()
	if t != nil && time.Since(t.loaded) < trustTTL {
		return t, nil
	}
	fresh, err := loadTrust(dir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	old := s.trusts[dir]
	s.trusts[dir] = fresh
	s.mu.Unlock()
	if old != nil {
		old.transport.CloseIdleConnections()
	}
	return fresh, nil
}

func loadTrust(dir string) (*trust, error) {
	tlsDir := filepath.Join(dir, "server", "tls")
	clientCA, err := poolOf(filepath.Join(tlsDir, "client-ca.crt"))
	if err != nil {
		return nil, err
	}
	serverCA, err := poolOf(filepath.Join(tlsDir, "server-ca.crt"))
	if err != nil {
		return nil, err
	}
	proxy, err := tls.LoadX509KeyPair(filepath.Join(tlsDir, "client-auth-proxy.crt"), filepath.Join(tlsDir, "client-auth-proxy.key"))
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "front.json"))
	if err != nil {
		return nil, err
	}
	var st frontState
	if err := json.Unmarshal(raw, &st); err != nil || st.Address == "" {
		return nil, errors.New("front.json names no address")
	}
	if _, _, err := net.SplitHostPort(st.Address); err != nil {
		return nil, err
	}
	return &trust{clientCA: clientCA, front: st.Address, loaded: time.Now(), transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: serverCA, Certificates: []tls.Certificate{proxy}, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
		DialContext:       (&net.Dialer{Timeout: upstreamDialWait}).DialContext,
	}}, nil
}

func poolOf(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("%s holds no PEM certificate", filepath.Base(path))
	}
	return pool, nil
}
