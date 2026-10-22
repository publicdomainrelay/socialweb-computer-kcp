package guestcontrolplane

import (
	"crypto/ecdsa"

	"github.com/golang-jwt/jwt/v5"
)

type es256Compat struct {
	standard *jwt.SigningMethodECDSA

	atproto jwt.SigningMethod
}

func (m es256Compat) Verify(signingString string, signature []byte, key any) error {
	if _, ok := key.(*ecdsa.PublicKey); ok {
		return m.standard.Verify(signingString, signature, key)
	}
	if m.atproto != nil {
		return m.atproto.Verify(signingString, signature, key)
	}
	return jwt.ErrInvalidKeyType
}

func (m es256Compat) Sign(signingString string, key any) ([]byte, error) {
	if _, ok := key.(*ecdsa.PrivateKey); ok {
		return m.standard.Sign(signingString, key)
	}
	if m.atproto != nil {
		return m.atproto.Sign(signingString, key)
	}
	return nil, jwt.ErrInvalidKeyType
}

func (m es256Compat) Alg() string { return "ES256" }

func init() {
	current := jwt.GetSigningMethod("ES256")
	standard := jwt.SigningMethodES256
	if current == nil || current == standard {
		current = nil
	}
	jwt.RegisterSigningMethod("ES256", func() jwt.SigningMethod {
		return es256Compat{standard: standard, atproto: current}
	})
}
