package bidder

import (
	"context"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

func (b *Bidder) Run(ctx context.Context) {
	defer b.wg.Wait()
	b.wg.Go(func() { b.bindBadgeBlueKey(ctx) })
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

func (b *Bidder) withdraw(ctx context.Context) {
	page, err := b.records.ListRecords(ctx, b.cfg.DID, market.OfferingNSID, 20, "")
	if err != nil {
		b.log.Warn("listing the market offerings to withdraw failed", "err", err)
		return
	}
	for _, rec := range page.Records {
		if err := b.records.DeleteRecord(ctx, b.cfg.DID, market.OfferingNSID, participant.LastSegment(rec.URI)); err != nil {
			b.log.Warn("withdrawing the market offering failed", "offering", rec.URI, "err", err)
			continue
		}
		b.log.Info("the bidder offering is withdrawn while its participation is suspended", "offering", rec.URI)
	}
}

func (b *Bidder) setOffering(uri string) {
	b.offerMu.Lock()
	b.offeringURI = uri
	b.offerMu.Unlock()
}

func (b *Bidder) ensureOffering(ctx context.Context) error {
	b.offerWrite.Lock()
	defer b.offerWrite.Unlock()
	return b.ensureOfferingLocked(ctx)
}

func (b *Bidder) ensureOfferingLocked(ctx context.Context) error {
	appliesTo := []string{market.ComputeVMNSID}
	now := b.now().UTC().Format(time.RFC3339)
	page, err := b.records.ListRecords(ctx, b.cfg.DID, market.OfferingNSID, 20, "")
	if err != nil {
		return err
	}
	if len(page.Records) > 0 {
		rec := page.Records[0]
		b.dropExtraOfferings(ctx, page.Records[1:])
		offering, err := market.ParseOffering(rec.Value)
		if err != nil {
			return err
		}
		if offering.EndpointURL == b.cfg.EndpointURL && sameStrings(offering.AppliesTo, appliesTo) {
			b.setOffering(rec.URI)
			return nil
		}
		record, err := market.BuildOffering(b.cfg.EndpointURL, appliesTo, offering.CreatedAt, now)
		if err != nil {
			return err
		}
		if _, err := b.putRecord(ctx, participant.LastSegment(rec.URI), record); err != nil {
			return err
		}
		b.setOffering(rec.URI)
		b.log.Info("the bidder offering is corrected", "offering", rec.URI, "endpoint", b.cfg.EndpointURL)
		return nil
	}
	record, err := market.BuildOffering(b.cfg.EndpointURL, appliesTo, now, now)
	if err != nil {
		return err
	}
	ref, err := b.putRecord(ctx, syntax.NewTIDNow(0).String(), record)
	if err != nil {
		return err
	}
	b.setOffering(ref.URI)
	b.log.Info("the bidder offering is written", "offering", ref.URI, "endpoint", b.cfg.EndpointURL)
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
	page, err := b.records.ListRecords(ctx, b.cfg.DID, market.OfferingNSID, 20, "")
	if err != nil {
		return err
	}
	if len(page.Records) == 0 {
		return b.ensureOfferingLocked(ctx)
	}
	rec := page.Records[0]
	b.dropExtraOfferings(ctx, page.Records[1:])
	offering, err := market.ParseOffering(rec.Value)
	if err != nil {
		return err
	}
	record, err := market.BuildOffering(b.cfg.EndpointURL, []string{market.ComputeVMNSID}, offering.CreatedAt, b.now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if _, err := b.putRecord(ctx, participant.LastSegment(rec.URI), record); err != nil {
		return err
	}
	b.log.Info("the bidder offering is refreshed for the relay", "offering", rec.URI)
	return nil
}

func (b *Bidder) dropExtraOfferings(ctx context.Context, extra []market.ListedRecord) {
	for _, rec := range extra {
		if err := b.records.DeleteRecord(ctx, b.cfg.DID, market.OfferingNSID, participant.LastSegment(rec.URI)); err != nil {
			b.log.Warn("deleting a second market offering failed", "offering", rec.URI, "err", err)
			continue
		}
		b.log.Info("a second market offering is deleted; the bidder keeps one", "offering", rec.URI)
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
