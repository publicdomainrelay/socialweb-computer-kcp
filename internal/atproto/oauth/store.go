package oauth

import (
	"context"
	"errors"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

type Account struct {
	DID    syntax.DID
	Handle string
}

type Session struct {
	ID               string
	Account          Account
	ClientID         string
	ClientAuthMethod string
	ClientKeyJKT     string
	Scope            string
	JKT              string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

type AuthorizationCode struct {
	ID                  string
	SessionID           string
	ClientID            string
	ClientAuthMethod    string
	ClientKeyJKT        string
	Account             Account
	Scope               string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	UsedAt              *time.Time
}

type PARRequest struct {
	RequestURI          string
	ClientID            string
	ClientAuthMethod    string
	ClientKeyJKT        string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	ResponseType        string
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

type TokenKind string

const (
	TokenAccess  TokenKind = "access"
	TokenRefresh TokenKind = "refresh"
)

type TokenRecord struct {
	JTI       string
	SessionID string
	Kind      TokenKind
	IssuedAt  time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type AccountStore interface {
	Authenticate(ctx context.Context, identifier, password string) (Account, error)
}

type SessionStore interface {
	CreateSession(ctx context.Context, session Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	ExtendSession(ctx context.Context, id string, expiresAt time.Time) error
	RevokeSession(ctx context.Context, id string, at time.Time) error
}

type AuthorizationCodeStore interface {
	CreateCode(ctx context.Context, code AuthorizationCode) error
	ConsumeCode(ctx context.Context, id string, at time.Time) (*AuthorizationCode, error)
}

type PARStore interface {
	StorePAR(ctx context.Context, request PARRequest) error
	ConsumePAR(ctx context.Context, requestURI string, at time.Time) (*PARRequest, error)
}

type TokenStore interface {
	CreateToken(ctx context.Context, token TokenRecord) error
	GetToken(ctx context.Context, jti string) (*TokenRecord, error)
	RevokeToken(ctx context.Context, jti string, at time.Time) error
	RevokeSessionTokens(ctx context.Context, sessionID string, at time.Time) error
	RotateRefreshToken(ctx context.Context, oldJTI string, now time.Time, newTokens []TokenRecord) error
}

type NonceStore interface {
	IssueNonce(ctx context.Context, origin string) (string, error)
	VerifyNonce(ctx context.Context, origin, nonce string) (bool, error)
	RecordJTI(ctx context.Context, jti string, expiresAt time.Time) (bool, error)
}

var (
	ErrAccountNotFound = errors.New("oauth: account not found")

	ErrInvalidCredentials = errors.New("oauth: invalid credentials")

	ErrSessionNotFound = errors.New("oauth: session not found")

	ErrSessionRevoked = errors.New("oauth: session revoked")

	ErrCodeNotFound = errors.New("oauth: authorization code not found")

	ErrCodeExpired = errors.New("oauth: authorization code expired")

	ErrCodeUsed = errors.New("oauth: authorization code already used")

	ErrPARNotFound = errors.New("oauth: pushed authorization request not found")

	ErrPARExpired = errors.New("oauth: pushed authorization request expired")

	ErrTokenNotFound = errors.New("oauth: token not found")

	ErrTokenRevoked = errors.New("oauth: token revoked")

	ErrTokenReused = errors.New("oauth: refresh token already used")

	ErrInvalidDPoPProof = errors.New("oauth: invalid DPoP proof")

	ErrUseDPoPNonce = errors.New("oauth: DPoP nonce required")

	ErrInvalidToken = errors.New("oauth: invalid token")

	ErrInvalidGrant = errors.New("oauth: invalid grant")

	ErrInvalidClient = errors.New("oauth: invalid client")

	ErrInvalidRequest = errors.New("oauth: invalid request")

	ErrInvalidScope = errors.New("oauth: invalid scope")

	ErrUnsupportedGrantType = errors.New("oauth: unsupported grant type")

	ErrUnsupportedResponseType = errors.New("oauth: unsupported response type")

	ErrInvalidClientMetadata = errors.New("oauth: invalid client metadata")

	ErrInvalidRedirectURI = errors.New("oauth: invalid redirect URI")

	ErrServerError = errors.New("oauth: server error")
)
