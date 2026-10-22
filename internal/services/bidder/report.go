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
		if vm.Spec.Owner.Handle != s.cfg.Provider.Handle || vm.DeletionTimestamp != nil {
			continue
		}
		if computevm.Ended(vm) || computevm.Releasing(vm) {
			continue
		}
		contract := vm.Spec.Contract
		if contract == nil || contract.ReceiptUri == "" || contract.ReceiptCid == "" {
			continue
		}
		held := vm.Status.Market
		if held == nil {
			continue
		}
		if held.Address != "" && held.OnNetworkUri == "" {
			s.publishReport(ctx, vm, contract, market.ComputeEventsVMOnNetworkNSID, map[string]any{
				"$type":     market.ComputeEventsVMOnNetworkNSID,
				"address":   held.Address,
				"createdAt": s.cfg.Now().UTC().Format(time.RFC3339),
			})
		}
		if len(held.HostKeys) > 0 && held.HostKeysUri == "" {
			observed := held.ReportedAt
			if observed == "" {
				observed = s.cfg.Now().UTC().Format(time.RFC3339)
			}
			keys := make([]any, 0, len(held.HostKeys))
			for _, key := range held.HostKeys {
				keys = append(keys, map[string]any{"type": key.Type, "publicKey": key.PublicKey, "fingerprint": key.Fingerprint})
			}
			s.publishReport(ctx, vm, contract, market.ComputeEventsVMHostKeysNSID, map[string]any{
				"$type":      market.ComputeEventsVMHostKeysNSID,
				"keys":       keys,
				"observedAt": observed,
				"createdAt":  s.cfg.Now().UTC().Format(time.RFC3339),
			})
		}
	}
}

func (s *Service) publishReport(ctx context.Context, vm *v1alpha1.ComputeVM, contract *v1alpha1.ComputeVMContract, collection string, payload map[string]any) {
	signed, err := market.SignRecord(payload, s.did, s.attestation)
	if err != nil {
		s.log.Warn("signing a ComputeVM market report failed", "computevm", vm.Name, "collection", collection, "err", err)
		return
	}
	payloadRef, err := s.records.CreateRecord(ctx, s.did, collection, signed)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("writing a ComputeVM market report failed", "computevm", vm.Name, "collection", collection, "err", err)
		}
		return
	}
	event, err := market.CreateEvent(ctx, s.records, s.did, market.EventOptions{
		Receipt:   market.RecordRef{URI: contract.ReceiptUri, CID: contract.ReceiptCid},
		Payload:   market.RecordRef{URI: payloadRef.URI, CID: payloadRef.CID},
		CreatedAt: s.cfg.Now().UTC().Format(time.RFC3339),
	}, s.attestation)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("writing a ComputeVM market event failed", "computevm", vm.Name, "err", err)
		}
		return
	}
	if contract.SubmitEventUrl != "" {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if err := s.callService(ctx, contract.SubmitEventUrl, market.SubmitEventNSID, market.SubmitEventNSID, body); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("submitting a ComputeVM market event failed", "computevm", vm.Name, "err", err)
			}
		}
	}
	report := v1alpha1.ComputeVMMarket{}
	if collection == market.ComputeEventsVMOnNetworkNSID {
		report.OnNetworkUri = payloadRef.URI
	} else {
		report.HostKeysUri = payloadRef.URI
	}
	if err := s.vms.PatchMarket(ctx, vm.Name, report); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("recording a ComputeVM market report URI failed", "computevm", vm.Name, "err", err)
		}
		return
	}
	s.log.Info("a ComputeVM market report is published", "computevm", vm.Name, "collection", collection, "payload", payloadRef.URI)
}
