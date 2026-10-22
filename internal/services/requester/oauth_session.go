package requester

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	oauthReplyLimit = 1 << 20

	oauthAccessSkew = 30 * time.Second

	oauthDPoPLifetime = 30 * time.Second

	oauthServiceAuthNSID = "com.atproto.server.getServiceAuth"
)

type oauthSessionData struct {
	AccountDID string `json:"account_did"`

	SessionID string `json:"session_id"`

	HostURL string `json:"host_url"`

	AuthServerURL string `json:"authserver_url"`

	AuthServerTokenEndpoint string `json:"authserver_token_endpoint"`

	AuthServerRevocationEndpoint string `json:"authserver_revocation_endpoint,omitempty"`

	Scopes []string `json:"scopes"`

	AccessToken string `json:"access_token"`

	RefreshToken string `json:"refresh_token"`

	DPoPAuthServerNonce string `json:"dpop_authserver_nonce"`

	DPoPHostNonce string `json:"dpop_host_nonce"`

	DPoPPrivateKeyMultibase string `json:"dpop_privatekey_multibase"`
}

type oauthStoreFile struct {
	Sessions map[string]oauthSessionData `json:"sessions"`

	Requests map[string]json.RawMessage `json:"auth_requests,omitempty"`
}

type OAuthSession struct {
	mu sync.Mutex

	data oauthSessionData

	key atcrypto.PrivateKeyExportable

	client *http.Client

	clientID string

	path string

	sessions map[string]oauthSessionData

	requests map[string]json.RawMessage

	did string

	hostURL string
}

func oauthSessionKey(did, sessionID string) string {
	return did + "/" + sessionID
}

func (o *OAuthSession) DID() string {
	if o == nil {
		return ""
	}
	return o.did
}

func (o *OAuthSession) HostURL() string {
	if o == nil {
		return ""
	}
	return o.hostURL
}

func (o *OAuthSession) AccessToken(ctx context.Context) (string, error) {
	if o == nil {
		return "", errors.New("requester: no oauth session")
	}
	o.mu.Lock()
	token := o.data.AccessToken
	o.mu.Unlock()
	if oauthAccessTokenLive(token) {
		return token, nil
	}
	if err := o.refresh(ctx); err != nil {
		return "", fmt.Errorf("requester: refreshing the oauth session: %w", err)
	}
	o.mu.Lock()
	token = o.data.AccessToken
	o.mu.Unlock()
	if token == "" {
		return "", errors.New("requester: the oauth session has no access token")
	}
	return token, nil
}

func (o *OAuthSession) HTTPClient(base *http.Client) *http.Client {
	var client http.Client
	if base != nil {
		client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &oauthHTTPTransport{session: o, base: transport}
	return &client
}

type oauthHTTPTransport struct {
	session *OAuthSession

	base http.RoundTripper
}

func (t *oauthHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.session == nil {
		return nil, errors.New("requester: no oauth session")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return t.session.doAuthorized(req.Context(), &http.Client{Transport: base}, req)
}

func (o *OAuthSession) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	if o == nil {
		return "", errors.New("requester: no oauth session")
	}
	if audience == "" {
		return "", errors.New("requester: no service auth audience")
	}
	query := url.Values{"aud": {audience}}
	if lxm != "" {
		query.Set("lxm", lxm)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.hostURL+"/xrpc/"+oauthServiceAuthNSID+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.doAuthorized(ctx, o.client, req)
	if err != nil {
		return "", fmt.Errorf("requester: service auth request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oauthReplyLimit))
	if err != nil {
		return "", fmt.Errorf("requester: reading the service auth reply: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("requester: service auth returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("requester: decoding the service auth reply: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("requester: service auth returned no token")
	}
	return out.Token, nil
}

type oauthDPoPHeader struct {
	Typ string        `json:"typ"`
	Alg string        `json:"alg"`
	JWK *atcrypto.JWK `json:"jwk"`
}

type oauthDPoPClaims struct {
	JTI string `json:"jti"`

	HTM string `json:"htm"`

	HTU string `json:"htu"`

	IAT int64 `json:"iat"`

	EXP int64 `json:"exp"`

	ATH string `json:"ath,omitempty"`

	Nonce string `json:"nonce,omitempty"`
}

func oauthDPoP(key atcrypto.PrivateKeyExportable, method, target, accessToken, nonce string) (string, error) {
	public, err := key.PublicKey()
	if err != nil {
		return "", err
	}
	jwk, err := public.JWK()
	if err != nil {
		return "", err
	}
	htu, ok := oauthHTU(target)
	if !ok {
		return "", fmt.Errorf("requester: the oauth request url %q is not absolute", target)
	}
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		return "", err
	}
	now := time.Now()
	claims := oauthDPoPClaims{
		JTI:   base64.RawURLEncoding.EncodeToString(nonceRaw),
		HTM:   strings.ToUpper(method),
		HTU:   htu,
		IAT:   now.Unix(),
		EXP:   now.Add(oauthDPoPLifetime).Unix(),
		Nonce: nonce,
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims.ATH = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	headerJSON, err := json.Marshal(oauthDPoPHeader{Typ: "dpop+jwt", Alg: "ES256", JWK: jwk})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	signature, err := key.HashAndSign([]byte(signingInput))
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func oauthHTU(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path, true
}

func (o *OAuthSession) doAuthorized(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		o.mu.Lock()
		accessToken := o.data.AccessToken
		nonce := o.data.DPoPHostNonce
		key := o.key
		o.mu.Unlock()
		proof, err := oauthDPoP(key, req.Method, req.URL.String(), accessToken, nonce)
		if err != nil {
			return nil, err
		}
		current := req
		if attempt > 0 {
			current, err = oauthCloneRequest(req)
			if err != nil {
				return nil, err
			}
		}
		current.Header.Set("Authorization", "DPoP "+accessToken)
		current.Header.Set("DPoP", proof)
		resp, err = client.Do(current)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		header := resp.Header.Get("WWW-Authenticate")
		nextNonce := resp.Header.Get("DPoP-Nonce")
		drainAndClose(resp.Body)
		switch {
		case strings.Contains(header, "use_dpop_nonce") && nextNonce != "":
			o.mu.Lock()
			o.data.DPoPHostNonce = nextNonce
			_ = o.persistLocked()
			o.mu.Unlock()
			continue
		case strings.Contains(header, "invalid_token"):
			if err := o.refresh(ctx); err != nil {
				return nil, fmt.Errorf("requester: refreshing the oauth session: %w", err)
			}
			continue
		}
		return nil, fmt.Errorf("requester: %s %s answered HTTP 401", req.Method, req.URL.Path)
	}
	return nil, errors.New("requester: the oauth session kept asking for a new DPoP nonce")
}

func oauthCloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	return clone, nil
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, oauthReplyLimit))
	_ = body.Close()
}

func (o *OAuthSession) refresh(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.data.RefreshToken == "" {
		return errors.New("the oauth session has no refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {o.data.RefreshToken},
		"client_id":     {o.clientID},
	}
	client := o.client
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := oauthDPoP(o.key, http.MethodPost, o.data.AuthServerTokenEndpoint, "", o.data.DPoPAuthServerNonce)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.data.AuthServerTokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err = client.Do(req)
		if err != nil {
			return err
		}
		nextNonce := resp.Header.Get("DPoP-Nonce")
		if resp.StatusCode == http.StatusBadRequest && nextNonce != "" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, oauthReplyLimit))
			resp.Body.Close()
			if strings.Contains(string(raw), "use_dpop_nonce") {
				o.data.DPoPAuthServerNonce = nextNonce
				continue
			}
			return fmt.Errorf("the token refresh answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		break
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oauthReplyLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the token refresh answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decoding the token refresh: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return errors.New("the token refresh returned no access or refresh token")
	}
	o.data.AccessToken = out.AccessToken
	o.data.RefreshToken = out.RefreshToken
	if out.Scope != "" {
		o.data.Scopes = strings.Fields(out.Scope)
	}
	if nextNonce := resp.Header.Get("DPoP-Nonce"); nextNonce != "" {
		o.data.DPoPAuthServerNonce = nextNonce
	}
	return o.persistLocked()
}

func (o *OAuthSession) persistLocked() error {
	if o.path == "" {
		return errors.New("requester: the oauth session has no store path")
	}
	if o.sessions == nil {
		o.sessions = map[string]oauthSessionData{}
	}
	o.sessions[oauthSessionKey(o.data.AccountDID, o.data.SessionID)] = o.data
	file := oauthStoreFile{Sessions: o.sessions, Requests: o.requests}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(o.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".oauth-session-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, o.path)
}

type tsOAuthSession struct {
	AccessJwt string `json:"accessJwt"`

	RefreshJwt string `json:"refreshJwt"`

	UserDid string `json:"userDid"`

	Handle string `json:"handle"`

	PDS string `json:"pds"`

	DPoPPrivateJwk struct {
		Kty string `json:"kty"`

		Crv string `json:"crv"`

		X string `json:"x"`

		Y string `json:"y"`

		D string `json:"d"`
	} `json:"dpopPrivateJwk"`
}

func openOAuthSession(ctx context.Context, cfg Config, dir identity.Directory) (*OAuthSession, error) {
	if cfg.OAuthSessionPath == "" {
		return nil, errors.New("requester: oauth identity mode has no token store path")
	}
	raw, err := os.ReadFile(cfg.OAuthSessionPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("requester: no oauth token store at %s", cfg.OAuthSessionPath)
		}
		return nil, fmt.Errorf("requester: the oauth token store %s: %w", cfg.OAuthSessionPath, err)
	}
	sessions, requests, tsDetected, err := oauthReadStore(raw, cfg.OAuthSessionPath)
	if err != nil {
		return nil, err
	}
	if tsDetected {
		var ts tsOAuthSession
		if err := json.Unmarshal(raw, &ts); err != nil {
			return nil, fmt.Errorf("requester: the oauth session %s: %w", cfg.OAuthSessionPath, err)
		}
		session, err := oauthSessionFromTS(ctx, cfg, &ts)
		if err != nil {
			return nil, err
		}
		sessions[oauthSessionKey(session.AccountDID, session.SessionID)] = *session
	}
	did, err := oauthRequesterDID(ctx, cfg, dir, sessions)
	if err != nil {
		return nil, err
	}
	session, ok := oauthSelectSession(sessions, did)
	if !ok {
		return nil, fmt.Errorf("requester: no oauth session stored for %s at %s", did, cfg.OAuthSessionPath)
	}
	if session.HostURL == "" {
		return nil, errors.New("requester: the oauth session names no PDS")
	}
	if cfg.RepoDID != "" && cfg.RepoDID != did {
		return nil, fmt.Errorf("requester: the oauth session is for %s, not %s", did, cfg.RepoDID)
	}
	if cfg.PDSURL != "" && strings.TrimRight(cfg.PDSURL, "/") != strings.TrimRight(session.HostURL, "/") {
		return nil, fmt.Errorf("requester: the oauth session names PDS %s, not %s", session.HostURL, cfg.PDSURL)
	}
	key, err := atcrypto.ParsePrivateMultibase(session.DPoPPrivateKeyMultibase)
	if err != nil {
		return nil, fmt.Errorf("requester: the oauth session DPoP key: %w", err)
	}
	if _, ok := key.(*atcrypto.PrivateKeyP256); !ok {
		return nil, fmt.Errorf("requester: the oauth session DPoP key type %T is not P-256", key)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	oauth := &OAuthSession{
		data:     session,
		key:      key,
		client:   client,
		clientID: cfg.OAuthClientID,
		path:     cfg.OAuthSessionPath,
		sessions: sessions,
		requests: requests,
		did:      did,
		hostURL:  strings.TrimRight(session.HostURL, "/"),
	}
	if !oauthAccessTokenLive(session.AccessToken) {
		if err := oauth.refresh(ctx); err != nil {
			return nil, fmt.Errorf("requester: refreshing the oauth session for %s: %w", did, err)
		}
	}
	return oauth, nil
}

func oauthReadStore(raw []byte, path string) (map[string]oauthSessionData, map[string]json.RawMessage, bool, error) {
	sessions := map[string]oauthSessionData{}
	requests := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return sessions, requests, false, fmt.Errorf("requester: no oauth session in %s", path)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, false, fmt.Errorf("requester: the oauth token store %s: %w", path, err)
	}
	if _, ok := probe["sessions"]; ok {
		var file oauthStoreFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, nil, false, fmt.Errorf("requester: the oauth token store %s: %w", path, err)
		}
		for key, session := range file.Sessions {
			sessions[key] = session
		}
		for key, request := range file.Requests {
			requests[key] = request
		}
		return sessions, requests, false, nil
	}
	if _, ok := probe["accessJwt"]; ok || probe["refreshJwt"] != nil {
		return sessions, requests, true, nil
	}
	var session oauthSessionData
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, nil, false, fmt.Errorf("requester: the oauth token store %s: %w", path, err)
	}
	if session.AccountDID == "" {
		return nil, nil, false, fmt.Errorf("requester: no oauth session in %s", path)
	}
	sessions[oauthSessionKey(session.AccountDID, session.SessionID)] = session
	return sessions, requests, false, nil
}

func oauthRequesterDID(ctx context.Context, cfg Config, dir identity.Directory, sessions map[string]oauthSessionData) (string, error) {
	if cfg.DID != "" {
		if _, err := syntax.ParseDID(cfg.DID); err != nil {
			return "", fmt.Errorf("requester: oauth identity DID: %w", err)
		}
		return cfg.DID, nil
	}
	if cfg.Handle != "" {
		if dir == nil {
			return "", errors.New("requester: no identity directory resolves the oauth handle")
		}
		atid, err := syntax.ParseAtIdentifier(cfg.Handle)
		if err != nil {
			return "", fmt.Errorf("requester: oauth handle %q: %w", cfg.Handle, err)
		}
		ident, err := dir.Lookup(ctx, atid)
		if err != nil {
			return "", fmt.Errorf("requester: resolving the oauth handle %q: %w", cfg.Handle, err)
		}
		return ident.DID.String(), nil
	}
	dids := map[string]bool{}
	for key := range sessions {
		did, _, found := strings.Cut(key, "/")
		if found {
			dids[did] = true
		}
	}
	if len(dids) != 1 {
		return "", errors.New("requester: oauth identity mode names no DID or handle")
	}
	for did := range dids {
		return did, nil
	}
	return "", errors.New("requester: oauth identity mode names no DID or handle")
}

func oauthSelectSession(sessions map[string]oauthSessionData, did string) (oauthSessionData, bool) {
	prefix := did + "/"
	keys := []string{}
	for key := range sessions {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return oauthSessionData{}, false
	}
	return sessions[keys[0]], true
}

func oauthAccessTokenLive(token string) bool {
	if token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(oauthAccessSkew).Before(time.Unix(claims.ExpiresAt, 0))
}

func oauthSessionFromTS(ctx context.Context, cfg Config, ts *tsOAuthSession) (*oauthSessionData, error) {
	did, err := syntax.ParseDID(ts.UserDid)
	if err != nil {
		return nil, fmt.Errorf("requester: the oauth session userDid: %w", err)
	}
	if ts.PDS == "" {
		return nil, errors.New("requester: the oauth session names no PDS")
	}
	pds, err := url.Parse(ts.PDS)
	if err != nil || pds.Host == "" || (pds.Scheme != "http" && pds.Scheme != "https") {
		return nil, fmt.Errorf("requester: the oauth session PDS %q is not an absolute http or https URL", ts.PDS)
	}
	if ts.DPoPPrivateJwk.D == "" {
		return nil, errors.New("requester: the oauth session has no DPoP private key")
	}
	if ts.DPoPPrivateJwk.Kty != "" && ts.DPoPPrivateJwk.Kty != "EC" {
		return nil, fmt.Errorf("requester: the oauth session DPoP key type %q is not EC", ts.DPoPPrivateJwk.Kty)
	}
	keyBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(ts.DPoPPrivateJwk.D, "="))
	if err != nil {
		return nil, fmt.Errorf("requester: the oauth session DPoP key: %w", err)
	}
	key, err := atcrypto.ParsePrivateBytesP256(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("requester: the oauth session DPoP key: %w", err)
	}
	authServer, tokenEndpoint, revokeEndpoint, err := oauthAuthServerFor(ctx, cfg, pds)
	if err != nil {
		return nil, err
	}
	return &oauthSessionData{
		AccountDID:                   did.String(),
		SessionID:                    oauthSessionID(did.String(), strings.TrimRight(ts.PDS, "/")),
		HostURL:                      strings.TrimRight(ts.PDS, "/"),
		AuthServerURL:                authServer,
		AuthServerTokenEndpoint:      tokenEndpoint,
		AuthServerRevocationEndpoint: revokeEndpoint,
		Scopes:                       append([]string(nil), cfg.OAuthScopes...),
		AccessToken:                  ts.AccessJwt,
		RefreshToken:                 ts.RefreshJwt,
		DPoPPrivateKeyMultibase:      key.Multibase(),
	}, nil
}

func oauthSessionID(did, host string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + host))
	return hex.EncodeToString(sum[:16])
}

func oauthAuthServerFor(ctx context.Context, cfg Config, pds *url.URL) (string, string, string, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	base := strings.TrimRight(pds.String(), "/")
	var protected struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := oauthGetJSON(ctx, client, base+"/.well-known/oauth-protected-resource", &protected); err != nil {
		return "", "", "", fmt.Errorf("requester: the PDS oauth metadata: %w", err)
	}
	if len(protected.AuthorizationServers) == 0 || protected.AuthorizationServers[0] == "" {
		return "", "", "", errors.New("requester: the PDS oauth metadata names no authorization server")
	}
	authURL, err := url.Parse(protected.AuthorizationServers[0])
	if err != nil || authURL.Host == "" || (authURL.Scheme != "http" && authURL.Scheme != "https") {
		return "", "", "", fmt.Errorf("requester: the authorization server %q is not an absolute http or https URL", protected.AuthorizationServers[0])
	}
	authServer := strings.TrimRight(authURL.String(), "/")
	var metadata struct {
		TokenEndpoint      string `json:"token_endpoint"`
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := oauthGetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", "", "", fmt.Errorf("requester: the auth server metadata: %w", err)
	}
	if metadata.TokenEndpoint == "" {
		return "", "", "", errors.New("requester: the auth server metadata names no token endpoint")
	}
	tokenURL, err := oauthEndpointURL(authServer, metadata.TokenEndpoint)
	if err != nil {
		return "", "", "", err
	}
	revokeURL := ""
	if metadata.RevocationEndpoint != "" {
		revokeURL, err = oauthEndpointURL(authServer, metadata.RevocationEndpoint)
		if err != nil {
			return "", "", "", err
		}
	}
	return authServer, tokenURL, revokeURL, nil
}

func oauthEndpointURL(base, raw string) (string, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("requester: the oauth endpoint %q: %w", raw, err)
	}
	baseURL, err := url.Parse(base + "/")
	if err != nil {
		return "", err
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.Host == "" || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return "", fmt.Errorf("requester: the oauth endpoint %q is not an absolute http or https URL", raw)
	}
	return resolved.String(), nil
}

func oauthGetJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oauthReplyLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", target, err)
	}
	return nil
}
