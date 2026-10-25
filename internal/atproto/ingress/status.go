package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type SubscriberState string

const (
	SubscriberConnecting SubscriberState = "connecting"

	SubscriberRegistered SubscriberState = "registered"

	SubscriberReconnecting SubscriberState = "reconnecting"

	SubscriberWaiting SubscriberState = "waiting"

	SubscriberClosed SubscriberState = "closed"
)

type SubscriberStatus struct {
	State SubscriberState

	RegisteredAt time.Time

	LastError string

	CloseCode int

	RelayVersion string
}

const (
	maxStatusError = 512

	healthTimeout = 5 * time.Second
)

var urlQuery = regexp.MustCompile(`((?:https?|wss?)://[^\s"'?]+)\?[^\s"']*`)

func StatusError(err error) string {
	if err == nil {
		return ""
	}
	text := urlQuery.ReplaceAllString(err.Error(), "$1")
	if len(text) > maxStatusError {
		text = text[:maxStatusError]
	}
	return text
}

func closeCode(err error) int {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code
	}
	return 0
}

func (s *Subscriber) Status() SubscriberStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

func (s *Subscriber) Relay() string {
	return httpOrigin(s.cfg.IngressProxyHost, s.cfg.TLS)
}

func (s *Subscriber) report(state SubscriberState, err error, code int, version string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.status.State == SubscriberClosed {
		return
	}
	next := s.status
	if err != nil {
		next.LastError = StatusError(err)
	}
	if code != 0 {
		next.CloseCode = code
	}
	if version != "" {
		next.RelayVersion = version
	}
	if next.State == state {
		s.status = next
		return
	}
	next.State = state
	if state == SubscriberRegistered {
		next.RegisteredAt = time.Now().UTC()
		next.LastError, next.CloseCode = "", 0
	}
	s.status = next
	if s.cfg.OnStatus != nil {
		s.cfg.OnStatus(next)
	}
}

func (s *Subscriber) relayVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Relay()+HealthPath, nil)
	if err != nil {
		return ""
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return ""
	}
	return strings.TrimSpace(health.Version)
}
