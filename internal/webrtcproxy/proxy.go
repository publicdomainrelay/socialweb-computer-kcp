package webrtcproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/pion/webrtc/v4"
)

const (
	STUNServer = "stun:stun.cloudflare.com:3478"

	DefaultGuestWait = 3 * time.Minute

	DefaultAnswerWait = 2 * time.Minute

	DefaultOpenTimeout = 60 * time.Second

	gatherTimeout = 20 * time.Second

	stunGatherTimeout = 2 * time.Second

	maxCiphertext = 16384

	fingerprintAttribute = "a=fingerprint:"
)

var ErrFingerprint = errors.New("webrtcproxy: the answer's DTLS fingerprint is not the one the guest reported in onNetwork")

type Guest struct {
	Key string

	Fingerprint string
}

type Signaling interface {
	PublishKey(ctx context.Context, tag, key string) error

	Guest(ctx context.Context, tag string) (Guest, error)

	PublishOffer(ctx context.Context, tag, ciphertext string) error

	Answers(ctx context.Context, tag string, each func(ciphertext string) bool) error
}

type Options struct {
	Tag string

	GuestWait time.Duration

	AnswerWait time.Duration

	OpenTimeout time.Duration

	Log *slog.Logger
}

type Timings struct {
	Guest time.Duration

	Offer time.Duration

	Answer time.Duration

	Open time.Duration
}

type offerPlaintext struct {
	SDP string `json:"sdp"`

	ReplyTo string `json:"replyTo"`
}

type answerPlaintext struct {
	SDP string `json:"sdp"`
}

func Dial(ctx context.Context, sig Signaling, opts Options) (*Conn, Timings, error) {
	var timings Timings
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tag := strings.TrimSpace(opts.Tag)
	if tag == "" {
		return nil, timings, errors.New("webrtcproxy: a tunnel tag is required")
	}
	log = log.With("tag", tag)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, timings, fmt.Errorf("webrtcproxy: generating the age identity: %w", err)
	}
	recipient := identity.Recipient()
	started := time.Now()
	if err := sig.PublishKey(ctx, tag, recipient.String()); err != nil {
		return nil, timings, fmt.Errorf("webrtcproxy: publishing this session's publicKey: %w", err)
	}
	guestCtx, cancel := context.WithTimeout(ctx, durationOr(opts.GuestWait, DefaultGuestWait))
	guest, err := sig.Guest(guestCtx, tag)
	cancel()
	if err != nil {
		return nil, timings, fmt.Errorf("webrtcproxy: waiting for the guest's publicKey and its onNetwork kind webrtc for tag %s: %w", tag, err)
	}
	guestKey, err := age.ParseX25519Recipient(strings.TrimSpace(guest.Key))
	if err != nil {
		return nil, timings, fmt.Errorf("webrtcproxy: the guest's publicKey is not an age X25519 recipient: %w", err)
	}
	expected := normalFingerprint(guest.Fingerprint)
	if expected == "" {
		return nil, timings, errors.New("webrtcproxy: the guest's onNetwork kind webrtc names no fingerprint")
	}
	timings.Guest = time.Since(started)
	log.Info("the guest's publicKey and DTLS fingerprint are known", "fingerprint", expected, "after", timings.Guest.String())
	c, err := newConn(tag, log)
	if err != nil {
		return nil, timings, err
	}
	offerAt := time.Now()
	ciphertext, err := c.offer(ctx, recipient.String(), guestKey)
	if err != nil {
		c.Close()
		return nil, timings, err
	}
	if err := sig.PublishOffer(ctx, tag, ciphertext); err != nil {
		c.Close()
		return nil, timings, fmt.Errorf("webrtcproxy: publishing the offer: %w", err)
	}
	timings.Offer = time.Since(offerAt)
	log.Info("the sealed offer is published", "ciphertextBytes", len(ciphertext), "after", timings.Offer.String())
	answerCtx, cancel := context.WithTimeout(ctx, durationOr(opts.AnswerWait, DefaultAnswerWait))
	answer := ""
	err = sig.Answers(answerCtx, tag, func(ciphertext string) bool {
		plaintext, err := open(identity, ciphertext)
		if err != nil {
			return false
		}
		var body answerPlaintext
		if err := json.Unmarshal(plaintext, &body); err != nil || body.SDP == "" {
			log.Warn("an answer opened with this session's identity but is not {sdp}", "bytes", len(plaintext))
			return false
		}
		answer = body.SDP
		return true
	})
	cancel()
	if answer == "" {
		c.Close()
		if err == nil {
			err = context.DeadlineExceeded
		}
		return nil, timings, fmt.Errorf("webrtcproxy: no answer sealed to this session arrived: %w", err)
	}
	timings.Answer = time.Since(offerAt)
	if err := checkFingerprints(answer, expected); err != nil {
		c.Close()
		return nil, timings, err
	}
	log.Info("the answer opened and its fingerprint matches onNetwork", "after", timings.Answer.String())
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		c.Close()
		return nil, timings, fmt.Errorf("webrtcproxy: the answer is not a session description: %w", err)
	}
	if err := c.waitOpen(ctx, durationOr(opts.OpenTimeout, DefaultOpenTimeout)); err != nil {
		c.Close()
		return nil, timings, err
	}
	timings.Open = time.Since(offerAt)
	log.Info("the datachannel is open", "offerToOpen", timings.Open.String(), "total", time.Since(started).String())
	return c, timings, nil
}

func durationOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func normalFingerprint(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func checkFingerprints(sdp, expected string) error {
	found := 0
	for line := range strings.SplitSeq(sdp, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, fingerprintAttribute)
		if !ok {
			continue
		}
		found++
		if got := normalFingerprint(value); !strings.EqualFold(got, expected) {
			return fmt.Errorf("%w: the answer has %q, onNetwork has %q", ErrFingerprint, got, expected)
		}
	}
	if found == 0 {
		return fmt.Errorf("%w: the answer has no a=fingerprint line", ErrFingerprint)
	}
	return nil
}

func newAPI(log *slog.Logger) *webrtc.API {
	settings := webrtc.SettingEngine{LoggerFactory: pionLogs{log: log}}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	settings.SetSTUNGatherTimeout(stunGatherTimeout)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

func (c *Conn) offer(ctx context.Context, replyTo string, guestKey *age.X25519Recipient) (string, error) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("webrtcproxy: creating the offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(c.pc)
	if err := c.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("webrtcproxy: setting the offer: %w", err)
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		c.log.Warn("ICE gathering did not complete; offering the candidates gathered", "after", gatherTimeout.String())
	case <-ctx.Done():
		return "", ctx.Err()
	}
	local := c.pc.LocalDescription()
	if local == nil {
		return "", errors.New("webrtcproxy: no local description after gathering")
	}
	plaintext, err := json.Marshal(offerPlaintext{SDP: local.SDP, ReplyTo: replyTo})
	if err != nil {
		return "", err
	}
	ciphertext, err := seal(guestKey, plaintext)
	if err != nil {
		return "", fmt.Errorf("webrtcproxy: sealing the offer: %w", err)
	}
	if len(ciphertext) > maxCiphertext {
		return "", fmt.Errorf("webrtcproxy: the sealed offer is %d bytes, over the %d of a signalWebRTC record (%d candidate bytes; too many local interfaces)",
			len(ciphertext), maxCiphertext, len(local.SDP))
	}
	return ciphertext, nil
}
