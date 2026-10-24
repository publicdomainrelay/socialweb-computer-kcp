package oauthclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
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

	ClientID string `json:"client_id,omitempty"`

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
