package atproto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	DefaultOAuthScope = "atproto"

	QRPollInterval = 3 * time.Second

	browserPendingTTL = 15 * time.Minute
)

type OAuthOptions struct {
	ClientID string

	RedirectURI string

	Scopes []string

	QRScopes []string

	QROrigin string

	Directory identity.Directory

	HTTPClient *http.Client

	States oauthclient.StateStore

	StateRef *commonv1.SecretRef

	CLIKey atcrypto.PrivateKey

	CLIDID string

	QRSigner oauthclient.ServiceAuthSigner
}

type qrTransfer struct {
	transfer *oauthclient.QRTransfer

	url string

	expires time.Time
}

var errNoSessionRef = errors.New("marketidentity: the identity has no sessionRef")

func (a *Accounts) oauthHTTP() *http.Client {
	if a.oauth.HTTPClient != nil {
		return a.oauth.HTTPClient
	}
	return a.http
}

func (a *Accounts) oauthScopes() []string {
	if len(a.oauth.Scopes) > 0 {
		return append([]string(nil), a.oauth.Scopes...)
	}
	return []string{DefaultOAuthScope}
}

func (a *Accounts) qrScopes() []string {
	if len(a.oauth.QRScopes) > 0 {
		return append([]string(nil), a.oauth.QRScopes...)
	}
	return a.oauthScopes()
}

func (a *Accounts) scopesFor(mode v1alpha1.MarketIdentityMode) []string {
	if mode == v1alpha1.MarketIdentityModeOAuthQr {
		return a.qrScopes()
	}
	return a.oauthScopes()
}

func (a *Accounts) statesFor(workspace string) oauthclient.StateStore {
	if a.oauth.States == nil && a.oauth.StateRef != nil {
		if vault := a.vaultFor(workspace); vault != nil {
			return &VaultStateStore{Vault: vault, Ref: *a.oauth.StateRef}
		}
	}
	return a.states
}

func (a *Accounts) oauthBrowser(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	session, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		return a.sessionSignedIn(ctx, status, generation, id, session, id.Spec.Handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		return failed(status, generation, "MissingSessionRef", "mode oauthBrowser needs spec.sessionRef for the oauth session; no secret is written to status"), nil
	}
	expired := oauthExpired(err)
	if !errors.Is(err, oauthclient.ErrNoSession) && !expired {
		return pending(status, generation, "SessionError", "restoring the oauth session: "+err.Error()), err
	}
	kind := v1alpha1.PendingActionVisitUrl
	phase := v1alpha1.MarketIdentityPhaseAwaitingAuthorization
	if expired {
		kind = v1alpha1.PendingActionSignInAgain
		phase = v1alpha1.MarketIdentityPhaseSessionExpired
	}
	now := time.Now()
	status.Phase = phase
	status.ObservedGeneration = generation
	status.SessionRef = ref
	status.Message = pendingMessage(phase)
	setReady(&status, generation, false, string(phase), status.Message)
	if expires, ok := pendingExpiry(id.Status.PendingAction, kind, now); ok {
		return Result{Status: status, RequeueAfter: untilOrZero(expires, now)}, nil
	}
	visit, expires, err := a.beginBrowserAuthorization(ctx, workspace, id)
	if err != nil {
		return failed(status, generation, "OAuthBeginFailed", err.Error()), err
	}
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: kind, URL: visit, ExpiresAt: expires.UTC().Format(time.RFC3339)}
	return Result{Status: status, RequeueAfter: time.Until(expires)}, nil
}

func (a *Accounts) oauthQr(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	key := qrKey(workspace, id.Name)
	session, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		a.qr.Delete(key)
		return a.sessionSignedIn(ctx, status, generation, id, session, id.Spec.Handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		return failed(status, generation, "MissingSessionRef", "mode oauthQr needs spec.sessionRef for the oauth session; no secret is written to status"), nil
	}
	expired := oauthExpired(err)
	if !errors.Is(err, oauthclient.ErrNoSession) && !expired {
		return pending(status, generation, "SessionError", "restoring the oauth session: "+err.Error()), err
	}
	status.SessionRef = ref
	qr, ok := a.qr.Get(key)
	if !ok || !time.Now().Before(qr.expires) {
		qr, err = a.newQrState(key)
		if err != nil {
			return failed(status, generation, "OAuthBeginFailed", err.Error()), nil
		}
		status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionScanQr, URL: qr.url,
			ExpiresAt: qr.expires.UTC().Format(time.RFC3339)}
		if expired {
			status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
			status.PendingAction.Kind = v1alpha1.PendingActionSignInAgain
			status.Message = pendingMessage(status.Phase)
			setReady(&status, generation, false, string(status.Phase), status.Message)
		} else {
			status.Phase = v1alpha1.MarketIdentityPhaseAwaitingScan
			status.Message = pendingMessage(status.Phase)
			setReady(&status, generation, false, "AwaitingScan", status.Message)
		}
		status.ObservedGeneration = generation
		return Result{Status: status, RequeueAfter: QRPollInterval}, nil
	}
	sessionQr, done, err := qr.transfer.PollOnce(ctx)
	if err != nil {
		if errors.Is(err, oauthclient.ErrQRConsumed) || errors.Is(err, oauthclient.ErrQRTimeout) {
			a.qr.Delete(key)
		}
		return pending(status, generation, "QRPollFailed", err.Error()), err
	}
	if !done {
		return Result{Status: qrPending(status, generation, qr), RequeueAfter: QRPollInterval}, nil
	}
	a.qr.Delete(key)
	data, err := sessionQr.SessionData(a.sessionClientID(id), a.qrScopes())
	if err != nil {
		return failed(status, generation, "QRSessionInvalid", err.Error()), err
	}
	data = a.sessionWithEndpoints(ctx, data)
	vault := a.vaultFor(workspace)
	if err := saveSession(ctx, vault, ref, data); err != nil {
		return pending(status, generation, "SessionStoreFailed", err.Error()), err
	}
	session, err = a.openSessionData(ctx, vault, id, data, ref)
	if err != nil {
		return pending(status, generation, "SessionRestoreFailed", err.Error()), err
	}
	return a.sessionSignedIn(ctx, status, generation, id, session, firstNonEmpty(sessionQr.Handle, id.Spec.Handle), ref), nil
}

func qrPending(status v1alpha1.MarketIdentityStatus, generation int64, qr *qrTransfer) v1alpha1.MarketIdentityStatus {
	status.Phase = v1alpha1.MarketIdentityPhaseAwaitingScan
	status.ObservedGeneration = generation
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionScanQr, URL: qr.url,
		ExpiresAt: qr.expires.UTC().Format(time.RFC3339)}
	status.Message = pendingMessage(status.Phase)
	setReady(&status, generation, false, "AwaitingScan", status.Message)
	return status
}

func (a *Accounts) oauthDelegated(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	session, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		return a.sessionSignedIn(ctx, status, generation, id, session, id.Spec.Handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) {
		return pending(status, generation, "AwaitingSession", "mode oauthDelegated waits for spec.sessionRef"), nil
	}
	if oauthExpired(err) {
		status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
		status.ObservedGeneration = generation
		status.SessionRef = ref
		status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionSignInAgain, Handle: id.Spec.Handle}
		status.Message = pendingMessage(status.Phase)
		setReady(&status, generation, false, "SessionExpired", status.Message)
		return Result{Status: status}, nil
	}
	return pending(status, generation, "SessionError", "restoring the oauth session: "+err.Error()), err
}

func (a *Accounts) sessionClientID(id *v1alpha1.MarketIdentity) string {
	if id != nil && id.Spec.Mode == v1alpha1.MarketIdentityModeOAuthQr {
		if clientID := oauthclient.QRClientID(a.oauth.QROrigin); clientID != "" {
			return clientID
		}
	}
	return a.oauthClientID(id)
}

func (a *Accounts) oauthClientID(id *v1alpha1.MarketIdentity) string {
	if id != nil && strings.TrimSpace(id.Spec.OAuthClientID) != "" {
		return strings.TrimSpace(id.Spec.OAuthClientID)
	}
	return strings.TrimSpace(a.oauth.ClientID)
}

func (a *Accounts) oauthRedirectURI(id *v1alpha1.MarketIdentity) string {
	if id != nil && strings.TrimSpace(id.Spec.OAuthRedirectURI) != "" {
		return strings.TrimSpace(id.Spec.OAuthRedirectURI)
	}
	return strings.TrimSpace(a.oauth.RedirectURI)
}

func (a *Accounts) beginBrowserAuthorization(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (string, time.Time, error) {
	clientID := a.oauthClientID(id)
	redirectURI := a.oauthRedirectURI(id)
	if clientID == "" {
		return "", time.Time{}, errors.New("marketidentity: mode oauthBrowser needs an oauth clientID")
	}
	if redirectURI == "" {
		return "", time.Time{}, errors.New("marketidentity: mode oauthBrowser needs an oauth redirectURI")
	}
	pdsURL, err := a.oauthPDSURL(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}
	client := a.oauthHTTP()
	authServer, tokenEndpoint, _, err := oauthclient.DiscoverAuthServer(ctx, client, pdsURL)
	if err != nil {
		return "", time.Time{}, err
	}
	var metadata struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := oauthclient.GetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", time.Time{}, fmt.Errorf("marketidentity: the auth server authorization metadata: %w", err)
	}
	if strings.TrimSpace(metadata.AuthorizationEndpoint) == "" {
		return "", time.Time{}, errors.New("marketidentity: the auth server metadata names no authorization endpoint")
	}
	authorize, err := oauthclient.EndpointURL(authServer, metadata.AuthorizationEndpoint)
	if err != nil {
		return "", time.Time{}, err
	}
	state, err := oauthclient.NewCallbackState()
	if err != nil {
		return "", time.Time{}, err
	}
	verifier, challenge, err := oauthclient.GeneratePKCE()
	if err != nil {
		return "", time.Time{}, err
	}
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return "", time.Time{}, err
	}
	scopes := a.oauthScopes()
	pendingAuth := oauthclient.PendingAuth{
		State:                   state,
		CodeVerifier:            verifier,
		RedirectURI:             redirectURI,
		ClientID:                clientID,
		HostURL:                 pdsURL,
		Issuer:                  metadata.Issuer,
		AuthServerURL:           authServer,
		TokenEndpoint:           tokenEndpoint,
		Scopes:                  scopes,
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	if err := a.statesFor(workspace).Put(ctx, pendingAuth); err != nil {
		return "", time.Time{}, err
	}
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if handle := strings.TrimSpace(id.Spec.Handle); handle != "" {
		query.Set("login_hint", handle)
	}
	parsed, err := url.Parse(authorize)
	if err != nil {
		return "", time.Time{}, err
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), time.Now().Add(browserPendingTTL), nil
}

func (a *Accounts) CompleteBrowserAuthorization(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, r *http.Request) error {
	if id.Spec.Mode != v1alpha1.MarketIdentityModeOAuthBrowser {
		return fmt.Errorf("marketidentity: MarketIdentity %s is mode %s, not oauthBrowser", id.Name, id.Spec.Mode)
	}
	ref := oauthSessionRef(id, &id.Status)
	if ref == nil {
		return errNoSessionRef
	}
	vault := a.vaultFor(workspace)
	if vault == nil {
		return errors.New("marketidentity: no vault writes the oauth session")
	}
	data, err := oauthclient.ExchangeCallback(ctx, a.statesFor(workspace), r, a.oauthHTTP())
	if err != nil {
		a.log.Info("oauth callback exchange failed", "workspace", workspace, "identity", id.Name, "err", err.Error())
		return err
	}
	return saveSession(ctx, vault, ref, data)
}

func (a *Accounts) oauthPDSURL(ctx context.Context, id *v1alpha1.MarketIdentity) (string, error) {
	if raw := strings.TrimSpace(id.Spec.PdsURL); raw != "" {
		return raw, nil
	}
	handle := strings.TrimSpace(id.Spec.Handle)
	if handle == "" || a.oauth.Directory == nil {
		return "", errors.New("marketidentity: the oauth mode names neither pdsUrl nor a resolvable handle")
	}
	atid, err := syntax.ParseAtIdentifier(handle)
	if err != nil {
		return "", fmt.Errorf("marketidentity: the oauth handle %q: %w", handle, err)
	}
	ident, err := a.oauth.Directory.Lookup(ctx, atid)
	if err != nil {
		return "", fmt.Errorf("marketidentity: resolving the oauth handle %q: %w", handle, err)
	}
	pds := strings.TrimSpace(ident.PDSEndpoint())
	if pds == "" {
		return "", fmt.Errorf("marketidentity: the identity %s names no PDS", ident.DID.String())
	}
	return pds, nil
}

func (a *Accounts) restoreOAuthSession(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity,
	status *v1alpha1.MarketIdentityStatus) (*oauthclient.Session, *commonv1.SecretRef, error) {
	ref := oauthSessionRef(id, status)
	if ref == nil {
		return nil, nil, errNoSessionRef
	}
	vault := a.vaultFor(workspace)
	if vault == nil {
		return nil, ref, errors.New("marketidentity: no vault reads the oauth session")
	}
	store := &vaultSessionStore{vault: vault, ref: *ref}
	session, err := oauthclient.Open(ctx, oauthclient.Options{
		DID:        status.Did,
		Handle:     id.Spec.Handle,
		RepoDID:    status.Did,
		PDSURL:     id.Spec.PdsURL,
		ClientID:   a.sessionClientID(id),
		Scopes:     a.scopesFor(id.Spec.Mode),
		Directory:  a.oauth.Directory,
		HTTPClient: a.oauthHTTP(),
		Store:      store,
	})
	if errors.Is(err, oauthclient.ErrInvalidSession) {
		_ = vault.Delete(ctx, *ref)
		return nil, ref, oauthclient.ErrNoSession
	}
	return session, ref, err
}

func (a *Accounts) sessionWithEndpoints(ctx context.Context, data oauthclient.SessionData) oauthclient.SessionData {
	if data.AuthServerURL != "" && data.AuthServerTokenEndpoint != "" {
		return data
	}
	authServer, tokenEndpoint, revokeEndpoint, err := oauthclient.DiscoverAuthServer(ctx, a.oauthHTTP(), data.HostURL)
	if err != nil {
		a.log.Warn("oauth_session_endpoints_not_discovered", "did", data.AccountDID, "err", err.Error())
		return data
	}
	data.AuthServerURL = authServer
	data.AuthServerTokenEndpoint = tokenEndpoint
	if revokeEndpoint != "" {
		data.AuthServerRevocationEndpoint = revokeEndpoint
	}
	return data
}

func (a *Accounts) openSessionData(ctx context.Context, vault SecretStore, id *v1alpha1.MarketIdentity, data oauthclient.SessionData,
	ref *commonv1.SecretRef) (*oauthclient.Session, error) {
	session, err := oauthclient.New(data, oauthclient.Options{
		DID:        data.AccountDID,
		Handle:     id.Spec.Handle,
		RepoDID:    data.AccountDID,
		PDSURL:     data.HostURL,
		ClientID:   a.sessionClientID(id),
		Scopes:     a.scopesFor(id.Spec.Mode),
		Directory:  a.oauth.Directory,
		HTTPClient: a.oauthHTTP(),
		Store:      &vaultSessionStore{vault: vault, ref: *ref},
	})
	if err != nil {
		return nil, err
	}
	_ = session.EnsureEndpoints(ctx)
	if _, err := session.AccessToken(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func saveSession(ctx context.Context, vault SecretStore, ref *commonv1.SecretRef, data oauthclient.SessionData) error {
	if ref == nil {
		return errNoSessionRef
	}
	if vault == nil {
		return errors.New("marketidentity: no vault writes the oauth session")
	}
	return (&vaultSessionStore{vault: vault, ref: *ref}).Save(ctx, data)
}

func (a *Accounts) sessionSignedIn(ctx context.Context, status v1alpha1.MarketIdentityStatus, generation int64, id *v1alpha1.MarketIdentity,
	session *oauthclient.Session, handle string, ref *commonv1.SecretRef) Result {
	token, _ := session.AccessToken(ctx)
	return signedIn(status, generation, id, session.DID(), handle, session.HostURL(), ref, token)
}

func signedIn(status v1alpha1.MarketIdentityStatus, generation int64, id *v1alpha1.MarketIdentity, did, handle, pdsURL string,
	ref *commonv1.SecretRef, token string) Result {
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	status.Did = strings.TrimSpace(did)
	status.Handle = firstNonEmpty(handle, id.Spec.Handle)
	status.PdsURL = strings.TrimSpace(pdsURL)
	status.SessionRef = ref
	status.Associations = nil
	status.PendingAction = nil
	status.Message = ""
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, true, "SignedIn", "the account session is live; the status carries no token")
	setReady(&status, generation, true, "SignedIn", status.Did)
	return Result{Status: status, RequeueAfter: refreshAfter(token)}
}

func oauthExpired(err error) bool {
	return errors.Is(err, oauthclient.ErrSessionExpired) || errors.Is(err, oauthclient.ErrNoRefreshToken)
}

func oauthSessionRef(id *v1alpha1.MarketIdentity, status *v1alpha1.MarketIdentityStatus) *commonv1.SecretRef {
	if id.Spec.SessionRef != nil {
		return id.Spec.SessionRef
	}
	return status.SessionRef
}

func pendingExpiry(action *v1alpha1.MarketIdentityPendingAction, kind v1alpha1.PendingActionKind, now time.Time) (time.Time, bool) {
	if action == nil || action.Kind != kind || strings.TrimSpace(action.URL) == "" {
		return time.Time{}, false
	}
	if action.ExpiresAt == "" {
		return time.Time{}, true
	}
	expires, err := time.Parse(time.RFC3339, action.ExpiresAt)
	if err != nil || !now.Before(expires) {
		return time.Time{}, false
	}
	return expires, true
}

func untilOrZero(at, now time.Time) time.Duration {
	if at.IsZero() {
		return 0
	}
	return at.Sub(now)
}

func pendingMessage(phase v1alpha1.MarketIdentityPhase) string {
	switch phase {
	case v1alpha1.MarketIdentityPhaseSessionExpired:
		return "the account session expired; sign in again"
	case v1alpha1.MarketIdentityPhaseAwaitingAuthorization:
		return "open the visit url to authorize the account"
	case v1alpha1.MarketIdentityPhaseAwaitingScan:
		return "scan the qr url to sign in"
	}
	return string(phase)
}

func qrKey(workspace, name string) string {
	return workspace + "/" + name
}

func (a *Accounts) newQrState(key string) (*qrTransfer, error) {
	did, signer, err := a.qrSigner()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.oauth.QROrigin) == "" {
		return nil, errors.New("marketidentity: mode oauthQr needs an oauth qrOrigin")
	}
	nonce, err := oauthclient.NewQRNonce()
	if err != nil {
		return nil, err
	}
	transfer, err := oauthclient.NewQRTransfer(a.oauth.QROrigin, did, nonce, signer)
	if err != nil {
		return nil, err
	}
	transfer.HTTPClient = a.oauthHTTP()
	transfer.TTL = opt.Positive(transfer.TTL, oauthclient.DefaultQRTTL)
	qrURL, err := transfer.URL()
	if err != nil {
		return nil, err
	}
	qr := &qrTransfer{transfer: transfer, url: qrURL, expires: time.Now().Add(transfer.TTL)}
	a.qr.SetTTL(key, qr, transfer.TTL)
	return qr, nil
}

func (a *Accounts) qrSigner() (string, oauthclient.ServiceAuthSigner, error) {
	if a.oauth.CLIKey != nil {
		public, err := a.oauth.CLIKey.PublicKey()
		if err != nil {
			return "", nil, err
		}
		did := public.DIDKey()
		return did, oauthclient.KeySigner{Issuer: syntax.DID(did), Key: a.oauth.CLIKey}, nil
	}
	if a.oauth.QRSigner != nil && strings.TrimSpace(a.oauth.CLIDID) != "" {
		return strings.TrimSpace(a.oauth.CLIDID), a.oauth.QRSigner, nil
	}
	return "", nil, errors.New("marketidentity: mode oauthQr needs a CLI key or a qr signer and cliDID")
}
