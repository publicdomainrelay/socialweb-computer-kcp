package pdsclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

const dpopProofLifetime = 30 * time.Second

func DPoPSourceFromData(data map[string]any) (DPoPSource, error) {
	key, err := dpopKey(data)
	if err != nil {
		return nil, err
	}
	return &dpopSigner{key: key}, nil
}

func dpopKey(data map[string]any) (atcrypto.PrivateKeyExportable, error) {
	if value, ok := data["dpopPrivateJwk"]; ok {
		key, err := dpopKeyValue(value)
		if err == nil {
			return key, nil
		}
	}
	for _, name := range []string{"dpopPrivateKeyMultibase", "dpop_privatekey_multibase", "privateKeyMultibase", "private_key_multibase", "dpopPrivateKey", "privateKey", "key", "value"} {
		value, ok := data[name]
		if !ok {
			continue
		}
		key, err := dpopKeyValue(value)
		if err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: the dpop key ref holds no private key", ErrInvalidConfig)
}

func dpopKeyValue(value any) (atcrypto.PrivateKeyExportable, error) {
	switch typed := value.(type) {
	case map[string]any:
		d, _ := typed["d"].(string)
		if d == "" {
			return nil, fmt.Errorf("%w: the dpop jwk has no d", ErrInvalidConfig)
		}
		return dpopKeyP256(d)
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return nil, fmt.Errorf("%w: the dpop key is empty", ErrInvalidConfig)
		}
		if strings.HasPrefix(raw, "{") {
			var jwk map[string]any
			if err := json.Unmarshal([]byte(raw), &jwk); err != nil {
				return nil, fmt.Errorf("%w: the dpop jwk: %v", ErrInvalidConfig, err)
			}
			return dpopKeyValue(jwk)
		}
		if strings.HasPrefix(raw, "z") {
			key, err := atcrypto.ParsePrivateMultibase(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: the dpop multibase key: %v", ErrInvalidConfig, err)
			}
			return key, nil
		}
		return dpopKeyP256(raw)
	}
	return nil, fmt.Errorf("%w: unsupported dpop key value", ErrInvalidConfig)
}

func dpopKeyP256(raw string) (atcrypto.PrivateKeyExportable, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(raw), "="))
	if err != nil {
		return nil, fmt.Errorf("%w: the dpop key: %v", ErrInvalidConfig, err)
	}
	key, err := atcrypto.ParsePrivateBytesP256(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: the dpop key: %v", ErrInvalidConfig, err)
	}
	return key, nil
}

type dpopSigner struct {
	key atcrypto.PrivateKeyExportable
}

func (s *dpopSigner) DPoPProof(ctx context.Context, method, target, accessToken string) (string, error) {
	if s == nil || s.key == nil {
		return "", fmt.Errorf("%w: no dpop key", ErrInvalidConfig)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%w: the dpop target %q is not absolute", ErrInvalidRequest, target)
	}
	public, err := s.key.PublicKey()
	if err != nil {
		return "", err
	}
	jwk, err := public.JWK()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(struct {
		Typ string        `json:"typ"`
		Alg string        `json:"alg"`
		JWK *atcrypto.JWK `json:"jwk"`
	}{Typ: "dpop+jwt", Alg: "ES256", JWK: jwk})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	htu := *parsed
	htu.RawQuery = ""
	htu.Fragment = ""
	now := time.Now()
	claims := map[string]any{
		"jti": base64.RawURLEncoding.EncodeToString(nonce),
		"htm": strings.ToUpper(method),
		"htu": htu.String(),
		"iat": now.Unix(),
		"exp": now.Add(dpopProofLifetime).Unix(),
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := s.key.HashAndSign([]byte(signingInput))
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
