//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	webSessionCookieName  = "swc_session"
	webClientMetadataPath = "/oauth-client-metadata.json"
	webCallbackPath       = "/oauth/callback"
	webClientTimeout      = 30 * time.Second
	webSignInTimeout      = 2 * time.Minute
)

type webAppSession struct {
	t      testing.TB
	origin string
	client *http.Client
}

func newWebAppSession(t testing.TB, origin string) *webAppSession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("the cookie jar: %v", err)
	}
	return &webAppSession{t: t, origin: origin, client: &http.Client{
		Timeout: webClientTimeout,
		Jar:     jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (w *webAppSession) do(method, path string, body any) (int, http.Header, []byte) {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("encoding the %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.origin+path, reader)
	if err != nil {
		w.t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Origin", w.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		w.t.Fatalf("reading %s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, raw
}

func startTCPProxy(t testing.TB, ln net.Listener, backend string) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				upstream, err := net.DialTimeout("tcp", backend, 5*time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				go io.Copy(upstream, conn)
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
}

func TestWebAppSignInOverLoopbackOriginIssuesAClientCertificate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	originListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = originListener.Close() })
	origin := "http://" + originListener.Addr().String()

	n := StartNetwork(t, NetworkOptions{
		Bidders:        1,
		WithGateway:    true,
		UpTimeout:      5 * time.Minute,
		GatewayOptions: map[string]string{"public-origin": origin},
	})
	startTCPProxy(t, originListener, n.xrpcAddress(t))

	tn := n.Tenant(t, "tenant", tenantReady)
	waitOwnerGrant(t, n, tn)
	acct := n.OwnerAccount(t)
	web := newWebAppSession(t, origin)

	code, _, raw := web.do(http.MethodGet, webClientMetadataPath, nil)
	for deadline := time.Now().Add(webSignInTimeout); code != http.StatusOK; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: %d %.200s", webClientMetadataPath, code, raw)
		}
		code, _, raw = web.do(http.MethodGet, webClientMetadataPath, nil)
	}
	var metadata struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("the client metadata is not JSON: %v %.200s", err, raw)
	}
	wantClientID := origin + webClientMetadataPath
	if metadata.ClientID != wantClientID {
		t.Fatalf("the client metadata client_id: want %s, got %s", wantClientID, metadata.ClientID)
	}
	wantCallback := origin + webCallbackPath
	registered := false
	for _, redirectURI := range metadata.RedirectURIs {
		if redirectURI == wantCallback {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("the client metadata does not register %s: %v", wantCallback, metadata.RedirectURIs)
	}

	code, _, raw = web.do(http.MethodPost, "/oauth/start", map[string]string{"handle": acct.DID})
	for deadline := time.Now().Add(webSignInTimeout); code != http.StatusOK; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("POST /oauth/start: %d %.300s", code, raw)
		}
		code, _, raw = web.do(http.MethodPost, "/oauth/start", map[string]string{"handle": acct.DID})
	}
	var started struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.AuthorizationURL == "" {
		t.Fatalf("POST /oauth/start returned no authorization URL: %v %.200s", err, raw)
	}
	authorize, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatalf("the authorization URL does not parse: %v", err)
	}
	pdsURL, err := url.Parse(acct.pds)
	if err != nil {
		t.Fatalf("the owner PDS URL does not parse: %v", err)
	}
	if authorize.Path != "/oauth/authorize" {
		t.Fatalf("the authorization URL path: want /oauth/authorize, got %s from %s", authorize.Path, started.AuthorizationURL)
	}
	if authorize.Port() != pdsURL.Port() {
		t.Fatalf("the authorization URL port: want the PDS port %s, got %s from %s", pdsURL.Port(), authorize.Port(), started.AuthorizationURL)
	}
	requestURI := authorize.Query().Get("request_uri")
	if requestURI == "" {
		t.Fatalf("the authorization URL carries no request_uri: %s", started.AuthorizationURL)
	}
	if clientID := authorize.Query().Get("client_id"); clientID != wantClientID {
		t.Fatalf("the authorization URL client_id: want %s, got %s", wantClientID, clientID)
	}
	t.Logf("the sign-in start pushed a PAR and named the PDS authorize URL with client_id %s", wantClientID)

	form := url.Values{
		"identifier":  {acct.identifier},
		"password":    {acct.password},
		"request_uri": {requestURI},
		"client_id":   {wantClientID},
	}
	resp, err := acct.client.PostForm(acct.pds+"/oauth/authorize", form)
	if err != nil {
		t.Fatalf("POST the PDS authorize: %v", err)
	}
	callbackRaw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("reading the PDS authorize answer: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST the PDS authorize: %d %.300s", resp.StatusCode, callbackRaw)
	}
	var authorized struct {
		RedirectURI string `json:"redirect_uri"`
		Code        string `json:"code"`
	}
	if err := json.Unmarshal(callbackRaw, &authorized); err != nil || authorized.RedirectURI == "" || authorized.Code == "" {
		t.Fatalf("the PDS authorize returned no code: %v %.200s", err, callbackRaw)
	}
	callback, err := url.Parse(authorized.RedirectURI)
	if err != nil {
		t.Fatalf("the callback URL does not parse: %v", err)
	}
	if callback.Path != webCallbackPath {
		t.Fatalf("the callback URL path: want %s, got %s from %s", webCallbackPath, callback.Path, authorized.RedirectURI)
	}
	if callback.Query().Get("state") == "" || callback.Query().Get("iss") == "" {
		t.Fatalf("the callback URL carries no state or iss: %s", authorized.RedirectURI)
	}

	code, header, raw := web.do(http.MethodGet, callback.Path+"?"+callback.RawQuery, nil)
	if code != http.StatusFound {
		t.Fatalf("GET %s: want 302, got %d %.300s", webCallbackPath, code, raw)
	}
	if location := header.Get("Location"); location != origin+"/" {
		t.Fatalf("the callback redirect: want %s/, got %q", origin, location)
	}
	sessionCookie := ""
	for _, value := range header.Values("Set-Cookie") {
		if strings.HasPrefix(value, webSessionCookieName+"=") {
			sessionCookie = value
		}
	}
	if sessionCookie == "" {
		t.Fatalf("the callback left no %s cookie: %v", webSessionCookieName, header.Values("Set-Cookie"))
	}
	for _, attribute := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sessionCookie, attribute) {
			t.Fatalf("the session cookie lacks %s: %s", attribute, sessionCookie)
		}
	}
	if strings.Contains(sessionCookie, "Secure") {
		t.Fatalf("the session cookie of the http origin is Secure: %s", sessionCookie)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		code, _, raw = web.do(http.MethodGet, "/session", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /session attempt %d: %d %.200s", attempt, code, raw)
		}
		var session struct {
			DID string `json:"did"`
		}
		if err := json.Unmarshal(raw, &session); err != nil || session.DID != acct.DID {
			t.Fatalf("GET /session attempt %d: want did %s, got %v %.200s", attempt, acct.DID, err, raw)
		}
	}

	code, _, raw = web.do(http.MethodPost, "/api/clusters/list", map[string]any{})
	if code != http.StatusOK || !strings.Contains(string(raw), `"tenant"`) {
		t.Fatalf("POST /api/clusters/list through the session cookie: %d %.300s", code, raw)
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		code, _, listRaw := web.do(http.MethodPost, "/api/clusters/list", map[string]any{})
		if code == http.StatusOK && strings.Contains(string(listRaw), `"name":"tenant"`) && strings.Contains(string(listRaw), `"phase":"Ready"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tenant cluster did not report Ready: %d %.300s", code, listRaw)
		}
		time.Sleep(2 * time.Second)
	}
	for {
		code, header, raw = web.do(http.MethodPost, "/api/clusters/kubeconfig", map[string]string{"name": "tenant"})
		if code == http.StatusOK {
			break
		}
		if code != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("POST /api/clusters/kubeconfig: %d %.300s", code, raw)
		}
		time.Sleep(2 * time.Second)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("the kubeconfig Cache-Control: want no-store, got %q", cacheControl)
	}
	var issued struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := json.Unmarshal(raw, &issued); err != nil || issued.Kubeconfig == "" {
		t.Fatalf("the kubeconfig download returned no kubeconfig: %v %.200s", err, raw)
	}
	cfg, err := clientcmd.Load([]byte(issued.Kubeconfig))
	if err != nil {
		t.Fatalf("the kubeconfig does not parse: %v", err)
	}
	if len(cfg.AuthInfos) != 1 {
		t.Fatalf("the kubeconfig holds %d users, want 1", len(cfg.AuthInfos))
	}
	for _, authInfo := range cfg.AuthInfos {
		if len(authInfo.ClientCertificateData) == 0 || len(authInfo.ClientKeyData) == 0 || authInfo.Exec != nil || authInfo.Token != "" {
			t.Fatalf("the kubeconfig user is not a plain client certificate: cert %d key %d exec %t token %t",
				len(authInfo.ClientCertificateData), len(authInfo.ClientKeyData), authInfo.Exec != nil, authInfo.Token != "")
		}
		block, _ := pem.Decode(authInfo.ClientCertificateData)
		if block == nil {
			t.Fatal("the client certificate is not PEM")
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the client certificate: %v", err)
		}
		if leaf.Subject.CommonName != acct.DID {
			t.Fatalf("the client certificate CN: want %s, got %s", acct.DID, leaf.Subject.CommonName)
		}
		if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != ownersGroup {
			t.Fatalf("the client certificate organization: want [%s], got %v", ownersGroup, leaf.Subject.Organization)
		}
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(issued.Kubeconfig))
	if err != nil {
		t.Fatalf("the kubeconfig REST config: %v", err)
	}
	restConfig.Timeout = 15 * time.Second
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("the kubeconfig client: %v", err)
	}
	if _, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("client-go Nodes().List with the session certificate: %v", err)
	}
	t.Logf("the web session issued a client certificate for %s that the tenant cluster accepted", acct.DID)
}
