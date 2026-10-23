package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

type OAuthSession = oauthclient.Session

type credentialSessionStore struct {
	resolver component.CredentialResolver

	writer component.CredentialWriter

	ref component.CredentialRef
}

var _ oauthclient.SessionStore = (*credentialSessionStore)(nil)

func (s *credentialSessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	if s == nil || s.resolver == nil {
		return nil, errors.New("requester: no credential resolver for the oauth session")
	}
	credential, err := s.resolver.Resolve(ctx, s.ref)
	if err != nil {
		return nil, err
	}
	data, ok := oauthclient.SessionFromSecret(credential.Data)
	if !ok {
		return nil, nil
	}
	return []oauthclient.SessionData{data}, nil
}

func (s *credentialSessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if s == nil || s.writer == nil {
		return errors.New("requester: no credential writer for the oauth session")
	}
	if session.AccountDID == "" {
		return errors.New("requester: the oauth session names no account DID")
	}
	return s.writer.WriteCredential(ctx, s.ref, oauthclient.SessionSecret(session))
}

func (s *credentialSessionStore) Delete(ctx context.Context, did, sessionID string) error {
	if s == nil {
		return nil
	}
	deleter, ok := s.resolver.(component.CredentialDeleter)
	if !ok {
		return nil
	}
	return deleter.DeleteCredential(ctx, s.ref)
}

type pdsAccountTokens struct {
	pdsURL string

	identifier string

	password string

	client *http.Client

	access *lease.Source[string]
}

func newPDSAccountTokens(pdsURL, identifier, password string, client *http.Client) *pdsAccountTokens {
	t := &pdsAccountTokens{pdsURL: pdsURL, identifier: identifier, password: password, client: client}
	t.access = lease.New(t.login)
	return t
}

func (t *pdsAccountTokens) AccessToken(ctx context.Context) (string, error) {
	return t.access.Get(ctx)
}

func (t *pdsAccountTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.access.Invalidate()
	return t.access.Get(ctx)
}

func (t *pdsAccountTokens) login(ctx context.Context) (string, time.Time, error) {
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: t.pdsURL, HTTPClient: t.client, Timeout: DefaultHTTPTimeout})
	if err != nil {
		return "", time.Time{}, err
	}
	session, err := pds.CreateSession(ctx, t.identifier, t.password)
	if err != nil {
		return "", time.Time{}, err
	}
	expires, ok := pdsclient.TokenExpiry(session.AccessJWT)
	if !ok {
		return session.AccessJWT, time.Now(), nil
	}
	return session.AccessJWT, expires.Add(-pdsclient.TokenExpirySkew), nil
}

type credentialTokens struct {
	mu sync.Mutex

	resolver component.CredentialResolver

	writer component.CredentialWriter

	ref component.CredentialRef

	pdsURL string

	client *http.Client

	access string
}

func (t *credentialTokens) AccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pdsclient.TokenLive(t.access) {
		return t.access, nil
	}
	return t.refreshLocked(ctx)
}

func (t *credentialTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshLocked(ctx)
}

func (t *credentialTokens) refreshLocked(ctx context.Context) (string, error) {
	if t.resolver == nil || t.pdsURL == "" {
		return t.access, nil
	}
	fullRef := t.ref
	fullRef.Keys = nil
	data, err := t.resolver.Resolve(ctx, fullRef)
	if err != nil {
		if t.access != "" {
			return t.access, nil
		}
		return "", err
	}
	access := strs.Lookup(data.Data, "accessJwt", "accessToken", "access_token", "token", "value")
	refresh := strs.Lookup(data.Data, "refreshJwt", "refreshToken", "refresh_token")
	if pdsclient.TokenLive(access) {
		t.access = access
		return access, nil
	}
	if refresh == "" {
		t.access = access
		return access, nil
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: t.pdsURL, HTTPClient: t.client, Timeout: DefaultHTTPTimeout})
	if err != nil {
		return "", err
	}
	session, err := pds.RefreshSession(ctx, refresh)
	if err != nil {
		if t.access != "" {
			return t.access, nil
		}
		return "", err
	}
	t.access = session.AccessJWT
	if t.writer != nil {
		updated := map[string]any{}
		maps.Copy(updated, data.Data)
		updated["accessJwt"] = session.AccessJWT
		updated["refreshJwt"] = session.RefreshJWT
		updated["access_token"] = session.AccessJWT
		updated["refresh_token"] = session.RefreshJWT
		if raw := strs.Lookup(data.Data, "session"); raw != "" {
			var stored oauthclient.SessionData
			if json.Unmarshal([]byte(raw), &stored) == nil {
				stored.AccessToken = session.AccessJWT
				stored.RefreshToken = session.RefreshJWT
				if encoded, err := json.Marshal(stored); err == nil {
					updated["session"] = string(encoded)
				}
			}
		}
		_ = t.writer.WriteCredential(ctx, t.ref, updated)
	}
	return t.access, nil
}

func openOAuthSession(ctx context.Context, cfg Config, dir identity.Directory) (*OAuthSession, error) {
	if dir == nil {
		dir = cfg.Directory
	}
	if cfg.OAuthSessionData != nil {
		data := *cfg.OAuthSessionData
		if data.ClientID == "" {
			data.ClientID = strings.TrimSpace(cfg.OAuthClientID)
		}
		opts := oauthclient.Options{
			DID:        data.AccountDID,
			RepoDID:    data.AccountDID,
			PDSURL:     data.HostURL,
			ClientID:   cfg.OAuthClientID,
			Scopes:     cfg.OAuthScopes,
			Directory:  dir,
			HTTPClient: cfg.HTTPClient,
		}
		if cfg.PDSAccessTokenRef != nil && cfg.Deps.Credentials != nil {
			if writer, ok := cfg.Deps.Credentials.(component.CredentialWriter); ok {
				opts.Store = &credentialSessionStore{resolver: cfg.Deps.Credentials, writer: writer, ref: *cfg.PDSAccessTokenRef}
			}
		}
		session, err := oauthclient.New(data, opts)
		if err != nil {
			return nil, fmt.Errorf("requester: the stored oauth session: %w", err)
		}
		_ = session.EnsureEndpoints(ctx)
		if opts.Store != nil {
			_ = session.Sync(ctx)
		}
		return session, nil
	}
	if cfg.OAuthSessionPath == "" {
		return nil, errors.New("requester: oauth identity mode has no token store path")
	}
	session, err := oauthclient.OpenFile(ctx, cfg.OAuthSessionPath, oauthclient.Options{
		DID:        cfg.DID,
		Handle:     cfg.Handle,
		RepoDID:    cfg.RepoDID,
		PDSURL:     cfg.PDSURL,
		ClientID:   cfg.OAuthClientID,
		Scopes:     cfg.OAuthScopes,
		Directory:  dir,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("requester: no oauth token store at %s", cfg.OAuthSessionPath)
		}
		return nil, err
	}
	return session, nil
}
