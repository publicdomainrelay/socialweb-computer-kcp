package relay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultTokenLifetime = 5 * time.Minute

	maxResponseBody = 8 << 20

	retryFirst = time.Second

	retryMax = time.Minute
)

type Config struct {
	Label string

	KeyFile string

	IngressProxyHost string

	TLS bool

	TokenLifetime time.Duration
}

type Deps struct {
	Handler http.Handler

	ServiceAuth pds.ServiceAuth

	DID func() string

	Clock host.Clock

	Log *slog.Logger
}

type Relay struct {
	cfg Config

	deps Deps

	log *slog.Logger

	clock host.Clock

	mu sync.Mutex

	subscriber *ingress.Subscriber

	closed bool
}

var _ guestcp.Relay = (*Relay)(nil)

func New(cfg Config, deps Deps) (*Relay, error) {
	cfg.IngressProxyHost = strings.TrimSpace(cfg.IngressProxyHost)
	switch {
	case strings.TrimSpace(cfg.KeyFile) == "":
		return nil, errors.New("guestcp: the relay names no keypair and there is no state dir")
	case cfg.IngressProxyHost == "":
		return nil, errors.New("guestcp: the relay names no ingress proxy host")
	case deps.Handler == nil:
		return nil, errors.New("guestcp: the relay has no handler")
	case deps.ServiceAuth == nil:
		return nil, errors.New("guestcp: the relay has no service-auth token source")
	case deps.DID == nil:
		return nil, errors.New("guestcp: the relay has no account DID source")
	}
	cfg.TokenLifetime = opt.Positive(cfg.TokenLifetime, DefaultTokenLifetime)
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Relay{cfg: cfg, deps: deps, log: log, clock: clock}, nil
}

func (r *Relay) Run(ctx context.Context, connected func()) error {
	signer, err := market.LoadOrCreateSignerFile(r.cfg.KeyFile)
	if err != nil {
		return err
	}
	did, err := signer.PublicDID()
	if err != nil {
		return err
	}
	delay := retryFirst
	for {
		subscriber, err := ingress.NewSubscriber(ctx, ingress.SubscriberConfig{
			Label:               r.cfg.Label,
			Keypair:             signerKeypair{signer: signer, did: did},
			GetServiceAuthToken: r.serviceAuth,
			IngressProxyHost:    r.cfg.IngressProxyHost,
			TLS:                 r.cfg.TLS,
			HandleRequest:       r.forward,
		})
		if err == nil {
			if !r.hold(subscriber) {
				subscriber.Close()
				return nil
			}
			if connected != nil {
				connected()
			}
			<-ctx.Done()
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		r.log.Warn("the guest relay did not connect; retrying", "err", err, "retry", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-r.clock.After(delay):
		}
		delay = min(delay*2, retryMax)
	}
}

func (r *Relay) hold(subscriber *ingress.Subscriber) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.subscriber = subscriber
	return true
}

func (r *Relay) Close() {
	r.mu.Lock()
	subscriber := r.subscriber
	r.subscriber = nil
	r.closed = true
	r.mu.Unlock()
	if subscriber != nil {
		subscriber.Close()
	}
}

func (r *Relay) URL() string {
	r.mu.Lock()
	subscriber := r.subscriber
	r.mu.Unlock()
	if subscriber == nil {
		return ""
	}
	host := r.cfg.IngressProxyHost
	scheme := "https"
	if !r.cfg.TLS && (strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost")) {
		scheme = "http"
	}
	return scheme + "://" + subscriber.Subdomain() + "." + host
}

func (r *Relay) serviceAuth(ctx context.Context, nsid string) (string, error) {
	did := strings.TrimSpace(r.deps.DID())
	if did == "" {
		return "", errors.New("guestcp: the relay account DID is not known yet")
	}
	audience := "did:web:" + hostport.URLHostname(r.cfg.IngressProxyHost)
	return r.deps.ServiceAuth.ServiceAuth(ctx, did, audience, nsid, r.clock.Now().Add(r.cfg.TokenLifetime))
}

func (r *Relay) forward(ctx context.Context, request *ingress.Request) (*ingress.Response, error) {
	if request == nil {
		return nil, errors.New("guestcp: the relay request is empty")
	}
	path := cmp.Or(request.Path, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(request.Params) > 0 {
		values := url.Values{}
		for key, value := range request.Params {
			values.Set(key, value)
		}
		path += "?" + values.Encode()
	}
	body := request.RawBody
	if body == "" && len(request.Body) > 0 {
		body = string(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, "http://relay.local"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	r.deps.Handler.ServeHTTP(recorder, req)
	result := recorder.Result()
	defer result.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(result.Body, maxResponseBody))
	if err != nil {
		return nil, err
	}
	return &ingress.Response{Status: result.StatusCode, Body: json.RawMessage(raw), ContentType: result.Header.Get("Content-Type")}, nil
}

type signerKeypair struct {
	signer *market.Signer

	did string
}

func (k signerKeypair) DID() string { return k.did }

func (k signerKeypair) Sign(data []byte) ([]byte, error) {
	return market.SignBytes(data, k.signer.PrivateKey)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
