//go:build !js

package nettransport

import (
	"context"
	"net"
	"net/http"
)

func WithDial(t *http.Transport, dial func(ctx context.Context, network, address string) (net.Conn, error)) *http.Transport {
	t.DialContext = dial
	return t
}
