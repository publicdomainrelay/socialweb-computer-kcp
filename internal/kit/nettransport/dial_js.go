//go:build js

package nettransport

import (
	"context"
	"net"
	"net/http"
)

func WithDial(t *http.Transport, _ func(ctx context.Context, network, address string) (net.Conn, error)) *http.Transport {
	return t
}
