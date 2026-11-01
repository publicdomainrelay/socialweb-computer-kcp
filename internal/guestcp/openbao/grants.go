package openbao

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

func (p *Plane) reconcileLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		}
		if failed := p.reconcile(ctx); failed && ctx.Err() == nil {
			p.retryLater(ctx)
		}
	}
}

func (p *Plane) reconcile(ctx context.Context) bool {
	did := p.DID()
	if did == "" {
		return false
	}
	in := p.current()
	failed := false
	live := map[string]bool{}
	for i := range in.ComputeVMs {
		vm := &in.ComputeVMs[i]
		if vm.DeletionTimestamp != nil || guestidentity.Ended(vm) || guestidentity.Releasing(vm) {
			continue
		}
		requester := vm.Spec.Requester.Did
		role := vm.Spec.VM.Role
		if requester == "" || role == "" {
			continue
		}
		subject, err := guestidentity.SubjectFor(did, requester, role)
		if err != nil {
			p.log.Warn("building a guest identity subject failed", "computevm", vm.Name, "err", err)
			continue
		}
		live[vm.Name] = true
		key := subject + "\x00" + vm.Name
		if prior, known := p.known[vm.Name]; !known || prior != key {
			attempt, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
			err := p.identity.SetWorkload(attempt, vm.Name, subject, map[string]string{"computevm": vm.Name})
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					p.log.Warn("setting the guest's workload subject failed", "computevm", vm.Name, "err", err)
				}
				failed = true
				continue
			}
			p.known[vm.Name] = key
			p.log.Info("the guest's workload subject is set on its login role", "computevm", vm.Name, "subject", subject)
		}
		if held := guestidentity.GuestReportOf(vm); held == nil || len(held.HostKeys) == 0 {
			p.recordHostKey(ctx, vm.Name)
		}
	}
	for name := range p.known {
		if live[name] {
			continue
		}
		delete(p.known, name)
	}
	return failed
}

const retryDelay = 5 * time.Second

func (p *Plane) retryLater(ctx context.Context) {
	p.mu.Lock()
	if p.retrying {
		p.mu.Unlock()
		return
	}
	p.retrying = true
	p.mu.Unlock()
	p.wg.Go(func() {
		defer func() {
			p.mu.Lock()
			p.retrying = false
			p.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
		case <-p.clock.After(retryDelay):
			nudge(p.kick)
		}
	})
}

func (p *Plane) recordHostKey(ctx context.Context, name string) {
	p.mu.Lock()
	if p.probing[name] {
		p.mu.Unlock()
		return
	}
	p.probing[name] = true
	p.mu.Unlock()
	p.wg.Go(func() {
		defer func() {
			p.mu.Lock()
			delete(p.probing, name)
			p.mu.Unlock()
		}()
		probe, cancel := context.WithTimeout(ctx, hostKeyProbe)
		defer cancel()
		done, err := p.guest.RecordBoundHostKey(probe, name)
		if err != nil && ctx.Err() == nil {
			p.log.Warn("recording the guest's bound host key failed", "computevm", name, "err", err)
		}
		if done {
			p.log.Info("the guest's bound host key is recorded", "computevm", name)
		}
	})
}
