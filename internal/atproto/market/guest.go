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
