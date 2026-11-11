package market

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const ContractAssociationLifetime = 24 * time.Hour

const RenterRotationKeyFile = "plc-rotation.key"

func GrantRecord(agent string, createdAt any) map[string]any {
	return map[string]any{"$type": AssociationNSID, "agent": agent, "createdAt": createdAt}
}

func ContractGrantRecord(agent string, contract, expiresAt, createdAt any) map[string]any {
	grant := GrantRecord(agent, createdAt)
	grant["contract"], grant["expiresAt"] = contract, expiresAt
	return grant
}

func EndorsementRecord(agent string) map[string]any {
	return map[string]any{"$type": EndorsementNSID, "agent": agent}
}

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
		ComputeEventsGuestAgentStartNSID, ComputeEventsGuestSecretsUpdatedNSID, ComputeEventsGuestR2StartNSID,
		ComputeEventsGuestR2ReleaseNSID:
		return true
	}
	return false
}

func IsGuestOriginatedEventNSID(nsid string) bool {
	switch nsid {
	case ComputeEventsGuestPublicKeyNSID, ComputeEventsGuestSignalWebRTCNSID, ComputeEventsGuestSecretsAppliedNSID,
		ComputeEventsGuestSecretsRequestedNSID, ComputeEventsGuestRequesterDIDNSID, ComputeEventsGuestR2ReleasedNSID:
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

var guestRequesterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidGuestRequesterName(name string) error {
	if !guestRequesterNameRE.MatchString(name) {
		return fmt.Errorf("market: the guest requester name %q is not a DNS label", name)
	}
	return nil
}
