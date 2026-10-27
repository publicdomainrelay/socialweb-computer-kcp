package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

var passwordKeys = []string{"password", "appPassword", "value"}

type Accounts struct {
	srv *atpds.Server

	secrets Secrets

	scope []string
}

func (a *Accounts) Describe(context.Context) (pds.Info, error) {
	domains := a.srv.AvailableUserDomains()
	base := strings.TrimRight(a.srv.PublicBaseURL(), "/")
	if len(domains) == 0 {
		domain := "localhost"
		if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
			domain = u.Hostname()
		}
		domains = []string{domain}
	}
	return pds.Info{DID: a.srv.ServiceDID(), AvailableUserDomains: domains, Version: a.srv.Version(), URL: base}, nil
}

func (a *Accounts) Account(ctx context.Context, raw string) (pds.Account, error) {
	if !inScope(a.scope, raw) {
		return pds.Account{}, fmt.Errorf("pds: the account %s is outside the granted scope %v", raw, a.scope)
	}
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return pds.Account{}, fmt.Errorf("pds: the account did %q: %w", raw, err)
	}
	return account(ctx, a.srv, did)
}

func account(ctx context.Context, srv *atpds.Server, did syntax.DID) (pds.Account, error) {
	store := srv.Store()
	if store == nil {
		return pds.Account{}, fmt.Errorf("%w: the Pds runs without accounts", pds.ErrUnsupported)
	}
	acct, err := store.GetAccount(ctx, did)
	if errors.Is(err, atpds.ErrAccountNotFound) {
		return pds.Account{}, fmt.Errorf("%w: %s: %v", pds.ErrNotFound, did, err)
	}
	if err != nil {
		return pds.Account{}, err
	}
	out := pds.Account{DID: acct.DID.String(), Handle: acct.Handle, Email: acct.Email, Active: acct.Active}
	if keys := srv.KeyStore(); keys != nil {
		if key, err := keys.GetRepoKey(ctx, did); err == nil {
			if public, err := key.PublicKey(); err == nil {
				out.SigningKey = public.DIDKey()
			}
		}
	}
	return out, nil
}

func (a *Accounts) CreateAccount(ctx context.Context, req pds.AccountRequest) (pds.Account, error) {
	password, err := secretValue(ctx, a.secrets, req.Credentials)
	if err != nil {
		return pds.Account{}, err
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := call(ctx, a.srv, "com.atproto.server.createAccount",
		map[string]string{"handle": req.Handle, "email": req.Email, "password": password}, &out); err != nil {
		return pds.Account{}, err
	}
	did, err := syntax.ParseDID(out.DID)
	if err != nil {
		return pds.Account{}, fmt.Errorf("pds: createAccount answered the did %q: %w", out.DID, err)
	}
	return account(ctx, a.srv, did)
}

type Sessions struct {
	srv *atpds.Server

	secrets Secrets

	scope []string
}

func (s *Sessions) SignIn(ctx context.Context, req pds.SessionRequest) (pds.Session, error) {
	store := s.srv.Store()
	if store == nil {
		return pds.Session{}, fmt.Errorf("%w: the Pds runs without accounts", pds.ErrUnsupported)
	}
	password, err := secretValue(ctx, s.secrets, req.Credentials)
	if err != nil {
		return pds.Session{}, err
	}
	acct, _, _, err := store.VerifyPassword(ctx, req.Identifier, password)
	if err != nil {
		return pds.Session{}, fmt.Errorf("pds: signing in %s: %w", req.Identifier, err)
	}
	if !inScope(s.scope, acct.DID.String()) {
		return pds.Session{}, fmt.Errorf("pds: the account %s is outside the granted scope %v", acct.DID, s.scope)
	}
	return pds.Session{DID: acct.DID.String(), Handle: acct.Handle, Active: acct.Active}, nil
}

func (s *Sessions) Session(ctx context.Context, raw string) (pds.Session, error) {
	if !inScope(s.scope, raw) {
		return pds.Session{}, fmt.Errorf("pds: the account %s is outside the granted scope %v", raw, s.scope)
	}
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return pds.Session{}, fmt.Errorf("pds: the account did %q: %w", raw, err)
	}
	acct, err := account(ctx, s.srv, did)
	if err != nil {
		return pds.Session{}, err
	}
	return pds.Session{DID: acct.DID, Handle: acct.Handle, Active: acct.Active}, nil
}

func (s *Sessions) SignOut(context.Context, string) error {
	return nil
}

type Sync struct {
	srv *atpds.Server
}

func (s *Sync) LatestCommit(ctx context.Context, raw string) (pds.Commit, error) {
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return pds.Commit{}, fmt.Errorf("pds: the repo did %q: %w", raw, err)
	}
	head, err := s.srv.Repos().Head(ctx, did)
	if err != nil {
		if errors.Is(err, repo.ErrRepoNotFound) {
			return pds.Commit{}, fmt.Errorf("%w: %v", pds.ErrNotFound, err)
		}
		return pds.Commit{}, err
	}
	return pds.Commit{CID: head.CID.String(), Rev: head.Rev.String()}, nil
}

func (s *Sync) GetRepo(ctx context.Context, raw, since string) (io.ReadCloser, error) {
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return nil, fmt.Errorf("pds: the repo did %q: %w", raw, err)
	}
	var sinceTID *syntax.TID
	if since != "" {
		tid, err := syntax.ParseTID(since)
		if err != nil {
			return nil, fmt.Errorf("pds: the since rev %q: %w", since, err)
		}
		sinceTID = &tid
	}
	car, err := s.srv.Repos().CAR(ctx, did, sinceTID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(car)), nil
}

func (s *Sync) SubscribeRepos(ctx context.Context, cursor *int64, fn func(pds.Event) error) error {
	return follow(ctx, s.srv, cursor, func(ev *repo.CommitEvent) error {
		out := pds.Event{Seq: ev.Seq, Repo: ev.Repo.String(), Commit: ev.Commit.String(), Rev: ev.Rev.String(), Time: ev.Time}
		if ev.Since != nil {
			out.Since = ev.Since.String()
		}
		for _, op := range ev.Ops {
			next := pds.Op{Action: string(op.Action), Path: op.Path}
			if op.CID != nil {
				next.CID = op.CID.String()
			}
			if op.Prev != nil {
				next.Prev = op.Prev.String()
			}
			out.Ops = append(out.Ops, next)
		}
		return fn(out)
	})
}

func secretValue(ctx context.Context, secrets Secrets, ref commonv1.SecretRef) (string, error) {
	if secrets == nil {
		return "", errors.New("pds: no secret resolver is configured")
	}
	data, err := secrets(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("pds: reading the credentials %s/%s: %w", ref.Mount, ref.Path, err)
	}
	keys := passwordKeys
	if ref.Key != "" {
		keys = []string{ref.Key}
	}
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", fmt.Errorf("pds: the credentials %s/%s hold no password", ref.Mount, ref.Path)
}

func call(ctx context.Context, srv *atpds.Server, nsid string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/xrpc/"+nsid, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code > 299 {
		var failure struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &failure)
		return fmt.Errorf("pds: %s answered %d: %s %s", nsid, rec.Code, failure.Error, failure.Message)
	}
	return json.Unmarshal(rec.Body.Bytes(), out)
}
