package bidder

import (
	"context"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
)

const (
	ReportInterval = 5 * time.Second
)

func (s *Service) reportVMs(ctx context.Context) {
	if s.records == nil || s.attestation == nil || s.vms == nil || s.did == "" {
		return
	}
	ticker := time.NewTicker(ReportInterval)
	defer ticker.Stop()
	for {
		s.reportOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) reportOnce(ctx context.Context) {
	vms, err := s.vms.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("listing ComputeVMs for market reports failed", "err", err)
		}
		return
	}
	for i := range vms {
		vm := &vms[i]
		if vm.Spec.Owner.Handle != s.cfg.Provider.Handle {
			continue
		}
		if vm.DeletionTimestamp != nil || computevm.Ended(vm) {
			continue
		}
		contract := vm.Spec.Contract
		if contract == nil || contract.Receipt.URI == "" || contract.Receipt.CID == "" {
			continue
		}
		held := vm.Status.Market
		if held == nil {
			continue
		}
		s.reportOnNetwork(ctx, vm, held)
		s.reportHostKeys(ctx, vm, held)
	}
}

func (s *Service) reportOnNetwork(ctx context.Context, vm *v1alpha1.ComputeVM, held *v1alpha1.ComputeVMMarket) {
	if held.Address == "" || held.OnNetworkURI != "" {
		return
	}
	createdAt := held.ReportedAt
	if createdAt == "" {
		createdAt = s.cfg.Now().UTC().Format(time.RFC3339)
	}
	record := map[string]any{
		"$type":     market.ComputeEventsVMOnNetworkNSID,
		"address":   held.Address,
		"createdAt": createdAt,
	}
	name, err := s.ensureReportSync(ctx, vm, market.ComputeEventsVMOnNetworkNSID, record)
	if err != nil {
		s.log.Warn("ensuring the onNetwork report StateSync failed", "computevm", vm.Name, "err", err)
		return
	}
	s.recordReportURI(ctx, vm, name, market.ComputeEventsVMOnNetworkNSID)
}

func (s *Service) reportHostKeys(ctx context.Context, vm *v1alpha1.ComputeVM, held *v1alpha1.ComputeVMMarket) {
	if len(held.HostKeys) == 0 || held.HostKeysURI != "" {
		return
	}
	observedAt := held.ReportedAt
	if observedAt == "" {
		observedAt = s.cfg.Now().UTC().Format(time.RFC3339)
	}
	keys := make([]any, 0, len(held.HostKeys))
	for _, key := range held.HostKeys {
		keys = append(keys, map[string]any{"type": key.Type, "publicKey": key.PublicKey, "fingerprint": key.Fingerprint})
	}
	record := map[string]any{
		"$type":      market.ComputeEventsVMHostKeysNSID,
		"keys":       keys,
		"observedAt": observedAt,
		"createdAt":  s.cfg.Now().UTC().Format(time.RFC3339),
	}
	name, err := s.ensureReportSync(ctx, vm, market.ComputeEventsVMHostKeysNSID, record)
	if err != nil {
		s.log.Warn("ensuring the hostKeys report StateSync failed", "computevm", vm.Name, "err", err)
		return
	}
	s.recordReportURI(ctx, vm, name, market.ComputeEventsVMHostKeysNSID)
}

func (s *Service) recordReportURI(ctx context.Context, vm *v1alpha1.ComputeVM, name, collection string) {
	uri := s.observedReportURI(ctx, name)
	if uri == "" {
		return
	}
	report := v1alpha1.ComputeVMMarket{}
	if collection == market.ComputeEventsVMOnNetworkNSID {
		report.OnNetworkURI = uri
	} else {
		report.HostKeysURI = uri
	}
	if err := s.vms.PatchMarket(ctx, vm.Name, report); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("recording a ComputeVM market report URI failed", "computevm", vm.Name, "err", err)
		}
		return
	}
	s.log.Info("a ComputeVM market report is recorded", "computevm", vm.Name, "collection", collection, "payload", uri)
}
