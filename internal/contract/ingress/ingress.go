package ingress

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
)

var ErrStableHostMismatch = errors.New("StableHostMismatch")

type Spec struct {
	Workspace string

	Exposure string

	Protocol string

	Path string
}

type Target interface {
	http.Handler

	DialStream(ctx context.Context) (net.Conn, error)
}

type Registration interface {
	URL() string

	Close() error
}

type Registrar interface {
	Register(ctx context.Context, spec Spec, target Target) (Registration, error)
}

type Provider interface {
	Start(ctx context.Context) error

	Ready(ctx context.Context) error

	Stop(ctx context.Context) error

	Endpoints() []component.Endpoint
}
