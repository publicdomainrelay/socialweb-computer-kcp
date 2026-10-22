package ingressproxy

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
)

type Provider interface {
	Start(ctx context.Context, deps component.Deps) error

	Stop(ctx context.Context) error

	Endpoints() []component.Endpoint

	IngressRegistrar() exposure.IngressRegistrar
}
