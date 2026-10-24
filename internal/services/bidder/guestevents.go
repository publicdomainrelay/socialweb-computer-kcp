package bidder

import (
	"context"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
)

type guestEvents struct {
	handlers *bus.Bus

	caller func(context.Context) context.Context

	self commonv1.ObjectRef

	target commonv1.ObjectRef
}

var _ guestcp.Events = guestEvents{}

func (s *Service) guestEvents() guestcp.Events {
	if s.deps.Bus == nil {
		return nil
	}
	return guestEvents{handlers: s.deps.Bus, caller: s.deps.Caller, self: s.self(),
		target: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: guestControlKind, Name: guestControlName, Workspace: s.workspace}}
}

func (g guestEvents) open(ctx context.Context) (guestcp.Events, error) {
	return bus.Resolve[guestcp.Events](g.caller(ctx), g.handlers, g.self, g.target, bus.Capability{Name: guestcp.CapabilityEvents})
}

func (g guestEvents) Append(ctx context.Context, computeVM string, event guestcp.QueuedEvent) (bool, error) {
	events, err := g.open(ctx)
	if err != nil {
		return false, err
	}
	return events.Append(ctx, computeVM, event)
}
