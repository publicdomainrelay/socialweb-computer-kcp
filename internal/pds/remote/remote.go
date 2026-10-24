package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/nettransport"
)

const (
	DefaultTimeout = 30 * time.Second

	dialTimeout = 30 * time.Second

	keepAlive = 30 * time.Second

	handshakeTimeout = 10 * time.Second

	idleTimeout = 90 * time.Second

	maxIdleConns = 32
)

type Secrets func(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error)

type Config struct {
	URL string

	DID string

	TLS *tls.Config

	Auth xrpc.Auth

	Secrets Secrets

	Timeout time.Duration
}

type Host struct {
	address string

	did syntax.DID

	tls *tls.Config

	http *http.Client

	auth xrpc.Auth

	secrets Secrets

	timeout time.Duration

	anon *pdsclient.RemotePDS

	mu sync.Mutex

	sessions map[string]*pdsclient.RemotePDS
}

var _ pds.Transport = (*Host)(nil)

func New(cfg Config) (*Host, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	var did syntax.DID
	if raw := strings.TrimSpace(cfg.DID); raw != "" {
		parsed, err := syntax.ParseDID(raw)
		if err != nil {
			return nil, fmt.Errorf("remote: the PDS did %q: %w", raw, err)
		}
		did = parsed
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var tlsConfig *tls.Config
	if cfg.TLS != nil {
		tlsConfig = cfg.TLS.Clone()
	}
	h := &Host{address: address, did: did, tls: tlsConfig, http: httpClient(tlsConfig), auth: cfg.Auth, secrets: cfg.Secrets,
		timeout: timeout, sessions: map[string]*pdsclient.RemotePDS{}}
	anon, err := h.client(nil)
	if err != nil {
		return nil, err
	}
	h.anon = anon
	return h, nil
}

func httpClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{
		Transport: nettransport.WithDial(&http.Transport{
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          maxIdleConns,
			IdleConnTimeout:       idleTimeout,
			TLSHandshakeTimeout:   handshakeTimeout,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       tlsConfig,
		}, (&net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}).DialContext),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (h *Host) client(auth xrpc.Auth) (*pdsclient.RemotePDS, error) {
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: h.address, DID: h.did, HTTPClient: h.http, TLS: h.tls, Auth: auth,
		Timeout: h.timeout})
}

func (h *Host) Address() string { return h.address }

func (h *Host) Client() *http.Client { return h.http }

func (h *Host) TLS() *tls.Config { return h.tls }

func (h *Host) Describe(ctx context.Context) (pds.Info, error) {
	info, err := h.anon.Describe(ctx)
	if err != nil {
		return pds.Info{}, err
	}
	return pds.Info{DID: info.DID.String(), AvailableUserDomains: info.AvailableUserDomains, Version: info.Version, URL: h.address}, nil
}

func (h *Host) Repo(did string) (*reporemote.Repo, error) {
	auth := h.auth
	if auth == nil {
		if client, ok := h.held(did); ok {
			held, err := client.Credential()
			if err != nil {
				return nil, err
			}
			auth = held
		}
	}
	if auth == nil {
		return nil, fmt.Errorf("%w: the RemotePDS holds no credentials for %s", pdsclient.ErrAuthenticationRequired, did)
	}
	return reporemote.New(reporemote.Config{DID: did, Transport: h, Auth: auth, Timeout: h.timeout})
}

func (h *Host) Accounts(scope []string) *Accounts {
	return &Accounts{host: h, scope: scope}
}

func (h *Host) Sessions(scope []string) *Sessions {
	return &Sessions{host: h, scope: scope}
}

func (h *Host) Sync() *Sync {
	return &Sync{host: h}
}

func (h *Host) password(ctx context.Context, ref commonv1.SecretRef) (string, error) {
	if h.secrets == nil {
		return "", errors.New("remote: no secret resolver is configured")
	}
	data, err := h.secrets(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("remote: reading the credentials %s/%s: %w", ref.Mount, ref.Path, err)
	}
	keys := []string{"password", "appPassword", "value"}
	if ref.Key != "" {
		keys = []string{ref.Key}
	}
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", fmt.Errorf("remote: the credentials %s/%s hold no password", ref.Mount, ref.Path)
}

func (h *Host) hold(session pdsclient.Session, client *pdsclient.RemotePDS) {
	h.mu.Lock()
	h.sessions[session.DID.String()] = client
	h.mu.Unlock()
}

func (h *Host) held(did string) (*pdsclient.RemotePDS, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	client, ok := h.sessions[did]
	return client, ok
}

func (h *Host) drop(did string) {
	h.mu.Lock()
	delete(h.sessions, did)
	h.mu.Unlock()
}

func inScope(scope []string, did string) error {
	if len(scope) == 0 {
		return nil
	}
	for _, entry := range scope {
		entry = strings.TrimSpace(entry)
		if entry == "*" || entry == did {
			return nil
		}
	}
	return fmt.Errorf("remote: the account %s is outside the granted scope %v", did, scope)
}

type Accounts struct {
	host *Host

	scope []string
}

var _ pds.Accounts = (*Accounts)(nil)

func (a *Accounts) Describe(ctx context.Context) (pds.Info, error) { return a.host.Describe(ctx) }

func (a *Accounts) CreateAccount(ctx context.Context, req pds.AccountRequest) (pds.Account, error) {
	password, err := a.host.password(ctx, req.Credentials)
	if err != nil {
		return pds.Account{}, err
	}
	client, err := a.host.client(nil)
	if err != nil {
		return pds.Account{}, err
	}
	session, err := client.CreateAccount(ctx, pdsclient.CreateAccountRequest{Identifier: req.Handle, Password: password, Email: req.Email})
	if err != nil {
		return pds.Account{}, err
	}
	if err := inScope(a.scope, session.DID.String()); err != nil {
		return pds.Account{}, err
	}
	a.host.hold(session, client)
	return pds.Account{DID: session.DID.String(), Handle: session.Handle, Email: session.Email, Active: session.Active}, nil
}

func (a *Accounts) Account(ctx context.Context, did string) (pds.Account, error) {
	if err := inScope(a.scope, did); err != nil {
		return pds.Account{}, err
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return pds.Account{}, fmt.Errorf("remote: the account did %q: %w", did, err)
	}
	if _, _, err := a.host.anon.GetLatestCommit(ctx, parsed); err != nil {
		var failure *pdsclient.XRPCError
		if errors.As(err, &failure) && (failure.StatusCode == http.StatusNotFound || failure.Code == "RepoNotFound") {
			return pds.Account{}, fmt.Errorf("%w: %v", pds.ErrNotFound, err)
		}
		return pds.Account{}, err
	}
	return pds.Account{DID: did, Active: true}, nil
}

type Sessions struct {
	host *Host

	scope []string
}

var _ pds.Sessions = (*Sessions)(nil)

func (s *Sessions) SignIn(ctx context.Context, req pds.SessionRequest) (pds.Session, error) {
	password, err := s.host.password(ctx, req.Credentials)
	if err != nil {
		return pds.Session{}, err
	}
	client, err := s.host.client(nil)
	if err != nil {
		return pds.Session{}, err
	}
	session, err := client.CreateSession(ctx, req.Identifier, password)
	if err != nil {
		return pds.Session{}, err
	}
	if err := inScope(s.scope, session.DID.String()); err != nil {
		return pds.Session{}, err
	}
	s.host.hold(session, client)
	return pds.Session{DID: session.DID.String(), Handle: session.Handle, Active: session.Active}, nil
}

func (s *Sessions) Session(ctx context.Context, did string) (pds.Session, error) {
	if err := inScope(s.scope, did); err != nil {
		return pds.Session{}, err
	}
	client, ok := s.host.held(did)
	if !ok {
		return pds.Session{}, fmt.Errorf("%w: no session is held for %s", pds.ErrNotFound, did)
	}
	session, err := client.GetSession(ctx)
	if err != nil {
		return pds.Session{}, err
	}
	return pds.Session{DID: session.DID.String(), Handle: session.Handle, Active: session.Active}, nil
}

func (s *Sessions) SignOut(ctx context.Context, did string) error {
	if err := inScope(s.scope, did); err != nil {
		return err
	}
	client, ok := s.host.held(did)
	if !ok {
		return nil
	}
	defer s.host.drop(did)
	return client.DeleteSession(ctx)
}

type Sync struct {
	host *Host
}

var _ pds.Sync = (*Sync)(nil)

func (s *Sync) LatestCommit(ctx context.Context, did string) (pds.Commit, error) {
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return pds.Commit{}, fmt.Errorf("remote: the repo did %q: %w", did, err)
	}
	head, rev, err := s.host.anon.GetLatestCommit(ctx, parsed)
	if err != nil {
		return pds.Commit{}, err
	}
	return pds.Commit{CID: head.String(), Rev: rev.String()}, nil
}

func (s *Sync) GetRepo(ctx context.Context, did, since string) (io.ReadCloser, error) {
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return nil, fmt.Errorf("remote: the repo did %q: %w", did, err)
	}
	var sinceTID *syntax.TID
	if since != "" {
		tid, err := syntax.ParseTID(since)
		if err != nil {
			return nil, fmt.Errorf("remote: the since rev %q: %w", since, err)
		}
		sinceTID = &tid
	}
	return s.host.anon.GetRepo(ctx, parsed, sinceTID)
}

func (s *Sync) SubscribeRepos(ctx context.Context, cursor *int64, fn func(pds.Event) error) error {
	stream, err := s.host.anon.SubscribeRepos(ctx, cursor)
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		ev, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		out := pds.Event{Seq: ev.Seq, Repo: ev.Repo.String(), Commit: ev.Commit.String(), Rev: ev.Rev.String(), Time: ev.Time, TooBig: ev.TooBig}
		if ev.Since != nil {
			out.Since = ev.Since.String()
		}
		for _, op := range ev.Ops {
			next := pds.Op{Action: op.Action, Path: op.Path}
			if op.CID != nil {
				next.CID = op.CID.String()
			}
			if op.Prev != nil {
				next.Prev = op.Prev.String()
			}
			out.Ops = append(out.Ops, next)
		}
		if err := fn(out); err != nil {
			return err
		}
	}
}
