package bidder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
		if vm.DeletionTimestamp != nil && (vm.Status.Market == nil || vm.Status.Market.Termination == nil) {
			continue
		}
		contract := vm.Spec.Contract
		if contract == nil || contract.ReceiptUri == "" || contract.ReceiptCid == "" {
			continue
		}
		if computevm.Releasing(vm) {
			if vm.Status.Market != nil && vm.Status.Market.Termination != nil && vm.Status.Market.Termination.PayloadUri == "" {
				s.reportTermination(ctx, vm, contract)
			}
			continue
		}
		if computevm.Ended(vm) {
			if vm.Status.Market == nil || vm.Status.Market.Termination == nil || vm.Status.Market.Termination.PayloadUri == "" {
				s.reportLostTermination(ctx, vm, contract)
			}
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

func (s *Service) reportTermination(ctx context.Context, vm *v1alpha1.ComputeVM, contract *v1alpha1.ComputeVMContract) {
	marker := vm.Status.Market.Termination
	seed := v1alpha1.ComputeVMTermination{Reason: marker.Reason, ObservedAt: marker.ObservedAt,
		ExitCode: marker.ExitCode, Request: marker.Request}
	if _, err := s.publishTermination(ctx, terminationPublish{
		VMName:         vm.Name,
		Receipt:        market.RecordRef{URI: contract.ReceiptUri, CID: contract.ReceiptCid},
		SubmitEventURL: contract.SubmitEventUrl,
		Seed:           seed,
	}); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("publishing the termination of a released ComputeVM failed", "computevm", vm.Name, "err", err)
		}
	}
}

func (s *Service) reportLostTermination(ctx context.Context, vm *v1alpha1.ComputeVM, contract *v1alpha1.ComputeVMContract) {
	seed := v1alpha1.ComputeVMTermination{
		Reason:     computevm.GoneReason(vm.Status.LostReason),
		ObservedAt: s.cfg.Now().UTC().Format(time.RFC3339),
	}
	if vm.Status.Market != nil && vm.Status.Market.Termination != nil {
		marker := vm.Status.Market.Termination
		if marker.Reason != "" {
			seed.Reason = marker.Reason
		}
		if marker.ObservedAt != "" {
			seed.ObservedAt = marker.ObservedAt
		}
		seed.ExitCode = marker.ExitCode
		seed.Request = marker.Request
	}
	if _, err := s.publishTermination(ctx, terminationPublish{
		VMName:         vm.Name,
		Receipt:        market.RecordRef{URI: contract.ReceiptUri, CID: contract.ReceiptCid},
		SubmitEventURL: contract.SubmitEventUrl,
		Seed:           seed,
	}); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("publishing the termination of a lost ComputeVM failed", "computevm", vm.Name, "err", err)
		}
	}
}

type terminationPublish struct {
	VMName string

	Receipt market.RecordRef

	SubmitEventURL string

	Seed v1alpha1.ComputeVMTermination

	Override bool
}

func (s *Service) publishTermination(ctx context.Context, req terminationPublish) (*v1alpha1.ComputeVMTermination, error) {
	if s.records == nil || s.attestation == nil || s.vms == nil || s.did == "" {
		return nil, errors.New("the bidder has no record store, attestation key or ComputeVM store")
	}
	s.terminationMu.Lock()
	defer s.terminationMu.Unlock()
	vm, err := s.vms.Get(ctx, req.VMName)
	if err != nil {
		return nil, err
	}
	if vm == nil {
		return nil, nil
	}
	if req.Receipt.URI == "" && vm.Spec.Contract != nil {
		req.Receipt = market.RecordRef{URI: vm.Spec.Contract.ReceiptUri, CID: vm.Spec.Contract.ReceiptCid}
	}
	if req.Receipt.URI == "" || req.Receipt.CID == "" {
		return nil, errors.New("the termination event names no receipt")
	}
	if req.SubmitEventURL == "" && vm.Spec.Contract != nil {
		req.SubmitEventURL = vm.Spec.Contract.SubmitEventUrl
	}
	var current *v1alpha1.ComputeVMTermination
	if vm.Status.Market != nil {
		current = vm.Status.Market.Termination
	}
	if current != nil && current.PayloadUri != "" && current.EventUri != "" {
		return current, nil
	}
	marker := v1alpha1.ComputeVMTermination{}
	if current != nil {
		marker = *current
	}
	if req.Override {
		if req.Seed.Reason != "" {
			marker.Reason = req.Seed.Reason
		}
		if req.Seed.ObservedAt != "" {
			marker.ObservedAt = req.Seed.ObservedAt
		}
		if req.Seed.ExitCode != nil {
			marker.ExitCode = req.Seed.ExitCode
		}
		if req.Seed.Request != nil {
			marker.Request = req.Seed.Request
		}
	} else {
		if marker.Reason == "" {
			marker.Reason = req.Seed.Reason
		}
		if marker.ObservedAt == "" {
			marker.ObservedAt = req.Seed.ObservedAt
		}
		if marker.ExitCode == nil {
			marker.ExitCode = req.Seed.ExitCode
		}
		if marker.Request == nil {
			marker.Request = req.Seed.Request
		}
	}
	if marker.Reason == "" || marker.ObservedAt == "" {
		return nil, errors.New("the termination event names no reason or observed time")
	}
	if err := s.vms.PatchMarket(ctx, req.VMName, v1alpha1.ComputeVMMarket{Termination: &marker}); err != nil {
		return nil, err
	}
	payloadRkey, eventRkey := terminationRecordKeys(req.Receipt.URI)
	payload := map[string]any{
		"$type":      market.ComputeEventsVMTerminatedNSID,
		"reason":     marker.Reason,
		"observedAt": marker.ObservedAt,
		"createdAt":  marker.ObservedAt,
	}
	if marker.ExitCode != nil {
		payload["exitCode"] = *marker.ExitCode
	}
	if marker.Request != nil && marker.Request.URI != "" {
		payload["request"] = market.NewStrongRef(marker.Request.URI, marker.Request.CID).Map()
	}
	payloadRec, payloadFound, err := s.getMarketRecord(ctx, market.ComputeEventsVMTerminatedNSID, payloadRkey)
	if err != nil {
		return nil, err
	}
	eventRec, eventFound, err := s.getMarketRecord(ctx, market.EventNSID, eventRkey)
	if err != nil {
		return nil, err
	}
	var payloadRef market.RecordRef
	if payloadFound {
		payloadRef = market.RecordRef{URI: payloadRec.URI, CID: payloadRec.CID}
	} else {
		signed, err := market.SignRecord(payload, s.did, s.attestation)
		if err != nil {
			return nil, fmt.Errorf("signing the termination payload: %w", err)
		}
		written, err := s.records.PutRecord(ctx, s.did, market.ComputeEventsVMTerminatedNSID, payloadRkey, signed)
		if err != nil {
			return nil, fmt.Errorf("writing the termination payload: %w", err)
		}
		payloadRef = market.RecordRef{URI: written.URI, CID: written.CID}
	}
	var event market.SignedRecord
	if eventFound {
		event = market.SignedRecord{URI: eventRec.URI, CID: eventRec.CID, Record: eventRec.Value}
	}
	if !payloadFound || !eventFound {
		event, err = market.PutEvent(ctx, s.records, s.did, eventRkey, market.EventOptions{
			Receipt:   req.Receipt,
			Payload:   payloadRef,
			CreatedAt: marker.ObservedAt,
		}, s.attestation)
		if err != nil {
			return nil, fmt.Errorf("writing the market event: %w", err)
		}
	}
	if req.SubmitEventURL != "" {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if err := s.callService(ctx, req.SubmitEventURL, market.SubmitEventNSID, market.SubmitEventNSID, body); err != nil {
			return nil, fmt.Errorf("submitting the market event: %w", err)
		}
	}
	verifiedPayload, err := s.records.GetRecord(ctx, s.did, market.ComputeEventsVMTerminatedNSID, payloadRkey)
	if err != nil {
		return nil, fmt.Errorf("verifying the termination payload: %w", err)
	}
	verifiedEvent, err := s.records.GetRecord(ctx, s.did, market.EventNSID, eventRkey)
	if err != nil {
		return nil, fmt.Errorf("verifying the market event: %w", err)
	}
	published := marker
	published.PayloadUri, published.PayloadCid = verifiedPayload.URI, verifiedPayload.CID
	published.EventUri, published.EventCid = verifiedEvent.URI, verifiedEvent.CID
	if err := s.vms.PatchMarket(ctx, req.VMName, v1alpha1.ComputeVMMarket{Termination: &published}); err != nil {
		return nil, err
	}
	s.log.Info("a ComputeVM termination is published", "computevm", req.VMName, "reason", published.Reason,
		"payload", published.PayloadUri, "event", published.EventUri)
	return &published, nil
}

func terminationRecordKeys(receiptURI string) (payloadRkey, eventRkey string) {
	payloadSum := sha256.Sum256([]byte("terminated:" + receiptURI))
	eventSum := sha256.Sum256([]byte("terminated:event:" + receiptURI))
	return "termination-payload-" + hex.EncodeToString(payloadSum[:16]),
		"termination-event-" + hex.EncodeToString(eventSum[:16])
}

func (s *Service) getMarketRecord(ctx context.Context, collection, rkey string) (market.ResolvedRecord, bool, error) {
	if s.records == nil || s.did == "" {
		return market.ResolvedRecord{}, false, nil
	}
	rec, err := s.records.GetRecord(ctx, s.did, collection, rkey)
	if err != nil {
		if errors.Is(err, market.ErrNotFound) {
			return market.ResolvedRecord{}, false, nil
		}
		return market.ResolvedRecord{}, false, err
	}
	return rec, true, nil
}

func (s *Service) publishReport(ctx context.Context, vm *v1alpha1.ComputeVM, contract *v1alpha1.ComputeVMContract, collection string, payload map[string]any) {
	payloadRef, event, err := s.publishMarketEvent(ctx, collection, payload,
		market.RecordRef{URI: contract.ReceiptUri, CID: contract.ReceiptCid}, contract.SubmitEventUrl)
	if err != nil {
		s.log.Warn("publishing a ComputeVM market report failed", "computevm", vm.Name, "collection", collection, "err", err)
		if payloadRef.URI == "" || event.URI == "" {
			return
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

func (s *Service) publishMarketEvent(ctx context.Context, collection string, payload map[string]any, receipt market.RecordRef, submitEventURL string) (market.StrongRef, market.SignedRecord, error) {
	signed, err := market.SignRecord(payload, s.did, s.attestation)
	if err != nil {
		return market.StrongRef{}, market.SignedRecord{}, fmt.Errorf("signing the record: %w", err)
	}
	payloadRef, err := s.records.CreateRecord(ctx, s.did, collection, signed)
	if err != nil {
		return market.StrongRef{}, market.SignedRecord{}, fmt.Errorf("writing the payload record: %w", err)
	}
	event, err := market.CreateEvent(ctx, s.records, s.did, market.EventOptions{
		Receipt:   receipt,
		Payload:   market.RecordRef{URI: payloadRef.URI, CID: payloadRef.CID},
		CreatedAt: s.cfg.Now().UTC().Format(time.RFC3339),
	}, s.attestation)
	if err != nil {
		return payloadRef, market.SignedRecord{}, fmt.Errorf("writing the market event: %w", err)
	}
	if submitEventURL != "" {
		body := map[string]any{"uri": event.URI, "cid": event.CID, "record": event.Record}
		if err := s.callService(ctx, submitEventURL, market.SubmitEventNSID, market.SubmitEventNSID, body); err != nil {
			return payloadRef, event, fmt.Errorf("submitting the market event: %w", err)
		}
	}
	return payloadRef, event, nil
}
