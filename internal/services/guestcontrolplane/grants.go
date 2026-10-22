package guestcontrolplane

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

type grantLoop struct {
	syncer *guestidentity.Syncer

	vms VMStore

	guest *guestidentity.Guest

	did string

	handle string

	interval time.Duration

	log *slog.Logger

	mu sync.Mutex

	known map[string]string

	wg sync.WaitGroup
}

func (g *grantLoop) logf() *slog.Logger {
	if g == nil || g.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return g.log
}

func (g *grantLoop) run(ctx context.Context) {
	if g == nil || g.syncer == nil || g.vms == nil {
		return
	}
	if err := g.scan(ctx); err != nil && ctx.Err() == nil {
		g.logf().Warn("syncing the guest identity grants failed", "err", err)
	}
	interval := g.interval
	if interval <= 0 {
		interval = DefaultGrantSync
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			g.wg.Wait()
			return
		case <-ticker.C:
			if err := g.scan(ctx); err != nil && ctx.Err() == nil {
				g.logf().Warn("syncing the guest identity grants failed", "err", err)
			}
		}
	}
}

func (g *grantLoop) scan(ctx context.Context) error {
	if g.did == "" {
		return nil
	}
	vms, err := g.vms.List(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range vms {
		vm := &vms[i]
		if g.handle != "" && vm.Spec.Owner.Handle != g.handle {
			continue
		}
		if vm.DeletionTimestamp != nil || computevm.Ended(vm) || computevm.Releasing(vm) {
			continue
		}
		requester := vm.Spec.Requester.Did
		role := vm.Spec.VM.Role
		if requester == "" || role == "" {
			continue
		}
		subject, err := guestidentity.SubjectFor(g.did, requester, role)
		if err != nil {
			g.logf().Warn("building a guest identity subject failed", "computevm", vm.Name, "err", err)
			continue
		}
		live[vm.Name] = true
		key := vm.Name + "\x00" + subject + "\x00" + requester
		g.mu.Lock()
		prior, known := g.known[vm.Name]
		g.known[vm.Name] = key
		g.mu.Unlock()
		if !known || prior != key {
			if err := g.syncer.Register(ctx, guestidentity.GrantSubject{
				ComputeVM:    vm.Name,
				OwnerUID:     string(vm.UID),
				RequesterDID: requester,
				Subject:      subject,
			}); err != nil {
				g.logf().Warn("registering a guest identity grant failed", "computevm", vm.Name, "err", err)
				continue
			}
		}
		if vm.Status.Market == nil || len(vm.Status.Market.HostKeys) == 0 {
			g.recordHostKey(ctx, vm.Name)
		}
	}
	g.mu.Lock()
	for name := range g.known {
		if live[name] {
			continue
		}
		delete(g.known, name)
		g.syncer.Forget(name)
	}
	g.mu.Unlock()
	return nil
}

func (g *grantLoop) recordHostKey(ctx context.Context, name string) {
	if g.guest == nil {
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		probe, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		done, err := g.guest.RecordBoundHostKey(probe, name)
		if err != nil && ctx.Err() == nil {
			g.logf().Warn("recording the guest's bound host key failed", "computevm", name, "err", err)
		}
		if done {
			g.logf().Info("the guest's bound host key is recorded", "computevm", name)
		}
	}()
}
