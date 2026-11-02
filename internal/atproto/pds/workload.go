package pds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/workloadauth/oidc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/workloadauth/rbac"
)

const schemaOAuthWorkloadGrants = `CREATE TABLE IF NOT EXISTS pds_oauth_workload_grants (
	session_id TEXT PRIMARY KEY,
	did TEXT NOT NULL,
	role TEXT NOT NULL,
	issuer TEXT NOT NULL,
	subject TEXT NOT NULL,
	audience TEXT NOT NULL,
	client_id TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	scope TEXT NOT NULL,
	created_at INTEGER NOT NULL
)`

const schemaOAuthWorkloadGrantsRole = `CREATE INDEX IF NOT EXISTS pds_oauth_workload_grants_role ON pds_oauth_workload_grants (did, role)`

var (
	_ oauth.WorkloadGrantStore   = (*Store)(nil)
	_ workloadauth.RecordReader  = (*workloadRecords)(nil)
	_ oauth.WorkloadAccounts     = (*workloadAccounts)(nil)
	_ repo.EventSink             = (*commitFanout)(nil)
	_ workloadauth.TokenVerifier = (*oidc.Verifier)(nil)
	_ workloadauth.Authorizer    = (*rbac.Authorizer)(nil)
)

type WorkloadOAuthConfig struct {
	Verifier workloadauth.TokenVerifier

	AllowLoopbackIssuers bool

	Log *slog.Logger
}

func (s *Store) PutWorkloadGrant(ctx context.Context, g oauth.WorkloadGrant) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_oauth_workload_grants
		 (session_id, did, role, issuer, subject, audience, client_id, redirect_uri, scope, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.SessionID, g.Grant.Account, g.Grant.Role, g.Grant.Issuer, g.Grant.Subject, g.Grant.Audience,
		g.ClientID, g.RedirectURI, g.Scope, unixMicro(g.CreatedAt))
	if err != nil {
		return fmt.Errorf("pds: storing a workload grant: %w", err)
	}
	return nil
}

func (s *Store) GetWorkloadGrant(ctx context.Context, sessionID string) (*oauth.WorkloadGrant, error) {
	var g oauth.WorkloadGrant
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT did, role, issuer, subject, audience, client_id, redirect_uri, scope, created_at
		 FROM pds_oauth_workload_grants WHERE session_id = ?`, sessionID).
		Scan(&g.Grant.Account, &g.Grant.Role, &g.Grant.Issuer, &g.Grant.Subject, &g.Grant.Audience,
			&g.ClientID, &g.RedirectURI, &g.Scope, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading a workload grant: %w", err)
	}
	g.SessionID = sessionID
	g.CreatedAt = fromUnixMicro(created)
	return &g, nil
}

func (s *Store) WorkloadGrantSessions(ctx context.Context, account, role string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.session_id FROM pds_oauth_workload_grants g
		 JOIN pds_oauth_sessions o ON o.id = g.session_id
		 WHERE g.did = ? AND g.role = ? AND o.revoked_at IS NULL`, account, role)
	if err != nil {
		return nil, fmt.Errorf("pds: listing workload grants: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) GetPAR(ctx context.Context, requestURI string, at time.Time) (*oauth.PARRequest, error) {
	var req oauth.PARRequest
	var created, expires int64
	err := s.db.QueryRowContext(ctx,
		`SELECT client_id, client_auth_method, client_key_jkt, redirect_uri, scope, state,
		        code_challenge, code_challenge_method, response_type, created_at, expires_at
		 FROM pds_oauth_pars WHERE request_uri = ?`, requestURI).
		Scan(&req.ClientID, &req.ClientAuthMethod, &req.ClientKeyJKT, &req.RedirectURI, &req.Scope, &req.State,
			&req.CodeChallenge, &req.CodeChallengeMethod, &req.ResponseType, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oauth.ErrPARNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading par: %w", err)
	}
	req.RequestURI = requestURI
	req.CreatedAt = fromUnixMicro(created)
	req.ExpiresAt = fromUnixMicro(expires)
	if !req.ExpiresAt.After(at) {
		return nil, oauth.ErrPARExpired
	}
	return &req, nil
}

type workloadRecords struct {
	repos repo.RepoStore
}

func (w *workloadRecords) Record(ctx context.Context, account, collection, rkey string) ([]byte, error) {
	did, err := syntax.ParseDID(account)
	if err != nil {
		return nil, workloadauth.ErrRecordNotFound
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return nil, workloadauth.ErrRecordNotFound
	}
	key, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return nil, workloadauth.ErrRecordNotFound
	}
	rec, err := w.repos.GetRecord(ctx, did, nsid, key)
	if errors.Is(err, repo.ErrRecordNotFound) || errors.Is(err, repo.ErrRepoNotFound) {
		return nil, workloadauth.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(rec.Value)
}

type workloadAccounts struct {
	store *Store
}

func (w *workloadAccounts) ActiveAccount(ctx context.Context, did syntax.DID) (oauth.Account, error) {
	acct, err := w.store.GetAccount(ctx, did)
	if err != nil {
		return oauth.Account{}, err
	}
	if acct == nil || !acct.Active {
		return oauth.Account{}, oauth.ErrAccountNotFound
	}
	return oauth.Account{DID: acct.DID, Handle: acct.Handle}, nil
}

type commitFanout struct {
	sinks []repo.EventSink
}

func (f *commitFanout) OnCommit(ctx context.Context, ev *repo.CommitEvent) error {
	var first error
	for _, sink := range f.sinks {
		if err := sink.OnCommit(ctx, ev); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Server) workloadOAuth(cfg *OAuthConfig) (*oauth.WorkloadConfig, error) {
	if cfg.Workload == nil {
		return nil, nil
	}
	verifier := cfg.Workload.Verifier
	if verifier == nil {
		verifier = oidc.New(oidc.Config{AllowLoopbackIssuers: cfg.Workload.AllowLoopbackIssuers})
	}
	authorizer, err := rbac.New(rbac.Config{Service: s.serviceDID}, &workloadRecords{repos: s.repos}, verifier)
	if err != nil {
		return nil, err
	}
	return &oauth.WorkloadConfig{
		Authorizer: authorizer,
		Grants:     s.store,
		Accounts:   &workloadAccounts{store: s.store},
		Log:        cfg.Workload.Log,
	}, nil
}

func (s *Server) onWorkloadCommit(_ context.Context, ev *repo.CommitEvent) error {
	if ev == nil || s.oauthServer == nil {
		return nil
	}
	for _, op := range ev.Ops {
		collection, rkey, ok := strings.Cut(op.Path, "/")
		if !ok || collection != workloadauth.RBACCollection {
			continue
		}
		if op.Action == repo.ActionCreate {
			continue
		}
		account := ev.Repo.String()
		server := s.oauthServer
		go server.RecheckWorkloadGrants(context.Background(), account, rkey)
	}
	return nil
}
