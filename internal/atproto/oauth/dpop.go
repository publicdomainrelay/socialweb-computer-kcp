package oauth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	HeaderDPoP      = "DPoP"
	HeaderDPoPNonce = "DPoP-Nonce"

	AuthSchemeDPoP = "DPoP"

	DefaultDPoPLeeway       = 30 * time.Second
	DefaultDPoPReplayWindow = 5 * time.Minute
	DefaultNonceLifetime    = 5 * time.Minute
	DefaultNonceGrace       = 30 * time.Second
)

type DPoPProof struct {
	JKT   string
	JTI   string
	Nonce string
}

type DPoPVerifier struct {
	Nonces       NonceStore
	Leeway       time.Duration
	ReplayWindow time.Duration
	RequireNonce bool
}

func NewDPoPVerifier(nonces NonceStore) *DPoPVerifier {
	return &DPoPVerifier{
		Nonces:       nonces,
		Leeway:       DefaultDPoPLeeway,
		ReplayWindow: DefaultDPoPReplayWindow,
		RequireNonce: true,
	}
}

type dpopHeader struct {
	Typ string   `json:"typ"`
	Alg string   `json:"alg"`
	JWK *dpopJWK `json:"jwk"`
}

type dpopJWK struct {
	Kty string          `json:"kty"`
	Crv string          `json:"crv"`
	X   string          `json:"x"`
	Y   string          `json:"y"`
	D   json.RawMessage `json:"d"`
}

type dpopPayload struct {
	HTM   string      `json:"htm"`
	HTU   string      `json:"htu"`
	IAT   json.Number `json:"iat"`
	JTI   string      `json:"jti"`
	ATH   string      `json:"ath"`
	Nonce string      `json:"nonce"`
	Exp   json.Number `json:"exp"`
}

func (v *DPoPVerifier) Verify(ctx context.Context, proof, method, requestURL, accessToken string) (*DPoPProof, error) {
	if proof == "" {
		return nil, fmt.Errorf("%w: missing DPoP proof", ErrUseDPoPNonce)
	}

	leeway := v.Leeway
	leeway = opt.Positive(leeway, DefaultDPoPLeeway)
	replayWindow := v.ReplayWindow
	replayWindow = opt.Positive(replayWindow, DefaultDPoPReplayWindow)

	headerPart, payloadPart, signaturePart, ok := splitJWT(proof)
	if !ok {
		return nil, fmt.Errorf("%w: malformed JWT", ErrInvalidDPoPProof)
	}

	var header dpopHeader
	if err := decodeSegment(headerPart, &header); err != nil {
		return nil, fmt.Errorf("%w: malformed header", ErrInvalidDPoPProof)
	}
	if header.Typ != "dpop+jwt" {
		return nil, fmt.Errorf("%w: unexpected typ", ErrInvalidDPoPProof)
	}
	if header.Alg != "ES256" {
		return nil, fmt.Errorf("%w: unexpected alg", ErrInvalidDPoPProof)
	}
	if header.JWK == nil {
		return nil, fmt.Errorf("%w: missing jwk", ErrInvalidDPoPProof)
	}
	if header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		return nil, fmt.Errorf("%w: unsupported jwk", ErrInvalidDPoPProof)
	}
	if len(header.JWK.D) > 0 && string(header.JWK.D) != "null" {
		return nil, fmt.Errorf("%w: private jwk", ErrInvalidDPoPProof)
	}
	if header.JWK.X == "" || header.JWK.Y == "" {
		return nil, fmt.Errorf("%w: incomplete jwk", ErrInvalidDPoPProof)
	}

	publicKey, err := atcrypto.ParsePublicJWK(atcrypto.JWK{
		KeyType: header.JWK.Kty,
		Curve:   header.JWK.Crv,
		X:       header.JWK.X,
		Y:       header.JWK.Y,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDPoPProof, err)
	}

	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed signature", ErrInvalidDPoPProof)
	}
	if err := publicKey.HashAndVerifyLenient([]byte(headerPart+"."+payloadPart), signature); err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrInvalidDPoPProof)
	}

	var payload dpopPayload
	if err := decodeSegment(payloadPart, &payload); err != nil {
		return nil, fmt.Errorf("%w: malformed payload", ErrInvalidDPoPProof)
	}
	if payload.HTM != strings.ToUpper(method) {
		return nil, fmt.Errorf("%w: htm mismatch", ErrInvalidDPoPProof)
	}
	proofTarget, ok := normalizeHTU(payload.HTU)
	if !ok {
		return nil, fmt.Errorf("%w: invalid htu", ErrInvalidDPoPProof)
	}
	requestTarget, ok := normalizeHTU(requestURL)
	if !ok {
		return nil, fmt.Errorf("%w: invalid request URL", ErrInvalidDPoPProof)
	}
	if proofTarget != requestTarget {
		return nil, fmt.Errorf("%w: htu mismatch", ErrInvalidDPoPProof)
	}
	if payload.JTI == "" {
		return nil, fmt.Errorf("%w: missing jti", ErrInvalidDPoPProof)
	}

	iat, err := strconv.ParseInt(payload.IAT.String(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: missing iat", ErrInvalidDPoPProof)
	}
	now := time.Now()
	skew := now.Unix() - iat
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > leeway {
		return nil, fmt.Errorf("%w: stale iat", ErrInvalidDPoPProof)
	}
	if payload.Exp != "" {
		exp, err := strconv.ParseInt(payload.Exp.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid exp", ErrInvalidDPoPProof)
		}
		if now.Unix() >= exp {
			return nil, fmt.Errorf("%w: expired proof", ErrInvalidDPoPProof)
		}
	}

	if accessToken != "" {
		if payload.ATH == "" {
			return nil, fmt.Errorf("%w: missing ath", ErrInvalidDPoPProof)
		}
		sum := sha256.Sum256([]byte(accessToken))
		expected := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(payload.ATH), []byte(expected)) != 1 {
			return nil, fmt.Errorf("%w: ath mismatch", ErrInvalidDPoPProof)
		}
	}

	if payload.Nonce != "" || v.RequireNonce {
		if payload.Nonce == "" {
			return nil, fmt.Errorf("%w: missing nonce", ErrUseDPoPNonce)
		}
		if v.Nonces == nil {
			return nil, fmt.Errorf("%w: no nonce store", ErrUseDPoPNonce)
		}
		origin, ok := dpopOrigin(requestURL)
		if !ok {
			return nil, fmt.Errorf("%w: invalid request URL", ErrInvalidDPoPProof)
		}
		valid, err := v.Nonces.VerifyNonce(ctx, origin, payload.Nonce)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUseDPoPNonce, err)
		}
		if !valid {
			return nil, fmt.Errorf("%w: unknown nonce", ErrUseDPoPNonce)
		}
	}

	if v.Nonces != nil {
		fresh, err := v.Nonces.RecordJTI(ctx, "dpop:"+payload.JTI, now.Add(replayWindow))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidDPoPProof, err)
		}
		if !fresh {
			return nil, fmt.Errorf("%w: replayed jti", ErrInvalidDPoPProof)
		}
	}

	return &DPoPProof{
		JKT:   jktFromJWK(header.JWK),
		JTI:   payload.JTI,
		Nonce: payload.Nonce,
	}, nil
}

func jktFromJWK(jwk *dpopJWK) string {
	canonical := struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{Crv: jwk.Crv, Kty: jwk.Kty, X: jwk.X, Y: jwk.Y}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func normalizeHTU(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", false
	}
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = host + ":" + port
	}
	path := parsed.EscapedPath()
	path = cmp.Or(path, "/")
	return scheme + "://" + host + path, true
}

func dpopOrigin(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", false
	}
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = host + ":" + port
	}
	return scheme + "://" + host, true
}

type memoryNonceEntry struct {
	current         string
	previous        string
	previousExpires time.Time
}

type MemoryNonceStore struct {
	issueMu sync.Mutex
	origins *ttlcache.Cache[string, memoryNonceEntry]
	jtis    *ttlcache.Cache[string, struct{}]
}

func NewMemoryNonceStore() *MemoryNonceStore {
	return &MemoryNonceStore{
		origins: ttlcache.New[string, memoryNonceEntry](ttlcache.Options{TTL: DefaultNonceLifetime}),
		jtis:    ttlcache.New[string, struct{}](ttlcache.Options{}),
	}
}

func (s *MemoryNonceStore) IssueNonce(_ context.Context, origin string) (string, error) {
	nonce, err := randomToken(16)
	if err != nil {
		return "", err
	}
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	entry, _ := s.origins.Get(origin)
	entry.previous = entry.current
	if entry.current != "" {
		entry.previousExpires = time.Now().Add(DefaultNonceGrace)
	}
	entry.current = nonce
	s.origins.Set(origin, entry)
	return nonce, nil
}

func (s *MemoryNonceStore) VerifyNonce(_ context.Context, origin, nonce string) (bool, error) {
	if nonce == "" {
		return false, nil
	}
	entry, ok := s.origins.Get(origin)
	if !ok {
		return false, nil
	}
	if entry.current == nonce {
		return true, nil
	}
	if entry.previous != "" && entry.previous == nonce && time.Now().Before(entry.previousExpires) {
		return true, nil
	}
	return false, nil
}

func (s *MemoryNonceStore) RecordJTI(_ context.Context, jti string, expiresAt time.Time) (bool, error) {
	ttl := time.Until(expiresAt)
	ttl = opt.Positive(ttl, -1)
	return s.jtis.Add(jti, struct{}{}, ttl), nil
}

var _ NonceStore = (*MemoryNonceStore)(nil)
