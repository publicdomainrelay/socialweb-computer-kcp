package requester

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
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
