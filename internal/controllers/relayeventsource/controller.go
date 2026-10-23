package relayeventsource

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcp"
)

const (
	Name = "relayeventsource"

	DefaultRelayKind = "Relay"
)

func Register(m *ctlruntime.Manager, opts eventsource.BackendOptions) (*eventsource.Backend[*eventsv1.RelayEventSource], error) {
	dial := eventsource.BusDial(opts.Bus)
	b := eventsource.NewBackend(eventsv1.RelayEventSourceKind, opts,
		func(_ context.Context, req ctlruntime.Request, obj *eventsv1.RelayEventSource) (eventsource.Upstream, error) {
			base, err := baseURL(req.Workspace, obj.Spec)
			if err != nil {
				return eventsource.Upstream{}, err
			}
			source, err := relay.New(relay.Config{Base: base}, relay.Deps{Dial: dial})
			if err != nil {
				return eventsource.Upstream{}, err
			}
			return eventsource.Upstream{Address: source.Address(), Source: source, CountGaps: true}, nil
		},
		func(o *eventsv1.RelayEventSource) *eventsv1.EventSourceStatus { return &o.Status })
	if err := ctlruntime.For[*eventsv1.RelayEventSource](m, Name, b, ctlruntime.Export(eventsource.Export)); err != nil {
		return nil, fmt.Errorf("relayeventsource: registering the RelayEventSource controller: %w", err)
	}
	return b, nil
}

func baseURL(workspace string, spec eventsv1.RelayEventSourceSpec) (string, error) {
	raw := strings.TrimSpace(spec.URL)
	switch {
	case spec.RelayRef != nil && raw != "":
		return "", errors.New("the RelayEventSource names both relayRef and url")
	case spec.RelayRef != nil:
		ref := *spec.RelayRef
		kind := strings.ToLower(cmp.Or(strings.TrimSpace(ref.Kind), DefaultRelayKind))
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		if name == "" {
			return "", errors.New("the RelayEventSource relayRef needs a name")
		}
		if ws := ref.InWorkspace(workspace); ws != "" && ws != workspace {
			return "http://" + kcp.ServiceFQDN(kind, "default", ws, kcp.DefaultServiceDomain), nil
		}
		return "http://" + kind + "." + name, nil
	case raw != "":
		return raw, nil
	default:
		return "", errors.New("the RelayEventSource needs relayRef or url")
	}
}
