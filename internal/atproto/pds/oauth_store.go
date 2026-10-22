package pds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

const schemaOAuthSessions = `CREATE TABLE IF NOT EXISTS pds_oauth_sessions (
	id TEXT PRIMARY KEY,
	did TEXT NOT NULL,
	handle TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_auth_method TEXT NOT NULL,
	client_key_jkt TEXT NOT NULL,
	scope TEXT NOT NULL,
	jkt TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER
)`

const schemaOAuthSessionsDID = `CREATE INDEX IF NOT EXISTS pds_oauth_sessions_did ON pds_oauth_sessions (did)`

const schemaOAuthCodes = `CREATE TABLE IF NOT EXISTS pds_oauth_codes (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_auth_method TEXT NOT NULL,
	client_key_jkt TEXT NOT NULL,
	did TEXT NOT NULL,
	handle TEXT NOT NULL,
	scope TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	code_challenge_method TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at INTEGER
)`

const schemaOAuthPARs = `CREATE TABLE IF NOT EXISTS pds_oauth_pars (
	request_uri TEXT PRIMARY KEY,
	client_id TEXT NOT NULL,
	client_auth_method TEXT NOT NULL,
	client_key_jkt TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	scope TEXT NOT NULL,
	state TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	code_challenge_method TEXT NOT NULL,
	response_type TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
)`

const schemaOAuthTokens = `CREATE TABLE IF NOT EXISTS pds_oauth_tokens (
	jti TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	issued_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER
)`

const schemaOAuthTokensSession = `CREATE INDEX IF NOT EXISTS pds_oauth_tokens_session ON pds_oauth_tokens (session_id)`

var (
	_ oauth.AccountStore           = (*Store)(nil)
	_ oauth.SessionStore           = (*Store)(nil)
	_ oauth.AuthorizationCodeStore = (*Store)(nil)
	_ oauth.PARStore               = (*Store)(nil)
	_ oauth.TokenStore             = (*Store)(nil)
)

func (s *Store) CreateSession(ctx context.Context, sess oauth.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_oauth_sessions
		 (id, did, handle, client_id, client_auth_method, client_key_jkt, scope, jkt, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Account.DID.String(), sess.Account.Handle,
		sess.ClientID, sess.ClientAuthMethod, sess.ClientKeyJKT, sess.Scope, sess.JKT,
		unixMicro(sess.CreatedAt), unixMicro(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("pds: creating oauth session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*oauth.Session, error) {
	var didRaw, handle, clientID, authMethod, clientJKT, scope, jkt string
	var created, expires int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT did, handle, client_id, client_auth_method, client_key_jkt, scope, jkt, created_at, expires_at, revoked_at
		 FROM pds_oauth_sessions WHERE id = ?`, id).
		Scan(&didRaw, &handle, &clientID, &authMethod, &clientJKT, &scope, &jkt, &created, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading oauth session: %w", err)
	}
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return nil, fmt.Errorf("pds: stored oauth session did: %w", err)
	}
	return &oauth.Session{
		ID:               id,
		Account:          oauth.Account{DID: did, Handle: handle},
		ClientID:         clientID,
		ClientAuthMethod: authMethod,
		ClientKeyJKT:     clientJKT,
		Scope:            scope,
		JKT:              jkt,
		CreatedAt:        fromUnixMicro(created),
		ExpiresAt:        fromUnixMicro(expires),
		RevokedAt:        scanTimePtr(revoked),
	}, nil
}

func (s *Store) ExtendSession(ctx context.Context, id string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pds_oauth_sessions SET expires_at = ? WHERE id = ? AND revoked_at IS NULL`,
		unixMicro(expiresAt), id)
	if err != nil {
		return fmt.Errorf("pds: extending oauth session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return oauth.ErrSessionNotFound
	}
	return nil
}

func (s *Store) RevokeSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pds_oauth_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		unixMicro(at), id)
	if err != nil {
		return fmt.Errorf("pds: revoking oauth session: %w", err)
	}
	return nil
}

func (s *Store) CreateCode(ctx context.Context, code oauth.AuthorizationCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_oauth_codes
		 (id, session_id, client_id, client_auth_method, client_key_jkt, did, handle, scope, redirect_uri,
		  code_challenge, code_challenge_method, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code.ID, code.SessionID, code.ClientID, code.ClientAuthMethod, code.ClientKeyJKT,
		code.Account.DID.String(), code.Account.Handle, code.Scope, code.RedirectURI,
		code.CodeChallenge, code.CodeChallengeMethod, unixMicro(code.CreatedAt), unixMicro(code.ExpiresAt))
	if err != nil {
		return fmt.Errorf("pds: creating oauth code: %w", err)
	}
	return nil
}

func (s *Store) ConsumeCode(ctx context.Context, id string, at time.Time) (*oauth.AuthorizationCode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pds: consuming oauth code: %w", err)
	}
	defer tx.Rollback()
	var code oauth.AuthorizationCode
	var didRaw string
	var created, expires int64
	var used sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT session_id, client_id, client_auth_method, client_key_jkt, did, handle, scope, redirect_uri,
		        code_challenge, code_challenge_method, created_at, expires_at, used_at
		 FROM pds_oauth_codes WHERE id = ?`, id).
		Scan(&code.SessionID, &code.ClientID, &code.ClientAuthMethod, &code.ClientKeyJKT, &didRaw, &code.Account.Handle,
			&code.Scope, &code.RedirectURI, &code.CodeChallenge, &code.CodeChallengeMethod, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oauth.ErrCodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading oauth code: %w", err)
	}
	code.ID = id
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		return nil, fmt.Errorf("pds: stored oauth code did: %w", err)
	}
	code.Account.DID = did
	code.CreatedAt = fromUnixMicro(created)
	code.ExpiresAt = fromUnixMicro(expires)
	code.UsedAt = scanTimePtr(used)
	if code.UsedAt != nil {
		return &code, oauth.ErrCodeUsed
	}
	if !code.ExpiresAt.After(at) {
		return &code, oauth.ErrCodeExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pds_oauth_codes SET used_at = ? WHERE id = ?`, unixMicro(at), id); err != nil {
		return nil, fmt.Errorf("pds: consuming oauth code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pds: consuming oauth code: %w", err)
	}
	usedAt := at.UTC()
	code.UsedAt = &usedAt
	return &code, nil
}

func (s *Store) StorePAR(ctx context.Context, req oauth.PARRequest) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO pds_oauth_pars
		 (request_uri, client_id, client_auth_method, client_key_jkt, redirect_uri, scope, state,
		  code_challenge, code_challenge_method, response_type, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.RequestURI, req.ClientID, req.ClientAuthMethod, req.ClientKeyJKT, req.RedirectURI,
		req.Scope, req.State, req.CodeChallenge, req.CodeChallengeMethod, req.ResponseType,
		unixMicro(req.CreatedAt), unixMicro(req.ExpiresAt))
	if err != nil {
		return fmt.Errorf("pds: storing par: %w", err)
	}
	return nil
}

func (s *Store) ConsumePAR(ctx context.Context, requestURI string, at time.Time) (*oauth.PARRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pds: consuming par: %w", err)
	}
	defer tx.Rollback()
	var req oauth.PARRequest
	var created, expires int64
	err = tx.QueryRowContext(ctx,
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM pds_oauth_pars WHERE request_uri = ?`, requestURI); err != nil {
		return nil, fmt.Errorf("pds: consuming par: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pds: consuming par: %w", err)
	}
	if !req.ExpiresAt.After(at) {
		return nil, oauth.ErrPARExpired
	}
	return &req, nil
}

func (s *Store) CreateToken(ctx context.Context, token oauth.TokenRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pds_oauth_tokens (jti, session_id, kind, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		token.JTI, token.SessionID, string(token.Kind), unixMicro(token.IssuedAt), unixMicro(token.ExpiresAt))
	if err != nil {
		return fmt.Errorf("pds: creating token: %w", err)
	}
	return nil
}

func (s *Store) GetToken(ctx context.Context, jti string) (*oauth.TokenRecord, error) {
	var sessionID, kind string
	var issued, expires int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id, kind, issued_at, expires_at, revoked_at FROM pds_oauth_tokens WHERE jti = ?`, jti).
		Scan(&sessionID, &kind, &issued, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pds: loading token: %w", err)
	}
	return &oauth.TokenRecord{
		JTI:       jti,
		SessionID: sessionID,
		Kind:      oauth.TokenKind(kind),
		IssuedAt:  fromUnixMicro(issued),
		ExpiresAt: fromUnixMicro(expires),
		RevokedAt: scanTimePtr(revoked),
	}, nil
}

func (s *Store) RevokeToken(ctx context.Context, jti string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pds_oauth_tokens SET revoked_at = ? WHERE jti = ? AND revoked_at IS NULL`,
		unixMicro(at), jti)
	if err != nil {
		return fmt.Errorf("pds: revoking token: %w", err)
	}
	return nil
}

func (s *Store) RevokeSessionTokens(ctx context.Context, sessionID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pds_oauth_tokens SET revoked_at = ? WHERE session_id = ? AND revoked_at IS NULL`,
		unixMicro(at), sessionID)
	if err != nil {
		return fmt.Errorf("pds: revoking session tokens: %w", err)
	}
	return nil
}

func (s *Store) RotateRefreshToken(ctx context.Context, oldJTI string, now time.Time, newTokens []oauth.TokenRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pds: rotating refresh token: %w", err)
	}
	defer tx.Rollback()
	var kind string
	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT kind, revoked_at FROM pds_oauth_tokens WHERE jti = ?`, oldJTI).Scan(&kind, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return oauth.ErrTokenNotFound
	}
	if err != nil {
		return fmt.Errorf("pds: rotating refresh token: %w", err)
	}
	if oauth.TokenKind(kind) != oauth.TokenRefresh || revoked.Valid {
		return oauth.ErrTokenReused
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pds_oauth_tokens SET revoked_at = ? WHERE jti = ? AND revoked_at IS NULL`,
		unixMicro(now), oldJTI); err != nil {
		return fmt.Errorf("pds: rotating refresh token: %w", err)
	}
	for _, token := range newTokens {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pds_oauth_tokens (jti, session_id, kind, issued_at, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			token.JTI, token.SessionID, string(token.Kind), unixMicro(token.IssuedAt), unixMicro(token.ExpiresAt)); err != nil {
			return fmt.Errorf("pds: rotating refresh token: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pds: rotating refresh token: %w", err)
	}
	return nil
}
