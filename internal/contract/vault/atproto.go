package vault

import (
	"context"
	"errors"
	"time"
)

const (
	CapabilityAtprotoSession = "vault.atproto.session"

	AtprotoSessionClient = "atproto-session-proxy"

	AtprotoSessionActive = "active"
)

var (
	ErrNoAtprotoSession = errors.New("vault: the atproto auth mount holds no session for the DID")

	ErrAtprotoSessionInactive = errors.New("vault: the atproto session is not active")
)

type AtprotoSession struct {
	DID string

	Handle string

	PDS string

	Scope string

	Status string

	StatusReason string

	ExpiresAt time.Time
}

type AtprotoProof struct {
	AccessToken string

	DPoP string
}

type AtprotoSessions interface {
	Session(ctx context.Context, mount, path string) (AtprotoSession, error)

	Proof(ctx context.Context, mount, path, method, target, nonce string) (AtprotoProof, error)
}
