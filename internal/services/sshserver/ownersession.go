package sshserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/ownersession"
)

func (s *Service) openOwner(did string, session ownersession.Session) (ownersession.Repo, error) {
	owner, err := syntax.ParseDID(did)
	if err != nil {
		return nil, fmt.Errorf("sshserver: the owner DID: %w", err)
	}
	client := s.httpClient(requestTimeout)
	tokens := &ownerTokens{did: owner, session: session, client: client}
	tokens.access = lease.New(tokens.login)
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    session.Service,
		DID:        owner,
		HTTPClient: client,
		Tokens:     tokens,
		Timeout:    requestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("sshserver: the owner PDS %s: %w", session.Service, err)
	}
	return remote.Wrap(pds, owner), nil
}

type ownerTokens struct {
	did syntax.DID

	session ownersession.Session

	client *http.Client

	access *lease.Source[string]
}

func (t *ownerTokens) AccessToken(ctx context.Context) (string, error) {
	return t.access.Get(ctx)
}

func (t *ownerTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.access.Invalidate()
	return t.access.Get(ctx)
}

func (t *ownerTokens) login(ctx context.Context) (string, time.Time, error) {
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    t.session.Service,
		DID:        t.did,
		HTTPClient: t.client,
		Timeout:    requestTimeout,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sshserver: the owner PDS %s: %w", t.session.Service, err)
	}
	session, err := pds.CreateSession(ctx, t.session.Identifier, t.session.Password)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sshserver: the owner session of %s: %w", t.did, err)
	}
	expires, ok := pdsclient.TokenExpiry(session.AccessJWT)
	if !ok {
		return session.AccessJWT, time.Now(), nil
	}
	return session.AccessJWT, expires.Add(-pdsclient.TokenExpirySkew), nil
}
