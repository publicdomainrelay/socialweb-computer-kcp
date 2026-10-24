package requester

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

func (q *Requester) resolveOAuthIdentity(ctx context.Context) error {
	ref := q.cfg.Spec.Identity.MarketIdentityRef
	workspaces := IdentityWorkspaces(q.cfg.Workspace)
	found, ok := q.deps.Inputs.AwaitIdentity(ctx, workspaces, ref.Name, IdentityWait)
	if !ok {
		return fmt.Errorf("requester: reading its MarketIdentity %s: not found", ref.Name)
	}
	did := strings.TrimSpace(found.Status.Did)
	if did == "" {
		return fmt.Errorf("requester: its MarketIdentity %s is not signed in (phase %s)", ref.Name, found.Status.Phase)
	}
	for _, ws := range workspaces {
		if _, has := q.deps.Inputs.Identity(ws, ref.Name); has {
			q.identityRef = commonv1.ObjectRef{Name: ref.Name, Workspace: ws}
			break
		}
	}
	q.did, q.repoDID, q.marketDID = did, did, did
	q.pdsURL = cmp.Or(q.pdsURL, strings.TrimSpace(found.Status.PdsURL))
	q.handle = cmp.Or(q.handle, found.Status.Handle)
	return nil
}

func (q *Requester) oauthServiceAuth() ServiceAuthSource {
	if q.deps.ServiceAuth == nil {
		return nil
	}
	identity, did, open := q.identityRef, q.did, q.deps.ServiceAuth
	return participant.PDSServiceAuth{DID: did, Open: func(ctx context.Context) (pds.ServiceAuth, error) {
		return open(ctx, identity, did)
	}}
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
	case participant.PDSServiceAuth:
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
	_ ServiceAuthSource        = participant.PDSServiceAuth{}
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
