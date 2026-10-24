package bidder

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const defaultBidderOAuthClientID = "https://socialweb.computer/oauth-client-metadata.json"

func (s *Service) restoreSession(ctx context.Context, egress component.Egress, log *slog.Logger) {
	if s.credentials == nil {
		return
	}
	data, err := s.credentials.Read(ctx, false)
	if err != nil {
		return
	}
	session, ok := oauthclient.SessionFromSecret(data)
	if !ok {
		source, err := pdsclient.DPoPSourceFromData(data)
		if err != nil {
			log.Warn("the bidder could not read the session DPoP key", "err", err.Error())
			return
		}
		s.pdsDPoP = source
		return
	}
	opts := oauthclient.Options{
		DID:        session.AccountDID,
		RepoDID:    session.AccountDID,
		PDSURL:     session.HostURL,
		ClientID:   cmp.Or(session.ClientID, defaultBidderOAuthClientID),
		Scopes:     session.Scopes,
		HTTPClient: egress.Client(DefaultCallTimeout),
	}
	if store, ok := s.credentials.(component.CredentialStore); ok && store.Writable() {
		opts.Store = &participant.SessionStore{Credentials: s.credentials, Merge: true}
	}
	restored, err := oauthclient.New(session, opts)
	if err == nil {
		s.pdsTokens = restored
		s.pdsDPoP = restored
		s.pdsURL = session.HostURL
		return
	}
	log.Warn("the bidder could not restore the oauth session", "err", err.Error())
	if session.AuthServerURL != "" && session.AuthServerTokenEndpoint != "" {
		return
	}
	authServer, tokenEndpoint, revokeEndpoint, discoverErr := oauthclient.DiscoverAuthServer(ctx, opts.HTTPClient, session.HostURL)
	if discoverErr != nil {
		log.Warn("the bidder could not discover the oauth auth server", "err", discoverErr.Error())
		return
	}
	session.AuthServerURL = authServer
	session.AuthServerTokenEndpoint = tokenEndpoint
	if revokeEndpoint != "" {
		session.AuthServerRevocationEndpoint = revokeEndpoint
	}
	if opts.Store != nil {
		_ = opts.Store.Save(ctx, session)
	}
}

func (s *Service) credentialTokens() pdsclient.TokenSource {
	if s.credentials == nil || strings.TrimSpace(s.pdsURL) == "" {
		return nil
	}
	tokens := &credentialPDSTokens{credentials: s.credentials, pdsURL: s.pdsURL, client: s.pdsHTTPClient()}
	if store, ok := s.credentials.(component.CredentialStore); ok {
		tokens.writable = store.Writable()
	}
	return tokens
}

type credentialPDSTokens struct {
	mu sync.Mutex

	credentials participant.Credentials

	writable bool

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
	data, err := t.credentials.Read(ctx, true)
	if err != nil {
		if t.access != "" {
			return t.access, nil
		}
		return "", err
	}
	access := strs.Lookup(data, "accessJwt", "accessToken", "access_token", "token", "value")
	refresh := strs.Lookup(data, "refreshJwt", "refreshToken", "refresh_token")
	if pdsclient.TokenLive(access) || refresh == "" {
		t.access = access
		return access, nil
	}
	remote, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: t.pdsURL, HTTPClient: t.client, Timeout: DefaultCallTimeout})
	if err != nil {
		return "", err
	}
	session, err := remote.RefreshSession(ctx, refresh)
	if err != nil {
		if t.access != "" {
			return t.access, nil
		}
		return "", err
	}
	t.access = session.AccessJWT
	if t.writable {
		updated := maps.Clone(data)
		updated["accessJwt"] = session.AccessJWT
		updated["refreshJwt"] = session.RefreshJWT
		updated["access_token"] = session.AccessJWT
		updated["refresh_token"] = session.RefreshJWT
		if raw := strs.Lookup(data, "session"); raw != "" {
			var stored oauthclient.SessionData
			if json.Unmarshal([]byte(raw), &stored) == nil {
				stored.AccessToken = session.AccessJWT
				stored.RefreshToken = session.RefreshJWT
				if encoded, err := json.Marshal(stored); err == nil {
					updated["session"] = string(encoded)
				}
			}
		}
		_ = t.credentials.Write(ctx, updated)
	}
	return t.access, nil
}

func (s *Service) pdsAuth() xrpc.Auth {
	var refresh xrpc.TokenFunc
	if refresher, ok := s.pdsTokens.(pdsclient.TokenRefresher); ok {
		refresh = refresher.RefreshAccessToken
	}
	source := s.pdsTokens
	tokens := xrpc.Refreshing(func(ctx context.Context) (string, error) {
		token, err := source.AccessToken(ctx)
		if err != nil {
			return "", err
		}
		if token = strings.TrimSpace(token); token == "" {
			return "", errors.New("the bidder has no PDS access token")
		}
		return token, nil
	}, refresh)
	if s.pdsDPoP == nil {
		return &xrpc.Bearer{Tokens: tokens}
	}
	dpop := s.pdsDPoP
	return xrpc.StatusRetry(&xrpc.DPoP{Tokens: tokens, Prover: xrpc.DPoPProverFunc(func(ctx context.Context, method, target, accessToken, _ string) (xrpc.DPoPProof, error) {
		proof, err := dpop.DPoPProof(ctx, method, target, accessToken)
		return xrpc.DPoPProof{Proof: proof}, err
	})})
}

func (s *Service) pdsEndpoint() string {
	raw := strings.TrimSpace(s.pdsURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimRight(raw, "/")
}

func (s *Service) pdsHTTPClient() *http.Client {
	if s.pdsInternal && s.deps.Bus != nil {
		return s.deps.Bus.HTTPClient()
	}
	return s.egress.Client(DefaultCallTimeout)
}
