package webrtc

import (
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httptunnel"
)

func Client(base *http.Client, dialer tunnel.Dialer) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	next := base.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	out := *base
	out.Transport = &router{next: next, webrtc: &httptunnel.Protocol{Dialer: dialer}}
	return &out
}

type router struct {
	next http.RoundTripper

	webrtc http.RoundTripper
}

func (r *router) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && req.URL.Scheme == tunnel.KindWebRTC {
		return r.webrtc.RoundTrip(req)
	}
	return r.next.RoundTrip(req)
}
