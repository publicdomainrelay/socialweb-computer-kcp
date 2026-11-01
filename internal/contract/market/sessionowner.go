package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	CapabilitySessionOwners = "market.sessionowners"

	SessionOwnerLabel = "apps.socialweb.computer/session-owner"

	SessionOwnerIdentityPrefix = "owner"

	SessionOwnerRequesterPrefix = "requester"

	DefaultMaxSessionOwners = 64
)

var ErrSessionOwnersFull = errors.New("market: every signed-in account slot of this ssh gateway is taken")

type SessionOwnerIntents interface {
	EnsureSessionOwner(ctx context.Context, did string) error

	DeleteSessionOwner(ctx context.Context, did string) error
}

func SessionOwnerName(prefix, did string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(did)))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}
