package oauthclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

const (
	RefreshErrorInvalidGrant = "invalid_grant"

	RefreshErrorPassThrough = "pass_through"
)

var (
	ErrRefreshPassThrough = errors.New("oauthclient: the token refresher does not refresh this client")

	ErrRefreshWrongSubject = errors.New("oauthclient: the token refresher returned a token set for another account")
)

type TokenRefresher interface {
	RefreshTokens(ctx context.Context, req TokenRefreshRequest) (TokenRefreshResponse, error)
}

type TokenRefresherFunc func(ctx context.Context, req TokenRefreshRequest) (TokenRefreshResponse, error)

func (f TokenRefresherFunc) RefreshTokens(ctx context.Context, req TokenRefreshRequest) (TokenRefreshResponse, error) {
	return f(ctx, req)
}

type DPoPPrivateJWK struct {
	KeyType string `json:"kty"`

	Curve string `json:"crv"`

	X string `json:"x"`

	Y string `json:"y"`

	D string `json:"d"`
}

type TokenRefreshRequest struct {
	SessionID string `json:"session_id"`

	AccountDID string `json:"did"`

	Issuer string `json:"issuer"`

	TokenEndpoint string `json:"token_endpoint"`

	ClientID string `json:"client_id"`

	RefreshToken string `json:"refresh_token"`

	DPoPKey DPoPPrivateJWK `json:"dpop_jwk"`

	DPoPNonce string `json:"dpop_nonce,omitempty"`
}

type TokenRefreshResponse struct {
	AccessToken string `json:"access_token"`

	RefreshToken string `json:"refresh_token"`

	TokenType string `json:"token_type"`

	Scope string `json:"scope"`

	Sub string `json:"sub"`

	ExpiresIn int64 `json:"expires_in,omitempty"`

	ExpiresAt int64 `json:"expires_at,omitempty"`

	DPoPNonce string `json:"dpop_nonce,omitempty"`
}

type TokenRefreshError struct {
	Code string `json:"error"`

	Description string `json:"error_description,omitempty"`

	Status int `json:"status,omitempty"`
}

func (e *TokenRefreshError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("oauthclient: the token refresher answered %s (HTTP %d): %s", e.Code, e.Status, e.Description)
	}
	return fmt.Sprintf("oauthclient: the token refresher answered %s: %s", e.Code, e.Description)
}

func (e *TokenRefreshError) Is(target error) bool {
	switch target {
	case ErrSessionExpired:
		return e.Code == RefreshErrorInvalidGrant
	case ErrRefreshPassThrough:
		return e.Code == RefreshErrorPassThrough
	}
	return false
}

type refresherSlot struct {
	refresher TokenRefresher
}

var defaultRefresher atomic.Pointer[refresherSlot]

func (s *Session) tokenRefresher() TokenRefresher {
	if s.refresher != nil {
		return s.refresher
	}
	if slot := defaultRefresher.Load(); slot != nil {
		return slot.refresher
	}
	return nil
}

func (s *Session) refreshVia(base context.Context, refresher TokenRefresher, data SessionData) (SessionData, error) {
	key, err := parseDPoPKey(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return SessionData{}, err
	}
	jwk, err := dpopPrivateJWK(key)
	if err != nil {
		return SessionData{}, err
	}
	ctx, cancel := context.WithTimeout(base, RefreshTimeout)
	defer cancel()
	out, err := refresher.RefreshTokens(ctx, TokenRefreshRequest{
		SessionID:     data.SessionID,
		AccountDID:    s.did,
		Issuer:        data.AuthServerURL,
		TokenEndpoint: data.AuthServerTokenEndpoint,
		ClientID:      data.ClientID,
		RefreshToken:  data.RefreshToken,
		DPoPKey:       jwk,
		DPoPNonce:     data.DPoPAuthServerNonce,
	})
	if err != nil {
		return SessionData{}, err
	}
	return s.acceptRefreshed(data, out)
}

func (s *Session) acceptRefreshed(data SessionData, out TokenRefreshResponse) (SessionData, error) {
	if out.AccessToken == "" || out.RefreshToken == "" {
		return SessionData{}, errors.New("oauthclient: the token refresher returned no access or refresh token")
	}
	if !strings.EqualFold(out.TokenType, "DPoP") {
		return SessionData{}, fmt.Errorf("oauthclient: the token refresher returned token_type %q, not DPoP", out.TokenType)
	}
	if out.Sub != s.did {
		return SessionData{}, fmt.Errorf("%w: sub %q, session %s", ErrRefreshWrongSubject, out.Sub, s.did)
	}
	granted := strings.Fields(out.Scope)
	if !slices.Contains(granted, atprotooauth.ScopeAtproto) {
		return SessionData{}, fmt.Errorf("%w: the token refresher: %w (granted %q)", ErrSessionExpired, ErrScopeRejected, out.Scope)
	}
	data.Scopes = granted
	data.AccessToken = out.AccessToken
	data.RefreshToken = out.RefreshToken
	if out.DPoPNonce != "" {
		data.DPoPAuthServerNonce = out.DPoPNonce
	}
	return data, nil
}

func dpopPrivateJWK(key atcrypto.PrivateKeyExportable) (DPoPPrivateJWK, error) {
	p256, ok := key.(*atcrypto.PrivateKeyP256)
	if !ok {
		return DPoPPrivateJWK{}, fmt.Errorf("oauthclient: the oauth session DPoP key type %T is not P-256", key)
	}
	public, err := p256.PublicKey()
	if err != nil {
		return DPoPPrivateJWK{}, err
	}
	jwk, err := public.JWK()
	if err != nil {
		return DPoPPrivateJWK{}, err
	}
	return DPoPPrivateJWK{
		KeyType: jwk.KeyType,
		Curve:   jwk.Curve,
		X:       jwk.X,
		Y:       jwk.Y,
		D:       base64.RawURLEncoding.EncodeToString(p256.Bytes()),
	}, nil
}
