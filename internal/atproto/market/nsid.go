package market

import "time"

//go:generate go run ../../lexgen -err ErrInvalidRecord -include com.publicdomainrelay.temp.market.,com.publicdomainrelay.temp.ingress.,com.publicdomainrelay.temp.compute.,com.publicdomainrelay.temp.badgeBlueKeys,network.attested.signature,com.atproto.repo.strongRef -trim com.publicdomainrelay.temp.market.,com.publicdomainrelay.temp.,com.atproto.repo.,network. -name network.attested.signature#signatures=Signatures,com.publicdomainrelay.temp.compute.events.vm.hostKeys#hostKey=HostKey

const (
	StrongRefNSID = "com.atproto.repo.strongRef"

	RFPNSID         = "com.publicdomainrelay.temp.market.rfp"
	BidNSID         = "com.publicdomainrelay.temp.market.bid"
	AcceptNSID      = "com.publicdomainrelay.temp.market.accept"
	ReceiptNSID     = "com.publicdomainrelay.temp.market.receipt"
	EventNSID       = "com.publicdomainrelay.temp.market.event"
	OfferingNSID    = "com.publicdomainrelay.temp.market.offering"
	HeartbeatNSID   = "com.publicdomainrelay.temp.market.heartbeat"
	AssociationNSID = "com.publicdomainrelay.temp.market.association"

	BidsFreeNSID     = "com.publicdomainrelay.temp.market.bids.free"
	BidsX402NSID     = "com.publicdomainrelay.temp.market.bids.x402"
	AcceptsFreeNSID  = "com.publicdomainrelay.temp.market.accepts.free"
	AcceptsX402NSID  = "com.publicdomainrelay.temp.market.accepts.x402"
	ReceiptsFreeNSID = "com.publicdomainrelay.temp.market.receipts.free"
	ReceiptsX402NSID = "com.publicdomainrelay.temp.market.receipts.x402"

	SubmitRFPNSID    = "com.publicdomainrelay.temp.market.submitRfp"
	SubmitBidNSID    = "com.publicdomainrelay.temp.market.submitBid"
	SubmitAcceptNSID = "com.publicdomainrelay.temp.market.submitAccept"
	SubmitEventNSID  = "com.publicdomainrelay.temp.market.submitEvent"

	SubmitRFPLXM    = SubmitRFPNSID
	SubmitBidLXM    = SubmitBidNSID
	SubmitAcceptLXM = SubmitAcceptNSID
	SubmitEventLXM  = SubmitEventNSID

	ComputeVMNSID                = "com.publicdomainrelay.temp.compute.vm"
	ComputeEventsVMDeleteNSID    = "com.publicdomainrelay.temp.compute.events.vm.delete"
	ComputeEventsVMOnNetworkNSID = "com.publicdomainrelay.temp.compute.events.vm.onNetwork"

	ComputeEventsVMSandboxNSID          = "com.publicdomainrelay.temp.compute.events.vm.sandbox"
	ComputeEventsVMStartedNSID          = "com.publicdomainrelay.temp.compute.events.vm.started"
	ComputeEventsVMTerminatedNSID       = "com.publicdomainrelay.temp.compute.events.vm.terminated"
	ComputeEventsVMHostKeysNSID         = "com.publicdomainrelay.temp.compute.events.vm.hostKeys"
	ComputeEventsVMRegisterIdentityNSID = "com.publicdomainrelay.temp.compute.events.vm.registerIdentity"

	GuestEventNSIDPrefix                   = "com.publicdomainrelay.temp.compute.events.guest."
	ComputeEventsGuestPublicKeyNSID        = GuestEventNSIDPrefix + "publicKey"
	ComputeEventsGuestSignalWebRTCNSID     = GuestEventNSIDPrefix + "signalWebRTC"
	ComputeEventsGuestSecretsNSID          = GuestEventNSIDPrefix + "secrets"
	ComputeEventsGuestSecretsAppliedNSID   = GuestEventNSIDPrefix + "secretsApplied"
	ComputeEventsGuestSecretsRequestedNSID = GuestEventNSIDPrefix + "secretsRequested"
	ComputeEventsGuestAgentStartNSID       = GuestEventNSIDPrefix + "agentStart"
	ComputeGuestSubscribeEventsNSID        = "com.publicdomainrelay.temp.compute.guest.subscribeEvents"

	IngressWebRTCNSID = "com.publicdomainrelay.temp.ingress.webRTC"

	IngressModeListen = "listen"
	IngressModeOffer  = "offer"
	IngressModeAnswer = "answer"

	BadgeBlueKeysNSID = "com.publicdomainrelay.temp.badgeBlueKeys"
	VouchNSID         = "sh.tangled.graph.vouch"
	RelaysNSID        = "com.publicdomainrelay.temp.market.relays"

	AttestedSignatureNSID = "network.attested.signature"
	AttestedProofNSID     = "network.attested.proof"
	AttestedVerifyNSID    = "network.attested.verify"

	DefaultMarketServiceID       = "pdr_temp_market"
	DefaultComputeEventServiceID = "pdr_temp_compute_event"

	HeartbeatRKey = "self"
)

const (
	HeartbeatInterval = 60 * time.Second
	HeartbeatTTL      = 300 * time.Second
)

var VMTerminatedReasons = []string{
	"guest_poweroff",
	"vmm_exited",
	"oom",
	"host_reboot",
	"identity_mismatch",
	"deleted_on_request",
}

var VMHostKeyTypes = []string{
	"ssh-ed25519",
	"ecdsa-sha2-nistp256",
	"ssh-rsa",
}
