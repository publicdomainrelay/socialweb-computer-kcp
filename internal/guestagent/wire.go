package guestagent

const (
	nsidPrefix = "com.publicdomainrelay.temp.compute."

	OnNetworkNSID = nsidPrefix + "events.vm.onNetwork"

	PublicKeyNSID = nsidPrefix + "events.guest.publicKey"

	SignalWebRTCNSID = nsidPrefix + "events.guest.signalWebRTC"

	SecretsNSID = nsidPrefix + "events.guest.secrets"

	SecretsAppliedNSID = nsidPrefix + "events.guest.secretsApplied"

	SecretsRequestedNSID = nsidPrefix + "events.guest.secretsRequested"

	AgentStartNSID = nsidPrefix + "events.guest.agentStart"

	secretsViaGetSecrets = "getSecrets"

	secretsViaSealed = "sealed"

	SubscribeEventsNSID = nsidPrefix + "guest.subscribeEvents"

	SubscribeEventsPath = "/xrpc/" + SubscribeEventsNSID

	EventsPath = "/v1/events"

	errFutureCursorName = "FutureCursor"

	publicKeyKindAgeX25519 = "age-x25519"

	onNetworkKindWebRTC = "webrtc"

	signalKindOffer = "offer"

	signalKindAnswer = "answer"
)
