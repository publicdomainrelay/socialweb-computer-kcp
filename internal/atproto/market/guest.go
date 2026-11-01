package market

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	OnNetworkKindIroh   = "iroh"
	OnNetworkKindHTTPS  = "https"
	OnNetworkKindWebRTC = "webrtc"

	OnNetworkKindSandbox = "sandbox"

	GuestPublicKeyKindAgeX25519 = "age-x25519"

	GuestSignalKindOffer  = "offer"
	GuestSignalKindAnswer = "answer"

	GuestSubscribeEventsErrFutureCursor = "FutureCursor"

	GuestSecretsTag = "secrets"

	MaxGuestSecretsCiphertext = 65536

	GuestSecretsViaGetSecrets = "getSecrets"
	GuestSecretsViaSealed     = "sealed"
)

func OnNetworkKind(r ComputeEventsVMOnNetwork) string {
	switch {
	case r.Kind != "":
		return r.Kind
	case r.Tag != "":
		return OnNetworkKindHTTPS
	default:
		return OnNetworkKindIroh
	}
}

func IsGuestEventNSID(nsid string) bool {
	return strings.HasPrefix(nsid, GuestEventNSIDPrefix) && len(nsid) > len(GuestEventNSIDPrefix)
}

func IsRequesterGuestEventNSID(nsid string) bool {
	switch nsid {
	case ComputeEventsGuestPublicKeyNSID, ComputeEventsGuestSignalWebRTCNSID, ComputeEventsGuestSecretsNSID,
		ComputeEventsGuestAgentStartNSID, ComputeEventsGuestSecretsUpdatedNSID:
		return true
	}
	return false
}

func IsGuestOriginatedEventNSID(nsid string) bool {
	switch nsid {
	case ComputeEventsGuestPublicKeyNSID, ComputeEventsGuestSignalWebRTCNSID, ComputeEventsGuestSecretsAppliedNSID,
		ComputeEventsGuestSecretsRequestedNSID, ComputeEventsGuestRequesterDIDNSID:
		return true
	}
	return false
}

func ValidGuestRequesterDID(did, authority string) error {
	parsed, err := syntax.ParseDID(strings.TrimSpace(did))
	switch {
	case err != nil || parsed.String() != did:
		return fmt.Errorf("market: the guest requester DID %q is not a DID", did)
	case parsed.Method() != "plc":
		return fmt.Errorf("market: the guest requester DID %s is not a did:plc; a guest-reported did:web would make the authority fetch a host the guest names", did)
	case did == authority:
		return fmt.Errorf("market: the guest requester DID %s is the authority's own DID", did)
	}
	return nil
}

func ContractScopedRKey(service, did, acceptURI string) string {
	sum := sha256.Sum256([]byte(did))
	return service + "-" + hex.EncodeToString(sum[:])[:32] + "-" + acceptURI[strings.LastIndex(acceptURI, "/")+1:]
}
