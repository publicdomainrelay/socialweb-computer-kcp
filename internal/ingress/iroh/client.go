package iroh

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

func Client(base *http.Client, dumbpipe string, log *slog.Logger) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	next := base.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	dialer := sshtransport.DumbpipeDialer{Path: dumbpipe, Log: log}
	pipes := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			ticket, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.Dial(context.WithoutCancel(ctx), ticket)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     time.Minute,
	}
	out := *base
	out.Transport = &router{next: next, iroh: pipes}
	return &out
}

type router struct {
	next http.RoundTripper

	iroh http.RoundTripper
}

func (r *router) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != URLScheme {
		return r.next.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	return r.iroh.RoundTrip(out)
}
