package bidder

import (
	"context"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
)

const (
	guestEventsRetry = time.Second

	guestEventsRetryMax = time.Minute
)

type guestEvents struct {
	handlers *bus.Bus

	caller func(context.Context) context.Context

	self commonv1.ObjectRef

	target commonv1.ObjectRef
}

var _ core.GuestQueue = guestEvents{}

func (s *Service) guestEvents() core.GuestQueue {
	if s.deps.Bus == nil {
		return nil
	}
	return s.guestEventsHandle()
}

func (s *Service) guestEventsHandle() guestEvents {
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

func (s *Service) serveGuestEvents(ctx context.Context, bidder *core.Bidder) {
	if s.deps.Bus == nil {
		return
	}
	handle := s.guestEventsHandle()
	delay := guestEventsRetry
	for {
		events, err := handle.open(ctx)
		if err == nil {
			cancel := events.Serve(bidder.PublishGuestEvent)
			s.log.Info("the bidder publishes the guest events of its GuestControlPlane", "guestControlPlane", guestControlName)
			<-ctx.Done()
			cancel()
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("the bidder cannot reach the guest event handle yet; retrying", "err", err, "retry", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, guestEventsRetryMax)
	}
}
