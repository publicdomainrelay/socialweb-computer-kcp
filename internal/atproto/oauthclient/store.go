package oauthclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

var (
	ErrNoSession = errors.New("oauthclient: no session stored")

	ErrSessionExpired = errors.New("oauthclient: the oauth session expired")

	ErrInvalidSession = errors.New("oauthclient: invalid stored oauth session")

	ErrNoRefreshToken = errors.New("oauthclient: the oauth session has no refresh token")

	ErrQRConsumed = errors.New("oauthclient: the qr session transfer was already consumed")

	ErrQRTimeout = errors.New("oauthclient: the qr session transfer timed out")

	ErrStateNotFound = errors.New("oauthclient: the oauth callback state is unknown")
)

type SessionData struct {
	AccountDID string `json:"account_did"`

	SessionID string `json:"session_id"`

	HostURL string `json:"host_url"`

	AuthServerURL string `json:"authserver_url"`

	AuthServerTokenEndpoint string `json:"authserver_token_endpoint"`

	AuthServerRevocationEndpoint string `json:"authserver_revocation_endpoint,omitempty"`

	Scopes []string `json:"scopes"`

	AccessToken string `json:"access_token"`

	RefreshToken string `json:"refresh_token"`

	DPoPAuthServerNonce string `json:"dpop_authserver_nonce"`

	DPoPHostNonce string `json:"dpop_host_nonce"`

	DPoPPrivateKeyMultibase string `json:"dpop_privatekey_multibase"`
}

type storeFile struct {
	Sessions map[string]SessionData `json:"sessions"`

	Requests map[string]json.RawMessage `json:"auth_requests,omitempty"`
}

type SessionStore interface {
	List(ctx context.Context) ([]SessionData, error)

	Save(ctx context.Context, session SessionData) error

	Delete(ctx context.Context, did, sessionID string) error
}

type MemoryStore struct {
	mu sync.Mutex

	sessions map[string]SessionData
}

func (m *MemoryStore) List(ctx context.Context) ([]SessionData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionData, 0, len(m.sessions))
	for _, session := range m.sessions {
		out = append(out, session)
	}
	return out, nil
}

func (m *MemoryStore) Save(ctx context.Context, session SessionData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session.AccountDID == "" {
		return errors.New("oauthclient: the oauth session names no account DID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]SessionData{}
	}
	m.sessions[SessionKey(session.AccountDID, session.SessionID)] = session
	return nil
}

func (m *MemoryStore) Delete(ctx context.Context, did, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, SessionKey(did, sessionID))
	return nil
}

type FileStore struct {
	Path string

	mu sync.Mutex
}

func (f *FileStore) List(ctx context.Context) ([]SessionData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, sessions, _, err := f.readLocked()
	if err != nil {
		return nil, err
	}
	out := make([]SessionData, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, session)
	}
	return out, nil
}

func (f *FileStore) Save(ctx context.Context, session SessionData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session.AccountDID == "" {
		return errors.New("oauthclient: the oauth session names no account DID")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	requests, sessions, _, err := f.readLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if sessions == nil {
		sessions = map[string]SessionData{}
	}
	sessions[SessionKey(session.AccountDID, session.SessionID)] = session
	return f.writeLocked(sessions, requests)
}

func (f *FileStore) Delete(ctx context.Context, did, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	requests, sessions, _, err := f.readLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(sessions, SessionKey(did, sessionID))
	return f.writeLocked(sessions, requests)
}

func (f *FileStore) readLocked() (map[string]json.RawMessage, map[string]SessionData, bool, error) {
	requests := map[string]json.RawMessage{}
	sessions := map[string]SessionData{}
	if f == nil || f.Path == "" {
		return nil, nil, false, errors.New("oauthclient: the oauth session store has no path")
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, nil, false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return requests, sessions, false, fmt.Errorf("oauthclient: no oauth session in %s", f.Path)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, false, fmt.Errorf("oauthclient: the oauth token store %s: %w", f.Path, err)
	}
	if _, ok := probe["sessions"]; ok {
		var file storeFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, nil, false, fmt.Errorf("oauthclient: the oauth token store %s: %w", f.Path, err)
		}
		for key, request := range file.Requests {
			requests[key] = request
		}
		for key, session := range file.Sessions {
			sessions[key] = session
		}
		return requests, sessions, false, nil
	}
	if _, ok := probe["accessJwt"]; ok || probe["refreshJwt"] != nil {
		var ts tsOAuthSession
		if err := json.Unmarshal(raw, &ts); err != nil {
			return nil, nil, false, fmt.Errorf("oauthclient: the oauth session %s: %w", f.Path, err)
		}
		session, err := ts.sessionData()
		if err != nil {
			return nil, nil, false, err
		}
		sessions[SessionKey(session.AccountDID, session.SessionID)] = session
		return requests, sessions, true, nil
	}
	var session SessionData
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, nil, false, fmt.Errorf("oauthclient: the oauth token store %s: %w", f.Path, err)
	}
	if session.AccountDID == "" {
		return nil, nil, false, fmt.Errorf("oauthclient: no oauth session in %s", f.Path)
	}
	sessions[SessionKey(session.AccountDID, session.SessionID)] = session
	return requests, sessions, false, nil
}

func (f *FileStore) writeLocked(sessions map[string]SessionData, requests map[string]json.RawMessage) error {
	if f == nil || f.Path == "" {
		return errors.New("oauthclient: the oauth session store has no path")
	}
	if sessions == nil {
		sessions = map[string]SessionData{}
	}
	raw, err := json.MarshalIndent(storeFile{Sessions: sessions, Requests: requests}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".oauth-session-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, f.Path)
}

type tsOAuthSession struct {
	AccessJwt string `json:"accessJwt"`

	RefreshJwt string `json:"refreshJwt"`

	UserDid string `json:"userDid"`

	Handle string `json:"handle"`

	PDS string `json:"pds"`

	DPoPPrivateJwk struct {
		Kty string `json:"kty"`

		Crv string `json:"crv"`

		X string `json:"x"`

		Y string `json:"y"`

		D string `json:"d"`
	} `json:"dpopPrivateJwk"`
}

func (ts *tsOAuthSession) sessionData() (SessionData, error) {
	if ts == nil {
		return SessionData{}, errors.New("oauthclient: no oauth session")
	}
	did, err := syntax.ParseDID(ts.UserDid)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the oauth session userDid: %w", err)
	}
	if ts.PDS == "" {
		return SessionData{}, errors.New("oauthclient: the oauth session names no PDS")
	}
	pds := strings.TrimRight(ts.PDS, "/")
	key, err := privateKeyFromJWK(ts.DPoPPrivateJwk.Kty, ts.DPoPPrivateJwk.D)
	if err != nil {
		return SessionData{}, err
	}
	return SessionData{
		AccountDID:              did.String(),
		SessionID:               SessionID(did.String(), pds),
		HostURL:                 pds,
		AccessToken:             ts.AccessJwt,
		RefreshToken:            ts.RefreshJwt,
		DPoPPrivateKeyMultibase: key.Multibase(),
	}, nil
}

func privateKeyFromJWK(kty, d string) (atcrypto.PrivateKeyExportable, error) {
	if d == "" {
		return nil, errors.New("oauthclient: the oauth session has no DPoP private key")
	}
	if kty != "" && kty != "EC" {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key type %q is not EC", kty)
	}
	keyBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(d, "="))
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key: %w", err)
	}
	key, err := atcrypto.ParsePrivateBytesP256(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the oauth session DPoP key: %w", err)
	}
	return key, nil
}

func SessionKey(did, sessionID string) string {
	return did + "/" + sessionID
}

func SessionID(did, host string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + host))
	return hex.EncodeToString(sum[:16])
}
