package guestagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/pion/webrtc/v4"
)

const (
	sessionConnectTimeout = 2 * time.Minute

	gatherTimeout = 20 * time.Second

	maxSessions = 64

	maxCiphertext = 16384
)

type offerPlaintext struct {
	SDP string `json:"sdp"`

	ReplyTo string `json:"replyTo"`
}

type answerPlaintext struct {
	SDP string `json:"sdp"`
}

type session struct {
	id string

	tag string

	port int

	pc *webrtc.PeerConnection

	log *slog.Logger

	once sync.Once

	connected chan struct{}

	connectedOnce sync.Once

	gc *time.Timer

	drop func()
}

func (a *Agent) offer(seq int64, tag, ciphertext string) {
	if ciphertext == "" || len(ciphertext) > maxCiphertext {
		a.log.Debug("a signal ciphertext is empty or too large", "seq", seq, "tag", tag, "bytes", len(ciphertext))
		return
	}
	plaintext, err := open(a.identity, ciphertext)
	if err != nil {
		a.log.Debug("a signal does not open with this guest's identity", "seq", seq, "tag", tag)
		return
	}
	var offer offerPlaintext
	if err := json.Unmarshal(plaintext, &offer); err != nil || offer.SDP == "" {
		a.log.Warn("an offer opened but is not {sdp, replyTo}", "seq", seq, "tag", tag, "bytes", len(plaintext))
		return
	}
	replyTo, err := age.ParseX25519Recipient(strings.TrimSpace(offer.ReplyTo))
	if err != nil {
		a.log.Warn("an offer names no age X25519 replyTo", "seq", seq, "tag", tag, "bytes", len(offer.ReplyTo))
		return
	}
	port, _ := a.cfg.Port(tag)
	s, err := a.newSession(tag, port)
	if err != nil {
		a.log.Warn("no session for an offer", "seq", seq, "tag", tag, "err", err.Error())
		return
	}
	s.log.Info("an offer opened; answering", "seq", seq, "offerBytes", len(offer.SDP))
	a.wg.Go(func() { a.answer(s, offer.SDP, replyTo) })
}

func (a *Agent) newSession(tag string, port int) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("the agent is stopping")
	}
	if len(a.sessions) >= maxSessions {
		return nil, fmt.Errorf("%d sessions are open", len(a.sessions))
	}
	pc, err := a.api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{{URLs: []string{STUNServer}}},
		Certificates: []webrtc.Certificate{a.certificate},
	})
	if err != nil {
		return nil, err
	}
	id := newID()
	s := &session{
		id:        id,
		tag:       tag,
		port:      port,
		pc:        pc,
		log:       a.log.With("session", id, "tag", tag),
		connected: make(chan struct{}),
	}
	s.drop = func() {
		a.mu.Lock()
		delete(a.sessions, id)
		open := len(a.sessions)
		a.mu.Unlock()
		s.log.Info("the session is gone", "open", open)
	}
	a.sessions[id] = s
	s.gc = time.AfterFunc(sessionConnectTimeout, func() {
		select {
		case <-s.connected:
		default:
			s.close("it did not connect within " + sessionConnectTimeout.String())
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.log.Info("the peer connection state changed", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			s.connectedOnce.Do(func() { close(s.connected); s.gc.Stop() })
			s.log.Info("the selected candidate pair", selectedPair(pc)...)
		case webrtc.PeerConnectionStateFailed:
			s.close("the peer connection failed")
		case webrtc.PeerConnectionStateClosed:
			s.close("the peer connection closed")
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { s.onDataChannel(dc) })
	return s, nil
}

func (s *session) close(reason string) {
	s.once.Do(func() {
		s.log.Info("closing the session", "reason", reason)
		s.gc.Stop()
		go func() {
			_ = s.pc.Close()
			s.drop()
		}()
	})
}

func (a *Agent) answer(s *session, offerSDP string, replyTo *age.X25519Recipient) {
	ctx, cancel := context.WithTimeout(a.ctx, sessionConnectTimeout)
	defer cancel()
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		s.close("the offer is not a session description: " + err.Error())
		return
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		s.close("creating the answer failed: " + err.Error())
		return
	}
	gathered := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		s.close("setting the answer failed: " + err.Error())
		return
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		s.log.Warn("ICE gathering did not complete; answering with the candidates gathered", "after", gatherTimeout.String())
	case <-ctx.Done():
		s.close("the agent is stopping")
		return
	}
	local := s.pc.LocalDescription()
	if local == nil {
		s.close("no local description")
		return
	}
	plaintext, err := json.Marshal(answerPlaintext{SDP: local.SDP})
	if err != nil {
		s.close(err.Error())
		return
	}
	ciphertext, err := seal(replyTo, plaintext)
	if err != nil {
		s.close("sealing the answer failed: " + err.Error())
		return
	}
	if len(ciphertext) > maxCiphertext {
		s.close(fmt.Sprintf("the sealed answer is %d bytes, over %d", len(ciphertext), maxCiphertext))
		return
	}
	s.log.Info("the answer is sealed", append([]any{"answerBytes", len(local.SDP), "ciphertextBytes", len(ciphertext)}, candidateTypes(local.SDP)...)...)
	record := signalRecord{
		Type:       SignalWebRTCNSID,
		Kind:       signalKindAnswer,
		Tag:        s.tag,
		Ciphertext: ciphertext,
		CreatedAt:  createdAt(),
	}
	if err := a.poster.Post(ctx, record.Type, s.tag, record); err != nil {
		s.close("posting the answer failed: " + err.Error())
	}
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func selectedPair(pc *webrtc.PeerConnection) []any {
	sctp := pc.SCTP()
	if sctp == nil {
		return []any{"local", "unknown", "remote", "unknown"}
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return []any{"local", "unknown", "remote", "unknown"}
	}
	return []any{"local", pair.Local.Typ.String(), "remote", pair.Remote.Typ.String(), "protocol", pair.Local.Protocol.String()}
}

func candidateTypes(sdp string) []any {
	counts := map[string]int{}
	for _, line := range strings.Split(sdp, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "typ" {
				counts[fields[i+1]]++
				break
			}
		}
	}
	return []any{"hostCandidates", counts["host"], "srflxCandidates", counts["srflx"]}
}
