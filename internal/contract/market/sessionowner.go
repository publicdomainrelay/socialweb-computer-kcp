package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const SessionOwnerLabel = "apps.socialweb.computer/session-owner"

const (
	SessionOwnerIdentityPrefix = "owner"

	SessionOwnerRequesterPrefix = "requester"
)

type SessionOwnerIntents interface {
	EnsureMarketIdentity(ctx context.Context, identity v1alpha1.MarketIdentity) error
}

func SessionOwnerName(prefix, did string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(did)))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}
