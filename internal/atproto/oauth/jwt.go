package oauth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	TokenTypeAccess  = "at+jwt"
	TokenTypeRefresh = "refresh+jwt"
)

type Signer struct {
	DID syntax.DID
	Key atcrypto.PrivateKey
}

type tokenHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
}

type tokenCNF struct {
	JKT string `json:"jkt"`
}

type tokenClaims struct {
	Iss    string   `json:"iss"`
	Sub    string   `json:"sub"`
	Handle string   `json:"handle,omitempty"`
	Scope  string   `json:"scope"`
	CNF    tokenCNF `json:"cnf"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
	JTI    string   `json:"jti"`
}

type signedToken struct {
	Token     string
	JTI       string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

func signingAlgorithm(key atcrypto.PrivateKey) (string, error) {
	switch key.(type) {
	case *atcrypto.PrivateKeyP256:
		return "ES256", nil
	case *atcrypto.PrivateKeyK256:
		return "ES256K", nil
	default:
		return "", fmt.Errorf("oauth: unsupported signing key type %T", key)
	}
}

func tokenType(kind TokenKind) string {
	if kind == TokenRefresh {
		return TokenTypeRefresh
	}
	return TokenTypeAccess
}

func issueSignedToken(signer Signer, now time.Time, kind TokenKind, session Session, ttl time.Duration) (*signedToken, error) {
	alg, err := signingAlgorithm(signer.Key)
	if err != nil {
		return nil, err
	}
	jti, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	header := tokenHeader{
		Typ: tokenType(kind),
		Alg: alg,
		Kid: signer.DID.String() + "#atproto",
	}
	claims := tokenClaims{
		Iss:    signer.DID.String(),
		Sub:    session.Account.DID.String(),
		Handle: session.Account.Handle,
		Scope:  session.Scope,
		CNF:    tokenCNF{JKT: session.JKT},
		Iat:    now.Unix(),
		Exp:    now.Add(ttl).Unix(),
		JTI:    jti,
	}
	headerPart, err := encodeSegment(header)
	if err != nil {
		return nil, err
	}
	payloadPart, err := encodeSegment(claims)
	if err != nil {
		return nil, err
	}
	signingInput := headerPart + "." + payloadPart
	signature, err := signer.Key.HashAndSign([]byte(signingInput))
	if err != nil {
		return nil, err
	}
	return &signedToken{
		Token:     signingInput + "." + base64.RawURLEncoding.EncodeToString(signature),
		JTI:       jti,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}, nil
}

func parseOAuthToken(token string, kind TokenKind, signer Signer, now time.Time) (*tokenClaims, error) {
	headerPart, payloadPart, signaturePart, ok := splitJWT(token)
	if !ok {
		return nil, fmt.Errorf("%w: malformed JWT", ErrInvalidToken)
	}
	var header tokenHeader
	if err := decodeSegment(headerPart, &header); err != nil {
		return nil, fmt.Errorf("%w: malformed header", ErrInvalidToken)
	}
	if header.Typ != tokenType(kind) {
		return nil, fmt.Errorf("%w: unexpected typ", ErrInvalidToken)
	}
	alg, err := signingAlgorithm(signer.Key)
	if err != nil {
		return nil, err
	}
	if header.Alg != alg {
		return nil, fmt.Errorf("%w: unexpected alg", ErrInvalidToken)
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed signature", ErrInvalidToken)
	}
	publicKey, err := signer.Key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if err := publicKey.HashAndVerifyLenient([]byte(headerPart+"."+payloadPart), signature); err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	var claims tokenClaims
	if err := decodeSegment(payloadPart, &claims); err != nil {
		return nil, fmt.Errorf("%w: malformed claims", ErrInvalidToken)
	}
	if claims.Iss != signer.DID.String() {
		return nil, fmt.Errorf("%w: unexpected issuer", ErrInvalidToken)
	}
	if _, err := syntax.ParseDID(claims.Sub); err != nil {
		return nil, fmt.Errorf("%w: invalid subject", ErrInvalidToken)
	}
	if claims.JTI == "" || claims.Exp == 0 || claims.Iat == 0 {
		return nil, fmt.Errorf("%w: missing claims", ErrInvalidToken)
	}
	if now.Unix() >= claims.Exp {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if claims.Iat > now.Add(5*time.Minute).Unix() {
		return nil, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	if claims.CNF.JKT == "" {
		return nil, fmt.Errorf("%w: missing cnf.jkt", ErrInvalidToken)
	}
	if !hasScope(claims.Scope, ScopeAtproto) {
		return nil, fmt.Errorf("%w: scope does not include atproto", ErrInvalidToken)
	}
	return &claims, nil
}

func hasScope(scope, want string) bool {
	for _, token := range strings.Fields(scope) {
		if token == want {
			return true
		}
	}
	return false
}
