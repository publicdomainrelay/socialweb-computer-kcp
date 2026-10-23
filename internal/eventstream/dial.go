package eventstream

import (
	"errors"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

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

func httpClientWithFallback(primary, external *http.Client) *http.Client {
	if primary == nil {
		if external != nil {
			return external
		}
		return &http.Client{Timeout: 20 * time.Second}
	}
	fallback := http.RoundTripper(http.DefaultTransport)
	if transport, ok := primary.Transport.(*http.Transport); ok && transport != nil {
		cloned := transport.Clone()
		cloned.Proxy = http.ProxyFromEnvironment
		fallback = cloned
	}
	if external != nil && external.Transport != nil {
		fallback = external.Transport
	}
	timeout := primary.Timeout
	timeout = opt.Positive(timeout, 20*time.Second)
	return &http.Client{
		Transport: fallbackHTTPTransport{primary: primary.Transport, fallback: fallback},
		Timeout:   timeout,
	}
}
