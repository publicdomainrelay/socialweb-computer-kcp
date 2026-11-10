package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/pion/webrtc/v4"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/channelconn"
)

const (
	MeshLabel = "market"

	MeshAnswerTimeout = 45 * time.Second

	MeshAnswerPoll = time.Second

	fingerprintAttribute = "a=fingerprint:"
)

var ErrMeshFingerprint = errors.New("webrtc mesh: the answer's DTLS fingerprint is not the listen record's")

type MeshListen struct {
	Key string

	Fingerprint string
}

type MeshSignals interface {
	Listen(ctx context.Context, address string) (MeshListen, error)

	Offer(ctx context.Context, address, ciphertext string) (string, error)

	Answer(ctx context.Context, address, session string) (string, error)

	Withdraw(ctx context.Context, address, session string) error
}

type MeshDialer struct {
	signals MeshSignals

	api *webrtc.API

	log *slog.Logger

	mu sync.Mutex

	// ponytail: one PeerConnection per answerer for the dialer's lifetime (until it fails);
	// close idle ones if a long-lived requester talks to many bidders.
	peers map[string]*meshPeer
}

type meshPeer struct {
	ready chan struct{}

	dead chan struct{}

	once sync.Once

	pc *webrtc.PeerConnection

	first *meshChannel

	err error
}

type meshChannel struct {
	conn *channelconn.Conn

	opened chan struct{}
}

func NewMeshDialer(signals MeshSignals, log *slog.Logger) *MeshDialer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &MeshDialer{signals: signals, api: newAPI(), log: log, peers: map[string]*meshPeer{}}
}

func MeshClient(base *http.Client, dialer *MeshDialer) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	next := base.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	channels := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, "webrtc", host)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     time.Minute,
	}
	out := *base
	out.Transport = &router{next: next, webrtc: meshTransport{channels}}
	return &out
}

type meshTransport struct {
	channels http.RoundTripper
}

func (t meshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	return t.channels.RoundTrip(out)
}

func (d *MeshDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	peer := d.peer(address)
	select {
	case <-peer.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if peer.err != nil {
		return nil, peer.err
	}
	d.mu.Lock()
	channel := peer.first
	peer.first = nil
	d.mu.Unlock()
	var err error
	if channel == nil {
		if channel, err = newMeshChannel(peer.pc); err != nil {
			return nil, err
		}
	}
	timer := time.NewTimer(OpenTimeout)
	defer timer.Stop()
	select {
	case <-channel.opened:
		return channel.conn, nil
	case <-peer.dead:
		err = errors.New("webrtc mesh: the peer connection closed before the datachannel opened")
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = fmt.Errorf("webrtc mesh: the datachannel did not open within %s", OpenTimeout)
	}
	_ = channel.conn.Close()
	return nil, err
}

func (d *MeshDialer) peer(address string) *meshPeer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current := d.peers[address]; current != nil {
		select {
		case <-current.dead:
		default:
			return current
		}
	}
	current := &meshPeer{ready: make(chan struct{}), dead: make(chan struct{})}
	d.peers[address] = current
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), MeshConnectTimeout)
		defer cancel()
		current.err = d.connect(ctx, address, current)
		if current.err != nil {
			d.log.Warn("webrtc_mesh_dial_failed", "address", address, "err", current.err.Error())
			current.kill()
		}
		close(current.ready)
	}()
	return current
}

func (p *meshPeer) kill() {
	p.once.Do(func() {
		close(p.dead)
		if p.pc != nil {
			_ = p.pc.Close()
		}
	})
}

func (d *MeshDialer) connect(ctx context.Context, address string, peer *meshPeer) error {
	started := time.Now()
	listen, err := d.signals.Listen(ctx, address)
	if err != nil {
		return fmt.Errorf("webrtc mesh: the listen record: %w", err)
	}
	recipient, err := age.ParseX25519Recipient(strings.TrimSpace(listen.Key))
	if err != nil {
		return errors.New("webrtc mesh: the listen record's key is not an age X25519 recipient")
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	pc, err := d.api.NewPeerConnection(configuration())
	if err != nil {
		return err
	}
	peer.pc = pc
	connected := make(chan struct{})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed:
			peer.kill()
		}
	})
	if peer.first, err = newMeshChannel(pc); err != nil {
		return err
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(GatherTimeout):
	case <-ctx.Done():
		return ctx.Err()
	}
	body, err := json.Marshal(meshOffer{SDP: pc.LocalDescription().SDP, ReplyTo: identity.Recipient().String()})
	if err != nil {
		return err
	}
	sealed, err := meshSeal(recipient, body)
	if err != nil || len(sealed) > MeshMaxCiphertext {
		return fmt.Errorf("webrtc mesh: sealing the offer (%d bytes, over %d means too many local candidates): %v", len(sealed), MeshMaxCiphertext, err)
	}
	session, err := d.signals.Offer(ctx, address, sealed)
	if err != nil {
		return fmt.Errorf("webrtc mesh: writing the offer: %w", err)
	}
	defer func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := d.signals.Withdraw(wctx, address, session); err != nil {
			d.log.Warn("webrtc_mesh_offer_delete_failed", "address", address, "session", session, "err", err.Error())
		}
	}()
	sdp, err := d.answer(ctx, address, session, identity)
	if err != nil {
		return err
	}
	if err := checkFingerprints(sdp, listen.Fingerprint); err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return fmt.Errorf("webrtc mesh: the answer: %w", err)
	}
	select {
	case <-connected:
	case <-peer.dead:
		return fmt.Errorf("webrtc mesh: ICE %s: a symmetric NAT on either side defeats STUN-only WebRTC (%s, no TURN)", pc.ICEConnectionState(), STUNServer)
	case <-ctx.Done():
		return fmt.Errorf("webrtc mesh: ICE %s after %s: %w", pc.ICEConnectionState(), time.Since(started).Round(time.Millisecond), ctx.Err())
	}
	d.log.Info("webrtc_mesh_connected", "address", address, "session", session, "ms", time.Since(started).Milliseconds())
	return nil
}

func (d *MeshDialer) answer(ctx context.Context, address, session string, identity *age.X25519Identity) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, MeshAnswerTimeout)
	defer cancel()
	for {
		ciphertext, err := d.signals.Answer(ctx, address, session)
		if err != nil {
			return "", fmt.Errorf("webrtc mesh: reading the answer: %w", err)
		}
		if ciphertext != "" {
			plaintext, err := meshOpen(identity, ciphertext)
			if err != nil {
				return "", errors.New("webrtc mesh: the answer is not sealed to this session")
			}
			var answer meshAnswer
			if err := json.Unmarshal(plaintext, &answer); err != nil || answer.SDP == "" {
				return "", errors.New("webrtc mesh: the answer is not {sdp}")
			}
			return answer.SDP, nil
		}
		select {
		case <-time.After(MeshAnswerPoll):
		case <-ctx.Done():
			return "", fmt.Errorf("webrtc mesh: no answer within %s: %w", MeshAnswerTimeout, ctx.Err())
		}
	}
}

func newMeshChannel(pc *webrtc.PeerConnection) (*meshChannel, error) {
	ordered := true
	dc, err := pc.CreateDataChannel(MeshLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, fmt.Errorf("webrtc mesh: the datachannel: %w", err)
	}
	out := &meshChannel{conn: channelconn.New(dc.Send, dc.Close), opened: make(chan struct{})}
	dc.OnOpen(func() { close(out.opened) })
	dc.OnMessage(func(message webrtc.DataChannelMessage) { out.conn.Push(message.Data) })
	dc.OnClose(func() { out.conn.CloseWithError(io.EOF) })
	return out, nil
}

func checkFingerprints(sdp, expected string) error {
	expected = strings.Join(strings.Fields(expected), " ")
	found := 0
	for line := range strings.SplitSeq(sdp, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), fingerprintAttribute)
		if !ok {
			continue
		}
		found++
		if got := strings.Join(strings.Fields(value), " "); !strings.EqualFold(got, expected) {
			return fmt.Errorf("%w: the answer has %q, the listen record %q", ErrMeshFingerprint, got, expected)
		}
	}
	if found == 0 {
		return fmt.Errorf("%w: the answer has no a=fingerprint line", ErrMeshFingerprint)
	}
	return nil
}
