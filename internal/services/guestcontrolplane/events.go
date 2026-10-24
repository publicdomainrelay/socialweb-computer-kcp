package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
)

func (s *Service) openQueue(ctx context.Context, inputs guestcp.Source) error {
	if s.stateDir == "" {
		return errors.New("guestcontrolplane: the guest event queue needs a state dir")
	}
	self := s.self()
	ctx = bus.WithCallerLabels(ctx, self, s.deps.Labels)
	queue, err := bus.Resolve[guestcp.Queue](ctx, s.deps.Bus, self, self, bus.Capability{Name: guestcp.CapabilityQueue})
	if err != nil {
		return fmt.Errorf("guestcontrolplane: resolving the guest event queue: %w", err)
	}
	if err := queue.Open(s.stateDir); err != nil {
		return err
	}
	unwatch := inputs.Watch(func(in guestcp.Inputs) {
		names := make([]string, 0, len(in.ComputeVMs))
		for i := range in.ComputeVMs {
			names = append(names, in.ComputeVMs[i].Name)
		}
		queue.Retain(names)
	})
	s.mu.Lock()
	s.queue, s.unwatchQueue = queue, unwatch
	s.mu.Unlock()
	return nil
}

func (s *Service) publishGuestEvent(ctx context.Context, event guestcp.GuestEvent) error {
	s.mu.Lock()
	queue := s.queue
	s.mu.Unlock()
	if queue == nil {
		return guestcp.ErrQueueNotOpen
	}
	return queue.Publish(ctx, event)
}
