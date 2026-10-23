package oauthclient

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

const (
	DPoPLifetime = 30 * time.Second

	ReplyLimit = 1 << 20
)

type DPoPHeader struct {
	Typ string        `json:"typ"`
	Alg string        `json:"alg"`
	JWK *atcrypto.JWK `json:"jwk"`
}

type DPoPClaims struct {
	JTI string `json:"jti"`

	HTM string `json:"htm"`

	HTU string `json:"htu"`

	IAT int64 `json:"iat"`

	EXP int64 `json:"exp"`

	ATH string `json:"ath,omitempty"`

	Nonce string `json:"nonce,omitempty"`
}

func DPoPProof(key atcrypto.PrivateKeyExportable, method, target, accessToken, nonce string) (string, error) {
	if key == nil {
		return "", errors.New("oauthclient: no DPoP key")
	}
	public, err := key.PublicKey()
	if err != nil {
		return "", err
	}
	jwk, err := public.JWK()
	if err != nil {
		return "", err
	}
	htu, ok := DPoPHTU(target)
	if !ok {
		return "", fmt.Errorf("oauthclient: the request url %q is not absolute", target)
	}
	jtiRaw := make([]byte, 16)
	if _, err := rand.Read(jtiRaw); err != nil {
		return "", err
	}
	now := time.Now()
	claims := DPoPClaims{
		JTI:   base64.RawURLEncoding.EncodeToString(jtiRaw),
		HTM:   strings.ToUpper(method),
		HTU:   htu,
		IAT:   now.Unix(),
		EXP:   now.Add(DPoPLifetime).Unix(),
		Nonce: nonce,
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims.ATH = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	headerJSON, err := json.Marshal(DPoPHeader{Typ: "dpop+jwt", Alg: "ES256", JWK: jwk})
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

func DPoPHTU(raw string) (string, bool) {
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
	path = cmp.Or(path, "/")
	return scheme + "://" + host + path, true
}
