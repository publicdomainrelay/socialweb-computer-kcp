package market

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

type secp256k1JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	D   string `json:"d"`
}

func ParseSecp256k1JWK(data []byte) ([]byte, error) {
	var jwk secp256k1JWK
	if err := json.Unmarshal(data, &jwk); err != nil {
		return nil, fmt.Errorf("market: secp256k1 jwk: %w", err)
	}
	if jwk.Kty != "EC" || jwk.Crv != "secp256k1" || jwk.D == "" {
		return nil, fmt.Errorf("market: not a secp256k1 private JWK")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(jwk.D, "="))
	if err != nil {
		return nil, fmt.Errorf("market: secp256k1 jwk d: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("market: secp256k1 jwk d is %d bytes, want 32", len(raw))
	}
	return raw, nil
}

func LoadOrCreateAttestationKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return ParseSecp256k1JWK(data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, err
	}
	jwk := secp256k1JWK{
		Kty: "EC",
		Crv: "secp256k1",
		Use: "sig",
		Alg: "ES256K",
		D:   base64.RawURLEncoding.EncodeToString(priv),
	}
	out, err := json.MarshalIndent(jwk, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return priv, nil
}

func LoadOrCreateSignerFile(path string) (*Signer, error) {
	raw, err := LoadOrCreateAttestationKey(path)
	if err != nil {
		return nil, err
	}
	priv, err := atcrypto.ParsePrivateBytesK256(raw)
	if err != nil {
		return nil, err
	}
	return &Signer{PrivateKey: priv}, nil
}
