package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/workloadoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

const (
	scrobbleCollection = "com.vibescrobble.scrobble"
	agentsCollection   = "com.fedfork.irc.agents"
	serviceDID         = "did:web:pds.test"
	workloadSubject    = "actx:bidder:plc:requester:role:vm1"
	roleName           = "vm1"
)

var appScopes = []string{"atproto", "repo:" + scrobbleCollection + "?action=create"}

type issuer struct {
	srv *httptest.Server
	url string
	key *ecdsa.PrivateKey
	kid string
}

const issuerPath = "/v1/auth/sshsig/wid"

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key, kid: "k1"}
	mux := http.NewServeMux()
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	is.url = is.srv.URL + issuerPath
	mux.HandleFunc("GET "+issuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": is.url, "jwks_uri": is.url + "/.well-known/keys"})
	})
	mux.HandleFunc("GET "+issuerPath+"/.well-known/keys", func(w http.ResponseWriter, r *http.Request) {
		pad := func(b []byte) string {
			out := make([]byte, 32)
			copy(out[32-len(b):], b)
			return base64.RawURLEncoding.EncodeToString(out)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "EC", "crv": "P-256", "kid": is.kid, "alg": "ES256", "use": "sig",
			"x": pad(key.PublicKey.X.Bytes()), "y": pad(key.PublicKey.Y.Bytes()),
		}}})
	})
	return is
}

func (is *issuer) token(t *testing.T, sub, aud string) string {
	t.Helper()
	now := time.Now()
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": is.url, "sub": sub, "aud": aud, "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti),
	})
	tok.Header["kid"] = is.kid
	signed, err := tok.SignedString(is.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type world struct {
	t        *testing.T
	pdsURL   string
	pds      *atpds.Server
	did      string
	password string
	issuer   *issuer
	other    *issuer
	app      *appServer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	plcSrv := httptest.NewServer(plc.NewServer(plc.Options{}))
	t.Cleanup(plcSrv.Close)
	dir := t.TempDir()
	repos, err := repo.NewSQLiteStore(ctx, dir+"/repos.db")
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := atpds.NewStore(ctx, dir+"/accounts.db")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := files.NewFileBlobStore(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		t.Fatal(err)
	}
	rotationPub, _ := rotation.PublicKey()
	submitter, err := plcsubmit.New(plcSrv.URL, rotation, plcSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	signerKey, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	server, err := atpds.New(atpds.Config{
		Mode:      atpds.ModeFull,
		Repos:     repos,
		Store:     accounts,
		KeyStore:  files.NewMemoryRepoKeyStore(),
		Directory: &identity.BaseDirectory{PLCURL: plcSrv.URL, HTTPClient: *plcSrv.Client(), PLCClient: plcSrv.Client()},
		PLC:       submitter,
		Blobs:     blobs,
		OAuth: &atpds.OAuthConfig{
			Issuer:   srv.URL,
			Signer:   oauth.Signer{DID: syntax.DID(serviceDID), Key: signerKey},
			Workload: &atpds.WorkloadOAuthConfig{AllowLoopbackIssuers: true},
		},
		ServiceDID:           serviceDID,
		PublicBaseURL:        srv.URL,
		RotationKey:          rotationPub.DIDKey(),
		SessionSecret:        []byte("0123456789abcdef0123456789abcdef"),
		AvailableUserDomains: []string{".test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = server.Handler()
	w := &world{t: t, pdsURL: srv.URL, pds: server, password: "correct horse battery staple", issuer: newIssuer(t), other: newIssuer(t)}
	var out struct {
		DID string `json:"did"`
	}
	w.xrpc(http.MethodPost, "com.atproto.server.createAccount", "", map[string]string{"handle": "alice.test", "password": w.password}, &out, http.StatusOK)
	w.did = out.DID
	w.app = newApp(t, w.pdsURL)
	return w
}

func (w *world) xrpc(method, nsid, bearer string, in, out any, want int) int {
	w.t.Helper()
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, w.pdsURL+"/xrpc/"+nsid, body)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if want != 0 && res.StatusCode != want {
		w.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, nsid, res.StatusCode, want, raw)
	}
	if out != nil && res.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode
}

func (w *world) passwordSession() string {
	var out struct {
		AccessJwt string `json:"accessJwt"`
	}
	w.xrpc(http.MethodPost, "com.atproto.server.createSession", "", map[string]string{"identifier": w.did, "password": w.password}, &out, http.StatusOK)
	return out.AccessJwt
}

func (w *world) allow(scopes []string) {
	w.t.Helper()
	record, err := workloadoauth.Record(workloadoauth.Role{
		Name: roleName, Account: w.did, Service: serviceDID, Issuer: w.issuer.url, Subject: workloadSubject,
		Clients: []workloadoauth.Client{{ID: w.app.clientID, Scopes: scopes}},
	}, time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	w.xrpc(http.MethodPost, "com.atproto.repo.putRecord", w.passwordSession(),
		map[string]any{"repo": w.did, "collection": workloadauth.RBACCollection, "rkey": roleName, "record": record}, nil, http.StatusOK)
}

func (w *world) revoke() {
	w.xrpc(http.MethodPost, "com.atproto.repo.deleteRecord", w.passwordSession(),
		map[string]any{"repo": w.did, "collection": workloadauth.RBACCollection, "rkey": roleName}, nil, http.StatusOK)
}

func (w *world) audience() string { return oauth.WorkloadAudience(w.did, roleName) }

func (w *world) recordExists(collection, rkey string) bool {
	q := url.Values{"repo": {w.did}, "collection": {collection}, "rkey": {rkey}}
	res, err := http.Get(w.pdsURL + "/xrpc/com.atproto.repo.getRecord?" + q.Encode())
	if err != nil {
		w.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

type appSession struct {
	data oauthclient.SessionData
	key  atcrypto.PrivateKeyExportable
}

type appServer struct {
	t        *testing.T
	srv      *httptest.Server
	pdsURL   string
	clientID string
	redirect string
	scopes   []string
	mu       sync.Mutex
	pending  map[string]oauthclient.PendingAuth
	sessions map[string]*appSession
}

func newApp(t *testing.T, pdsURL string) *appServer {
	a := &appServer{t: t, pdsURL: pdsURL, pending: map[string]oauthclient.PendingAuth{}, sessions: map[string]*appSession{}, scopes: appScopes}
	mux := http.NewServeMux()
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	a.redirect = a.srv.URL + "/oauth/callback"
	q := url.Values{"redirect_uri": {a.redirect}, "scope": {strings.Join(a.scopes, " ")}}
	a.clientID = "http://localhost?" + q.Encode()
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET /oauth/callback", a.callback)
	mux.HandleFunc("POST /post", a.post)
	mux.HandleFunc("POST /refresh", a.refresh)
	return a
}

func dpopForm(ctx context.Context, key atcrypto.PrivateKeyExportable, target string, form url.Values) (*http.Response, []byte, error) {
	nonce := ""
	for attempt := 0; ; attempt++ {
		proof, err := oauthclient.DPoPProof(key, http.MethodPost, target, "", nonce)
		if err != nil {
			return nil, nil, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", proof)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if attempt == 0 && strings.Contains(string(raw), "use_dpop_nonce") {
			nonce = res.Header.Get("DPoP-Nonce")
			continue
		}
		return res, raw, nil
	}
}

func (a *appServer) login(w http.ResponseWriter, r *http.Request) {
	key, _ := atcrypto.GeneratePrivateKeyP256()
	state, _ := oauthclient.NewCallbackState()
	verifier, challenge, _ := oauthclient.GeneratePKCE()
	form := url.Values{
		"client_id": {a.clientID}, "redirect_uri": {a.redirect}, "response_type": {"code"}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {strings.Join(a.scopes, " ")},
		"login_hint": {r.URL.Query().Get("handle")},
	}
	res, raw, err := dpopForm(r.Context(), key, a.pdsURL+"/oauth/par", form)
	if err != nil || res.StatusCode != http.StatusOK {
		http.Error(w, "par failed: "+string(raw), http.StatusBadGateway)
		return
	}
	var par struct {
		RequestURI string `json:"request_uri"`
	}
	_ = json.Unmarshal(raw, &par)
	a.mu.Lock()
	a.pending[state] = oauthclient.PendingAuth{State: state, CodeVerifier: verifier, RedirectURI: a.redirect, ClientID: a.clientID,
		HostURL: a.pdsURL, Issuer: a.pdsURL, AuthServerURL: a.pdsURL, TokenEndpoint: a.pdsURL + "/oauth/token",
		Scopes: a.scopes, DPoPPrivateKeyMultibase: key.Multibase()}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "state", Value: state, Path: "/"})
	q := url.Values{"client_id": {a.clientID}, "request_uri": {par.RequestURI}}
	http.Redirect(w, r, a.pdsURL+"/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (a *appServer) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("state")
	state := r.URL.Query().Get("state")
	if err != nil || cookie.Value != state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	pending, ok := a.pending[state]
	delete(a.pending, state)
	a.mu.Unlock()
	if !ok || r.URL.Query().Get("iss") != a.pdsURL {
		http.Error(w, "unknown state or issuer", http.StatusBadRequest)
		return
	}
	data, err := oauthclient.ExchangeCode(r.Context(), http.DefaultClient, pending, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	key, _ := atcrypto.ParsePrivateMultibase(pending.DPoPPrivateKeyMultibase)
	sid, _ := oauthclient.NewCallbackState()
	a.mu.Lock()
	a.sessions[sid] = &appSession{data: data, key: key}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/"})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *appServer) session(r *http.Request) *appSession {
	cookie, err := r.Cookie("sid")
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[cookie.Value]
}

func (a *appServer) post(w http.ResponseWriter, r *http.Request) {
	s := a.session(r)
	if s == nil {
		http.Error(w, "sign in", http.StatusUnauthorized)
		return
	}
	var in struct {
		Collection string `json:"collection"`
		Text       string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	body, _ := json.Marshal(map[string]any{"repo": s.data.AccountDID, "collection": in.Collection,
		"record": map[string]any{"$type": in.Collection, "text": in.Text, "createdAt": time.Now().UTC().Format(time.RFC3339)}})
	target := a.pdsURL + "/xrpc/com.atproto.repo.createRecord"
	nonce := ""
	for attempt := 0; ; attempt++ {
		proof, _ := oauthclient.DPoPProof(s.key, http.MethodPost, target, s.data.AccessToken, nonce)
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "DPoP "+s.data.AccessToken)
		req.Header.Set("DPoP", proof)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if attempt == 0 && strings.Contains(string(raw), "use_dpop_nonce") {
			nonce = res.Header.Get("DPoP-Nonce")
			continue
		}
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(raw)
		return
	}
}

func (a *appServer) refresh(w http.ResponseWriter, r *http.Request) {
	s := a.session(r)
	if s == nil {
		http.Error(w, "sign in", http.StatusUnauthorized)
		return
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.data.RefreshToken}, "client_id": {a.clientID}}
	res, raw, err := dpopForm(r.Context(), s.key, a.pdsURL+"/oauth/token", form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if res.StatusCode == http.StatusOK {
		var out struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(raw, &out)
		a.mu.Lock()
		s.data.AccessToken, s.data.RefreshToken = out.AccessToken, out.RefreshToken
		a.mu.Unlock()
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(raw)
}

func newJar(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func appCall(t *testing.T, client *http.Client, path string, in any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(in)
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func tokenFor(t *testing.T, is *issuer, sub string) func(context.Context, string) (string, error) {
	return func(_ context.Context, aud string) (string, error) { return is.token(t, sub, aud), nil }
}

func statusOf(err error) int {
	var se *workloadoauth.StatusError
	if err == nil {
		return 0
	}
	if e, ok := err.(*workloadoauth.StatusError); ok {
		return e.Status
	}
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return -1
		}
		err = u.Unwrap()
		if e, ok := err.(*workloadoauth.StatusError); ok {
			se = e
			return se.Status
		}
	}
}

func signIn(t *testing.T, w *world, sub string, is *issuer) (*http.Client, error) {
	t.Helper()
	browser := newJar(t)
	session, err := workloadoauth.NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.ApproveLogin(context.Background(), browser, w.app.srv.URL+"/login", "alice.test", w.did, roleName, tokenFor(t, is, sub))
	return browser, err
}

func TestWorkloadSignsInToAnAppAsItsAccountAndTheAppWritesWithinItsScope(t *testing.T) {
	w := newWorld(t)
	w.allow(appScopes)
	browser, err := signIn(t, w, workloadSubject, w.issuer)
	if err != nil {
		t.Fatalf("the workload sign-in: %v", err)
	}
	status, body := appCall(t, browser, w.app.srv.URL+"/post", map[string]string{"collection": scrobbleCollection, "text": "\U0001F680 shipped"})
	if status != http.StatusOK {
		t.Fatalf("the app's scrobble write answered %d: %s", status, body)
	}
	var created struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	parts := strings.Split(created.URI, "/")
	if len(parts) < 5 || !w.recordExists(scrobbleCollection, parts[len(parts)-1]) {
		t.Fatalf("the scrobble record %q is not in the account's repo", created.URI)
	}
	status, body = appCall(t, browser, w.app.srv.URL+"/post", map[string]string{"collection": agentsCollection, "text": "outside the grant"})
	if status != http.StatusForbidden || !strings.Contains(body, "ScopeMissingError") {
		t.Fatalf("a write outside the session's scope answered %d: %s", status, body)
	}
}

func TestAnotherWorkloadSubjectOrIssuerIsRefused(t *testing.T) {
	w := newWorld(t)
	w.allow(appScopes)
	if _, err := signIn(t, w, "actx:bidder:plc:requester:role:other", w.issuer); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a token for another subject: %v", err)
	}
	if _, err := signIn(t, w, workloadSubject, w.other); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a token from another issuer: %v", err)
	}
}

func TestNoRecordNoSignIn(t *testing.T) {
	w := newWorld(t)
	if _, err := signIn(t, w, workloadSubject, w.issuer); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a sign-in with no record: %v", err)
	}
}

func TestAScopeOutsideTheRecordIsRefusedAndTheRequestStaysPending(t *testing.T) {
	w := newWorld(t)
	w.allow([]string{"atproto"})
	ctx := context.Background()
	browser := newJar(t)
	request, err := workloadoauth.StartLogin(ctx, browser, w.app.srv.URL+"/login", "alice.test")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := workloadoauth.NewSession(nil)
	token, err := session.Exchange(ctx, request.Origin, w.audience(), w.issuer.token(t, workloadSubject, w.audience()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Approve(ctx, request.Origin, token, request.RequestURI); statusOf(err) != http.StatusForbidden {
		t.Fatalf("an approval of a scope the record does not allow: %v", err)
	}
	w.allow(appScopes)
	token, err = session.Exchange(ctx, request.Origin, w.audience(), w.issuer.token(t, workloadSubject, w.audience()))
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := session.Approve(ctx, request.Origin, token, request.RequestURI)
	if err != nil {
		t.Fatalf("the same pending request after the record allows it: %v", err)
	}
	if _, _, err := workloadoauth.Callback(ctx, browser, redirect); err != nil {
		t.Fatal(err)
	}
}

func TestTokensAreSingleUseAndBoundToTheirKey(t *testing.T) {
	w := newWorld(t)
	w.allow(appScopes)
	ctx := context.Background()
	browser := newJar(t)
	request, err := workloadoauth.StartLogin(ctx, browser, w.app.srv.URL+"/login", "alice.test")
	if err != nil {
		t.Fatal(err)
	}
	session, _ := workloadoauth.NewSession(nil)
	subject := w.issuer.token(t, workloadSubject, w.audience())
	token, err := session.Exchange(ctx, request.Origin, w.audience(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exchange(ctx, request.Origin, w.audience(), subject); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a second exchange of one workload token: %v", err)
	}
	thief, _ := workloadoauth.NewSession(nil)
	if _, err := thief.Approve(ctx, request.Origin, token, request.RequestURI); statusOf(err) != http.StatusUnauthorized {
		t.Fatalf("the PDS token under another DPoP key: %v", err)
	}
	if _, err := session.Approve(ctx, request.Origin, token, request.RequestURI); err != nil {
		t.Fatalf("the approval: %v", err)
	}
	if _, err := session.Approve(ctx, request.Origin, token, request.RequestURI); statusOf(err) != http.StatusUnauthorized {
		t.Fatalf("a second use of the PDS token: %v", err)
	}
}

func TestDeletingTheRecordEndsTheSessionsItAdmitted(t *testing.T) {
	w := newWorld(t)
	w.allow(appScopes)
	browser, err := signIn(t, w, workloadSubject, w.issuer)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := appCall(t, browser, w.app.srv.URL+"/refresh", nil); status != http.StatusOK {
		t.Fatalf("a refresh while the record stands answered %d: %s", status, body)
	}
	w.revoke()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _ := appCall(t, browser, w.app.srv.URL+"/post", map[string]string{"collection": scrobbleCollection, "text": "\U0001F680 after revoke"})
		if status == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session still writes after its record was deleted (HTTP %d)", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, body := appCall(t, browser, w.app.srv.URL+"/refresh", nil); status != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("a refresh after the record was deleted answered %d: %s", status, body)
	}
}
