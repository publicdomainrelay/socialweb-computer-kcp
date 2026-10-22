package networkdial

import (
	"crypto/tls"

	"github.com/gorilla/websocket"
)

func NewWebSocketDialer(hosts Resolver, tlsConfig *tls.Config) (*websocket.Dialer, error) {
	return NewWebSocketDialerWithOptions(DialOptions{Hosts: hosts, TLSConfig: tlsConfig})
}

func NewWebSocketDialerWithOptions(opts DialOptions) (*websocket.Dialer, error) {
	d, err := NewDialerWithOptions(opts)
	if err != nil {
		return nil, err
	}
	return &websocket.Dialer{
		NetDialContext:   d.DialContext,
		TLSClientConfig:  d.TLSConfig(),
		HandshakeTimeout: DefaultHandshakeTimeout,
	}, nil
}

func NewNetworkWebSocketDialer(hosts Resolver, caBundlePEM []byte) (*websocket.Dialer, error) {
	cfg, err := TLSConfigFromCA(caBundlePEM, "")
	if err != nil {
		return nil, err
	}
	return NewWebSocketDialerWithOptions(DialOptions{Hosts: hosts, TLSConfig: cfg})
}
