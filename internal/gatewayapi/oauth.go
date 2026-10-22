package gatewayapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	WebSessionCookie = "swc_session"

	WebOAuthStateCookie = "swc_oauth_state"

	WebOAuthCallbackPath = "/oauth/callback"

	DefaultCookieMaxAge = 30 * 24 * time.Hour

	DefaultClientMetadataCache = time.Minute

	OAuthStateMaxAge = 10 * time.Minute

	MaxIdentifierLength = 256
)

const (
	cookieSameSiteStrict = "Strict"
	cookieSameSiteLax    = "Lax"
)

const clientMetadataRefreshTimeout = 30 * time.Second

type WebUser struct {
	DID    string
	Handle string
	PDS    string
}

type WebCaller struct {
	WebUser
	Token string
}

type SignInStart struct {
	AuthorizationURL string
	State            string
}

type SignInCompleteParams struct {
	State string
	Code  string
	ISS   string
}

type SignInGrant struct {
	WebUser
	Token        string
	LeaseSeconds int64
}

type SignOutResult struct {
	Revoked     bool
	RevokeError string
}

type RecordRef struct {
	URI string
	CID string
}

type Record struct {
	URI   string
	CID   string
	Value map[string]any
}

type Write struct {
	Action     string
	Collection string
	Rkey       string
	Record     map[string]any
}

type WebAccountAgent interface {
	GetServiceAuth(ctx context.Context, aud, lxm string) (string, error)
	ListRecords(ctx context.Context, did, collection string, limit int) ([]Record, error)
	CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (RecordRef, error)
	ApplyWrites(ctx context.Context, did string, writes []Write) error
}

type WebSignIn interface {
	ClientMetadata(ctx context.Context) (json.RawMessage, error)
	Start(ctx context.Context, identifier, redirectURI string) (SignInStart, error)
	Complete(ctx context.Context, params SignInCompleteParams) (SignInGrant, error)
	Identify(ctx context.Context, token string) (*WebUser, error)
	SignOut(ctx context.Context, caller *WebCaller) (SignOutResult, error)
	AgentFor(caller *WebCaller) WebAccountAgent
}

func CookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(part[:eq]) == name {
			return strings.TrimSpace(part[eq+1:])
		}
	}
	return ""
}

func serializeCookie(name, value string, maxAge int, sameSite, path string, secure bool) string {
	out := fmt.Sprintf("%s=%s; Path=%s; Max-Age=%d; HttpOnly; SameSite=%s", name, value, path, maxAge, sameSite)
	if secure {
		out += "; Secure"
	}
	return out
}

type RequestAuthOptions struct {
	SignIn WebSignIn

	PublicOrigin string

	CookieName string

	Log *slog.Logger
}

type RequestAuth struct {
	signIn       WebSignIn
	publicOrigin string
	cookieName   string
	log          *slog.Logger
}

func NewRequestAuth(opts RequestAuthOptions) (*RequestAuth, error) {
	if opts.SignIn == nil {
		return nil, errors.New("gatewayapi: request auth has no sign-in backend")
	}
	if opts.CookieName == "" {
		opts.CookieName = WebSessionCookie
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &RequestAuth{
		signIn:       opts.SignIn,
		publicOrigin: strings.TrimRight(opts.PublicOrigin, "/"),
		cookieName:   opts.CookieName,
		log:          log,
	}, nil
}

var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

func (a *RequestAuth) originAllowed(r *http.Request) bool {
	if safeMethods[strings.ToUpper(r.Method)] {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if a.publicOrigin != "" {
		return origin == a.publicOrigin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func (a *RequestAuth) Authenticate(r *http.Request) (*WebCaller, error) {
	token := CookieValue(r.Header.Get("Cookie"), a.cookieName)
	if token == "" {
		return nil, nil
	}
	if !a.originAllowed(r) {
		a.log.Warn("web_origin_refused", "method", r.Method, "origin", r.Header.Get("Origin"))
		return nil, nil
	}
	user, err := a.signIn.Identify(r.Context(), token)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}
	return &WebCaller{WebUser: *user, Token: token}, nil
}

type OAuthOptions struct {
	SignIn WebSignIn

	Auth *RequestAuth

	PublicOrigin string

	CookieName string

	CookieMaxAge time.Duration

	ClientMetadataPath string

	ClientMetadataCache time.Duration

	MaxBodyBytes int64

	PostSignInURL string

	OnSignedIn func(context.Context, *WebUser) error

	OnSignedOut func(context.Context, *WebUser) error

	Log *slog.Logger
}

type OAuth struct {
	opts               OAuthOptions
	cookieName         string
	cookieMaxAge       int
	publicOrigin       string
	secure             bool
	maxBody            int64
	postSignInURL      string
	metadataPath       string
	metadataCache      time.Duration
	stateCookie        func(value string, maxAge int) string
	sessionCookie      func(value string, maxAge int) string
	auth               *RequestAuth
	mu                 sync.Mutex
	metadata           json.RawMessage
	metadataAt         time.Time
	metadataLoaded     bool
	metadataRefreshing bool
}

func NewOAuth(opts OAuthOptions) (*OAuth, error) {
	if opts.SignIn == nil {
		return nil, errors.New("gatewayapi: OAuth has no sign-in backend")
	}
	if opts.CookieName == "" {
		opts.CookieName = WebSessionCookie
	}
	if opts.CookieMaxAge <= 0 {
		opts.CookieMaxAge = DefaultCookieMaxAge
	}
	if opts.ClientMetadataPath == "" {
		opts.ClientMetadataPath = "/oauth-client-metadata.json"
	}
	if opts.ClientMetadataCache <= 0 {
		opts.ClientMetadataCache = DefaultClientMetadataCache
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	publicOrigin := strings.TrimRight(opts.PublicOrigin, "/")
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	opts.Log = log
	auth := opts.Auth
	if auth == nil {
		var err error
		auth, err = NewRequestAuth(RequestAuthOptions{
			SignIn:       opts.SignIn,
			PublicOrigin: publicOrigin,
			CookieName:   opts.CookieName,
			Log:          log,
		})
		if err != nil {
			return nil, err
		}
	}
	o := &OAuth{
		opts:          opts,
		cookieName:    opts.CookieName,
		cookieMaxAge:  int(opts.CookieMaxAge / time.Second),
		publicOrigin:  publicOrigin,
		secure:        strings.HasPrefix(publicOrigin, "https://"),
		maxBody:       opts.MaxBodyBytes,
		postSignInURL: strings.TrimSpace(opts.PostSignInURL),
		metadataPath:  opts.ClientMetadataPath,
		metadataCache: opts.ClientMetadataCache,
		auth:          auth,
	}
	o.stateCookie = func(value string, maxAge int) string {
		return serializeCookie(WebOAuthStateCookie, value, maxAge, cookieSameSiteLax, "/oauth", o.secure)
	}
	o.sessionCookie = func(value string, maxAge int) string {
		return serializeCookie(o.cookieName, value, maxAge, cookieSameSiteStrict, "/", o.secure)
	}
	return o, nil
}

func (o *OAuth) Auth() *RequestAuth { return o.auth }

func (o *OAuth) SignIn() WebSignIn { return o.opts.SignIn }

func (o *OAuth) CallerFor(who *WebCaller) ClusterCaller {
	if who == nil {
		return nil
	}
	agent := o.opts.SignIn.AgentFor(who)
	return NewCaller(who.DID, func(ctx context.Context, audience, lxm string) (string, error) {
		return agent.GetServiceAuth(ctx, audience, lxm)
	})
}

func (o *OAuth) RequestCaller(r *http.Request) (ClusterCaller, error) {
	who, err := o.auth.Authenticate(r)
	if err != nil || who == nil {
		return nil, err
	}
	return o.CallerFor(who), nil
}

func (o *OAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+o.metadataPath, o.clientMetadata)
	mux.HandleFunc("POST /oauth/start", o.start)
	mux.HandleFunc("GET "+WebOAuthCallbackPath, o.callback)
	mux.HandleFunc("GET /session", o.sessionGET)
	mux.HandleFunc("DELETE /session", o.sessionDELETE)
}

func (o *OAuth) Handler() http.Handler {
	mux := http.NewServeMux()
	o.Register(mux)
	return mux
}

func (o *OAuth) clientMetadata(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	if !o.metadataLoaded {
		o.mu.Unlock()
		doc, err := o.opts.SignIn.ClientMetadata(r.Context())
		if err != nil {
			o.opts.Log.Error("client_metadata_unavailable", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, struct {
				Error string `json:"error"`
			}{"client metadata is not available from the vault"})
			return
		}
		o.mu.Lock()
		o.metadata = append(o.metadata[:0], doc...)
		o.metadataAt = time.Now()
		o.metadataLoaded = true
		cached := append(json.RawMessage(nil), o.metadata...)
		o.mu.Unlock()
		o.writeClientMetadata(w, cached)
		return
	}
	if !o.metadataRefreshing && time.Since(o.metadataAt) >= o.metadataRefreshAfter() {
		o.metadataRefreshing = true
		go o.refreshClientMetadata()
	}
	cached := append(json.RawMessage(nil), o.metadata...)
	o.mu.Unlock()
	o.writeClientMetadata(w, cached)
}

func (o *OAuth) metadataRefreshAfter() time.Duration {
	after := o.metadataCache / 2
	if after < time.Second {
		after = time.Second
	}
	return after
}

func (o *OAuth) refreshClientMetadata() {
	timeout := o.metadataCache
	if timeout <= 0 || timeout > clientMetadataRefreshTimeout {
		timeout = clientMetadataRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	doc, err := o.opts.SignIn.ClientMetadata(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.metadataRefreshing = false
	if err != nil {
		o.opts.Log.Warn("client_metadata_refresh_failed", "err", err)
		o.metadataAt = time.Now()
		return
	}
	o.metadata = append(o.metadata[:0], doc...)
	o.metadataAt = time.Now()
	o.metadataLoaded = true
}

func (o *OAuth) writeClientMetadata(w http.ResponseWriter, doc json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(o.metadataCache/time.Second)))
	_, _ = w.Write(doc)
}

func (o *OAuth) start(w http.ResponseWriter, r *http.Request) {
	if o.publicOrigin == "" {
		writeJSON(w, http.StatusServiceUnavailable, struct {
			Error string `json:"error"`
		}{"this deployment has no public-origin, so it cannot sign anyone in"})
		return
	}
	if declared, err := parseContentLength(r); err == nil && declared > o.maxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, struct {
			Error string `json:"error"`
		}{"body too large"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, o.maxBody+1))
	if err != nil || int64(len(raw)) > o.maxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, struct {
			Error string `json:"error"`
		}{"body too large"})
		return
	}
	var body struct {
		Handle any `json:"handle"`
	}
	_ = json.Unmarshal(raw, &body)
	identifier := ""
	if handle, ok := body.Handle.(string); ok {
		identifier = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	}
	if identifier == "" || len(identifier) > MaxIdentifierLength || strings.ContainsFunc(identifier, unicode.IsSpace) {
		writeJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
		}{"expected a handle or a DID"})
		return
	}
	started, err := o.opts.SignIn.Start(r.Context(), identifier, o.publicOrigin+WebOAuthCallbackPath)
	if err != nil {
		o.opts.Log.Error("web_sign_in_start_failed", "identifier", identifier, "err", err)
		writeJSON(w, http.StatusBadGateway, struct {
			Error string `json:"error"`
		}{"could not start the sign-in: " + err.Error()})
		return
	}
	o.opts.Log.Info("web_sign_in_started", "identifier", identifier)
	w.Header().Set("Set-Cookie", o.stateCookie(started.State, int(OAuthStateMaxAge/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}{started.AuthorizationURL})
}

type signInCompletion struct {
	SignedIn bool   `json:"signedIn"`
	DID      string `json:"did,omitempty"`
	Handle   string `json:"handle,omitempty"`
}

func (o *OAuth) finish(w http.ResponseWriter, r *http.Request, status int, completion *signInCompletion, errorMessage string, extraCookies ...string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Set-Cookie", o.stateCookie("", 0))
	for _, cookie := range extraCookies {
		w.Header().Add("Set-Cookie", cookie)
	}
	if o.postSignInURL != "" {
		http.Redirect(w, r, o.completionRedirect(errorMessage), http.StatusFound)
		return
	}
	if errorMessage != "" {
		writeJSON(w, status, struct {
			Error string `json:"error"`
		}{errorMessage})
		return
	}
	if completion == nil {
		completion = &signInCompletion{SignedIn: true}
	}
	writeJSON(w, status, completion)
}

func (o *OAuth) completionRedirect(errorMessage string) string {
	if errorMessage == "" {
		return o.postSignInURL
	}
	parsed, err := url.Parse(o.postSignInURL)
	if err != nil {
		return o.postSignInURL + "?" + url.Values{"error": {errorMessage}}.Encode()
	}
	values := parsed.Query()
	values.Set("error", errorMessage)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

func (o *OAuth) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if denied := query.Get("error"); denied != "" {
		description := query.Get("error_description")
		if description == "" {
			description = denied
		}
		o.finish(w, r, http.StatusUnauthorized, nil, description)
		return
	}
	state := query.Get("state")
	code := query.Get("code")
	if state == "" || code == "" {
		o.finish(w, r, http.StatusBadRequest, nil, "the authorization server returned no code")
		return
	}
	presented := CookieValue(r.Header.Get("Cookie"), WebOAuthStateCookie)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(state)) != 1 {
		o.opts.Log.Warn("web_sign_in_state_not_bound")
		o.finish(w, r, http.StatusBadRequest, nil, "this sign-in was not started in this browser; start it again")
		return
	}
	grant, err := o.opts.SignIn.Complete(r.Context(), SignInCompleteParams{
		State: state, Code: code, ISS: query.Get("iss"),
	})
	if err != nil {
		o.opts.Log.Error("web_sign_in_failed", "err", err)
		o.finish(w, r, http.StatusBadGateway, nil, "sign-in failed: "+err.Error())
		return
	}
	user := &WebUser{DID: grant.DID, Handle: grant.Handle, PDS: grant.PDS}
	if o.opts.OnSignedIn != nil {
		if err := o.opts.OnSignedIn(r.Context(), user); err != nil {
			o.opts.Log.Error("web_signed_in_hook_failed", "did", grant.DID, "err", err)
			o.finish(w, r, http.StatusBadGateway, nil, "sign-in failed: "+err.Error())
			return
		}
	}
	o.opts.Log.Info("web_signed_in", "did", grant.DID, "handle", grant.Handle)
	o.finish(w, r, http.StatusOK, &signInCompletion{SignedIn: true, DID: grant.DID, Handle: grant.Handle}, "", o.sessionCookie(grant.Token, o.cookieMaxAge))
}

func (o *OAuth) authenticate(w http.ResponseWriter, r *http.Request) (*WebCaller, bool) {
	caller, err := o.auth.Authenticate(r)
	if err != nil {
		o.opts.Log.Error("web_identify_failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, struct {
			Error string `json:"error"`
		}{"the vault is not answering"})
		return nil, false
	}
	if caller == nil {
		return nil, true
	}
	return caller, true
}

func (o *OAuth) sessionGET(w http.ResponseWriter, r *http.Request) {
	caller, ok := o.authenticate(w, r)
	if !ok {
		return
	}
	if caller == nil {
		writeFailure(w, &apiFailure{Status: http.StatusUnauthorized, Error: "Unauthorized", Message: "sign in"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}{caller.DID, caller.Handle})
}

func (o *OAuth) sessionDELETE(w http.ResponseWriter, r *http.Request) {
	caller, ok := o.authenticate(w, r)
	if !ok {
		return
	}
	if caller == nil {
		w.Header().Set("Set-Cookie", o.sessionCookie("", 0))
		writeFailure(w, &apiFailure{Status: http.StatusUnauthorized, Error: "Unauthorized", Message: "sign in"})
		return
	}
	result, err := o.opts.SignIn.SignOut(r.Context(), caller)
	if err != nil {
		o.opts.Log.Error("web_sign_out_failed", "did", caller.DID, "err", err)
		w.Header().Set("Set-Cookie", o.sessionCookie("", 0))
		writeFailure(w, &apiFailure{Status: http.StatusBadGateway, Error: "could not sign out"})
		return
	}
	if o.opts.OnSignedOut != nil {
		if err := o.opts.OnSignedOut(r.Context(), &WebUser{DID: caller.DID, Handle: caller.Handle, PDS: caller.PDS}); err != nil {
			o.opts.Log.Warn("web_signed_out_hook_failed", "did", caller.DID, "err", err)
		}
	}
	o.opts.Log.Info("web_signed_out", "did", caller.DID, "revoked", result.Revoked, "revokeError", result.RevokeError)
	w.Header().Set("Set-Cookie", o.sessionCookie("", 0))
	writeJSON(w, http.StatusOK, struct {
		Revoked     bool   `json:"revoked"`
		RevokeError string `json:"revokeError,omitempty"`
	}{result.Revoked, result.RevokeError})
}

func parseContentLength(r *http.Request) (int64, error) {
	raw := r.Header.Get("Content-Length")
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
