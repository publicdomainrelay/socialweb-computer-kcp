package webrtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/channelconn"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httptunnel"
)

const (
	STUNServer = tunnel.DefaultSTUN

	GatherTimeout = 10 * time.Second

	OpenTimeout = 20 * time.Second

	RequestDeadline = 60 * time.Second

	stunGatherTimeout = 3 * time.Second

	controlLabel = "control"

	requestLabel = "xrpc"
)

type ServiceConfig struct {
	Handler http.Handler

	Log *slog.Logger

	Options tunnel.ServiceOptions
}

type Service struct {
	handler http.Handler

	log *slog.Logger

	api *webrtc.API

	options tunnel.ServiceOptions

	ctx context.Context

	cancel context.CancelFunc

	mu sync.Mutex

	// ponytail: every offer keeps its PeerConnection until it fails or the
	// service closes; cap per peer if a long-lived requester accumulates them.
	offers map[string]*offerState

	closed bool
}

type offerState struct {
	id string

	session string

	peer string

	accepted bool

	timer *time.Timer

	pc *webrtc.PeerConnection
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Handler == nil {
		return nil, errors.New("webrtc: an ingress needs an HTTP handler")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	options := cfg.Options
	if len(options.STUN) == 0 {
		options.STUN = []string{STUNServer}
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = 64
	}
	if options.SessionTTL <= 0 {
		options.SessionTTL = 10 * time.Minute
	}
	return &Service{
		handler: cfg.Handler,
		log:     log,
		api:     newAPI(),
		options: options,
		ctx:     ctx,
		cancel:  cancel,
		offers:  map[string]*offerState{},
	}, nil
}

func newAPI() *webrtc.API {
	settings := webrtc.SettingEngine{}
	tuneSettings(&settings)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

func configuration() webrtc.Configuration {
	return configurationWithSTUN(nil)
}

func configurationWithSTUN(stun []string) webrtc.Configuration {
	if len(stun) == 0 {
		stun = []string{STUNServer}
	}
	return webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: stun}}}
}

func (s *Service) Offer(ctx context.Context, owner, peer string) (tunnel.Offer, error) {
	if s == nil {
		return tunnel.Offer{}, errors.New("webrtc: the ingress is not running")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return tunnel.Offer{}, errors.New("webrtc: the ingress is closed")
	}
	id := tunnel.ID(owner)
	if id == "" {
		return tunnel.Offer{}, errors.New("webrtc: the offer names no requester identity")
	}
	if strings.TrimSpace(peer) == "" {
		return tunnel.Offer{}, errors.New("webrtc: the offer names no peer")
	}
	session := tunnel.NewSession()
	if session == "" {
		return tunnel.Offer{}, errors.New("webrtc: the offer session could not be generated")
	}
	s.mu.Lock()
	full := s.pendingLocked() >= s.options.MaxSessions
	s.mu.Unlock()
	if full {
		return tunnel.Offer{}, fmt.Errorf("webrtc: the ingress holds %d pending sessions", s.options.MaxSessions)
	}
	pc, err := s.api.NewPeerConnection(configurationWithSTUN(s.options.STUN))
	if err != nil {
		return tunnel.Offer{}, fmt.Errorf("webrtc: the peer connection: %w", err)
	}
	ordered := true
	control, err := pc.CreateDataChannel(controlLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		_ = pc.Close()
		return tunnel.Offer{}, fmt.Errorf("webrtc: the control datachannel: %w", err)
	}
	control.OnOpen(func() {
		s.log.Info("webrtc_ingress_control_open", "peer", peer)
	})
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		s.serve(peer, channel)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			s.drop(session, pc)
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return tunnel.Offer{}, fmt.Errorf("webrtc: creating the offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return tunnel.Offer{}, fmt.Errorf("webrtc: setting the offer: %w", err)
	}
	if err := waitGather(ctx, gathered); err != nil {
		_ = pc.Close()
		return tunnel.Offer{}, err
	}
	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return tunnel.Offer{}, errors.New("webrtc: no local description after gathering")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = pc.Close()
		return tunnel.Offer{}, errors.New("webrtc: the ingress is closed")
	}
	if s.pendingLocked() >= s.options.MaxSessions {
		s.mu.Unlock()
		_ = pc.Close()
		return tunnel.Offer{}, fmt.Errorf("webrtc: the ingress holds %d pending sessions", s.options.MaxSessions)
	}
	state := &offerState{id: id, session: session, peer: peer, pc: pc}
	if s.options.SessionTTL > 0 {
		state.timer = time.AfterFunc(s.options.SessionTTL, func() {
			s.expire(session, pc)
		})
	}
	s.offers[session] = state
	s.mu.Unlock()
	s.log.Info("webrtc_ingress_offer", "peer", peer, "id", id, "session", session, "sdpBytes", len(local.SDP))
	return tunnel.Offer{Kind: tunnel.KindWebRTC, ID: id, Session: session, Peer: peer, Type: tunnel.SignalOffer, SDP: local.SDP}, nil
}

func (s *Service) Accept(ctx context.Context, answer tunnel.Answer) error {
	if s == nil {
		return errors.New("webrtc: the ingress is not running")
	}
	if !tunnel.ValidAnswer(answer) {
		return errors.New("webrtc: the answer is not a webrtc answer")
	}
	s.mu.Lock()
	state := s.offers[answer.Session]
	s.mu.Unlock()
	if state == nil {
		return fmt.Errorf("webrtc: no offer names session %s", answer.Session)
	}
	if state.id != answer.ID {
		return fmt.Errorf("webrtc: the answer names ingress %s and the offer %s", answer.ID, state.id)
	}
	if state.peer != answer.Peer {
		return fmt.Errorf("webrtc: the answer names peer %s and the offer %s", answer.Peer, state.peer)
	}
	if err := state.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return fmt.Errorf("webrtc: the answer is not a session description: %w", err)
	}
	s.mu.Lock()
	if s.offers[answer.Session] != state {
		s.mu.Unlock()
		return errors.New("webrtc: the offer expired before the answer was applied")
	}
	state.accepted = true
	if state.timer != nil {
		state.timer.Stop()
	}
	s.mu.Unlock()
	s.log.Info("webrtc_ingress_answer", "peer", answer.Peer, "id", answer.ID, "session", answer.Session)
	return nil
}

func (s *Service) serve(peer string, channel *webrtc.DataChannel) {
	conn := channelconn.New(channel.Send, channel.Close)
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		conn.Push(message.Data)
	})
	channel.OnClose(func() {
		conn.CloseWithError(io.EOF)
	})
	stop := context.AfterFunc(s.ctx, func() {
		conn.CloseWithError(context.Canceled)
	})
	go func() {
		defer stop()
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(RequestDeadline))
		if err := httptunnel.Serve(s.ctx, conn, s.handler); err != nil && s.ctx.Err() == nil && !errors.Is(err, io.EOF) {
			s.log.Warn("webrtc_ingress_request_failed", "peer", peer, "err", err)
		}
	}()
}

func (s *Service) drop(session string, pc *webrtc.PeerConnection) {
	s.mu.Lock()
	if state := s.offers[session]; state != nil && state.pc == pc {
		delete(s.offers, session)
		if state.timer != nil {
			state.timer.Stop()
		}
	}
	s.mu.Unlock()
}

func (s *Service) pendingLocked() int {
	count := 0
	for _, state := range s.offers {
		if !state.accepted {
			count++
		}
	}
	return count
}

func (s *Service) expire(session string, pc *webrtc.PeerConnection) {
	s.mu.Lock()
	state := s.offers[session]
	if state == nil || state.pc != pc || state.accepted {
		s.mu.Unlock()
		return
	}
	delete(s.offers, session)
	s.mu.Unlock()
	_ = pc.Close()
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	offers := s.offers
	s.offers = map[string]*offerState{}
	s.mu.Unlock()
	s.cancel()
	for _, state := range offers {
		if state.timer != nil {
			state.timer.Stop()
		}
		_ = state.pc.Close()
	}
	return nil
}

func waitGather(ctx context.Context, gathered <-chan struct{}) error {
	timer := time.NewTimer(GatherTimeout)
	defer timer.Stop()
	select {
	case <-gathered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("webrtc: ICE gathering did not complete within %s", GatherTimeout)
	}
}

type Peer struct {
	log *slog.Logger

	api *webrtc.API

	mu sync.Mutex

	sessions map[string]*session

	// ponytail: answered offers keep their PeerConnection so a late bid can
	// dial any live one; cap per ingress id if a bidder runs long enough.
	byID map[string][]*session

	changed chan struct{}

	wake chan struct{}

	closed bool
}

type session struct {
	id string

	session string

	pc *webrtc.PeerConnection

	ready chan struct{}

	failed chan struct{}

	err error
}

func NewPeer(log *slog.Logger) *Peer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Peer{
		log:      log,
		api:      newAPI(),
		sessions: map[string]*session{},
		byID:     map[string][]*session{},
		changed:  make(chan struct{}),
		wake:     make(chan struct{}),
	}
}

func (p *Peer) Answer(ctx context.Context, offer tunnel.Offer) (tunnel.Answer, error) {
	if p == nil {
		return tunnel.Answer{}, errors.New("webrtc: the peer is not running")
	}
	if !tunnel.ValidOffer(offer) {
		return tunnel.Answer{}, errors.New("webrtc: the offer is not a webrtc offer")
	}
	pc, err := p.api.NewPeerConnection(configuration())
	if err != nil {
		return tunnel.Answer{}, fmt.Errorf("webrtc: the peer connection: %w", err)
	}
	current := &session{id: offer.ID, session: offer.Session, pc: pc, ready: make(chan struct{}), failed: make(chan struct{})}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-current.ready:
			default:
				close(current.ready)
			}
			p.wakeAll()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			p.fail(current, fmt.Errorf("webrtc: the peer connection is %s", state.String()))
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		_ = pc.Close()
		return tunnel.Answer{}, fmt.Errorf("webrtc: the offer is not a session description: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return tunnel.Answer{}, fmt.Errorf("webrtc: creating the answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return tunnel.Answer{}, fmt.Errorf("webrtc: setting the answer: %w", err)
	}
	if err := waitGather(ctx, gathered); err != nil {
		_ = pc.Close()
		return tunnel.Answer{}, err
	}
	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return tunnel.Answer{}, errors.New("webrtc: no local description after gathering")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = pc.Close()
		return tunnel.Answer{}, errors.New("webrtc: the peer is closed")
	}
	previous := p.sessions[offer.Session]
	p.sessions[offer.Session] = current
	kept := make([]*session, 0, len(p.byID[offer.ID])+1)
	kept = append(kept, current)
	for _, existing := range p.byID[offer.ID] {
		if existing != previous && existing != current {
			kept = append(kept, existing)
		}
	}
	p.byID[offer.ID] = kept
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	if previous != nil && previous != current {
		_ = previous.pc.Close()
	}
	p.log.Info("webrtc_answerer_ready", "requester", offer.Peer, "id", offer.ID, "session", offer.Session, "sdpBytes", len(local.SDP))
	return tunnel.Answer{Kind: tunnel.KindWebRTC, ID: offer.ID, Session: offer.Session, Peer: offer.Peer, Type: tunnel.SignalAnswer, SDP: local.SDP}, nil
}

func (p *Peer) fail(s *session, err error) {
	p.mu.Lock()
	if s.err == nil {
		s.err = err
		candidates := p.byID[s.id]
		kept := make([]*session, 0, len(candidates))
		for _, existing := range candidates {
			if existing != s {
				kept = append(kept, existing)
			}
		}
		p.byID[s.id] = kept
		close(s.failed)
		if !p.closed {
			close(p.wake)
			p.wake = make(chan struct{})
		}
	}
	p.mu.Unlock()
}

func (p *Peer) wakeAll() {
	p.mu.Lock()
	if !p.closed {
		close(p.wake)
		p.wake = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *Peer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if p == nil {
		return nil, errors.New("webrtc: the peer is not running")
	}
	if network != tunnel.NetworkWebRTC {
		return nil, fmt.Errorf("webrtc: the network %q is not %s", network, tunnel.NetworkWebRTC)
	}
	id := strings.TrimSpace(address)
	if parsed, ok := tunnel.ParseRef(id); ok {
		id = parsed
	}
	current, err := p.pick(ctx, id)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(OpenTimeout)
	defer timer.Stop()
	select {
	case <-current.failed:
		return nil, current.err
	default:
	}
	ordered := true
	channel, err := current.pc.CreateDataChannel(requestLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, fmt.Errorf("webrtc: the request datachannel: %w", err)
	}
	opened := make(chan struct{})
	channel.OnOpen(func() {
		select {
		case <-opened:
		default:
			close(opened)
		}
	})
	conn := channelconn.New(channel.Send, channel.Close)
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		conn.Push(message.Data)
	})
	channel.OnClose(func() {
		conn.CloseWithError(io.EOF)
	})
	select {
	case <-opened:
	case <-ctx.Done():
		_ = channel.Close()
		return nil, ctx.Err()
	case <-timer.C:
		_ = channel.Close()
		return nil, fmt.Errorf("webrtc: the request datachannel to %s did not open within %s", id, OpenTimeout)
	}
	return conn, nil
}

func (p *Peer) pick(ctx context.Context, id string) (*session, error) {
	timer := time.NewTimer(OpenTimeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		candidates := append([]*session(nil), p.byID[id]...)
		changed := p.changed
		wake := p.wake
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, errors.New("webrtc: the peer is closed")
		}
		for _, current := range candidates {
			select {
			case <-current.failed:
				continue
			default:
			}
			select {
			case <-current.ready:
				select {
				case <-current.failed:
					continue
				default:
					return current, nil
				}
			default:
			}
		}
		var pending *session
		for _, current := range candidates {
			select {
			case <-current.failed:
				continue
			default:
			}
			pending = current
			break
		}
		if pending == nil {
			select {
			case <-changed:
			case <-wake:
			case <-ctx.Done():
				return nil, fmt.Errorf("webrtc: no webrtc offer names %s: %w", id, ctx.Err())
			case <-timer.C:
				return nil, fmt.Errorf("webrtc: no webrtc offer names %s within %s", id, OpenTimeout)
			}
			continue
		}
		select {
		case <-pending.ready:
			return pending, nil
		case <-pending.failed:
		case <-changed:
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("webrtc: the connection to %s did not open within %s", id, OpenTimeout)
		}
	}
}

func (p *Peer) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sessions := p.sessions
	p.sessions = map[string]*session{}
	p.byID = map[string][]*session{}
	close(p.changed)
	close(p.wake)
	p.mu.Unlock()
	for _, current := range sessions {
		_ = current.pc.Close()
	}
	return nil
}

var (
	_ tunnel.Service  = (*Service)(nil)
	_ tunnel.Answerer = (*Peer)(nil)
	_ tunnel.Dialer   = (*Peer)(nil)
)
