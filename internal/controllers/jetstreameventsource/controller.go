package jetstreameventsource

import (
	"context"
	"fmt"

	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/jetstream"
)

const Name = "jetstreameventsource"

func Register(ctx context.Context, m *ctlruntime.Manager, opts eventsource.BackendOptions) (*eventsource.Backend[*eventsv1.JetstreamEventSource], error) {
	dial := eventsource.BusDial(opts.Bus)
	b := eventsource.NewBackend(ctx, eventsv1.JetstreamEventSourceKind, opts,
		func(_ context.Context, _ ctlruntime.Request, obj *eventsv1.JetstreamEventSource) (eventsource.Upstream, error) {
			source, err := jetstream.New(jetstream.Config{URL: obj.Spec.URL}, jetstream.Deps{Dial: dial, Log: opts.Log})
			if err != nil {
				return eventsource.Upstream{}, err
			}
			return eventsource.Upstream{Address: source.Address(), Source: source}, nil
		},
		func(o *eventsv1.JetstreamEventSource) *eventsv1.EventSourceStatus { return &o.Status })
	if err := ctlruntime.For[*eventsv1.JetstreamEventSource](m, Name, b, ctlruntime.Export(eventsource.Export)); err != nil {
		return nil, fmt.Errorf("jetstreameventsource: registering the JetstreamEventSource controller: %w", err)
	}
	return b, nil
}
