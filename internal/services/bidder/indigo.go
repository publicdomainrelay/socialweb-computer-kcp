package bidder

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

const defaultBidderOAuthClientID = "https://socialweb.computer/oauth-client-metadata.json"

type bidderSessionStore struct {
	resolver component.CredentialResolver

	writer component.CredentialWriter

	ref component.CredentialRef
}

var _ oauthclient.SessionStore = (*bidderSessionStore)(nil)

func (s *bidderSessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	if s == nil || s.resolver == nil {
		return nil, errors.New("bidder: no credential resolver for the oauth session")
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

func (s *bidderSessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if s == nil || s.writer == nil {
		return errors.New("bidder: no credential writer for the oauth session")
	}
	if session.AccountDID == "" {
		return errors.New("bidder: the oauth session names no account DID")
	}
	data := map[string]any{}
	if s.resolver != nil {
		fullRef := s.ref
		fullRef.Keys = nil
		if credential, err := s.resolver.Resolve(ctx, fullRef); err == nil {
			maps.Copy(data, credential.Data)
		}
	}
	maps.Copy(data, oauthclient.SessionSecret(session))
	return s.writer.WriteCredential(ctx, s.ref, data)
}

func (s *bidderSessionStore) Delete(ctx context.Context, did, sessionID string) error {
	deleter, ok := s.resolver.(component.CredentialDeleter)
	if !ok {
		return nil
	}
	return deleter.DeleteCredential(ctx, s.ref)
}

type credentialPDSTokens struct {
	mu sync.Mutex

	resolver component.CredentialResolver

	writer component.CredentialWriter

	ref component.CredentialRef

	pdsURL string

	client *http.Client

	access string
}

func (t *credentialPDSTokens) AccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pdsclient.TokenLive(t.access) {
		return t.access, nil
	}
	return t.refreshLocked(ctx)
}

func (t *credentialPDSTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshLocked(ctx)
}

func (t *credentialPDSTokens) refreshLocked(ctx context.Context) (string, error) {
	if t.resolver == nil || t.pdsURL == "" {
		return t.access, nil
	}
	fullRef := t.ref
	fullRef.Keys = nil
	credential, err := t.resolver.Resolve(ctx, fullRef)
	if err != nil {
		if t.access != "" {
			return t.access, nil
		}
		return "", err
	}
	access := strs.Lookup(credential.Data, "accessJwt", "accessToken", "access_token", "token", "value")
	refresh := strs.Lookup(credential.Data, "refreshJwt", "refreshToken", "refresh_token")
	if pdsclient.TokenLive(access) {
		t.access = access
		return access, nil
	}
	if refresh == "" {
		t.access = access
		return access, nil
	}
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: t.pdsURL, HTTPClient: t.client, Timeout: DefaultCallTimeout})
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
		maps.Copy(updated, credential.Data)
		updated["accessJwt"] = session.AccessJWT
		updated["refreshJwt"] = session.RefreshJWT
		updated["access_token"] = session.AccessJWT
		updated["refresh_token"] = session.RefreshJWT
		if raw := strs.Lookup(credential.Data, "session"); raw != "" {
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

func indigoSessionPDS(data oauthclient.SessionData) (pdsclient.PDS, error) {
	key, err := atcrypto.ParsePrivateMultibase(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return nil, err
	}
	scopes := append([]string(nil), data.Scopes...)
	if !slices.Contains(scopes, "atproto") {
		scopes = append([]string{"atproto"}, scopes...)
	}
	config := oauth.NewPublicConfig(defaultBidderOAuthClientID, "", scopes)
	session := &oauth.ClientSession{
		Client:         &http.Client{Timeout: 30 * time.Second},
		Config:         &config,
		DPoPPrivateKey: key,
		Data: &oauth.ClientSessionData{
			AccountDID:                   syntax.DID(data.AccountDID),
			SessionID:                    data.SessionID,
			HostURL:                      data.HostURL,
			AuthServerURL:                data.AuthServerURL,
			AuthServerTokenEndpoint:      data.AuthServerTokenEndpoint,
			AuthServerRevocationEndpoint: data.AuthServerRevocationEndpoint,
			Scopes:                       scopes,
			AccessToken:                  data.AccessToken,
			RefreshToken:                 data.RefreshToken,
			DPoPAuthServerNonce:          data.DPoPAuthServerNonce,
			DPoPHostNonce:                data.DPoPHostNonce,
			DPoPPrivateKeyMultibase:      data.DPoPPrivateKeyMultibase,
		},
	}
	api := atclient.NewAPIClient(strings.TrimRight(data.HostURL, "/"))
	api.Client = &http.Client{Timeout: 30 * time.Second}
	api.Auth = session
	return &pdsclient.IndigoPDS{API: api, BaseURL: data.HostURL}, nil
}
