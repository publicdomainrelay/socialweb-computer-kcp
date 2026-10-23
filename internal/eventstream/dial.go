package eventstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

func streamDialContext(handlers *component.HandlerRegistry) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if handlers != nil {
			conn, err := handlers.DialHTTP(ctx, address)
			if err == nil {
				return conn, nil
			}
			if !errors.Is(err, component.ErrNoHandler) {
				return nil, err
			}
		}
		return (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
}

type fallbackHTTPTransport struct {
	primary  http.RoundTripper
	fallback http.RoundTripper
}

func (t fallbackHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.primary != nil && req.URL != nil && req.URL.Scheme == "http" {
		response, err := t.primary.RoundTrip(req)
		if err == nil {
			return response, nil
		}
		if !errors.Is(err, component.ErrNoHandler) {
			return nil, err
		}
	}
	if t.fallback == nil {
		return nil, errors.New("eventstream: no HTTP transport")
	}
	return t.fallback.RoundTrip(req)
}

func httpClientWithFallback(primary *http.Client) *http.Client {
	if primary == nil {
		return &http.Client{Timeout: 20 * time.Second}
	}
	fallback := http.RoundTripper(http.DefaultTransport)
	if transport, ok := primary.Transport.(*http.Transport); ok && transport != nil {
		cloned := transport.Clone()
		cloned.Proxy = http.ProxyFromEnvironment
		fallback = cloned
	}
	timeout := primary.Timeout
	timeout = opt.Positive(timeout, 20*time.Second)
	return &http.Client{
		Transport: fallbackHTTPTransport{primary: primary.Transport, fallback: fallback},
		Timeout:   timeout,
	}
}
