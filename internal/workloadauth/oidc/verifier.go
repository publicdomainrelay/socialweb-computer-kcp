package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/util/ssrf"
	"github.com/golang-jwt/jwt/v5"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

const (
	DefaultKeysTTL = 5 * time.Minute

	DefaultLeeway = 30 * time.Second

	DefaultMaxTokenBytes = 8192

	DefaultMaxLifetime = time.Hour

	maxDocumentBytes = 1 << 20

	minRefetch = 10 * time.Second
)

var (
	ErrRefused = errors.New("oidc: workload token refused")

	signingMethods = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "EdDSA"}
)

type Config struct {
	HTTPClient *http.Client

	AllowLoopbackIssuers bool

	KeysTTL time.Duration

	Leeway time.Duration

	MaxTokenBytes int

	MaxLifetime time.Duration

	Now func() time.Time
}

type Verifier struct {
	public *http.Client

	loopback *http.Client

	allowLoopback bool

	keysTTL time.Duration

	leeway time.Duration

	maxTokenBytes int

	maxLifetime time.Duration

	now func() time.Time

	mu sync.Mutex

	issuers map[string]*issuerKeys
}

type issuerKeys struct {
	keys map[string]any

	fetched time.Time
}

var _ workloadauth.TokenVerifier = (*Verifier)(nil)

func New(cfg Config) *Verifier {
	public := cfg.HTTPClient
	if public == nil {
		public = &http.Client{Timeout: 10 * time.Second, Transport: ssrf.PublicOnlyTransport()}
	}
	public = noRedirects(public)
	v := &Verifier{
		public:        public,
		allowLoopback: cfg.AllowLoopbackIssuers,
		keysTTL:       positive(cfg.KeysTTL, DefaultKeysTTL),
		leeway:        positive(cfg.Leeway, DefaultLeeway),
		maxTokenBytes: cfg.MaxTokenBytes,
		maxLifetime:   positive(cfg.MaxLifetime, DefaultMaxLifetime),
		now:           cfg.Now,
		issuers:       map[string]*issuerKeys{},
	}
	if v.maxTokenBytes <= 0 {
		v.maxTokenBytes = DefaultMaxTokenBytes
	}
	if v.now == nil {
		v.now = time.Now
	}
	if cfg.AllowLoopbackIssuers {
		base := cfg.HTTPClient
		if base == nil {
			base = &http.Client{Timeout: 10 * time.Second}
		}
		v.loopback = noRedirects(base)
	}
	return v
}

func positive(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func noRedirects(base *http.Client) *http.Client {
	cloned := *base
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cloned
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

func (v *Verifier) Verify(ctx context.Context, token, issuer, audience string) (workloadauth.Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return workloadauth.Claims{}, refused("no token")
	}
	if len(token) > v.maxTokenBytes {
		return workloadauth.Claims{}, refused("the token is longer than %d bytes", v.maxTokenBytes)
	}
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if _, err := v.clientFor(issuer); err != nil {
		return workloadauth.Claims{}, err
	}
	if audience == "" {
		return workloadauth.Claims{}, refused("no audience to check")
	}
	unverified := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, unverified); err != nil {
		return workloadauth.Claims{}, refused("malformed token: %v", err)
	}
	if iss, _ := unverified["iss"].(string); strings.TrimRight(iss, "/") != issuer {
		return workloadauth.Claims{}, refused("the token's issuer is not %s", issuer)
	}
	claims, err := v.parse(ctx, token, issuer, audience, false)
	if err != nil {
		claims, err = v.parse(ctx, token, issuer, audience, true)
	}
	if err != nil {
		return workloadauth.Claims{}, err
	}
	return claims, nil
}

func (v *Verifier) parse(ctx context.Context, token, issuer, audience string, refresh bool) (workloadauth.Claims, error) {
	keys, err := v.keys(ctx, issuer, refresh)
	if err != nil {
		return workloadauth.Claims{}, err
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods(signingMethods),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(v.leeway),
		jwt.WithTimeFunc(v.now),
	)
	mapped := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(token, mapped, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if key, ok := keys[kid]; ok && kid != "" {
			return key, nil
		}
		if kid == "" && len(keys) == 1 {
			for _, key := range keys {
				return key, nil
			}
		}
		return nil, fmt.Errorf("no key %q in the issuer's set", kid)
	}); err != nil {
		return workloadauth.Claims{}, refused("token verification failed: %v", err)
	}
	out := workloadauth.Claims{Issuer: issuer}
	out.Subject, _ = mapped["sub"].(string)
	if out.Subject == "" {
		return workloadauth.Claims{}, refused("the token names no subject")
	}
	out.Audience, _ = mapped.GetAudience()
	if exp, err := mapped.GetExpirationTime(); err == nil && exp != nil {
		out.Expiry = exp.Time
	}
	if iat, err := mapped.GetIssuedAt(); err == nil && iat != nil {
		out.IssuedAt = iat.Time
	}
	if nbf, err := mapped.GetNotBefore(); err == nil && nbf != nil {
		out.NotBefore = nbf.Time
	}
	start := out.IssuedAt
	if start.IsZero() {
		start = v.now()
	}
	if out.Expiry.Sub(start) > v.maxLifetime {
		return workloadauth.Claims{}, refused("the token lives longer than %s", v.maxLifetime)
	}
	out.ID, _ = mapped["jti"].(string)
	return out, nil
}

func (v *Verifier) clientFor(issuer string) (*http.Client, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, refused("issuer %q is not an https URL", issuer)
	}
	host := u.Hostname()
	loopback := host == "localhost" || isLoopbackIP(host)
	switch {
	case loopback && v.allowLoopback:
		return v.loopback, nil
	case loopback:
		return nil, refused("issuer %q is a loopback address", issuer)
	case u.Scheme != "https":
		return nil, refused("issuer %q is not https", issuer)
	}
	return v.public, nil
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (v *Verifier) keys(ctx context.Context, issuer string, refresh bool) (map[string]any, error) {
	now := v.now()
	v.mu.Lock()
	held := v.issuers[issuer]
	v.mu.Unlock()
	if held != nil {
		fresh := now.Sub(held.fetched) < v.keysTTL
		if fresh && (!refresh || now.Sub(held.fetched) < minRefetch) {
			return held.keys, nil
		}
	}
	keys, err := v.fetchKeys(ctx, issuer)
	if err != nil {
		if held != nil && now.Sub(held.fetched) < v.keysTTL {
			return held.keys, nil
		}
		return nil, err
	}
	v.mu.Lock()
	v.issuers[issuer] = &issuerKeys{keys: keys, fetched: now}
	v.mu.Unlock()
	return keys, nil
}

func (v *Verifier) fetchKeys(ctx context.Context, issuer string) (map[string]any, error) {
	client, err := v.clientFor(issuer)
	if err != nil {
		return nil, err
	}
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, client, issuer+"/.well-known/openid-configuration", &discovery); err != nil {
		return nil, refused("the issuer's discovery document: %v", err)
	}
	if strings.TrimRight(discovery.Issuer, "/") != issuer {
		return nil, refused("the discovery document names issuer %q, not %q", discovery.Issuer, issuer)
	}
	jwksClient, err := v.clientFor(discovery.JWKSURI)
	if err != nil {
		return nil, refused("the issuer's jwks_uri: %v", err)
	}
	var raw json.RawMessage
	if err := getJSON(ctx, jwksClient, discovery.JWKSURI, &raw); err != nil {
		return nil, refused("the issuer's keys: %v", err)
	}
	return ParseJWKSet(raw)
}

func getJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered HTTP %d", target, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxDocumentBytes {
		return fmt.Errorf("GET %s answered more than %d bytes", target, maxDocumentBytes)
	}
	return json.Unmarshal(body, out)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func ParseJWKSet(raw []byte) (map[string]any, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, refused("the keys are not a JWK set: %v", err)
	}
	out := map[string]any{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Alg != "" && !slices.Contains(signingMethods, k.Alg) {
			continue
		}
		key, err := parseJWK(k)
		if err != nil {
			continue
		}
		if _, dup := out[k.Kid]; dup {
			return nil, refused("the key set names kid %q twice", k.Kid)
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, refused("the key set holds no usable signing key")
	}
	return out, nil
}

func decode(field string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(field, "="))
}

func parseJWK(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := decode(k.N)
		if err != nil || len(n) == 0 {
			return nil, errors.New("no modulus")
		}
		e, err := decode(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("no exponent")
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		modulus := new(big.Int).SetBytes(n)
		if exponent < 3 || exponent%2 == 0 || modulus.BitLen() < 2048 {
			return nil, errors.New("weak RSA key")
		}
		return &rsa.PublicKey{N: modulus, E: exponent}, nil
	case "EC":
		var curve elliptic.Curve
		size := 0
		switch k.Crv {
		case "P-256":
			curve, size = elliptic.P256(), 32
		case "P-384":
			curve, size = elliptic.P384(), 48
		default:
			return nil, errors.New("unsupported curve")
		}
		x, err := decode(k.X)
		if err != nil || len(x) != size {
			return nil, errors.New("bad x")
		}
		y, err := decode(k.Y)
		if err != nil || len(y) != size {
			return nil, errors.New("bad y")
		}
		point := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(point.X, point.Y) {
			return nil, errors.New("not on curve")
		}
		return point, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, errors.New("unsupported OKP curve")
		}
		x, err := decode(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad x")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errors.New("unsupported kty")
}

type narrowed struct {
	next workloadauth.TokenVerifier

	issuers []string
}

func Narrow(next workloadauth.TokenVerifier, issuers []string) workloadauth.TokenVerifier {
	if len(issuers) == 0 {
		return next
	}
	clean := make([]string, 0, len(issuers))
	for _, issuer := range issuers {
		clean = append(clean, strings.TrimRight(strings.TrimSpace(issuer), "/"))
	}
	return &narrowed{next: next, issuers: clean}
}

func (n *narrowed) Verify(ctx context.Context, token, issuer, audience string) (workloadauth.Claims, error) {
	if !slices.Contains(n.issuers, strings.TrimRight(strings.TrimSpace(issuer), "/")) {
		return workloadauth.Claims{}, refused("issuer %q is not one this PDS trusts", issuer)
	}
	return n.next.Verify(ctx, token, issuer, audience)
}
