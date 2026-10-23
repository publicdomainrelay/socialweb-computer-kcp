package identitycontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

var ErrSecretNotFound = errors.New("identitycontroller: secret not found")

type SecretStore interface {
	Get(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error)

	Put(ctx context.Context, ref commonv1.SecretRef, data map[string]any) error

	Delete(ctx context.Context, ref commonv1.SecretRef) error
}

type VaultStateStore struct {
	Vault SecretStore

	Ref commonv1.SecretRef

	ttl time.Duration
}

const defaultStateTTL = 15 * time.Minute

func (s *VaultStateStore) Put(ctx context.Context, pending oauthclient.PendingAuth) error {
	if s == nil || s.Vault == nil {
		return errors.New("identitycontroller: no vault for the oauth callback state")
	}
	if strings.TrimSpace(pending.State) == "" {
		return errors.New("identitycontroller: the oauth callback state is empty")
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = defaultStateTTL
	}
	return s.Vault.Put(ctx, s.ref(pending.State), map[string]any{
		"state":     string(raw),
		"expiresAt": time.Now().Add(ttl).UTC().Format(time.RFC3339),
	})
}

func (s *VaultStateStore) Take(ctx context.Context, state string) (oauthclient.PendingAuth, bool, error) {
	if s == nil || s.Vault == nil {
		return oauthclient.PendingAuth{}, false, errors.New("identitycontroller: no vault for the oauth callback state")
	}
	data, err := s.Vault.Get(ctx, s.ref(state))
	if errors.Is(err, ErrSecretNotFound) {
		return oauthclient.PendingAuth{}, false, nil
	}
	if err != nil {
		return oauthclient.PendingAuth{}, false, err
	}
	if err := s.Vault.Delete(ctx, s.ref(state)); err != nil && !errors.Is(err, ErrSecretNotFound) {
		return oauthclient.PendingAuth{}, false, err
	}
	if raw := firstString(data, "state"); raw != "" {
		var pending oauthclient.PendingAuth
		if err := json.Unmarshal([]byte(raw), &pending); err != nil {
			return oauthclient.PendingAuth{}, false, fmt.Errorf("identitycontroller: the oauth callback state: %w", err)
		}
		return pending, pending.State != "", nil
	}
	return oauthclient.PendingAuth{}, false, nil
}

func (s *VaultStateStore) ref(state string) commonv1.SecretRef {
	return commonv1.SecretRef{Mount: s.Ref.Mount, Path: strings.Trim(s.Ref.Path, "/") + "/pending/" + state}
}

type vaultSessionStore struct {
	vault SecretStore

	ref commonv1.SecretRef
}

var _ oauthclient.SessionStore = (*vaultSessionStore)(nil)

func (s *vaultSessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	if s == nil || s.vault == nil {
		return nil, errors.New("identitycontroller: no vault for the oauth session")
	}
	data, err := s.vault.Get(ctx, s.ref)
	if errors.Is(err, ErrSecretNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session, ok, err := sessionFromSecret(data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return []oauthclient.SessionData{session}, nil
}

func (s *vaultSessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if s == nil || s.vault == nil {
		return errors.New("identitycontroller: no vault for the oauth session")
	}
	if session.AccountDID == "" {
		return errors.New("identitycontroller: the oauth session names no account DID")
	}
	return s.vault.Put(ctx, s.ref, secretFromSession(session))
}

func (s *vaultSessionStore) Delete(ctx context.Context, did, sessionID string) error {
	if s == nil || s.vault == nil {
		return errors.New("identitycontroller: no vault for the oauth session")
	}
	err := s.vault.Delete(ctx, s.ref)
	if errors.Is(err, ErrSecretNotFound) {
		return nil
	}
	return err
}

func secretFromSession(session oauthclient.SessionData) map[string]any {
	raw, _ := json.Marshal(session)
	return map[string]any{
		"session":                   string(raw),
		"account_did":               session.AccountDID,
		"session_id":                session.SessionID,
		"did":                       session.AccountDID,
		"host_url":                  session.HostURL,
		"pds":                       session.HostURL,
		"client_id":                 session.ClientID,
		"handle":                    "",
		"authserver_url":            session.AuthServerURL,
		"authserver_token_endpoint": session.AuthServerTokenEndpoint,
		"scopes":                    strings.Join(session.Scopes, " "),
		"access_token":              session.AccessToken,
		"refresh_token":             session.RefreshToken,
		"accessJwt":                 session.AccessToken,
		"refreshJwt":                session.RefreshToken,
		"dpop_authserver_nonce":     session.DPoPAuthServerNonce,
		"dpop_host_nonce":           session.DPoPHostNonce,
		"dpop_privatekey_multibase": session.DPoPPrivateKeyMultibase,
		"dpopPrivateKeyMultibase":   session.DPoPPrivateKeyMultibase,
	}
}

func sessionFromSecret(data map[string]any) (oauthclient.SessionData, bool, error) {
	if raw := firstString(data, "session"); raw != "" {
		var session oauthclient.SessionData
		if err := json.Unmarshal([]byte(raw), &session); err != nil {
			return oauthclient.SessionData{}, false, fmt.Errorf("identitycontroller: the oauth session: %w", err)
		}
		if session.AccountDID == "" {
			return oauthclient.SessionData{}, false, nil
		}
		return session, true, nil
	}
	session := oauthclient.SessionData{
		AccountDID:                   firstString(data, "account_did", "did", "userDid"),
		SessionID:                    firstString(data, "session_id"),
		HostURL:                      firstString(data, "host_url", "pds"),
		ClientID:                     firstString(data, "client_id", "clientId"),
		AuthServerURL:                firstString(data, "authserver_url"),
		AuthServerTokenEndpoint:      firstString(data, "authserver_token_endpoint"),
		AuthServerRevocationEndpoint: firstString(data, "authserver_revocation_endpoint"),
		Scopes:                       strings.Fields(firstString(data, "scopes", "scope")),
		AccessToken:                  firstString(data, "access_token", "accessJwt", "accessToken"),
		RefreshToken:                 firstString(data, "refresh_token", "refreshJwt", "refreshToken"),
		DPoPAuthServerNonce:          firstString(data, "dpop_authserver_nonce"),
		DPoPHostNonce:                firstString(data, "dpop_host_nonce"),
		DPoPPrivateKeyMultibase:      firstString(data, "dpop_privatekey_multibase"),
	}
	if session.AccountDID == "" || session.AccessToken == "" {
		return oauthclient.SessionData{}, false, nil
	}
	return session, true, nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := data[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case json.Number:
			if typed.String() != "" {
				return typed.String()
			}
		}
	}
	return ""
}
