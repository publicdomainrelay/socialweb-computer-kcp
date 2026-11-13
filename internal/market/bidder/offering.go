package bidder

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

func (b *Bidder) Run(ctx context.Context) {
	defer b.wg.Wait()
	b.wg.Go(func() { b.bindBadgeBlueKey(ctx) })
	b.wg.Go(func() { b.watchMemory(ctx) })
	if b.cfg.Suspend {
		b.withdraw(ctx)
		return
	}
	b.serveMarket(ctx)
}

func (b *Bidder) bindBadgeBlueKey(ctx context.Context) {
	b.badgeMu.Lock()
	defer b.badgeMu.Unlock()
	if b.badgeBound.Load() {
		return
	}
	key, err := b.signer.PublicDID()
	if err != nil {
		b.log.Warn("reading the attestation key DID failed", "err", err)
		return
	}
	if _, err := market.CreateBadgeBlueKeysRecord(ctx, b.records, market.BadgeBlueKeysRecordOptions{
		DID:     b.cfg.DID,
		KeyID:   key,
		Service: "bidder_attestation",
	}); err != nil {
		if ctx.Err() == nil {
			b.log.Warn("binding the badge.blue key failed", "err", err)
		}
		return
	}
	b.badgeBound.Store(true)
	b.log.Info("the bidder's badge.blue attestation key is bound", "did", b.cfg.DID, "key", key)
}

func (b *Bidder) serveMarket(ctx context.Context) {
	if !b.awaitImage(ctx) {
		return
	}
	if err := b.ensureOffering(ctx); err != nil {
		b.log.Warn("creating the market offering failed", "err", err)
	}
	if err := b.beat(ctx); err != nil {
		b.log.Warn("sending the market heartbeat failed", "err", err)
	}
	b.verifyVisibility(ctx)
	if b.cfg.Heartbeat <= 0 {
		b.log.Warn("the market heartbeat is disabled", "period", b.cfg.Heartbeat)
		return
	}
	heartbeat := time.NewTicker(b.cfg.Heartbeat)
	defer heartbeat.Stop()
	refreshed := b.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
		}
		if !b.badgeBound.Load() {
			b.bindBadgeBlueKey(ctx)
		}
		if b.currentOffering() == "" {
			if err := b.ensureOffering(ctx); err != nil && ctx.Err() == nil {
				b.log.Warn("creating the market offering failed", "err", err)
			}
		}
		if err := b.beat(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("sending the market heartbeat failed", "err", err)
		}
		if b.cfg.OfferingRefresh <= 0 || b.now().Sub(refreshed) < b.cfg.OfferingRefresh {
			continue
		}
		refreshed = b.now()
		if err := b.ensureOffering(ctx); err != nil {
			if ctx.Err() == nil {
				b.log.Warn("refreshing the market offering failed", "err", err)
			}
			continue
		}
		b.verifyVisibility(ctx)
	}
}

func (b *Bidder) awaitImage(ctx context.Context) bool {
	if b.cfg.ImageReady == nil || !b.servesKind(string(v1alpha1.ComputeKindVM)) {
		return true
	}
	for withdrawn := false; ; {
		err := b.cfg.ImageReady(ctx)
		if err == nil {
			b.imageErr.Store(nil)
			b.log.Info("bidder_guest_image_ready")
			return true
		}
		msg := err.Error()
		b.imageErr.Store(&msg)
		b.log.Error("bidder_guest_image_unready: the pinned guest image does not verify; the offering is withdrawn and no RFP is bid until it does", "err", msg)
		if !withdrawn {
			b.withdraw(ctx)
			withdrawn = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(ImageRetry):
		}
	}
}

func (b *Bidder) withdraw(ctx context.Context) {
	b.offerWrite.Lock()
	defer b.offerWrite.Unlock()
	prior, readErr := b.selfOffering(ctx)
	if readErr != nil {
		b.log.Warn("reading the market offering to withdraw failed; it is withdrawn anyway", "err", readErr)
	}
	endpoints := b.offeredEndpoints(b.ensureListen(ctx))
	if prior != nil && market.OfferingWithdrawn(*prior) && sameEndpoints(prior.Endpoints, endpoints) {
		return
	}
	createdAt := ""
	if prior != nil {
		createdAt = prior.CreatedAt
	}
	record, err := market.BuildWithdrawnOffering(createdAt, b.now().UTC().Format(time.RFC3339), endpoints)
	if err != nil {
		b.log.Warn("building the withdrawn market offering failed", "err", err)
		return
	}
	written := false
	if prior != nil || readErr != nil {
		written = b.withdrawSelf(ctx, record)
	}
	if err := b.findLegacyOffering(ctx); err != nil {
		b.log.Warn("reading the heartbeat to withdraw the legacy market offering failed", "err", err)
		if !written && prior == nil {
			b.withdrawSelf(ctx, record)
		}
		return
	}
	if b.legacyOffering == "" {
		return
	}
	if !written {
		b.withdrawSelf(ctx, record)
	}
	b.mirrorLegacy(ctx, record)
}

func (b *Bidder) withdrawSelf(ctx context.Context, record market.Offering) bool {
	ref, err := b.putRecord(ctx, market.OfferingRKey, record)
	if err != nil {
		b.log.Warn("withdrawing the market offering failed", "err", err)
		return false
	}
	b.log.Info("the bidder offering is withdrawn while its participation is suspended", "offering", ref.URI)
	return true
}

func (b *Bidder) setOffering(uri string) {
	b.offerMu.Lock()
	b.offeringURI = uri
	b.offerMu.Unlock()
}

func (b *Bidder) ensureOffering(ctx context.Context) error {
	b.offerWrite.Lock()
	defer b.offerWrite.Unlock()
	return b.ensureOfferingLocked(ctx, false)
}

func (b *Bidder) ensureOfferingLocked(ctx context.Context, touch bool) error {
	appliesTo := []string{market.ComputeVMNSID}
	now := b.now().UTC().Format(time.RFC3339)
	prior, err := b.selfOffering(ctx)
	if err != nil {
		return err
	}
	if err := b.findLegacyOffering(ctx); err != nil {
		return err
	}
	endpoints := b.offeredEndpoints(b.ensureListen(ctx))
	limits := b.vmLimits()
	createdAt := now
	if prior != nil {
		createdAt = prior.CreatedAt
		if !touch && prior.WithdrawnAt == "" && sameEndpoints(prior.Endpoints, endpoints) && sameStrings(prior.AppliesTo, appliesTo) &&
			prior.MaxCpus == limits.cpus && prior.MaxMem == limits.mem() && prior.NestedVirt == limits.nestedVirt && prior.Images == limits.images {
			if b.legacyStale {
				b.mirrorLegacy(ctx, *prior)
			}
			b.setOffering(b.heartbeatOffering())
			return nil
		}
	}
	record, err := market.BuildOffering(endpoints, appliesTo, nil, createdAt, now)
	if err != nil {
		return err
	}
	record.MaxCpus, record.MaxMem, record.NestedVirt, record.Images = limits.cpus, limits.mem(), limits.nestedVirt, limits.images
	uri, err := b.putOffering(ctx, record)
	if err != nil {
		return err
	}
	b.setOffering(b.heartbeatOffering())
	switch {
	case touch:
		b.log.Info("the bidder offering is refreshed for the relay", "offering", uri, "legacy", b.legacyOffering)
	case prior == nil:
		b.log.Info("the bidder offering is written", "offering", uri, "endpoints", endpoints, "legacy", b.legacyOffering)
	default:
		b.log.Info("the bidder offering is corrected", "offering", uri, "endpoints", endpoints, "legacy", b.legacyOffering)
	}
	return nil
}

func (b *Bidder) TouchOffering(ctx context.Context) {
	if b.cfg.Suspend {
		return
	}
	if err := b.touchOffering(ctx); err != nil {
		b.log.Warn("republishing the offering after the relay crawl failed", "err", err.Error())
	}
}

func (b *Bidder) touchOffering(ctx context.Context) error {
	b.offerWrite.Lock()
	defer b.offerWrite.Unlock()
	return b.ensureOfferingLocked(ctx, true)
}

func (b *Bidder) selfOffering(ctx context.Context) (*market.Offering, error) {
	rec, err := b.records.GetRecord(ctx, b.cfg.DID, market.OfferingNSID, market.OfferingRKey)
	if errors.Is(err, market.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	offering, err := market.ParseOffering(rec.Value)
	if err != nil {
		b.log.Warn("the bidder's own market offering is invalid; it is written again", "offering", rec.URI, "err", err)
		return nil, nil
	}
	return offering, nil
}

func (b *Bidder) findLegacyOffering(ctx context.Context) error {
	if b.legacyChecked {
		return nil
	}
	rkey, err := market.HeartbeatOfferingRKey(ctx, b.records, b.cfg.DID)
	switch {
	case errors.Is(err, market.ErrNotFound), errors.Is(err, market.ErrInvalidRecord):
		b.legacyChecked = true
		return nil
	case err != nil:
		return err
	}
	if _, err := syntax.ParseTID(rkey); rkey == market.OfferingRKey || err != nil {
		b.legacyChecked = true
		return nil
	}
	rec, err := b.records.GetRecord(ctx, b.cfg.DID, market.OfferingNSID, rkey)
	switch {
	case errors.Is(err, market.ErrNotFound):
	case err != nil:
		return err
	case rec.CID == "":
		b.log.Warn("the legacy market offering has no cid; it is not mirrored", "legacy", rkey)
	default:
		b.legacyOffering = rkey
		b.legacyCID = rec.CID
		b.log.Info("the bidder mirrors its offering to the legacy record its heartbeat named", "legacy", rkey)
	}
	b.legacyChecked = true
	return nil
}

func (b *Bidder) heartbeatOffering() string {
	if b.legacyOffering != "" {
		return "at://" + b.cfg.DID + "/" + market.OfferingNSID + "/" + b.legacyOffering
	}
	return market.OfferingURI(b.cfg.DID)
}

func (b *Bidder) putOffering(ctx context.Context, record market.Offering) (string, error) {
	ref, err := b.putRecord(ctx, market.OfferingRKey, record)
	if err != nil {
		return "", err
	}
	b.mirrorLegacy(ctx, record)
	return ref.URI, nil
}

func (b *Bidder) mirrorLegacy(ctx context.Context, record market.Offering) {
	if b.legacyOffering == "" {
		return
	}
	value, err := market.EncodeRecord(record)
	if err != nil {
		b.log.Warn("encoding the legacy market offering failed", "legacy", b.legacyOffering, "err", err)
		b.legacyStale = true
		return
	}
	ref, err := b.records.PutRecordSwap(ctx, b.cfg.DID, market.OfferingNSID, b.legacyOffering, value, b.legacyCID)
	switch {
	case errors.Is(err, market.ErrSwapMismatch), errors.Is(err, market.ErrNotFound):
		b.log.Warn("the legacy market offering changed or is gone; the mirror stops and the heartbeat names self", "legacy", b.legacyOffering, "err", err)
		b.legacyOffering, b.legacyCID, b.legacyStale = "", "", false
	case err != nil:
		if ctx.Err() == nil {
			b.log.Warn("updating the legacy market offering failed", "legacy", b.legacyOffering, "err", err)
		}
		b.legacyStale = true
	default:
		b.legacyCID, b.legacyStale = ref.CID, false
	}
}

func (b *Bidder) currentOffering() string {
	b.offerMu.Lock()
	defer b.offerMu.Unlock()
	return b.offeringURI
}

func (b *Bidder) beat(ctx context.Context) error {
	b.offerMu.Lock()
	offering := b.offeringURI
	b.offerMu.Unlock()
	if offering == "" {
		return nil
	}
	record, err := market.BuildHeartbeat(offering, b.now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	_, err = b.putRecord(ctx, market.HeartbeatRKey, record)
	return err
}

func (b *Bidder) putRecord(ctx context.Context, rkey string, record market.Record) (market.StrongRef, error) {
	value, err := market.EncodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	return b.records.PutRecord(ctx, b.cfg.DID, record.NSID(), rkey, value)
}

func (b *Bidder) verifyVisibility(ctx context.Context) {
	if len(b.cfg.Relays) == 0 {
		return
	}
	relays := append([]string(nil), b.cfg.Relays...)
	b.wg.Go(func() {
		market.VerifyRelayVisibility(ctx, market.RelayVisibilityOptions{
			RelayURLs:  relays,
			BidderDID:  b.cfg.DID,
			Collection: market.OfferingNSID,
			Log:        func(level, msg string, fields map[string]any) { b.log.Info(msg, "fields", fields) },
		})
	})
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func sameEndpoints(a, b []market.Endpoint) bool {
	return slices.EqualFunc(a, b, func(x, y market.Endpoint) bool { return x.Kind == y.Kind && x.Address == y.Address })
}
