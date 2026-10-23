package requester

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

func (q *Requester) openOAuthSession(ctx context.Context) (*oauthclient.Session, error) {
	if stored := q.cfg.OAuthSession; stored != nil {
		data := *stored
		if data.ClientID == "" {
			data.ClientID = strings.TrimSpace(q.cfg.OAuthClientID)
		}
		opts := oauthclient.Options{
			DID:        data.AccountDID,
			RepoDID:    data.AccountDID,
			PDSURL:     data.HostURL,
			ClientID:   q.cfg.OAuthClientID,
			Scopes:     q.cfg.OAuthScopes,
			Directory:  q.deps.Directory,
			HTTPClient: q.deps.HTTP,
		}
		if q.deps.SaveSessions && q.deps.Sessions != nil {
			opts.Store = q.deps.Sessions
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
	path := q.cfg.stateFile(DefaultOAuthSessionFile)
	if path == "" {
		return nil, errors.New("requester: oauth identity mode has no token store path")
	}
	session, err := oauthclient.OpenFile(ctx, path, oauthclient.Options{
		DID:        q.did,
		Handle:     q.handle,
		RepoDID:    q.repoDID,
		PDSURL:     q.pdsURL,
		ClientID:   q.cfg.OAuthClientID,
		Scopes:     q.cfg.OAuthScopes,
		Directory:  q.deps.Directory,
		HTTPClient: q.deps.HTTP,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("requester: no oauth token store at %s", path)
		}
		return nil, err
	}
	return session, nil
}

func (q *Requester) authenticateOwnerSession(ctx context.Context) error {
	session, ok := q.cfg.OwnerSessions[q.repoDID]
	if !ok {
		return fmt.Errorf("requester: the owner sessions file names no account %s", q.repoDID)
	}
	did, err := syntax.ParseDID(q.repoDID)
	if err != nil {
		return fmt.Errorf("requester: owner DID: %w", err)
	}
	if _, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    session.Service,
		DID:        did,
		HTTPClient: q.deps.HTTP,
		Timeout:    DefaultHTTPTimeout,
	}); err != nil {
		return fmt.Errorf("requester: the owner PDS %s: %w", session.Service, err)
	}
	tokens := newPDSAccountTokens(session.Service, session.Identifier, session.Password, q.deps.HTTP)
	if _, err := tokens.RefreshAccessToken(ctx); err != nil {
		return fmt.Errorf("requester: the owner session of %s: %w", q.repoDID, err)
	}
	q.pdsURL = session.Service
	q.ownerServiceAuth = newOwnerSessionServiceAuth(session.Service, tokens, q.deps.HTTP)
	return nil
}

func (q *Requester) bindSignerKey(ctx context.Context) error {
	if q.repo == nil || q.signer == nil {
		return errors.New("requester: the signer binding has no repo or signer")
	}
	key, err := q.signer.PublicDID()
	if err != nil {
		return fmt.Errorf("requester: the signer key: %w", err)
	}
	rkey := market.BadgeBlueKeysRkey(q.repoDID, key)
	if _, err := market.CreateBadgeBlueKeysRecord(ctx, q.repo, market.BadgeBlueKeysRecordOptions{
		DID:       q.repoDID,
		KeyID:     key,
		Service:   ownerSessionSigningService,
		CreatedAt: q.nowISO(),
	}); err != nil {
		return fmt.Errorf("requester: publishing the signer key binding: %w", err)
	}
	q.log.Info("requester_signer_key_bound", "did", q.repoDID, "key", key, "rkey", rkey)
	return nil
}

func serviceAuthName(source ServiceAuthSource) string {
	switch source.(type) {
	case nil:
		return "local"
	case *oauthclient.Session:
		return "oauth"
	case *ownerSessionServiceAuth:
		return "owner-session"
	}
	return "other"
}

type ownerSessionServiceAuth struct {
	minter xrpc.PDSMinter
}

var (
	_ ServiceAuthSource        = (*ownerSessionServiceAuth)(nil)
	_ ServiceAuthSource        = (*oauthclient.Session)(nil)
	_ pdsclient.TokenRefresher = (*pdsAccountTokens)(nil)
)

func newOwnerSessionServiceAuth(pdsURL string, tokens pdsclient.TokenSource, client *http.Client) *ownerSessionServiceAuth {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	var refresh xrpc.TokenFunc
	if refresher, ok := tokens.(pdsclient.TokenRefresher); ok {
		refresh = refresher.RefreshAccessToken
	}
	auth := &xrpc.Bearer{Tokens: xrpc.Refreshing(tokens.AccessToken, refresh)}
	return &ownerSessionServiceAuth{minter: xrpc.PDSMinter{Client: &xrpc.Client{Host: pdsURL, HTTP: client, Auth: auth}}}
}

func (a *ownerSessionServiceAuth) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	token, err := a.minter.ServiceAuth(ctx, audience, lxm)
	if err != nil {
		return "", fmt.Errorf("requester: getServiceAuth: %w", err)
	}
	return token, nil
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
