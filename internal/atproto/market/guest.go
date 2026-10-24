package market

import "strings"

const (
	OnNetworkKindIroh   = "iroh"
	OnNetworkKindHTTPS  = "https"
	OnNetworkKindWebRTC = "webrtc"

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
	case ComputeEventsGuestPublicKeyNSID, ComputeEventsGuestSignalWebRTCNSID, ComputeEventsGuestSecretsNSID:
		return true
	}
	return false
}

func IsGuestOriginatedEventNSID(nsid string) bool {
	switch nsid {
	case ComputeEventsGuestPublicKeyNSID, ComputeEventsGuestSignalWebRTCNSID, ComputeEventsGuestSecretsAppliedNSID,
		ComputeEventsGuestSecretsRequestedNSID:
		return true
	}
	return false
}
