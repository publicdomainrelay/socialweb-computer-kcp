package guestidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenRefused = errors.New("guestidentity: guest token refused")

	errNoKey = errors.New("guestidentity: no key in the issuer set matches the token")
)

var verificationMethods = []string{"RS256", "PS256", "ES256"}

type TokenClaims struct {
	Audience Audience

	API string

	Actx string

	ComputeVM string

	Claims map[string]any
}

type Verifier struct {
	Keys func(context.Context) ([]byte, error)

	Issuer func() string

	ClockTolerance time.Duration

	Now func() time.Time

	MaxTokenBytes int

	mu  sync.Mutex
	set map[string]any
}

func NewVerifier(keys func(context.Context) ([]byte, error), issuer func() string) *Verifier {
	return &Verifier{Keys: keys, Issuer: issuer, MaxTokenBytes: 8192, ClockTolerance: 5 * time.Second}
}

func NewOpenBaoVerifier(identity *Client, ingressURL func() string) *Verifier {
	return NewVerifier(IdentityKeys(identity), func() string { return OIDCIssuer(ingressURL()) })
}

func (v *Verifier) Refresh(ctx context.Context) error {
	raw, err := v.Keys(ctx)
	if err != nil {
		return fmt.Errorf("guestidentity: reading the OIDC keys: %w", err)
	}
	set, err := parseJWKSet(raw)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.set = set
	v.mu.Unlock()
	return nil
}

func (v *Verifier) Verify(ctx context.Context, token string) (TokenClaims, error) {
	if v == nil || v.Keys == nil || v.Issuer == nil {
		return TokenClaims{}, refused("no verifier")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return TokenClaims{}, refused("no token")
	}
	if limit := v.MaxTokenBytes; limit > 0 && len(token) > limit {
		return TokenClaims{}, refused("the token is longer than %d bytes", limit)
	}
	unverified := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, unverified); err != nil {
		return TokenClaims{}, refused("malformed token: %v", err)
	}
	auds, err := unverified.GetAudience()
	if err != nil || len(auds) != 1 {
		return TokenClaims{}, refused("the token names no single audience")
	}
	audience, err := ParseAudience(auds[0])
	if err != nil {
		return TokenClaims{}, refused("%v", err)
	}
	if audience.API != KCPAPI {
		return TokenClaims{}, refused("the token's audience is not for %s", KCPAPI)
	}
	issuer := strings.TrimRight(strings.TrimSpace(v.Issuer()), "/")
	if issuer == "" {
		return TokenClaims{}, refused("the verifier names no issuer")
	}

	claims, err := v.parse(ctx, token, issuer, audience)
	if err != nil && (errors.Is(err, jwt.ErrTokenUnverifiable) || errors.Is(err, jwt.ErrTokenSignatureInvalid) || errors.Is(err, errNoKey)) {
		if rerr := v.Refresh(ctx); rerr == nil {
			claims, err = v.parse(ctx, token, issuer, audience)
		}
	}
	if err != nil {
		return TokenClaims{}, refused("token verification failed: %v", err)
	}
	computevm, _ := claims["computevm"].(string)
	if computevm == "" || computevm != audience.Actx {
		return TokenClaims{}, refused("the token's computevm claim is not the audience's context")
	}
	if !ValidComputeVM(computevm) {
		return TokenClaims{}, refused("the token's computevm claim is not a ComputeVM name")
	}
	return TokenClaims{Audience: audience, API: audience.API, Actx: audience.Actx, ComputeVM: computevm, Claims: claims}, nil
}

func (v *Verifier) VerifyAudience(ctx context.Context, token, rawAudience string) (map[string]any, error) {
	if v == nil || v.Keys == nil || v.Issuer == nil {
		return nil, refused("no verifier")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, refused("no token")
	}
	if limit := v.MaxTokenBytes; limit > 0 && len(token) > limit {
		return nil, refused("the token is longer than %d bytes", limit)
	}
	audience, err := ParseAudience(rawAudience)
	if err != nil {
		return nil, refused("%v", err)
	}
	issuer := strings.TrimRight(strings.TrimSpace(v.Issuer()), "/")
	if issuer == "" {
		return nil, refused("the verifier names no issuer")
	}
	claims, err := v.parse(ctx, token, issuer, audience)
	if err != nil && (errors.Is(err, jwt.ErrTokenUnverifiable) || errors.Is(err, jwt.ErrTokenSignatureInvalid) || errors.Is(err, errNoKey)) {
		if rerr := v.Refresh(ctx); rerr == nil {
			claims, err = v.parse(ctx, token, issuer, audience)
		}
	}
	if err != nil {
		return nil, refused("token verification failed: %v", err)
	}
	return claims, nil
}

func (v *Verifier) parse(ctx context.Context, token, issuer string, audience Audience) (jwt.MapClaims, error) {
	v.mu.Lock()
	set := v.set
	v.mu.Unlock()
	if set == nil {
		if err := v.Refresh(ctx); err != nil {
			return nil, err
		}
		v.mu.Lock()
		set = v.set
		v.mu.Unlock()
	}
	options := []jwt.ParserOption{
		jwt.WithValidMethods(verificationMethods),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience.String()),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(v.ClockTolerance),
	}
	if v.Now != nil {
		options = append(options, jwt.WithTimeFunc(v.Now))
	}
	parser := jwt.NewParser(options...)
	claims := jwt.MapClaims{}
	parsed, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		key, err := selectKey(set, t)
		if err != nil {
			return nil, err
		}
		if _, ok := key.(*ecdsa.PublicKey); ok && t.Method.Alg() == jwt.SigningMethodES256.Alg() {
			t.Method = jwt.SigningMethodES256
		}
		return key, nil
	})
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("the token is not valid")
	}
	return claims, nil
}

func selectKey(set map[string]any, t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if key, ok := set[kid]; ok {
		return key, nil
	}
	if kid == "" && len(set) == 1 {
		for _, key := range set {
			return key, nil
		}
	}
	return nil, errNoKey
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTokenRefused, fmt.Sprintf(format, args...))
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

func parseJWKSet(raw []byte) (map[string]any, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("guestidentity: the OIDC keys are not a JWK set: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("guestidentity: the OIDC key set is empty")
	}
	out := map[string]any{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Alg != "" && !contains(verificationMethods, k.Alg) {
			continue
		}
		key, err := parseJWK(k)
		if err != nil {
			return nil, err
		}
		if _, dup := out[k.Kid]; dup {
			return nil, fmt.Errorf("guestidentity: the OIDC key set names kid %q twice", k.Kid)
		}
		out[k.Kid] = key
	}
	if len(out) == 0 {
		return nil, errors.New("guestidentity: the OIDC key set holds no signing key these methods verify")
	}
	return out, nil
}

func parseJWK(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil || len(n) == 0 {
			return nil, fmt.Errorf("guestidentity: the OIDC RSA key %q carries no modulus", k.Kid)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, fmt.Errorf("guestidentity: the OIDC RSA key %q carries no exponent", k.Kid)
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 || exponent%2 == 0 {
			return nil, fmt.Errorf("guestidentity: the OIDC RSA key %q carries exponent %d", k.Kid, exponent)
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			return nil, fmt.Errorf("guestidentity: the OIDC RSA key %q is %d bits, under 2048", k.Kid, modulus.BitLen())
		}
		return &rsa.PublicKey{N: modulus, E: exponent}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("guestidentity: the OIDC EC key %q names curve %q", k.Kid, k.Crv)
		}
		if k.Alg != "" && k.Alg != "ES256" {
			return nil, fmt.Errorf("guestidentity: the OIDC EC key %q names algorithm %q", k.Kid, k.Alg)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != 32 {
			return nil, fmt.Errorf("guestidentity: the OIDC EC key %q carries no 32-byte x", k.Kid)
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil || len(y) != 32 {
			return nil, fmt.Errorf("guestidentity: the OIDC EC key %q carries no 32-byte y", k.Kid)
		}
		point := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !point.Curve.IsOnCurve(point.X, point.Y) {
			return nil, fmt.Errorf("guestidentity: the OIDC EC key %q is not on P-256", k.Kid)
		}
		return point, nil
	default:
		return nil, fmt.Errorf("guestidentity: the OIDC key %q has kty %q", k.Kid, k.Kty)
	}
}
