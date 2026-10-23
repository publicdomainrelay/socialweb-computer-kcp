//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"
	"github.com/ipfs/go-cid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	fakedynamic "k8s.io/client-go/dynamic/fake"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	eventsvc "github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	servicespds "github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
)

const (
	marketFixtureCollection = "com.publicdomainrelay.temp.market.bid"
	marketFixtureAccept     = "com.publicdomainrelay.temp.market.accept"
	marketFixtureReceipt    = "com.publicdomainrelay.temp.market.receipt"
	marketFixtureRFP        = "com.publicdomainrelay.temp.market.rfp"
	marketFixtureHandler    = "market-fixture"
)

func marketRandomHex(t testing.TB, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	return hex.EncodeToString(buf)
}

func marketRandomDID(t testing.TB) syntax.DID {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random did: %v", err)
	}
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	did, err := syntax.ParseDID("did:plc:" + id)
	if err != nil {
		t.Fatalf("fixture did: %v", err)
	}
	return did
}

func marketJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".fixture"
}

type marketPDSOptions struct {
	URL      string
	DID      string
	Handle   string
	Password string
}

type marketPDSFixture struct {
	t testing.TB

	URL      string
	DID      syntax.DID
	Handle   string
	password string

	local  bool
	server *httptest.Server
	pds    *pds.Server
	store  *pds.Store
	repos  *repo.SQLiteStore
	keys   *servicespds.FileRepoKeyStore
}

func newMarketPDSFixture(t testing.TB, opts marketPDSOptions) *marketPDSFixture {
	t.Helper()
	fx := &marketPDSFixture{t: t, local: opts.URL == ""}
	if !fx.local {
		fx.URL = strings.TrimRight(opts.URL, "/")
		fx.Handle = opts.Handle
		if fx.Handle == "" {
			fx.Handle = os.Getenv("SOCIALWEB_IT_MARKET_PDS_HANDLE")
		}
		did, err := syntax.ParseDID(strings.TrimSpace(opts.DID))
		if err != nil {
			t.Fatalf("fixture remote PDS did %q: %v", opts.DID, err)
		}
		fx.DID = did
	} else {
		fx.DID = marketRandomDID(t)
		fx.Handle = "market" + marketRandomHex(t, 4) + ".test"
		dir := t.TempDir()
		var err error
		fx.repos, err = repo.NewSQLiteStore(dir + "/repo.db")
		if err != nil {
			t.Fatalf("fixture repo store: %v", err)
		}
		fx.store, err = pds.NewStore(dir + "/account.db")
		if err != nil {
			t.Fatalf("fixture account store: %v", err)
		}
		fx.keys, err = servicespds.NewFileRepoKeyStore(dir + "/keys")
		if err != nil {
			t.Fatalf("fixture key store: %v", err)
		}
		password := "fixture-" + marketRandomHex(t, 8)
		if err := fx.seedAccount(context.Background(), password); err != nil {
			t.Fatalf("fixture seed account: %v", err)
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			t.Fatalf("fixture session secret: %v", err)
		}
		fx.pds, err = pds.New(pds.Config{
			Mode:          pds.ModeFull,
			Repos:         fx.repos,
			Store:         fx.store,
			KeyStore:      fx.keys,
			ServiceDID:    fx.DID.String(),
			SessionSecret: secret,
		})
		if err != nil {
			t.Fatalf("fixture pds: %v", err)
		}
		fx.server = httptest.NewServer(fx.pds.Handler())
		fx.URL = fx.server.URL
		t.Cleanup(fx.Close)
	}
	if opts.Password != "" {
		fx.password = opts.Password
	}
	return fx
}

func (f *marketPDSFixture) seedAccount(ctx context.Context, password string) error {
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return err
	}
	if _, err := f.store.CreateAccount(ctx, pds.NewAccount{
		DID:      f.DID,
		Handle:   f.Handle,
		Password: password,
	}); err != nil {
		return err
	}
	if err := f.keys.PutRepoKey(ctx, f.DID, key); err != nil {
		return err
	}
	if _, err := f.repos.CreateRepo(ctx, f.DID, key); err != nil {
		return err
	}
	f.password = password
	return nil
}

func (f *marketPDSFixture) Close() {
	if f.server != nil {
		f.server.Close()
		f.server = nil
	}
	if f.store != nil {
		_ = f.store.Close()
		f.store = nil
	}
	if f.repos != nil {
		_ = f.repos.Close()
		f.repos = nil
	}
}

func (f *marketPDSFixture) passwordValue() string {
	if f.password != "" {
		return f.password
	}
	return os.Getenv("SOCIALWEB_IT_MARKET_PDS_PASSWORD")
}

func (f *marketPDSFixture) createSession(t testing.TB) pdsclient.Session {
	t.Helper()
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: f.URL, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("fixture pds client: %v", err)
	}
	session, err := client.CreateSession(context.Background(), f.Handle, f.passwordValue())
	if err != nil {
		t.Fatalf("fixture create session at %s: %v", f.URL, err)
	}
	f.DID = session.DID
	return session
}

func (f *marketPDSFixture) clientFor(token string) pdsclient.PDS {
	tokens := pdsclient.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL: f.URL,
		Tokens:  tokens,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		f.t.Fatalf("fixture authenticated pds client: %v", err)
	}
	return client
}

type marketRemoteFixture struct {
	Object  pdsv1alpha1.RemotePDS
	Secrets pdsclient.SecretResolver
}

func newMarketRemoteFixture(t testing.TB, pdsfx *marketPDSFixture, session pdsclient.Session, withAuth bool) *marketRemoteFixture {
	t.Helper()
	object := pdsv1alpha1.RemotePDS{
		ObjectMeta: metav1.ObjectMeta{Name: "market-remote"},
		Spec: pdsv1alpha1.RemotePDSSpec{
			URL: pdsfx.URL,
			DID: pdsfx.DID.String(),
		},
	}
	fx := &marketRemoteFixture{Object: object}
	if !withAuth {
		return fx
	}
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatalf("fixture dpop key: %v", err)
	}
	fx.Object.Spec.Auth = pdsv1alpha1.RemotePDSAuth{
		AccessTokenRef: "market/access",
		DPoPKeyRef:     "market/dpop",
	}
	secrets := map[string]map[string]any{
		"market/access": {"accessJwt": session.AccessJWT},
		"market/dpop":   {"dpopPrivateKeyMultibase": key.Multibase()},
	}
	fx.Secrets = pdsclient.SecretResolverFunc(func(_ context.Context, ref string) (map[string]any, error) {
		data, ok := secrets[strings.TrimSpace(ref)]
		if !ok {
			return nil, fmt.Errorf("market fixture: no secret %s", ref)
		}
		out := map[string]any{}
		for k, v := range data {
			out[k] = v
		}
		return out, nil
	})
	return fx
}

func (f *marketRemoteFixture) resolve(t testing.TB) pdsclient.ResolvedRepo {
	t.Helper()
	resolver := &pdsclient.RepoResolver{Secrets: f.Secrets, Timeout: 10 * time.Second}
	resolved, err := resolver.Resolve(context.Background(), pdsclient.RepoBinding{
		Kind:      "RemotePDS",
		Name:      f.Object.Name,
		DID:       f.Object.Spec.DID,
		RemotePDS: &f.Object,
	})
	if err != nil {
		t.Fatalf("fixture resolve RemotePDS: %v", err)
	}
	return resolved
}

type marketTokenServer struct {
	server *httptest.Server
	mode   string
	mu     sync.Mutex
	access string
}

func newMarketTokenServer(t testing.TB, mode string) *marketTokenServer {
	t.Helper()
	fx := &marketTokenServer{mode: mode}
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/token") {
			http.NotFound(w, r)
			return
		}
		if fx.mode == "revoked" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"the session was revoked"}`))
			return
		}
		fx.mu.Lock()
		fx.access = marketJWT(time.Now().Add(time.Hour))
		access := fx.access
		fx.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  access,
			"refresh_token": "fixture-refresh-" + marketRandomHex(t, 4),
			"scope":         "atproto",
		})
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

func (f *marketTokenServer) TokenURL() string { return f.server.URL + "/token" }

type marketOAuthFixture struct {
	Store   *oauthclient.MemoryStore
	Data    oauthclient.SessionData
	Session *oauthclient.Session
}

func newMarketOAuthBrowserFixture(t testing.TB, pdsfx *marketPDSFixture, delegated bool) *marketOAuthFixture {
	t.Helper()
	session := pdsfx.createSession(t)
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatalf("fixture oauth key: %v", err)
	}
	did := session.DID.String()
	data := oauthclient.SessionData{
		AccountDID:              did,
		SessionID:               oauthclient.SessionID(did, pdsfx.URL),
		HostURL:                 pdsfx.URL,
		Scopes:                  []string{"atproto"},
		AccessToken:             session.AccessJWT,
		RefreshToken:            session.RefreshJWT,
		DPoPPrivateKeyMultibase: key.Multibase(),
		AuthServerTokenEndpoint: newMarketTokenServer(t, "refresh").TokenURL(),
	}
	store := &oauthclient.MemoryStore{}
	if err := store.Save(context.Background(), data); err != nil {
		t.Fatalf("fixture oauth store: %v", err)
	}
	clientID := "fixture-browser"
	if delegated {
		clientID = "fixture-delegated"
	}
	opened, err := oauthclient.Open(context.Background(), oauthclient.Options{
		DID:        did,
		PDSURL:     pdsfx.URL,
		ClientID:   clientID,
		Scopes:     data.Scopes,
		Store:      store,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("fixture oauth open: %v", err)
	}
	return &marketOAuthFixture{Store: store, Data: data, Session: opened}
}

func newMarketOAuthQrFixture(t testing.TB, pdsfx *marketPDSFixture, qr *marketQRFixture) *marketOAuthFixture {
	t.Helper()
	session := pdsfx.createSession(t)
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatalf("fixture qr key: %v", err)
	}
	did := session.DID.String()
	qrSession := oauthclient.QRSession{
		AccessJWT:      session.AccessJWT,
		RefreshJWT:     session.RefreshJWT,
		UserDID:        did,
		Handle:         pdsfx.Handle,
		PDS:            pdsfx.URL,
		DPoPPrivateJWK: map[string]any{"kty": "EC", "d": base64.RawURLEncoding.EncodeToString(key.Bytes())},
	}
	nonce, err := oauthclient.NewQRNonce()
	if err != nil {
		t.Fatalf("fixture qr nonce: %v", err)
	}
	cliDID := "did:web:fixture.invalid"
	transfer, err := oauthclient.NewQRTransfer(qr.URL(), cliDID, nonce, oauthclient.ServiceAuthSignerFunc(
		func(context.Context, string, string, time.Duration) (string, error) {
			return "fixture-service-auth", nil
		}))
	if err != nil {
		t.Fatalf("fixture qr transfer: %v", err)
	}
	qr.Publish(nonce, qrSession)
	polled, ok, err := transfer.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("fixture qr poll: %v", err)
	}
	if !ok {
		t.Fatalf("fixture qr poll returned no session")
	}
	data, err := polled.SessionData("fixture-qr", []string{"atproto"})
	if err != nil {
		t.Fatalf("fixture qr session data: %v", err)
	}
	store := &oauthclient.MemoryStore{}
	if err := store.Save(context.Background(), data); err != nil {
		t.Fatalf("fixture qr store: %v", err)
	}
	opened, err := oauthclient.Open(context.Background(), oauthclient.Options{
		DID:        data.AccountDID,
		PDSURL:     data.HostURL,
		ClientID:   "fixture-qr",
		Scopes:     data.Scopes,
		Store:      store,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("fixture qr open: %v", err)
	}
	return &marketOAuthFixture{Store: store, Data: data, Session: opened}
}

type marketQRFixture struct {
	server  *httptest.Server
	mu      sync.Mutex
	byNonce map[string]oauthclient.QRSession
	burned  map[string]bool
}

func newMarketQRFixture(t testing.TB) *marketQRFixture {
	t.Helper()
	fx := &marketQRFixture{byNonce: map[string]oauthclient.QRSession{}, burned: map[string]bool{}}
	path := "/xrpc/" + oauthclient.QRNSID
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "missing service auth", http.StatusUnauthorized)
				return
			}
			var body struct {
				Nonce string `json:"nonce"`
			}
			raw := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			body.Nonce, _ = raw["nonce"].(string)
			if body.Nonce == "" {
				http.Error(w, "missing nonce", http.StatusBadRequest)
				return
			}
			session := oauthclient.QRSession{}
			encoded, _ := json.Marshal(raw)
			_ = json.Unmarshal(encoded, &session)
			fx.mu.Lock()
			fx.byNonce[body.Nonce] = session
			fx.burned[body.Nonce] = false
			fx.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "stored"})
		case http.MethodGet:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "missing service auth", http.StatusUnauthorized)
				return
			}
			nonce := r.URL.Query().Get("nonce")
			if nonce == "" {
				http.Error(w, "missing nonce", http.StatusBadRequest)
				return
			}
			fx.mu.Lock()
			session, ok := fx.byNonce[nonce]
			burned := fx.burned[nonce]
			fx.mu.Unlock()
			if !ok || burned {
				http.NotFound(w, r)
				return
			}
			fx.mu.Lock()
			fx.burned[nonce] = true
			fx.mu.Unlock()
			_ = json.NewEncoder(w).Encode(session)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

func (f *marketQRFixture) URL() string { return f.server.URL }

func (f *marketQRFixture) Publish(nonce string, session oauthclient.QRSession) {
	f.mu.Lock()
	f.byNonce[nonce] = session
	f.burned[nonce] = false
	f.mu.Unlock()
}

type marketEvent struct {
	Seq        int64
	DID        string
	Collection string
	Rkey       string
	CID        string
	Rev        string
	Time       time.Time
	Record     map[string]any
}

type marketRelayFixture struct {
	t      testing.TB
	server *httptest.Server
	format string

	mu         sync.Mutex
	events     []marketEvent
	subs       map[chan marketEvent]struct{}
	cursors    []int64
	closeAfter int
}

func newMarketRelayFixture(t testing.TB, format string) *marketRelayFixture {
	t.Helper()
	fx := &marketRelayFixture{t: t, format: format, subs: map[chan marketEvent]struct{}{}}
	fx.server = httptest.NewServer(http.HandlerFunc(fx.serve))
	t.Cleanup(fx.server.Close)
	return fx
}

func (f *marketRelayFixture) URL() string { return f.server.URL }

func (f *marketRelayFixture) SetCloseAfter(n int) {
	f.mu.Lock()
	f.closeAfter = n
	f.mu.Unlock()
}

func (f *marketRelayFixture) Cursors() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.cursors...)
}

func (f *marketRelayFixture) Publish(event marketEvent) {
	f.mu.Lock()
	f.events = append(f.events, event)
	subs := make([]chan marketEvent, 0, len(f.subs))
	for sub := range f.subs {
		subs = append(subs, sub)
	}
	f.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub <- event:
		default:
		}
	}
}

func (f *marketRelayFixture) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/xrpc/com.atproto.sync.subscribeRepos") {
		http.NotFound(w, r)
		return
	}
	cursor := int64(-1)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			cursor = parsed
		}
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	sub := make(chan marketEvent, 128)
	f.mu.Lock()
	f.cursors = append(f.cursors, cursor)
	f.subs[sub] = struct{}{}
	backlog := append([]marketEvent(nil), f.events...)
	closeAfter := f.closeAfter
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.subs, sub)
		f.mu.Unlock()
	}()
	sent := 0
	send := func(event marketEvent) bool {
		if event.Seq <= cursor {
			return true
		}
		if err := f.writeEvent(conn, event); err != nil {
			return false
		}
		cursor = event.Seq
		sent++
		return !(closeAfter > 0 && sent >= closeAfter)
	}
	for _, event := range backlog {
		if !send(event) {
			return
		}
	}
	for {
		select {
		case event := <-sub:
			if !send(event) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (f *marketRelayFixture) writeEvent(conn *websocket.Conn, event marketEvent) error {
	switch f.format {
	case "cbor":
		header, err := cbor.Marshal(map[string]any{"op": int64(1), "t": "#commit"})
		if err != nil {
			return err
		}
		parsed, err := cid.Decode(event.CID)
		if err != nil {
			return err
		}
		tagged := cbor.Tag{Number: 42, Content: append([]byte{0}, parsed.Bytes()...)}
		body, err := cbor.Marshal(map[string]any{
			"repo":   event.DID,
			"seq":    event.Seq,
			"commit": tagged,
			"rev":    event.Rev,
			"since":  nil,
			"blocks": []byte{},
			"ops": []any{map[string]any{
				"action": "create",
				"path":   event.Collection + "/" + event.Rkey,
				"cid":    tagged,
				"prev":   nil,
			}},
			"blobs":  []any{},
			"tooBig": false,
			"time":   event.Time.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, append(header, body...))
	default:
		parsed, err := cid.Decode(event.CID)
		if err != nil {
			return err
		}
		frame := map[string]any{
			"$type":  "com.atproto.sync.subscribeRepos#commit",
			"seq":    event.Seq,
			"repo":   event.DID,
			"commit": map[string]any{"$link": parsed.String()},
			"rev":    event.Rev,
			"blocks": map[string]any{},
			"ops":    []any{map[string]any{"action": "create", "path": event.Collection + "/" + event.Rkey, "cid": map[string]any{"$link": parsed.String()}}},
			"blobs":  []any{},
			"tooBig": false,
			"time":   event.Time.UTC().Format(time.RFC3339Nano),
		}
		return conn.WriteJSON(frame)
	}
}

type marketJetstreamFixture struct {
	t      testing.TB
	server *httptest.Server

	mu      sync.Mutex
	events  []marketEvent
	subs    map[chan marketEvent]struct{}
	cursors []int64
}

func newMarketJetstreamFixture(t testing.TB) *marketJetstreamFixture {
	t.Helper()
	fx := &marketJetstreamFixture{t: t, subs: map[chan marketEvent]struct{}{}}
	fx.server = httptest.NewServer(http.HandlerFunc(fx.serve))
	t.Cleanup(fx.server.Close)
	return fx
}

func (f *marketJetstreamFixture) URL() string { return f.server.URL }

func (f *marketJetstreamFixture) Cursors() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.cursors...)
}

func (f *marketJetstreamFixture) Publish(event marketEvent) {
	f.mu.Lock()
	f.events = append(f.events, event)
	subs := make([]chan marketEvent, 0, len(f.subs))
	for sub := range f.subs {
		subs = append(subs, sub)
	}
	f.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub <- event:
		default:
		}
	}
}

func (f *marketJetstreamFixture) serve(w http.ResponseWriter, r *http.Request) {
	cursor := int64(-1)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			cursor = parsed
		}
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	sub := make(chan marketEvent, 128)
	f.mu.Lock()
	f.cursors = append(f.cursors, cursor)
	f.subs[sub] = struct{}{}
	backlog := append([]marketEvent(nil), f.events...)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.subs, sub)
		f.mu.Unlock()
	}()
	send := func(event marketEvent) bool {
		if event.Seq <= cursor {
			return true
		}
		frame := map[string]any{
			"did":     event.DID,
			"time_us": event.Seq,
			"kind":    "commit",
			"commit": map[string]any{
				"rev":        event.Rev,
				"operation":  "create",
				"collection": event.Collection,
				"rkey":       event.Rkey,
				"record":     event.Record,
				"cid":        event.CID,
			},
		}
		if err := conn.WriteJSON(frame); err != nil {
			return false
		}
		cursor = event.Seq
		return true
	}
	for _, event := range backlog {
		if !send(event) {
			return
		}
	}
	for {
		select {
		case event := <-sub:
			if !send(event) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

type marketInbox struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	applies int
}

func newMarketInbox() *marketInbox {
	return &marketInbox{objects: map[string]map[string]any{}}
}

func (i *marketInbox) Deliver(uri, cid string, value map[string]any) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.applies++
	key := uri + "#" + cid
	if _, ok := i.objects[key]; ok {
		return false
	}
	i.objects[key] = value
	return true
}

func (i *marketInbox) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.objects)
}

func (i *marketInbox) Applies() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.applies
}

type marketAccountFixture struct {
	Mode        string
	PDS         *marketPDSFixture
	Client      pdsclient.PDS
	DID         syntax.DID
	Session     pdsclient.Session
	Association v1alpha1.AssociationMode
}

func newMarketAccountFixture(t testing.TB, mode string) *marketAccountFixture {
	t.Helper()
	remoteOpts := marketPDSOptions{
		URL:      os.Getenv("SOCIALWEB_IT_MARKET_PDS_URL"),
		DID:      os.Getenv("SOCIALWEB_IT_MARKET_PDS_DID"),
		Handle:   os.Getenv("SOCIALWEB_IT_MARKET_PDS_HANDLE"),
		Password: os.Getenv("SOCIALWEB_IT_MARKET_PDS_PASSWORD"),
	}
	pdsfx := newMarketPDSFixture(t, remoteOpts)
	session := pdsfx.createSession(t)
	acct := &marketAccountFixture{Mode: mode, PDS: pdsfx, Session: session, DID: session.DID}
	switch mode {
	case "managed-associated":
		acct.Association = v1alpha1.AssociationRequireApproval
		acct.Client = pdsfx.clientFor(session.AccessJWT)
	case "managed-unassociated":
		acct.Association = v1alpha1.AssociationSkip
		acct.Client = pdsfx.clientFor(session.AccessJWT)
	case "existing-pds-account":
		remote := newMarketRemoteFixture(t, pdsfx, session, true)
		acct.Client = remote.resolve(t).PDS
	case "existing-oauth-browser":
		oauth := newMarketOAuthBrowserFixture(t, pdsfx, false)
		acct.Client = pdsfx.oauthClient(t, oauth.Session)
	case "existing-oauth-delegated":
		oauth := newMarketOAuthBrowserFixture(t, pdsfx, true)
		acct.Client = pdsfx.oauthClient(t, oauth.Session)
	case "existing-oauth-qr":
		qr := newMarketQRFixture(t)
		oauth := newMarketOAuthQrFixture(t, pdsfx, qr)
		acct.Client = pdsfx.oauthClient(t, oauth.Session)
	default:
		t.Fatalf("unknown market account mode %q", mode)
	}
	return acct
}

func (f *marketPDSFixture) oauthClient(t testing.TB, session *oauthclient.Session) pdsclient.PDS {
	t.Helper()
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL: f.URL,
		Tokens:  session,
		DPoP:    session,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("fixture oauth pds client: %v", err)
	}
	return client
}

type marketRecordWrite struct {
	URI        string
	CID        string
	Rev        string
	Collection string
	Rkey       string
	Value      map[string]any
}

func (a *marketAccountFixture) writeRecord(t testing.TB, collection, rkey string) marketRecordWrite {
	t.Helper()
	value := map[string]any{
		"$type":     collection,
		"id":        rkey,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("fixture record: %v", err)
	}
	path := pdsclient.RepoPath{
		Repo:       a.DID,
		Collection: syntax.NSID(collection),
		Rkey:       syntax.RecordKey(rkey),
	}
	result, err := a.Client.CreateRecord(context.Background(), path, raw, pdsclient.WriteOptions{})
	if err != nil {
		t.Fatalf("fixture create record %s/%s: %v", collection, rkey, err)
	}
	rev := ""
	if result.Commit != nil {
		rev = result.Commit.Rev.String()
	}
	return marketRecordWrite{URI: result.URI.String(), CID: result.CID.String(), Rev: rev, Collection: collection, Rkey: rkey, Value: value}
}

func (a *marketAccountFixture) getRecord(t testing.TB, collection, rkey string) (map[string]any, string) {
	t.Helper()
	path := pdsclient.RepoPath{
		Repo:       a.DID,
		Collection: syntax.NSID(collection),
		Rkey:       syntax.RecordKey(rkey),
	}
	record, err := a.Client.GetRecord(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("fixture get record %s/%s: %v", collection, rkey, err)
	}
	value := map[string]any{}
	if err := json.Unmarshal(record.Value, &value); err != nil {
		t.Fatalf("fixture decode record: %v", err)
	}
	return value, record.CID.String()
}

type marketStreamWatcher struct {
	dyn    *fakedynamic.FakeDynamicClient
	name   string
	events chan eventsvc.Event
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *marketStreamWatcher) waitEvent(t testing.TB, timeout time.Duration) eventsvc.Event {
	t.Helper()
	select {
	case event := <-w.events:
		return event
	case <-time.After(timeout):
		t.Fatalf("event stream %s delivered no event within %s", w.name, timeout)
		return eventsvc.Event{}
	}
}

func (w *marketStreamWatcher) waitPhase(t testing.TB, phase eventsv1alpha1.EventStreamPhase, timeout time.Duration) eventsv1alpha1.EventStreamStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		object, err := w.dyn.Resource(eventsvc.EventStreamGVR).Get(context.Background(), w.name, metav1.GetOptions{})
		if err == nil {
			var eventStream eventsv1alpha1.EventStream
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &eventStream); err == nil && eventStream.Status.Phase == phase {
				return eventStream.Status
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("event stream %s never reached phase %s", w.name, phase)
	return eventsv1alpha1.EventStreamStatus{}
}

func (w *marketStreamWatcher) Close() {
	w.cancel()
	<-w.done
}

func startMarketEventStream(t testing.TB, kind, baseURL string, collections []string, did string) *marketStreamWatcher {
	t.Helper()
	name := "market-" + marketRandomHex(t, 4)
	source := map[string]any{"kind": kind}
	collectionsValue := make([]any, 0, len(collections))
	for _, collection := range collections {
		collectionsValue = append(collectionsValue, collection)
	}
	source["collections"] = collectionsValue
	if did != "" {
		source["dids"] = []any{did}
	}
	switch kind {
	case "relay":
		source["relayUrls"] = []any{baseURL}
	case "jetstream":
		source["jetstreamUrls"] = []any{baseURL}
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": eventsv1alpha1.GroupVersion.String(),
		"kind":       "EventStream",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"source": source,
			"sink":   map[string]any{"kind": "handler", "name": marketFixtureHandler},
		},
	}}
	listKinds := map[schema.GroupVersionResource]string{
		eventsvc.EventStreamGVR: "EventStreamList",
		eventsvc.RecordWatchGVR: "AtprotoRecordWatchList",
		eventsvc.RemotePDSGVR:   "RemotePDSList",
	}
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, object)
	handlers := eventsvc.NewHandlerRegistry()
	watcher := &marketStreamWatcher{dyn: dyn, name: name, events: make(chan eventsvc.Event, 128), done: make(chan struct{})}
	if err := handlers.Register(marketFixtureHandler, eventsvc.HandlerFunc(func(_ context.Context, event eventsvc.Event) error {
		select {
		case watcher.events <- event:
		default:
		}
		return nil
	})); err != nil {
		t.Fatalf("register fixture handler: %v", err)
	}
	controller, err := eventsvc.New(eventsvc.Options{
		Workspace:        "root",
		Dynamic:          func(string) (dynamic.Interface, error) { return dyn, nil },
		Resolver:         &pdsclient.Resolver{},
		Handlers:         handlers,
		RetryAfter:       20 * time.Millisecond,
		StatusInterval:   50 * time.Millisecond,
		ReconnectDelay:   20 * time.Millisecond,
		MaxReconnectWait: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("fixture event stream controller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	watcher.cancel = cancel
	go func() {
		defer close(watcher.done)
		_ = controller.Run(ctx)
	}()
	t.Cleanup(watcher.Close)
	return watcher
}

func marketEventFor(write marketRecordWrite, seq int64) marketEvent {
	return marketEvent{
		Seq:        seq,
		DID:        didOfURI(write.URI),
		Collection: write.Collection,
		Rkey:       write.Rkey,
		CID:        write.CID,
		Rev:        write.Rev,
		Time:       time.Now().UTC(),
		Record:     write.Value,
	}
}

func didOfURI(uri string) string {
	trimmed := strings.TrimPrefix(uri, "at://")
	if index := strings.IndexByte(trimmed, '/'); index >= 0 {
		return trimmed[:index]
	}
	return trimmed
}

func pathOfEvent(event eventsvc.Event) (string, string, bool) {
	if len(event.Ops) == 0 {
		return "", "", false
	}
	parts := strings.SplitN(event.Ops[0].Path, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (a *marketAccountFixture) deliverEvent(t testing.TB, inbox *marketInbox, event eventsvc.Event) bool {
	t.Helper()
	collection, rkey, ok := pathOfEvent(event)
	if !ok {
		t.Fatalf("fixture event has no record path: %+v", event)
	}
	value, cid := a.getRecord(t, collection, rkey)
	return inbox.Deliver("at://"+event.Repo.String()+"/"+collection+"/"+rkey, cid, value)
}

func (a *marketAccountFixture) deliverWrite(t testing.TB, inbox *marketInbox, write marketRecordWrite) bool {
	t.Helper()
	value, cid := a.getRecord(t, write.Collection, write.Rkey)
	return inbox.Deliver(write.URI, cid, value)
}

func (a *marketAccountFixture) deliverSubscribeRepos(t testing.TB, inbox *marketInbox, writeFn func() marketRecordWrite) {
	t.Helper()
	stream, err := a.Client.SubscribeRepos(context.Background(), nil)
	if err != nil {
		t.Fatalf("fixture subscribeRepos: %v", err)
	}
	defer stream.Close()
	write := writeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("fixture subscribeRepos next: %v", err)
	}
	if event.Seq < 0 {
		t.Fatalf("fixture subscribeRepos returned seq %d", event.Seq)
	}
	if len(event.Ops) == 0 {
		t.Fatalf("fixture subscribeRepos returned no ops")
	}
	path := event.Ops[0].Path
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] != write.Collection || parts[1] != write.Rkey {
		t.Fatalf("fixture subscribeRepos path %q, want %s/%s", path, write.Collection, write.Rkey)
	}
	a.deliverWrite(t, inbox, write)
}

func (a *marketAccountFixture) deliverAssociationClaim(t testing.TB, subject string) pdsclient.RepoPath {
	t.Helper()
	rkey := "fixture-claim-" + strings.ReplaceAll(subject, ":", "-")
	path := pdsclient.RepoPath{
		Repo:       a.DID,
		Collection: syntax.NSID(appctl.BadgeBlueKeysNSID),
		Rkey:       syntax.RecordKey(rkey),
	}
	record, err := json.Marshal(map[string]any{
		"$type":     appctl.BadgeBlueKeysNSID,
		"keyId":     subject,
		"challenge": a.DID.String(),
		"service":   "fixture",
		"nonce":     marketRandomHex(t, 8),
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("fixture claim record: %v", err)
	}
	if _, err := a.Client.PutRecord(context.Background(), path, record, pdsclient.WriteOptions{}); err != nil {
		t.Fatalf("fixture put claim: %v", err)
	}
	return path
}

func (a *marketAccountFixture) deleteRecord(t testing.TB, path pdsclient.RepoPath) {
	t.Helper()
	if err := a.Client.DeleteRecord(context.Background(), path, pdsclient.WriteOptions{}); err != nil {
		t.Fatalf("fixture delete record: %v", err)
	}
}

func (a *marketAccountFixture) recordExists(t testing.TB, path pdsclient.RepoPath) bool {
	t.Helper()
	_, err := a.Client.GetRecord(context.Background(), path, nil)
	if err == nil {
		return true
	}
	var xrpc *pdsclient.XRPCError
	if errors.As(err, &xrpc) {
		return xrpc.Code != "RecordNotFound" && xrpc.Code != "RepoNotFound" && xrpc.StatusCode != http.StatusNotFound
	}
	return false
}

func marketFixtureURL(env, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return value
	}
	return fallback
}

func marketGenerateP256() (string, error) {
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return "", err
	}
	return key.Multibase(), nil
}
