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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
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
	revokedFetchTimeout = 5 * time.Second
	denyTTL             = 2 * time.Second
	gateFetchTimeout    = 5 * time.Second
	gateIdle            = 10 * time.Minute
	gateRetry           = time.Second
	gateRetryMax        = 30 * time.Second
	readHeaderLimit     = 10 * time.Second
	shutdownTimeout     = 5 * time.Second
	upstreamDialWait    = 5 * time.Second
	lifetimeSlack       = 5 * time.Minute
)

var pathPattern = regexp.MustCompile(`^/clusters/root:([a-z0-9]+):([a-z0-9]([a-z0-9-]*[a-z0-9])?):clusters(/.*)?$`)

type Records interface {
	CertificateState(ctx context.Context, did, cluster, serial string) (kcpclient.CertificateState, error)
	FollowClusterGate(ctx context.Context, home, name string, on func(kcpclient.ClusterGate)) error
}

type Config struct {
	Address         string
	CertFile        string
	KeyFile         string
	EdgeDir         string
	ClusterStateDir string
	Deny            revocation.Deny
	Records         Records
	Logins          *openbao.Logins
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
	bearer    *http.Transport
}

type openRequest struct{ cancel context.CancelFunc }

type revokedEntry struct {
	serials map[string]struct{}
	fetched time.Time
}

type revokedFlight struct {
	done    chan struct{}
	serials map[string]struct{}
	err     error
}

type gateWatch struct {
	ready  chan struct{}
	once   sync.Once
	cancel context.CancelFunc
	idle   *time.Timer
	used   time.Time
	gate   kcpclient.ClusterGate
	synced bool
	seen   bool
	err    error
}

type server struct {
	c          Config
	log        *slog.Logger
	edgePin    [32]byte
	ctx        context.Context
	tasks      *supervise.Group
	mu         sync.Mutex
	trusts     *ttlcache.Cache[string, *trust]
	revoked    map[string]*revokedEntry
	refreshing map[string]*revokedFlight
	admitted   *ttlcache.Cache[string, struct{}]
	gates      map[string]*gateWatch
	open       map[string]map[*openRequest]struct{}
}

func Run(ctx context.Context, c Config) error {
	if c.Deny == nil {
		return errors.New("clientcertfront: no deny lookup")
	}
	if c.Records == nil {
		return errors.New("clientcertfront: no certificate records")
	}
	if c.Logins == nil {
		return errors.New("clientcertfront: no OpenBao logins")
	}
	edgeCert, _, err := EnsureEdgeClient(c.EdgeDir)
	if err != nil {
		return fmt.Errorf("clientcertfront: the edge client certificate: %w", err)
	}
	edgeDER, err := readCertDER(edgeCert)
	if err != nil {
		return err
	}
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "client-cert-front")
	reloader, err := tlsreload.New(c.CertFile, []string{c.KeyFile}, log)
	if err != nil {
		return fmt.Errorf("clientcertfront: the certificate %s and key %s: %w", c.CertFile, c.KeyFile, err)
	}
	tasks := supervise.New(ctx)
	defer func() { _ = tasks.Stop() }()
	s := &server{c: c, log: log, edgePin: sha256.Sum256(edgeDER), ctx: tasks.Context(), tasks: tasks,
		trusts: ttlcache.New[string, *trust](ttlcache.Options{TTL: trustTTL}).OnEvict(func(_ string, t *trust) {
			t.transport.CloseIdleConnections()
			t.bearer.CloseIdleConnections()
		}),
		revoked: map[string]*revokedEntry{}, refreshing: map[string]*revokedFlight{},
		admitted: ttlcache.New[string, struct{}](ttlcache.Options{TTL: denyTTL}),
		gates:    map[string]*gateWatch{}, open: map[string]map[*openRequest]struct{}{}}
	defer s.closeGates()
	ln, err := net.Listen("tcp", c.Address)
	if err != nil {
		return fmt.Errorf("clientcertfront: listening on %s: %w", c.Address, err)
	}
	tlsConfig := reloader.TLSConfig(&tls.Config{ClientAuth: tls.RequestClientCert,
		MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	srv := &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: readHeaderLimit,
		TLSConfig: tlsConfig}
	s.log.Info("listening", "addr", ln.Addr().String())
	if c.OnListen != nil {
		c.OnListen(ln.Addr())
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		srv.Shutdown(sctx)
		<-done
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

func (s *server) caller(r *http.Request) (*x509.Certificate, int, string) {
	header := r.Header.Get(HeaderClientCertificate)
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		if header != "" {
			return nil, http.StatusBadRequest, "a certificate header from a peer that is not the edge"
		}
		return nil, 0, ""
	}
	peer := r.TLS.PeerCertificates[0]
	if sha256.Sum256(peer.Raw) != s.edgePin {
		if header != "" {
			return nil, http.StatusBadRequest, "a certificate header from a peer that is not the edge"
		}
		return peer, 0, ""
	}
	if header == "" {
		return nil, 0, ""
	}
	der, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, http.StatusBadRequest, "the edge certificate header is not base64"
	}
	user, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, http.StatusBadRequest, "the edge certificate header is not a certificate"
	}
	return user, 0, ""
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	user, status, reason := s.caller(r)
	if status != 0 {
		s.refuse(w, r, status, "", "", "", reason)
		return
	}
	serial := ""
	if user != nil {
		serial = user.SerialNumber.Text(16)
	}
	m := pathPattern.FindStringSubmatch(r.URL.Path)
	if m == nil {
		s.refuse(w, r, http.StatusNotFound, "", "", serial, "path")
		return
	}
	account, cluster, rest := m[1], m[2], m[4]
	if rest == "" {
		rest = "/"
	}
	home := kcpclient.RootWorkspace + ":" + account + ":" + cluster
	if user == nil {
		s.serveBearer(w, r, account, home, cluster, rest)
		return
	}
	s.serveCertificate(w, r, user, serial, account, home, cluster, rest)
}

func (s *server) serveBearer(w http.ResponseWriter, r *http.Request, account, home, cluster, rest string) {
	if scheme, _, ok := strings.Cut(r.Header.Get("Authorization"), " "); !ok || !strings.EqualFold(scheme, "Bearer") {
		s.refuse(w, r, http.StatusUnauthorized, "", cluster, "", "neither a client certificate nor a bearer token")
		return
	}
	did, status, reason := s.admitGate(r.Context(), account, home, cluster)
	if status != 0 {
		s.refuse(w, r, status, did, cluster, "", reason)
		return
	}
	key, err := kcpclient.ClusterKey(home)
	if err != nil {
		s.refuse(w, r, http.StatusNotFound, did, cluster, "", "cluster key")
		return
	}
	dir, err := clusterprovider.ClusterDataDir(s.c.ClusterStateDir, key)
	if err != nil {
		s.refuse(w, r, http.StatusNotFound, did, cluster, "", "cluster key")
		return
	}
	t, err := s.trustFor(r.Context(), dir)
	if err != nil {
		s.refuse(w, r, http.StatusServiceUnavailable, did, cluster, "", "cluster not reachable: "+err.Error())
		return
	}
	if status, reason := s.admit(r.Context(), did); status != 0 {
		s.refuse(w, r, status, did, cluster, "", reason)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer s.hold(home, cancel)()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rewrite(pr, t.front, rest)
		},
		FlushInterval: -1,
		Transport:     t.bearer,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.refuse(w, r, http.StatusServiceUnavailable, did, cluster, "", "upstream: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (s *server) serveCertificate(w http.ResponseWriter, r *http.Request, user *x509.Certificate, serial, account, home, cluster, rest string) {
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
	t, err := s.trustFor(r.Context(), dir)
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
	did, status, reason := s.admitGate(r.Context(), account, home, cluster)
	switch {
	case status != 0:
		s.refuse(w, r, status, cn, cluster, serial, reason)
		return
	case did != cn:
		s.refuse(w, r, http.StatusForbidden, cn, cluster, serial, "the Cluster belongs to another account")
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), user.NotAfter)
	defer cancel()
	defer s.hold(home, cancel)()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rewrite(pr, t.front, rest)
			pr.Out.Header.Del("Authorization")
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

func rewrite(pr *httputil.ProxyRequest, front, rest string) {
	pr.Out.URL.Scheme = "https"
	pr.Out.URL.Host = front
	pr.Out.URL.Path = rest
	pr.Out.URL.RawPath = ""
	pr.Out.Host = front
	for k := range pr.Out.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Remote-") {
			delete(pr.Out.Header, k)
		}
	}
	pr.Out.Header.Del(HeaderClientCertificate)
}

func (s *server) admitGate(ctx context.Context, account, home, cluster string) (string, int, string) {
	g, err := s.gateFor(ctx, home, cluster)
	switch {
	case err != nil:
		return "", http.StatusServiceUnavailable, "cluster state: " + err.Error()
	case g.Account == "" || kcpclient.AccountHash(g.Account) != account:
		return "", http.StatusNotFound, "no such cluster in this account"
	case !g.Serving:
		return g.Account, http.StatusServiceUnavailable, "the cluster is not serving"
	}
	return g.Account, 0, ""
}

func (s *server) gateFor(ctx context.Context, home, cluster string) (kcpclient.ClusterGate, error) {
	w := s.gateWatcher(home, cluster)
	deadline := time.NewTimer(gateFetchTimeout)
	defer deadline.Stop()
	select {
	case <-w.ready:
	case <-deadline.C:
	case <-ctx.Done():
		return kcpclient.ClusterGate{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case w.synced:
		return w.gate, nil
	case w.err != nil:
		return kcpclient.ClusterGate{}, w.err
	}
	return kcpclient.ClusterGate{}, errors.New("the Cluster watch has not synced")
}

func (s *server) gateWatcher(home, cluster string) *gateWatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.gates[home]; w != nil {
		w.used = time.Now()
		w.idle.Reset(gateIdle)
		return w
	}
	ctx, cancel := context.WithCancel(s.ctx)
	w := &gateWatch{ready: make(chan struct{}), cancel: cancel, used: time.Now()}
	w.idle = time.AfterFunc(gateIdle, func() { s.expireGate(home, w) })
	s.gates[home] = w
	s.tasks.Go("clientcertfront: gate "+home, func(context.Context) error {
		s.followGate(ctx, w, home, cluster)
		return nil
	})
	return w
}

func (s *server) expireGate(home string, w *gateWatch) {
	s.mu.Lock()
	if s.gates[home] != w || len(s.open[home]) > 0 || time.Since(w.used) < gateIdle {
		s.mu.Unlock()
		return
	}
	delete(s.gates, home)
	s.mu.Unlock()
	w.cancel()
}

func (s *server) closeGates() {
	s.mu.Lock()
	gates := s.gates
	s.gates = map[string]*gateWatch{}
	s.mu.Unlock()
	for _, w := range gates {
		w.idle.Stop()
		w.cancel()
	}
}

func (s *server) followGate(ctx context.Context, w *gateWatch, home, cluster string) {
	delay := gateRetry
	for {
		began := time.Now()
		err := s.c.Records.FollowClusterGate(ctx, home, cluster, func(g kcpclient.ClusterGate) { s.setGate(home, w, g) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the Cluster watch ended")
		}
		if s.failGate(home, w, err) {
			return
		}
		if time.Since(began) > gateRetryMax {
			delay = gateRetry
		}
		s.log.Warn("the Cluster watch restarts; new requests are refused until it syncs again, open requests stay", "home", home, "after", delay.String(), "err", err.Error())
		retry := time.NewTimer(delay)
		delay = min(2*delay, gateRetryMax)
		select {
		case <-ctx.Done():
			retry.Stop()
			return
		case <-retry.C:
		}
	}
}

func (s *server) setGate(home string, w *gateWatch, g kcpclient.ClusterGate) {
	s.mu.Lock()
	w.gate, w.synced, w.seen, w.err = g, true, true, nil
	var cut []*openRequest
	if !g.Serving && s.gates[home] == w {
		for o := range s.open[home] {
			cut = append(cut, o)
		}
	}
	s.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
	for _, o := range cut {
		o.cancel()
	}
	if len(cut) > 0 {
		s.log.Info("the cluster stopped serving; its open requests are closed", "home", home, "closed", len(cut))
	}
}

func (s *server) failGate(home string, w *gateWatch, err error) bool {
	s.mu.Lock()
	w.synced, w.err = false, err
	never := !w.seen
	if never && s.gates[home] == w {
		delete(s.gates, home)
		w.idle.Stop()
	}
	s.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
	if never {
		w.cancel()
	}
	return never
}

func (s *server) hold(home string, cancel context.CancelFunc) func() {
	o := &openRequest{cancel: cancel}
	s.mu.Lock()
	if s.open[home] == nil {
		s.open[home] = map[*openRequest]struct{}{}
	}
	s.open[home][o] = struct{}{}
	w := s.gates[home]
	stopped := w != nil && w.synced && !w.gate.Serving
	s.mu.Unlock()
	if stopped {
		cancel()
	}
	return func() {
		s.mu.Lock()
		delete(s.open[home], o)
		if len(s.open[home]) == 0 {
			delete(s.open, home)
			if w := s.gates[home]; w != nil {
				w.used = time.Now()
				w.idle.Reset(gateIdle)
			}
		}
		s.mu.Unlock()
	}
}

func (s *server) admit(ctx context.Context, did string) (int, string) {
	if _, ok := s.admitted.Get(did); ok {
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
	s.admitted.Set(did, struct{}{})
	return 0, ""
}

func (s *server) revokedFor(ctx context.Context, dir string) (map[string]struct{}, error) {
	s.mu.Lock()
	e := s.revoked[dir]
	if e != nil && time.Since(e.fetched) < revokedTTL {
		s.mu.Unlock()
		return e.serials, nil
	}
	f := s.refreshing[dir]
	if f == nil {
		f = &revokedFlight{done: make(chan struct{})}
		s.refreshing[dir] = f
		s.tasks.Go("clientcertfront: revocations "+dir, func(context.Context) error {
			s.refresh(dir, f)
			return nil
		})
	}
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch {
	case f.err == nil:
		return f.serials, nil
	case e != nil && time.Since(e.fetched) < revokedStale:
		s.log.Warn("the revocation list refresh failed; serving the last known list", "dir", dir, "err", f.err.Error())
		return e.serials, nil
	case e != nil:
		return nil, fmt.Errorf("the revocation list has not refreshed for ten minutes: %w", f.err)
	}
	return nil, f.err
}

func (s *server) refresh(dir string, f *revokedFlight) {
	ctx, cancel := context.WithTimeout(s.ctx, revokedFetchTimeout)
	defer cancel()
	serials, err := clientcert.FetchRevoked(ctx, s.c.Logins, dir)
	s.mu.Lock()
	f.serials, f.err = serials, err
	if err == nil {
		s.revoked[dir] = &revokedEntry{serials: serials, fetched: time.Now()}
	}
	delete(s.refreshing, dir)
	s.mu.Unlock()
	close(f.done)
}

type frontState struct {
	Address string `json:"address"`
}

func (s *server) trustFor(ctx context.Context, dir string) (*trust, error) {
	return s.trusts.GetOrLoad(ctx, dir, func(context.Context) (*trust, time.Duration, error) {
		t, err := loadTrust(dir)
		return t, 0, err
	})
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
	return &trust{clientCA: clientCA, front: st.Address, transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: serverCA, Certificates: []tls.Certificate{proxy}, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
		DialContext:       (&net.Dialer{Timeout: upstreamDialWait}).DialContext,
	}, bearer: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: serverCA, MinVersion: tls.VersionTLS12},
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
