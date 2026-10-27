package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"time"
)

const (
	KindWebRTC = "webrtc"

	SignalOffer  = "offer"
	SignalAnswer = "answer"

	NetworkWebRTC = "webrtc"

	DefaultSTUN = "stun:stun.cloudflare.com:3478"
)

type Offer struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Session string `json:"session"`
	Peer    string `json:"peer"`
	Type    string `json:"type"`
	SDP     string `json:"sdp"`
}

type Answer struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Session string `json:"session"`
	Peer    string `json:"peer"`
	Type    string `json:"type"`
	SDP     string `json:"sdp"`
}

type ServiceOptions struct {
	STUN []string

	SessionTTL time.Duration

	MaxSessions int
}

type Service interface {
	Offer(ctx context.Context, owner, peer string) (Offer, error)

	Accept(ctx context.Context, answer Answer) error

	Close() error
}

type Answerer interface {
	Answer(ctx context.Context, offer Offer) (Answer, error)
}

type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

func ID(owner string) string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("socialweb.computer/ingress/" + owner))
	return hex.EncodeToString(sum[:16])
}

func Ref(id string) string {
	if id == "" {
		return ""
	}
	return KindWebRTC + "://" + id
}

func ParseRef(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), KindWebRTC+"://")
	if !ok || rest == "" || strings.ContainsAny(rest, "/?#@ \t\r\n") {
		return "", false
	}
	return rest, true
}

func IsRef(raw string) bool {
	_, ok := ParseRef(raw)
	return ok
}

func NewSession() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func ValidOffer(offer Offer) bool {
	return offer.Kind == KindWebRTC && offer.ID != "" && offer.Session != "" && offer.Peer != "" && offer.Type == SignalOffer && offer.SDP != ""
}

func ValidAnswer(answer Answer) bool {
	return answer.Kind == KindWebRTC && answer.ID != "" && answer.Session != "" && answer.Peer != "" && answer.Type == SignalAnswer && answer.SDP != ""
}
