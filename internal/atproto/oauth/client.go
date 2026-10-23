package oauth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/bluesky-social/indigo/util/ssrf"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	ClientAuthNone          = "none"
	ClientAuthPrivateKeyJWT = "private_key_jwt"

	ClientAssertionTypeJWTBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	MaxClientMetadataBytes = 1 << 20

	DefaultClientMetadataTTL = 5 * time.Minute
)

type JWKS struct {
	Keys []atcrypto.JWK `json:"keys"`
}

type ClientMetadata struct {
	ClientID                    string   `json:"client_id"`
	ApplicationType             *string  `json:"application_type,omitempty"`
	GrantTypes                  []string `json:"grant_types"`
	Scope                       string   `json:"scope"`
	ResponseTypes               []string `json:"response_types"`
	RedirectURIs                []string `json:"redirect_uris"`
	TokenEndpointAuthMethod     string   `json:"token_endpoint_auth_method"`
	TokenEndpointAuthSigningAlg *string  `json:"token_endpoint_auth_signing_alg,omitempty"`
	DPoPBoundAccessTokens       bool     `json:"dpop_bound_access_tokens"`
	JWKS                        *JWKS    `json:"jwks,omitempty"`
	JWKSURI                     *string  `json:"jwks_uri,omitempty"`
	ClientName                  *string  `json:"client_name,omitempty"`
	ClientURI                   *string  `json:"client_uri,omitempty"`
	LogoURI                     *string  `json:"logo_uri,omitempty"`
	TosURI                      *string  `json:"tos_uri,omitempty"`
	PolicyURI                   *string  `json:"policy_uri,omitempty"`
}

func (m *ClientMetadata) Validate(clientID string) error {
	if m.ClientID == "" || m.ClientID != clientID {
		return fmt.Errorf("client_id mismatch")
	}
	if m.ApplicationType != nil && *m.ApplicationType != "web" && *m.ApplicationType != "native" {
		return fmt.Errorf("application_type must be web or native")
	}
	if !containsString(m.GrantTypes, "authorization_code") {
		return fmt.Errorf("grant_types must include authorization_code")
	}
	if !containsString(strings.Fields(m.Scope), ScopeAtproto) {
		return fmt.Errorf("scope must include atproto")
	}
	if !containsString(m.ResponseTypes, "code") {
		return fmt.Errorf("response_types must include code")
	}
	if len(m.RedirectURIs) == 0 {
		return fmt.Errorf("redirect_uris must not be empty")
	}
	web := m.ApplicationType == nil || *m.ApplicationType == "web"
	for _, raw := range m.RedirectURIs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid redirect_uri")
		}
		if web && parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" {
			return fmt.Errorf("web redirect_uris must use https")
		}
	}
	if m.TokenEndpointAuthMethod != ClientAuthNone && m.TokenEndpointAuthMethod != ClientAuthPrivateKeyJWT {
		return fmt.Errorf("unsupported token_endpoint_auth_method")
	}
	if m.TokenEndpointAuthSigningAlg != nil && *m.TokenEndpointAuthSigningAlg == "none" {
		return fmt.Errorf("token_endpoint_auth_signing_alg must not be none")
	}
	if !m.DPoPBoundAccessTokens {
		return fmt.Errorf("dpop_bound_access_tokens must be true")
	}
	if m.JWKSURI != nil && *m.JWKSURI == "" {
		return fmt.Errorf("jwks_uri must not be empty")
	}
	if m.TokenEndpointAuthMethod == ClientAuthPrivateKeyJWT {
		hasKeys := m.JWKS != nil && len(m.JWKS.Keys) > 0
		hasURI := m.JWKSURI != nil && *m.JWKSURI != ""
		if !hasKeys && !hasURI {
			return fmt.Errorf("confidential clients need jwks or jwks_uri")
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type ClientMetadataResolver struct {
	Nonces         NonceStore
	Client         *http.Client
	CacheTTL       time.Duration
	AllowLocalhost bool

	cache *ttlcache.Cache[string, *ClientMetadata]
}

func NewClientMetadataResolver(nonces NonceStore) *ClientMetadataResolver {
	return &ClientMetadataResolver{
		Nonces:         nonces,
		Client:         DefaultHTTPClient(),
		CacheTTL:       DefaultClientMetadataTTL,
		AllowLocalhost: true,
		cache:          ttlcache.New[string, *ClientMetadata](ttlcache.Options{TTL: DefaultClientMetadataTTL}),
	}
}

func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: ssrf.PublicOnlyTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func hardenHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		return DefaultHTTPClient()
	}
	cloned := *base
	if cloned.Transport == nil {
		cloned.Transport = ssrf.PublicOnlyTransport()
	}
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &cloned
}

func loopbackHTTPClient(base *http.Client) *http.Client {
	timeout := 15 * time.Second
	var tlsConfig *tls.Config
	if base != nil {
		if base.Timeout > 0 {
			timeout = base.Timeout
		}
		if transport, ok := base.Transport.(*http.Transport); ok && transport != nil && transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsConfig,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (r *ClientMetadataResolver) httpClientFor(target *url.URL) *http.Client {
	if isLoopbackHostname(target.Hostname()) {
		return loopbackHTTPClient(r.Client)
	}
	return hardenHTTPClient(r.Client)
}

func (r *ClientMetadataResolver) Resolve(ctx context.Context, clientID string) (*ClientMetadata, error) {
	if metadata, ok := r.cache.Get(clientID); ok {
		return metadata, nil
	}

	var metadata *ClientMetadata
	var err error
	if r.AllowLocalhost && isLocalhostClientID(clientID) {
		metadata, err = localhostClientMetadata(clientID)
	} else {
		metadata, err = r.fetchClientMetadata(ctx, clientID)
	}
	if err != nil {
		return nil, err
	}
	if err := metadata.Validate(clientID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}

	ttl := r.CacheTTL
	ttl = opt.Positive(ttl, DefaultClientMetadataTTL)
	r.cache.SetTTL(clientID, metadata, ttl)
	return metadata, nil
}

func isLocalhostClientID(clientID string) bool {
	parsed, err := url.Parse(clientID)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" &&
		strings.EqualFold(parsed.Hostname(), "localhost") &&
		parsed.Port() == "" &&
		(parsed.Path == "" || parsed.Path == "/") &&
		parsed.User == nil &&
		parsed.Fragment == ""
}

func localhostClientMetadata(clientID string) (*ClientMetadata, error) {
	parsed, err := url.Parse(clientID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	query := parsed.Query()
	redirectURIs := query["redirect_uri"]
	if len(redirectURIs) == 0 {
		redirectURIs = []string{"http://127.0.0.1/", "http://[::1]/"}
	}
	for _, redirectURI := range redirectURIs {
		if !isLoopbackRedirectURI(redirectURI) {
			return nil, fmt.Errorf("%w: localhost redirect_uri must use http on 127.0.0.1 or [::1]", ErrInvalidClientMetadata)
		}
	}
	scope := query.Get("scope")
	scope = cmp.Or(scope, ScopeAtproto)
	native := "native"
	metadata := &ClientMetadata{
		ClientID:                clientID,
		ApplicationType:         &native,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		Scope:                   scope,
		ResponseTypes:           []string{"code"},
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: ClientAuthNone,
		DPoPBoundAccessTokens:   true,
	}
	return metadata, nil
}

func (r *ClientMetadataResolver) fetchClientMetadata(ctx context.Context, clientID string) (*ClientMetadata, error) {
	target, err := metadataDocumentURL(clientID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	req.Header.Set("Accept", "application/json")
	client := r.httpClientFor(target)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrInvalidClientMetadata, resp.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return nil, fmt.Errorf("%w: content type is not application/json", ErrInvalidClientMetadata)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxClientMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	if len(raw) > MaxClientMetadataBytes {
		return nil, fmt.Errorf("%w: document too large", ErrInvalidClientMetadata)
	}
	var metadata ClientMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	return &metadata, nil
}

func metadataDocumentURL(clientID string) (*url.URL, error) {
	parsed, err := url.Parse(clientID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientMetadata, err)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: client_id must be an https URL", ErrInvalidClientMetadata)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHostname(parsed.Hostname()) {
			return nil, fmt.Errorf("%w: client_id may use http only on a loopback host", ErrInvalidClientMetadata)
		}
	default:
		return nil, fmt.Errorf("%w: client_id must be an https URL", ErrInvalidClientMetadata)
	}
	return parsed, nil
}

func RedirectURIAllowed(metadata *ClientMetadata, clientID, redirectURI string) bool {
	if isLocalhostClientID(clientID) && !isLoopbackRedirectURI(redirectURI) {
		return false
	}
	for _, registered := range metadata.RedirectURIs {
		if registered == redirectURI {
			return true
		}
	}
	if !isLocalhostClientID(clientID) {
		return false
	}
	want, err := url.Parse(redirectURI)
	if err != nil || want.Fragment != "" {
		return false
	}
	for _, registered := range metadata.RedirectURIs {
		have, err := url.Parse(registered)
		if err != nil || have.Fragment != "" {
			continue
		}
		if have.Scheme == want.Scheme &&
			strings.EqualFold(have.Hostname(), want.Hostname()) &&
			have.Path == want.Path {
			return true
		}
	}
	return false
}

func isLoopbackRedirectURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

type assertionHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type assertionClaims struct {
	Iss string          `json:"iss"`
	Sub string          `json:"sub"`
	Aud json.RawMessage `json:"aud"`
	Exp json.Number     `json:"exp"`
	Iat json.Number     `json:"iat"`
	JTI string          `json:"jti"`
}

func (r *ClientMetadataResolver) VerifyClientAssertion(ctx context.Context, assertion, clientID, audience string, now time.Time) (*ClientMetadata, string, error) {
	headerPart, payloadPart, signaturePart, ok := splitJWT(assertion)
	if !ok {
		return nil, "", fmt.Errorf("%w: malformed client assertion", ErrInvalidClient)
	}
	var header assertionHeader
	if err := decodeSegment(headerPart, &header); err != nil {
		return nil, "", fmt.Errorf("%w: malformed client assertion header", ErrInvalidClient)
	}
	if header.Alg != "ES256" {
		return nil, "", fmt.Errorf("%w: unsupported client assertion alg", ErrInvalidClient)
	}
	if header.Typ != "" && header.Typ != "JWT" {
		return nil, "", fmt.Errorf("%w: unsupported client assertion typ", ErrInvalidClient)
	}
	var claims assertionClaims
	if err := decodeSegment(payloadPart, &claims); err != nil {
		return nil, "", fmt.Errorf("%w: malformed client assertion claims", ErrInvalidClient)
	}
	if claims.Iss != clientID || claims.Sub != clientID {
		return nil, "", fmt.Errorf("%w: client assertion issuer or subject mismatch", ErrInvalidClient)
	}
	if !audienceMatches(claims.Aud, audience) {
		return nil, "", fmt.Errorf("%w: client assertion audience mismatch", ErrInvalidClient)
	}
	exp, err := parseNumericDate(claims.Exp)
	if err != nil {
		return nil, "", fmt.Errorf("%w: client assertion has no valid exp", ErrInvalidClient)
	}
	iat, err := parseNumericDate(claims.Iat)
	if err != nil {
		return nil, "", fmt.Errorf("%w: client assertion has no valid iat", ErrInvalidClient)
	}
	if now.Unix() >= exp {
		return nil, "", fmt.Errorf("%w: client assertion expired", ErrInvalidClient)
	}
	if iat > now.Add(60*time.Second).Unix() {
		return nil, "", fmt.Errorf("%w: client assertion issued in the future", ErrInvalidClient)
	}
	if claims.JTI == "" {
		return nil, "", fmt.Errorf("%w: client assertion has no jti", ErrInvalidClient)
	}

	metadata, err := r.Resolve(ctx, clientID)
	if err != nil {
		return nil, "", err
	}
	if metadata.TokenEndpointAuthMethod != ClientAuthPrivateKeyJWT {
		return nil, "", fmt.Errorf("%w: client is not using private_key_jwt", ErrInvalidClient)
	}
	keys, err := r.clientKeys(ctx, metadata)
	if err != nil {
		return nil, "", err
	}
	key, err := selectClientKey(keys, header.Kid)
	if err != nil {
		return nil, "", err
	}
	if key.Curve != "P-256" {
		return nil, "", fmt.Errorf("%w: client assertion key is not P-256", ErrInvalidClient)
	}
	publicKey, err := atcrypto.ParsePublicJWK(*key)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidClient, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return nil, "", fmt.Errorf("%w: malformed client assertion signature", ErrInvalidClient)
	}
	if err := publicKey.HashAndVerifyLenient([]byte(headerPart+"."+payloadPart), signature); err != nil {
		return nil, "", fmt.Errorf("%w: client assertion signature invalid", ErrInvalidClient)
	}
	if r.Nonces != nil {
		fresh, err := r.Nonces.RecordJTI(ctx, "client-assertion:"+claims.JTI, time.Unix(exp, 0).Add(60*time.Second))
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrInvalidClient, err)
		}
		if !fresh {
			return nil, "", fmt.Errorf("%w: client assertion replayed", ErrInvalidClient)
		}
	}
	return metadata, jwkThumbprint(key), nil
}

func jwkThumbprint(key *atcrypto.JWK) string {
	canonical := struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{Crv: key.Curve, Kty: key.KeyType, X: key.X, Y: key.Y}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func parseNumericDate(value json.Number) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("missing")
	}
	return strconv.ParseInt(value.String(), 10, 64)
}

func audienceMatches(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == want
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		for _, audience := range multiple {
			if audience == want {
				return true
			}
		}
	}
	return false
}

func selectClientKey(keys []atcrypto.JWK, kid string) (*atcrypto.JWK, error) {
	if kid != "" {
		for i := range keys {
			if keys[i].KeyID != nil && *keys[i].KeyID == kid {
				return &keys[i], nil
			}
		}
		return nil, fmt.Errorf("%w: client assertion key not found", ErrInvalidClient)
	}
	if len(keys) != 1 {
		return nil, fmt.Errorf("%w: client assertion key id required", ErrInvalidClient)
	}
	return &keys[0], nil
}

func (r *ClientMetadataResolver) clientKeys(ctx context.Context, metadata *ClientMetadata) ([]atcrypto.JWK, error) {
	if metadata.JWKS != nil && len(metadata.JWKS.Keys) > 0 {
		return metadata.JWKS.Keys, nil
	}
	if metadata.JWKSURI == nil || *metadata.JWKSURI == "" {
		return nil, fmt.Errorf("%w: client has no jwks", ErrInvalidClient)
	}
	target, err := metadataDocumentURL(*metadata.JWKSURI)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid jwks_uri", ErrInvalidClient)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClient, err)
	}
	req.Header.Set("Accept", "application/json")
	client := r.httpClientFor(target)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClient, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: jwks HTTP %d", ErrInvalidClient, resp.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return nil, fmt.Errorf("%w: jwks content type is not application/json", ErrInvalidClient)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxClientMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClient, err)
	}
	if len(raw) > MaxClientMetadataBytes {
		return nil, fmt.Errorf("%w: jwks document too large", ErrInvalidClient)
	}
	var jwks struct {
		Keys []atcrypto.JWK `json:"keys"`
	}
	if err := json.Unmarshal(raw, &jwks); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClient, err)
	}
	if len(jwks.Keys) == 0 {
		return nil, fmt.Errorf("%w: jwks has no keys", ErrInvalidClient)
	}
	return jwks.Keys, nil
}
