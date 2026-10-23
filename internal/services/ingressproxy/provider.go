package ingressproxy

import (
	"context"
	"errors"
	"net"
	"net/http"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
)

type busTarget struct {
	bus *bus.Bus

	key component.Key

	handler string

	caller commonv1.ObjectRef
}

var _ ingress.Target = busTarget{}

func (t busTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler, ok := t.bus.HandlerNamed(t.key, t.handler)
	if !ok {
		http.Error(w, "the target handler is not registered", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func (t busTarget) DialStream(ctx context.Context) (net.Conn, error) {
	return t.bus.DialStream(bus.WithCaller(ctx, t.caller), t.key, t.handler)
}

type registrar struct {
	impl ingress.Registrar

	bus *bus.Bus
}

var _ exposure.IngressRegistrar = registrar{}

func (r registrar) Register(ctx context.Context, spec exposure.IngressSpec) (exposure.IngressRegistration, error) {
	var target ingress.Target
	if r.bus != nil {
		target = busTarget{bus: r.bus, key: spec.Target, handler: spec.Handler, caller: spec.Caller()}
	}
	registration, err := r.impl.Register(ctx, ingress.Spec{
		Workspace: spec.Workspace,
		Exposure:  spec.Exposure,
		Protocol:  string(spec.Protocol),
		Path:      spec.Path,
	}, target)
	if err != nil {
		if errors.Is(err, ingress.ErrStableHostMismatch) {
			return nil, stableHostError{err: err}
		}
		return nil, err
	}
	return registration, nil
}

type stableHostError struct {
	err error
}

func (e stableHostError) Error() string {
	return e.err.Error()
}

func (e stableHostError) Unwrap() []error {
	return []error{e.err, exposure.ErrStableHostMismatch}
}
