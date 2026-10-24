package atproto

import (
	"cmp"
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
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
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
	key := qrKey(workspace, id.Name)
	session, handle, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		a.browser.Delete(key)
		return a.sessionSignedIn(ctx, status, generation, session, handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		return failed(status, generation, "MissingSessionRef", "mode oauthBrowser needs spec.sessionRef for the oauth session; no secret is written to status"), nil
	}
	need, ok := signInNeeded(&status, err)
	if !ok {
		return pending(status, generation, sessionErrorReason(err), "restoring the oauth session: "+err.Error()), err
	}
	kind := v1alpha1.PendingActionVisitUrl
	now := time.Now()
	status.SessionRef = ref
	issue, issued := a.browser.Get(key)
	if issued && issue.failure != "" && !strings.Contains(need.message, issue.failure) {
		need.message = cmp.Or(need.message, pendingMessage(v1alpha1.MarketIdentityPhaseAwaitingAuthorization)) + "; the last sign-in did not complete: " + issue.failure
	}
	if need.again {
		kind = v1alpha1.PendingActionSignInAgain
		signInAgainStatus(&status, generation, need.reason, need.message)
	} else {
		status.Phase = v1alpha1.MarketIdentityPhaseAwaitingAuthorization
		status.ObservedGeneration = generation
		status.Message = cmp.Or(need.message, pendingMessage(status.Phase))
		reason := cmp.Or(need.reason, string(status.Phase))
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, false, reason, status.Message)
		setReady(&status, generation, false, reason, status.Message)
	}
	if expires, ok := pendingExpiry(id.Status.PendingAction, kind, now); ok && a.browserIssueLive(ctx, workspace, id, generation, issue, issued) {
		return Result{Status: status, RequeueAfter: untilOrZero(expires, now)}, nil
	}
	if issued && !issue.spent {
		a.spendBrowserIssue(ctx, workspace, id, issue.state, nil)
	}
	visit, state, expires, err := a.beginBrowserAuthorization(ctx, workspace, id)
	if err != nil {
		return failed(status, generation, "OAuthBeginFailed", err.Error()), err
	}
	a.browser.SetTTL(key, browserIssue{state: state, url: visit, generation: generation, account: accountKey(id), failure: issue.failure}, browserPendingTTL)
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: kind, URL: visit, Handle: strings.TrimSpace(id.Spec.Handle),
		ExpiresAt: expires.UTC().Format(time.RFC3339)}
	return Result{Status: status, RequeueAfter: time.Until(expires)}, nil
}

type browserIssue struct {
	state string

	url string

	generation int64

	account string

	failure string

	spent bool
}

func (a *Accounts) browserIssueLive(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64, issue browserIssue, issued bool) bool {
	action := id.Status.PendingAction
	if !issued || issue.spent || action == nil || issue.url != action.URL || issue.generation != generation || issue.account != accountKey(id) {
		return false
	}
	live, err := a.statesFor(workspace).Has(ctx, issue.state)
	if err != nil {
		a.log.Warn("the oauth callback state is not readable; the visit url stays", "workspace", workspace, "identity", id.Name, "err", err.Error())
		return true
	}
	return live
}

func (a *Accounts) spendBrowserIssue(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, state string, failure error) {
	key := qrKey(workspace, id.Name)
	issue, ok := a.browser.Get(key)
	if !ok || (state != "" && state != issue.state) {
		return
	}
	if _, _, err := a.statesFor(workspace).Take(ctx, issue.state); err != nil {
		a.log.Warn("the spent oauth callback state is not removed", "workspace", workspace, "identity", id.Name, "err", err.Error())
	}
	issue.spent = true
	if failure != nil {
		issue.failure = strings.TrimPrefix(strings.TrimPrefix(failure.Error(), "marketidentity: "), "oauthclient: ")
	}
	a.browser.SetTTL(key, issue, browserPendingTTL)
}

type signInNeed struct {
	again bool

	reason string

	message string
}

func signInNeeded(status *v1alpha1.MarketIdentityStatus, err error) (signInNeed, bool) {
	var mismatch *accountMismatchError
	if errors.As(err, &mismatch) {
		status.Did, status.Handle, status.PdsURL = "", "", ""
		return signInNeed{reason: "AccountMismatch", message: mismatch.text}, true
	}
	if reason, message, again := signInAgainReason(err); again {
		return signInNeed{again: true, reason: reason, message: message}, true
	}
	if !errors.Is(err, oauthclient.ErrNoSession) {
		return signInNeed{}, false
	}
	if signedOut(status) {
		return signInNeed{reason: v1alpha1.ReasonSignedOut, message: signedOutMessage}, true
	}
	return signInNeed{}, true
}

func sessionErrorReason(err error) string {
	var unverified *accountUnverifiedError
	if errors.As(err, &unverified) {
		return "AccountUnverified"
	}
	return "SessionError"
}

func (a *Accounts) oauthQr(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	key := qrKey(workspace, id.Name)
	session, handle, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		a.qr.Delete(key)
		return a.sessionSignedIn(ctx, status, generation, session, handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		return failed(status, generation, "MissingSessionRef", "mode oauthQr needs spec.sessionRef for the oauth session; no secret is written to status"), nil
	}
	need, ok := signInNeeded(&status, err)
	if !ok {
		return pending(status, generation, sessionErrorReason(err), "restoring the oauth session: "+err.Error()), err
	}
	status.SessionRef = ref
	qr, ok := a.qr.Get(key)
	if !ok || !time.Now().Before(qr.expires) {
		qr, err = a.newQrState(key)
		if err != nil {
			return failed(status, generation, "OAuthBeginFailed", err.Error()), nil
		}
		return Result{Status: qrPending(status, generation, qr, need), RequeueAfter: QRPollInterval}, nil
	}
	sessionQr, done, err := qr.transfer.PollOnce(ctx)
	if err != nil {
		if errors.Is(err, oauthclient.ErrQRConsumed) || errors.Is(err, oauthclient.ErrQRTimeout) {
			a.qr.Delete(key)
		}
		return pending(status, generation, "QRPollFailed", err.Error()), err
	}
	if !done {
		return Result{Status: qrPending(status, generation, qr, need), RequeueAfter: QRPollInterval}, nil
	}
	a.qr.Delete(key)
	data, err := sessionQr.SessionData(a.sessionClientID(id), a.qrScopes())
	if err != nil {
		return failed(status, generation, "QRSessionInvalid", err.Error()), err
	}
	data = a.sessionWithEndpoints(ctx, data)
	session, handle, err = a.openSessionData(ctx, workspace, id, data, ref, sessionQr.Handle)
	if need, ok := signInNeeded(&status, err); ok && err != nil {
		if qr, err = a.newQrState(key); err != nil {
			return failed(status, generation, "OAuthBeginFailed", err.Error()), nil
		}
		return Result{Status: qrPending(status, generation, qr, need), RequeueAfter: QRPollInterval}, nil
	}
	if err != nil {
		return pending(status, generation, "SessionRestoreFailed", err.Error()), err
	}
	return a.sessionSignedIn(ctx, status, generation, session, handle, ref), nil
}

func qrPending(status v1alpha1.MarketIdentityStatus, generation int64, qr *qrTransfer, need signInNeed) v1alpha1.MarketIdentityStatus {
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionScanQr, URL: qr.url,
		ExpiresAt: qr.expires.UTC().Format(time.RFC3339)}
	if need.again {
		status.PendingAction.Kind = v1alpha1.PendingActionSignInAgain
		signInAgainStatus(&status, generation, need.reason, need.message)
		return status
	}
	status.Phase = v1alpha1.MarketIdentityPhaseAwaitingScan
	status.ObservedGeneration = generation
	status.Message = cmp.Or(need.message, pendingMessage(status.Phase))
	reason := cmp.Or(need.reason, "AwaitingScan")
	if need.reason != "" {
		conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, false, reason, status.Message)
	}
	setReady(&status, generation, false, reason, status.Message)
	return status
}

func (a *Accounts) oauthDelegated(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	session, handle, ref, err := a.restoreOAuthSession(ctx, workspace, id, &status)
	if err == nil {
		return a.sessionSignedIn(ctx, status, generation, session, handle, ref), nil
	}
	if errors.Is(err, errNoSessionRef) {
		return pending(status, generation, "AwaitingSession", "mode oauthDelegated waits for spec.sessionRef"), nil
	}
	if need, ok := signInNeeded(&status, err); ok && need.reason != "" {
		status.SessionRef = ref
		status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionSignInAgain, Handle: id.Spec.Handle}
		signInAgainStatus(&status, generation, need.reason, need.message)
		return Result{Status: status}, nil
	}
	return pending(status, generation, sessionErrorReason(err), "restoring the oauth session: "+err.Error()), err
}

func (a *Accounts) pinnedClientID(id *v1alpha1.MarketIdentity) string {
	if id != nil && id.Spec.Mode == v1alpha1.MarketIdentityModeOAuthQr {
		return oauthclient.QRClientID(a.oauth.QROrigin)
	}
	return ""
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

func (a *Accounts) beginBrowserAuthorization(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (string, string, time.Time, error) {
	clientID := a.oauthClientID(id)
	redirectURI := a.oauthRedirectURI(id)
	if clientID == "" {
		return "", "", time.Time{}, errors.New("marketidentity: mode oauthBrowser needs an oauth clientID")
	}
	if redirectURI == "" {
		return "", "", time.Time{}, errors.New("marketidentity: mode oauthBrowser needs an oauth redirectURI")
	}
	pdsURL, err := a.oauthPDSURL(ctx, id)
	if err != nil {
		return "", "", time.Time{}, err
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return "", "", time.Time{}, err
	}
	authServer, tokenEndpoint, _, err := oauthclient.DiscoverAuthServer(ctx, client, pdsURL)
	if err != nil {
		return "", "", time.Time{}, err
	}
	var metadata struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := oauthclient.GetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", "", time.Time{}, fmt.Errorf("marketidentity: the auth server authorization metadata: %w", err)
	}
	if strings.TrimSpace(metadata.AuthorizationEndpoint) == "" {
		return "", "", time.Time{}, errors.New("marketidentity: the auth server metadata names no authorization endpoint")
	}
	authorize, err := oauthclient.EndpointURL(authServer, metadata.AuthorizationEndpoint)
	if err != nil {
		return "", "", time.Time{}, err
	}
	state, err := oauthclient.NewCallbackState()
	if err != nil {
		return "", "", time.Time{}, err
	}
	verifier, challenge, err := oauthclient.GeneratePKCE()
	if err != nil {
		return "", "", time.Time{}, err
	}
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return "", "", time.Time{}, err
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
		return "", "", time.Time{}, err
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
	if hint := cmp.Or(strings.TrimSpace(id.Spec.Did), strings.TrimSpace(id.Spec.Handle)); hint != "" {
		query.Set("login_hint", hint)
	}
	parsed, err := url.Parse(authorize)
	if err != nil {
		return "", "", time.Time{}, err
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), state, time.Now().Add(browserPendingTTL), nil
}

func (a *Accounts) CompleteBrowserAuthorization(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, r *http.Request) error {
	if id.Spec.Mode != v1alpha1.MarketIdentityModeOAuthBrowser {
		return fmt.Errorf("marketidentity: MarketIdentity %s is mode %s, not oauthBrowser", id.Name, id.Spec.Mode)
	}
	err := a.completeBrowserAuthorization(ctx, workspace, id, r)
	if err != nil {
		a.log.Info("the oauth callback failed; the visit url is reissued", "workspace", workspace, "identity", id.Name, "err", err.Error())
		state := ""
		if r != nil && r.URL != nil {
			state = r.URL.Query().Get("state")
		}
		a.spendBrowserIssue(ctx, workspace, id, state, err)
	}
	return err
}

func (a *Accounts) completeBrowserAuthorization(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, r *http.Request) error {
	ref := oauthSessionRef(id, &id.Status)
	if ref == nil {
		return errNoSessionRef
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return err
	}
	data, err := oauthclient.ExchangeCallback(ctx, a.statesFor(workspace), r, client)
	if err != nil {
		return err
	}
	_, _, err = a.openSessionData(ctx, workspace, id, a.sessionWithEndpoints(ctx, data), ref, "")
	return err
}

func (a *Accounts) oauthPDSURL(ctx context.Context, id *v1alpha1.MarketIdentity) (string, error) {
	if raw := strings.TrimSpace(id.Spec.PdsURL); raw != "" {
		return raw, nil
	}
	account := cmp.Or(strings.TrimSpace(id.Spec.Did), strings.TrimSpace(id.Spec.Handle))
	if account == "" || a.oauth.Directory == nil {
		return "", errors.New("marketidentity: the oauth mode names neither pdsUrl nor a resolvable did or handle")
	}
	atid, err := syntax.ParseAtIdentifier(account)
	if err != nil {
		return "", fmt.Errorf("marketidentity: the oauth account %q: %w", account, err)
	}
	ident, err := a.oauth.Directory.Lookup(ctx, atid)
	if err != nil {
		return "", fmt.Errorf("marketidentity: resolving the oauth account %q: %w", account, err)
	}
	pds := strings.TrimSpace(ident.PDSEndpoint())
	if pds == "" {
		return "", fmt.Errorf("marketidentity: the identity %s names no PDS", ident.DID.String())
	}
	return pds, nil
}

func (a *Accounts) restoreOAuthSession(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity,
	status *v1alpha1.MarketIdentityStatus) (*oauthclient.Session, string, *commonv1.SecretRef, error) {
	ref := oauthSessionRef(id, status)
	if ref == nil {
		return nil, "", nil, errNoSessionRef
	}
	held := a.held.get(workspace, id)
	held.open.Lock()
	defer held.open.Unlock()
	if session := held.auth.Session(); session != nil && held.ref == *ref {
		if account := accountKey(id); held.account != account {
			handle, err := a.checkAccount(ctx, id, session.DID(), session.HostURL(), "")
			if err != nil {
				held.auth.Drop()
				held.account = ""
				return nil, "", ref, err
			}
			held.account, held.handle = account, handle
		}
		if _, err := session.AccessToken(ctx); err != nil {
			if oauthExpired(err) {
				held.auth.Drop()
			}
			return nil, "", ref, err
		}
		if err := a.scopeCheck(workspace, id, held, session.Scopes()); err != nil {
			held.auth.Drop()
			return nil, "", ref, err
		}
		return session, held.handle, ref, nil
	}
	vault := a.vaultFor(workspace)
	if vault == nil {
		return nil, "", ref, errors.New("marketidentity: no vault reads the oauth session")
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return nil, "", ref, err
	}
	store := &vaultSessionStore{vault: vault, ref: *ref, clientID: a.pinnedClientID(id)}
	stored, err := store.List(ctx)
	if err != nil {
		return nil, "", ref, err
	}
	if len(stored) == 0 {
		return nil, "", ref, oauthclient.ErrNoSession
	}
	handle, err := a.checkAccount(ctx, id, stored[0].AccountDID, stored[0].HostURL, "")
	if err != nil {
		return nil, "", ref, err
	}
	session, err := oauthclient.Open(ctx, oauthclient.Options{
		DID:        stored[0].AccountDID,
		Handle:     handle,
		RepoDID:    stored[0].AccountDID,
		ClientID:   a.sessionClientID(id),
		Scopes:     a.scopesFor(id.Spec.Mode),
		Directory:  a.oauth.Directory,
		HTTPClient: client,
		Store:      store,
		Log:        a.log,
	})
	if errors.Is(err, oauthclient.ErrInvalidSession) {
		_ = vault.Delete(ctx, *ref)
		return nil, "", ref, oauthclient.ErrNoSession
	}
	if err != nil {
		return nil, "", ref, err
	}
	if err := a.scopeCheck(workspace, id, held, session.Scopes()); err != nil {
		return nil, "", ref, err
	}
	held.ref = *ref
	held.account, held.handle = accountKey(id), handle
	held.auth.Hold(session)
	return session, handle, ref, nil
}

type scopeChangedError struct {
	missing []string
}

func (e *scopeChangedError) Error() string {
	return "marketidentity: " + e.message()
}

func (e *scopeChangedError) message() string {
	return "the oauth session lacks scopes the identity now needs (" + strings.Join(e.subjects(), ", ") + "); sign in again"
}

func (e *scopeChangedError) subjects() []string {
	subjects := make([]string, 0, len(e.missing))
	for _, scope := range e.missing {
		subjects = append(subjects, atprotooauth.ScopeSubject(scope))
	}
	return subjects
}

func (a *Accounts) requiredScopes(id *v1alpha1.MarketIdentity) []string {
	if id.Spec.Mode == v1alpha1.MarketIdentityModeOAuthSessionProxy {
		return strings.Fields(id.Spec.SessionScope)
	}
	return a.oauthScopes()
}

func (a *Accounts) scopeCheck(workspace string, id *v1alpha1.MarketIdentity, held *heldAccount, granted []string) error {
	if len(granted) == 0 {
		return nil
	}
	missing := atprotooauth.MissingScopes(a.requiredScopes(id), granted)
	if lacking := strings.Join(missing, " "); lacking != held.lacking {
		held.lacking = lacking
		if lacking != "" {
			a.log.Info("the oauth session lacks scopes the identity now needs", "workspace", workspace, "identity", id.Name, "missing", missing)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &scopeChangedError{missing: missing}
}

func signInAgainReason(err error) (string, string, bool) {
	var changed *scopeChangedError
	switch {
	case errors.As(err, &changed):
		return "ScopeChanged", changed.message(), true
	case oauthExpired(err):
		return string(v1alpha1.MarketIdentityPhaseSessionExpired), pendingMessage(v1alpha1.MarketIdentityPhaseSessionExpired), true
	}
	return "", "", false
}

func signInAgainStatus(status *v1alpha1.MarketIdentityStatus, generation int64, reason, message string) {
	status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
	status.ObservedGeneration = generation
	status.Message = message
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, false, reason, message)
	setReady(status, generation, false, reason, message)
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

func (a *Accounts) openSessionData(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, data oauthclient.SessionData,
	ref *commonv1.SecretRef, claimed string) (*oauthclient.Session, string, error) {
	vault := a.vaultFor(workspace)
	if vault == nil {
		return nil, "", errors.New("marketidentity: no vault writes the oauth session")
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return nil, "", err
	}
	handle, err := a.checkAccount(ctx, id, data.AccountDID, data.HostURL, claimed)
	if err != nil {
		return nil, "", err
	}
	held := a.held.get(workspace, id)
	held.open.Lock()
	defer held.open.Unlock()
	held.auth.Drop()
	held.account = ""
	if err := saveSession(ctx, vault, ref, data); err != nil {
		return nil, "", err
	}
	session, err := oauthclient.New(data, oauthclient.Options{
		DID:        data.AccountDID,
		Handle:     handle,
		RepoDID:    data.AccountDID,
		PDSURL:     data.HostURL,
		ClientID:   a.sessionClientID(id),
		Scopes:     a.scopesFor(id.Spec.Mode),
		Directory:  a.oauth.Directory,
		HTTPClient: client,
		Store:      &vaultSessionStore{vault: vault, ref: *ref, clientID: a.pinnedClientID(id)},
		Log:        a.log,
	})
	if err != nil {
		return nil, "", err
	}
	_ = session.EnsureEndpoints(ctx)
	if _, err := session.AccessToken(ctx); err != nil {
		return nil, "", err
	}
	if err := a.scopeCheck(workspace, id, held, session.Scopes()); err != nil {
		return nil, "", err
	}
	held.ref = *ref
	held.account, held.handle = accountKey(id), handle
	held.auth.Hold(session)
	return session, handle, nil
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

func (a *Accounts) sessionSignedIn(ctx context.Context, status v1alpha1.MarketIdentityStatus, generation int64,
	session *oauthclient.Session, handle string, ref *commonv1.SecretRef) Result {
	token, _ := session.AccessToken(ctx)
	return signedIn(status, generation, session.DID(), handle, session.HostURL(), ref, token)
}

func signedIn(status v1alpha1.MarketIdentityStatus, generation int64, did, handle, pdsURL string,
	ref *commonv1.SecretRef, token string) Result {
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	status.Did = strings.TrimSpace(did)
	status.Handle = strings.TrimSpace(handle)
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
