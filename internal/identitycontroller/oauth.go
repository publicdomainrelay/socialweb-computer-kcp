package identitycontroller

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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

const (
	DefaultOAuthScope = "atproto"

	browserPendingTTL = 15 * time.Minute

	qrPollInterval = 3 * time.Second
)

type OAuthOptions struct {
	ClientID string

	RedirectURI string

	Scopes []string

	QROrigin string

	Directory identity.Directory

	HTTPClient *http.Client

	States oauthclient.StateStore

	StateRef *v1alpha1.VaultRef

	CLIKey atcrypto.PrivateKey

	CLIDID string

	QRSigner oauthclient.ServiceAuthSigner
}

type qrTransfer struct {
	transfer *oauthclient.QRTransfer

	url string

	expires time.Time

	lastPoll time.Time
}

var errNoSessionRef = errors.New("identitycontroller: the identity has no sessionRef")

func (c *Controller) oauthHTTP() *http.Client {
	if c.oauth.HTTPClient != nil {
		return c.oauth.HTTPClient
	}
	if c.http != nil {
		return c.http
	}
	return &http.Client{Timeout: requestTimeout}
}

func (c *Controller) oauthScopes() []string {
	if len(c.oauth.Scopes) > 0 {
		return append([]string(nil), c.oauth.Scopes...)
	}
	return []string{DefaultOAuthScope}
}

func (c *Controller) reconcileOAuthBrowser(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus) {
	session, ref, err := c.restoreOAuthSession(ctx, id, &status)
	if err == nil {
		c.signedIn(ctx, dyn, id, generation, status, session.DID(), id.Spec.Handle, session.HostURL(), ref)
		return
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		c.failed(ctx, dyn, id, generation, status, "MissingSessionRef", "mode oauthBrowser needs spec.sessionRef for the oauth session; no secret is written to status")
		return
	}
	expired := oauthExpired(err)
	if err != nil && !errors.Is(err, oauthclient.ErrNoSession) && !expired {
		c.pending(ctx, dyn, id, generation, status, "SessionError", "restoring the oauth session: "+err.Error())
		return
	}
	kind := v1alpha1.PendingActionVisitUrl
	phase := v1alpha1.MarketIdentityPhaseAwaitingAuthorization
	if expired {
		kind = v1alpha1.PendingActionSignInAgain
		phase = v1alpha1.MarketIdentityPhaseSessionExpired
	}
	if pendingValid(id.Status.PendingAction, kind, time.Now()) {
		status.Phase = phase
		status.ObservedGeneration = generation
		status.SessionRef = ref
		status.Message = pendingMessage(phase)
		setReady(&status, generation, false, string(phase), status.Message)
		c.write(ctx, dyn, id.GetName(), status)
		return
	}
	url, expires, err := c.beginBrowserAuthorization(ctx, id)
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "OAuthBeginFailed", err.Error())
		return
	}
	status.Phase = phase
	status.ObservedGeneration = generation
	status.SessionRef = ref
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: kind, Url: url, ExpiresAt: expires.UTC().Format(time.RFC3339)}
	status.Message = pendingMessage(phase)
	setReady(&status, generation, false, string(phase), status.Message)
	c.write(ctx, dyn, id.GetName(), status)
}

func (c *Controller) reconcileOAuthQr(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus) {
	session, ref, err := c.restoreOAuthSession(ctx, id, &status)
	if err == nil {
		c.clearQr(id.Name)
		c.signedIn(ctx, dyn, id, generation, status, session.DID(), id.Spec.Handle, session.HostURL(), ref)
		return
	}
	if errors.Is(err, errNoSessionRef) || ref == nil {
		c.failed(ctx, dyn, id, generation, status, "MissingSessionRef", "mode oauthQr needs spec.sessionRef for the oauth session; no secret is written to status")
		return
	}
	expired := oauthExpired(err)
	if err != nil && !errors.Is(err, oauthclient.ErrNoSession) && !expired {
		c.pending(ctx, dyn, id, generation, status, "SessionError", "restoring the oauth session: "+err.Error())
		return
	}
	qr, err := c.qrState(id.Name)
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "MissingOAuthConfig", err.Error())
		return
	}
	if qr == nil || !time.Now().Before(qr.expires) {
		qr, err = c.newQrState(id)
		if err != nil {
			c.failed(ctx, dyn, id, generation, status, "OAuthBeginFailed", err.Error())
			return
		}
		status.SessionRef = ref
		status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionScanQr, Url: qr.url,
			ExpiresAt: qr.expires.UTC().Format(time.RFC3339)}
		if expired {
			status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
			status.PendingAction.Kind = v1alpha1.PendingActionSignInAgain
			status.Message = pendingMessage(status.Phase)
			setReady(&status, generation, false, string(status.Phase), status.Message)
		} else {
			status.Phase = v1alpha1.MarketIdentityPhaseAwaitingScan
			status.Message = "scan the qr url to sign in"
			setReady(&status, generation, false, "AwaitingScan", status.Message)
		}
		status.ObservedGeneration = generation
		c.write(ctx, dyn, id.GetName(), status)
		return
	}
	status.SessionRef = ref
	if time.Since(qr.lastPoll) < qrPollInterval {
		c.writeQrPending(ctx, dyn, id, generation, status, qr)
		return
	}
	qr.lastPoll = time.Now()
	sessionQr, ok, err := qr.transfer.PollOnce(ctx)
	if err != nil {
		if errors.Is(err, oauthclient.ErrQRConsumed) || errors.Is(err, oauthclient.ErrQRTimeout) {
			c.clearQr(id.Name)
		}
		c.pending(ctx, dyn, id, generation, status, "QRPollFailed", err.Error())
		return
	}
	if !ok {
		c.writeQrPending(ctx, dyn, id, generation, status, qr)
		return
	}
	data, err := sessionQr.SessionData(c.oauth.ClientID, c.oauthScopes())
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "QRSessionInvalid", err.Error())
		return
	}
	if err := c.saveSession(ctx, ref, data); err != nil {
		c.pending(ctx, dyn, id, generation, status, "SessionStoreFailed", err.Error())
		return
	}
	c.clearQr(id.Name)
	session, err = c.openSessionData(ctx, id, data, ref)
	if err != nil {
		c.pending(ctx, dyn, id, generation, status, "SessionRestoreFailed", err.Error())
		return
	}
	c.signedIn(ctx, dyn, id, generation, status, session.DID(), firstNonEmpty(sessionQr.Handle, id.Spec.Handle), session.HostURL(), ref)
}

func (c *Controller) writeQrPending(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus, qr *qrTransfer) {
	status.Phase = v1alpha1.MarketIdentityPhaseAwaitingScan
	status.ObservedGeneration = generation
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionScanQr, Url: qr.url,
		ExpiresAt: qr.expires.UTC().Format(time.RFC3339)}
	status.Message = "scan the qr url to sign in"
	setReady(&status, generation, false, "AwaitingScan", status.Message)
	c.write(ctx, dyn, id.GetName(), status)
}

func (c *Controller) reconcileOAuthDelegated(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus) {
	session, ref, err := c.restoreOAuthSession(ctx, id, &status)
	if err == nil {
		c.signedIn(ctx, dyn, id, generation, status, session.DID(), id.Spec.Handle, session.HostURL(), ref)
		return
	}
	if errors.Is(err, errNoSessionRef) {
		c.pending(ctx, dyn, id, generation, status, "AwaitingSession", "mode oauthDelegated waits for spec.sessionRef")
		return
	}
	if oauthExpired(err) {
		status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
		status.ObservedGeneration = generation
		status.SessionRef = ref
		status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionSignInAgain, Handle: id.Spec.Handle}
		status.Message = pendingMessage(status.Phase)
		setReady(&status, generation, false, "SessionExpired", status.Message)
		c.write(ctx, dyn, id.GetName(), status)
		return
	}
	c.pending(ctx, dyn, id, generation, status, "SessionError", "restoring the oauth session: "+err.Error())
}

func (c *Controller) oauthClientID(id *v1alpha1.MarketIdentity) string {
	if id != nil && strings.TrimSpace(id.Spec.OAuthClientID) != "" {
		return strings.TrimSpace(id.Spec.OAuthClientID)
	}
	return strings.TrimSpace(c.oauth.ClientID)
}

func (c *Controller) oauthRedirectURI(id *v1alpha1.MarketIdentity) string {
	if id != nil && strings.TrimSpace(id.Spec.OAuthRedirectURI) != "" {
		return strings.TrimSpace(id.Spec.OAuthRedirectURI)
	}
	return strings.TrimSpace(c.oauth.RedirectURI)
}

func (c *Controller) beginBrowserAuthorization(ctx context.Context, id *v1alpha1.MarketIdentity) (string, time.Time, error) {
	clientID := c.oauthClientID(id)
	redirectURI := c.oauthRedirectURI(id)
	if clientID == "" {
		return "", time.Time{}, errors.New("identitycontroller: mode oauthBrowser needs an oauth clientID")
	}
	if redirectURI == "" {
		return "", time.Time{}, errors.New("identitycontroller: mode oauthBrowser needs an oauth redirectURI")
	}
	pdsURL, err := c.oauthPDSURL(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}
	client := c.oauthHTTP()
	authServer, tokenEndpoint, _, err := oauthclient.DiscoverAuthServer(ctx, client, pdsURL)
	if err != nil {
		return "", time.Time{}, err
	}
	var metadata struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := oauthclient.GetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", time.Time{}, fmt.Errorf("identitycontroller: the auth server authorization metadata: %w", err)
	}
	if strings.TrimSpace(metadata.AuthorizationEndpoint) == "" {
		return "", time.Time{}, errors.New("identitycontroller: the auth server metadata names no authorization endpoint")
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
	scopes := c.oauthScopes()
	pending := oauthclient.PendingAuth{
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
	if err := c.states.Put(ctx, pending); err != nil {
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

func (c *Controller) oauthPDSURL(ctx context.Context, id *v1alpha1.MarketIdentity) (string, error) {
	if raw := strings.TrimSpace(id.Spec.PdsUrl); raw != "" {
		return raw, nil
	}
	handle := strings.TrimSpace(id.Spec.Handle)
	if handle == "" || c.oauth.Directory == nil {
		return "", errors.New("identitycontroller: the oauth mode names neither pdsUrl nor a resolvable handle")
	}
	atid, err := syntax.ParseAtIdentifier(handle)
	if err != nil {
		return "", fmt.Errorf("identitycontroller: the oauth handle %q: %w", handle, err)
	}
	ident, err := c.oauth.Directory.Lookup(ctx, atid)
	if err != nil {
		return "", fmt.Errorf("identitycontroller: resolving the oauth handle %q: %w", handle, err)
	}
	pds := strings.TrimSpace(ident.PDSEndpoint())
	if pds == "" {
		return "", fmt.Errorf("identitycontroller: the identity %s names no PDS", ident.DID.String())
	}
	return pds, nil
}

func (c *Controller) restoreOAuthSession(ctx context.Context, id *v1alpha1.MarketIdentity,
	status *v1alpha1.MarketIdentityStatus) (*oauthclient.Session, *v1alpha1.VaultRef, error) {
	ref := oauthSessionRef(id, status)
	if ref == nil {
		return nil, nil, errNoSessionRef
	}
	if c.vault == nil {
		return nil, ref, errors.New("identitycontroller: no vault reads the oauth session")
	}
	session, err := c.openSessionDataRef(ctx, id, *ref, status.Did)
	return session, ref, err
}

func (c *Controller) openSessionDataRef(ctx context.Context, id *v1alpha1.MarketIdentity, ref v1alpha1.VaultRef, did string) (*oauthclient.Session, error) {
	store := &vaultSessionStore{vault: c.vault, ref: ref}
	session, err := oauthclient.Open(ctx, oauthclient.Options{
		DID:        did,
		Handle:     id.Spec.Handle,
		RepoDID:    did,
		PDSURL:     id.Spec.PdsUrl,
		ClientID:   c.oauthClientID(id),
		Scopes:     c.oauthScopes(),
		Directory:  c.oauth.Directory,
		HTTPClient: c.oauthHTTP(),
		Store:      store,
	})
	if errors.Is(err, oauthclient.ErrInvalidSession) {
		_ = c.vault.Delete(ctx, ref)
		return nil, oauthclient.ErrNoSession
	}
	return session, err
}

func (c *Controller) openSessionData(ctx context.Context, id *v1alpha1.MarketIdentity, data oauthclient.SessionData,
	ref *v1alpha1.VaultRef) (*oauthclient.Session, error) {
	opts := oauthclient.Options{
		DID:        data.AccountDID,
		Handle:     id.Spec.Handle,
		RepoDID:    data.AccountDID,
		PDSURL:     data.HostURL,
		ClientID:   c.oauthClientID(id),
		Scopes:     c.oauthScopes(),
		Directory:  c.oauth.Directory,
		HTTPClient: c.oauthHTTP(),
	}
	if ref != nil {
		opts.Store = &vaultSessionStore{vault: c.vault, ref: *ref}
	}
	session, err := oauthclient.New(data, opts)
	if err != nil {
		return nil, err
	}
	if _, err := session.AccessToken(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func (c *Controller) saveSession(ctx context.Context, ref *v1alpha1.VaultRef, data oauthclient.SessionData) error {
	if ref == nil {
		return errNoSessionRef
	}
	if c.vault == nil {
		return errors.New("identitycontroller: no vault writes the oauth session")
	}
	return (&vaultSessionStore{vault: c.vault, ref: *ref}).Save(ctx, data)
}

func (c *Controller) signedIn(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus, did, handle, pdsURL string, ref *v1alpha1.VaultRef) {
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	status.Did = strings.TrimSpace(did)
	status.Handle = firstNonEmpty(strings.TrimSpace(handle), strings.TrimSpace(id.Spec.Handle))
	status.PdsUrl = strings.TrimSpace(pdsURL)
	status.SessionRef = ref
	status.Associations = nil
	status.PendingAction = nil
	status.Message = ""
	setSignedIn(&status, generation, true, "SignedIn", "the account session is live; the status carries no token")
	setReady(&status, generation, true, "SignedIn", status.Did)
	c.write(ctx, dyn, id.GetName(), status)
}

func (c *Controller) failed(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus, reason, message string) {
	status.Phase = v1alpha1.MarketIdentityPhaseFailed
	status.ObservedGeneration = generation
	status.Message = message
	setReady(&status, generation, false, reason, message)
	c.write(ctx, dyn, id.GetName(), status)
}

func setSignedIn(status *v1alpha1.MarketIdentityStatus, generation int64, signedIn bool, reason, message string) {
	value := metav1.ConditionFalse
	if signedIn {
		value = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionSignedIn, Status: value, Reason: reason,
		Message: message, ObservedGeneration: generation})
}

func oauthExpired(err error) bool {
	return errors.Is(err, oauthclient.ErrSessionExpired) || errors.Is(err, oauthclient.ErrNoRefreshToken)
}

func oauthSessionRef(id *v1alpha1.MarketIdentity, status *v1alpha1.MarketIdentityStatus) *v1alpha1.VaultRef {
	if id.Spec.SessionRef != nil {
		return id.Spec.SessionRef
	}
	if status.SessionRef != nil {
		return status.SessionRef
	}
	return nil
}

func pendingValid(action *v1alpha1.MarketIdentityPendingAction, kind v1alpha1.PendingActionKind, now time.Time) bool {
	if action == nil || action.Kind != kind || strings.TrimSpace(action.Url) == "" {
		return false
	}
	if action.ExpiresAt == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339, action.ExpiresAt)
	if err != nil {
		return false
	}
	return now.Before(expires)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (c *Controller) qrState(name string) (*qrTransfer, error) {
	c.qrMu.Lock()
	defer c.qrMu.Unlock()
	qr := c.qr[name]
	return qr, nil
}

func (c *Controller) clearQr(name string) {
	c.qrMu.Lock()
	defer c.qrMu.Unlock()
	delete(c.qr, name)
}

func (c *Controller) newQrState(id *v1alpha1.MarketIdentity) (*qrTransfer, error) {
	did, signer, err := c.qrSigner()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.oauth.QROrigin) == "" {
		return nil, errors.New("identitycontroller: mode oauthQr needs an oauth qrOrigin")
	}
	nonce, err := oauthclient.NewQRNonce()
	if err != nil {
		return nil, err
	}
	transfer, err := oauthclient.NewQRTransfer(c.oauth.QROrigin, did, nonce, signer)
	if err != nil {
		return nil, err
	}
	transfer.HTTPClient = c.oauthHTTP()
	if transfer.TTL <= 0 {
		transfer.TTL = oauthclient.DefaultQRTTL
	}
	url, err := transfer.URL()
	if err != nil {
		return nil, err
	}
	qr := &qrTransfer{transfer: transfer, url: url, expires: time.Now().Add(transfer.TTL)}
	c.qrMu.Lock()
	if c.qr == nil {
		c.qr = map[string]*qrTransfer{}
	}
	c.qr[id.Name] = qr
	c.qrMu.Unlock()
	return qr, nil
}

func (c *Controller) qrSigner() (string, oauthclient.ServiceAuthSigner, error) {
	if c.oauth.CLIKey != nil {
		public, err := c.oauth.CLIKey.PublicKey()
		if err != nil {
			return "", nil, err
		}
		did := public.DIDKey()
		return did, oauthclient.KeySigner{Issuer: syntax.DID(did), Key: c.oauth.CLIKey}, nil
	}
	if c.oauth.QRSigner != nil && strings.TrimSpace(c.oauth.CLIDID) != "" {
		return strings.TrimSpace(c.oauth.CLIDID), c.oauth.QRSigner, nil
	}
	return "", nil, errors.New("identitycontroller: mode oauthQr needs a CLI key or a qr signer and cliDID")
}

func (c *Controller) BrowserCallbackHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := c.CompleteBrowserAuthorization(r.Context(), name, r); err != nil {
			http.Error(w, "the oauth callback failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("signed in\n"))
	})
}

func (c *Controller) CompleteBrowserAuthorization(ctx context.Context, name string, r *http.Request) error {
	c.log.Info("oauth callback complete: start", "identity", name, "states", fmt.Sprintf("%T", c.states))
	dyn, err := c.dynamic(c.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(MarketIdentityGVR)
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var id v1alpha1.MarketIdentity
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &id); err != nil {
		return err
	}
	if id.Spec.Mode != v1alpha1.MarketIdentityModeOAuthBrowser {
		return fmt.Errorf("identitycontroller: MarketIdentity %s is mode %s, not oauthBrowser", name, id.Spec.Mode)
	}
	c.log.Info("oauth callback complete: identity read", "identity", name)
	ref := oauthSessionRef(&id, &id.Status)
	if ref == nil {
		return errNoSessionRef
	}
	if c.vault == nil {
		return errors.New("identitycontroller: no vault writes the oauth session")
	}
	c.log.Info("oauth callback complete: exchanging code", "identity", name)
	data, err := oauthclient.ExchangeCallback(ctx, c.states, r, c.oauthHTTP())
	if err != nil {
		c.log.Info("oauth callback exchange failed", "identity", name, "err", err.Error())
		return err
	}
	c.log.Info("oauth callback complete: saving session", "identity", name)
	if err := c.saveSession(ctx, ref, data); err != nil {
		return err
	}
	status := id.Status
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = id.Generation
	status.Did = data.AccountDID
	status.Handle = strings.TrimSpace(id.Spec.Handle)
	status.PdsUrl = data.HostURL
	status.SessionRef = ref
	status.PendingAction = nil
	status.Message = ""
	setSignedIn(&status, id.Generation, true, "SignedIn", "the account session is live; the status carries no token")
	setReady(&status, id.Generation, true, "SignedIn", status.Did)
	return c.writeStatus(ctx, dyn, name, status)
}
