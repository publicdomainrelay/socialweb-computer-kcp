package bidder

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

type marketLoopStats struct {
	Offerings int

	Heartbeats int

	LastError string

	OfferingURI string

	RelayVisibility *market.RelayVisibilityResult

	UpdatedAt string
}

type marketLoop struct {
	mu sync.Mutex

	wg sync.WaitGroup

	records *market.RepoStore

	signer *market.Signer

	did string

	endpointURL string

	relays []string

	offeringRefresh time.Duration

	offeringRefreshDisabled bool

	heartbeat time.Duration

	log *slog.Logger

	now func() time.Time

	statsV marketLoopStats

	offeringURI string

	offeringRKey string
}

func (l *marketLoop) logf() *slog.Logger {
	if l == nil || l.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l.log
}

func (l *marketLoop) timestamp() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *marketLoop) stats() marketLoopStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.statsV
}

func (l *marketLoop) setError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.statsV.LastError = ""
		return
	}
	l.statsV.LastError = err.Error()
	l.statsV.UpdatedAt = l.timestamp().UTC().Format(time.RFC3339)
}

func (l *marketLoop) run(ctx context.Context) {
	if l == nil || l.records == nil || l.did == "" {
		l.logf().Info("the bidder market loop waits for a record store and a DID")
		return
	}
	defer l.wg.Wait()
	if err := l.ensureOffering(ctx); err != nil {
		l.logf().Warn("creating the market offering failed", "err", err)
		l.setError(err)
	} else {
		l.setError(nil)
	}
	if err := l.beat(ctx); err != nil {
		l.logf().Warn("sending the market heartbeat failed", "err", err)
	}
	l.verifyVisibility(ctx)
	heartbeatTicker := time.NewTicker(l.heartbeat)
	defer heartbeatTicker.Stop()
	var refreshTicker *time.Ticker
	var refreshCh <-chan time.Time
	if !l.offeringRefreshDisabled {
		refreshTicker = time.NewTicker(l.offeringRefresh)
		defer refreshTicker.Stop()
		refreshCh = refreshTicker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := l.beat(ctx); err != nil {
				if ctx.Err() == nil {
					l.logf().Warn("sending the market heartbeat failed", "err", err)
				}
				l.setError(err)
			}
		case <-refreshCh:
			if err := l.ensureOffering(ctx); err != nil {
				if ctx.Err() == nil {
					l.logf().Warn("refreshing the market offering failed", "err", err)
				}
				l.setError(err)
				continue
			}
			l.setError(nil)
			l.verifyVisibility(ctx)
		}
	}
}

func (l *marketLoop) withdraw(ctx context.Context) {
	if l == nil || l.records == nil || l.did == "" {
		return
	}
	page, err := l.records.ListRecords(ctx, l.did, market.OfferingNSID, 20, "")
	if err != nil {
		l.logf().Warn("listing the market offerings to withdraw failed", "err", err)
		l.setError(err)
		return
	}
	for _, rec := range page.Records {
		if err := l.records.DeleteRecord(ctx, l.did, market.OfferingNSID, lastSegment(rec.URI)); err != nil {
			l.logf().Warn("withdrawing the market offering failed", "offering", rec.URI, "err", err)
			l.setError(err)
			continue
		}
		l.logf().Info("the bidder offering is withdrawn while its participation is suspended", "offering", rec.URI)
	}
}

func (l *marketLoop) ensureOffering(ctx context.Context) error {
	appliesTo := []string{market.ComputeVMNSID}
	now := l.timestamp().UTC().Format(time.RFC3339)
	page, err := l.records.ListRecords(ctx, l.did, market.OfferingNSID, 20, "")
	if err != nil {
		return err
	}
	if len(page.Records) > 0 {
		rec := page.Records[0]
		rkey := lastSegment(rec.URI)
		offering, err := market.ParseOffering(rec.Value)
		if err != nil {
			return err
		}
		matched := offering.EndpointURL == l.endpointURL && sameStrings(offering.AppliesTo, appliesTo)
		if matched {
			l.mu.Lock()
			l.offeringURI, l.offeringRKey = rec.URI, rkey
			l.statsV.OfferingURI = rec.URI
			l.mu.Unlock()
			return nil
		}
		record, err := market.BuildOffering(l.endpointURL, appliesTo, offering.CreatedAt, now)
		if err != nil {
			return err
		}
		if _, err := l.records.PutRecord(ctx, l.did, market.OfferingNSID, rkey, record); err != nil {
			return err
		}
		l.mu.Lock()
		l.offeringURI, l.offeringRKey = rec.URI, rkey
		l.statsV.OfferingURI = rec.URI
		l.statsV.Offerings++
		l.statsV.UpdatedAt = now
		l.mu.Unlock()
		l.logf().Info("the bidder offering is corrected", "offering", rec.URI, "endpoint", l.endpointURL)
		return nil
	}
	rkey := syntax.NewTIDNow(0).String()
	record, err := market.BuildOffering(l.endpointURL, appliesTo, now, now)
	if err != nil {
		return err
	}
	ref, err := l.records.PutRecord(ctx, l.did, market.OfferingNSID, rkey, record)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.offeringURI, l.offeringRKey = ref.URI, rkey
	l.statsV.OfferingURI = ref.URI
	l.statsV.Offerings++
	l.statsV.UpdatedAt = now
	l.mu.Unlock()
	l.logf().Info("the bidder offering is written", "offering", ref.URI, "endpoint", l.endpointURL)
	return nil
}

func (l *marketLoop) touchOffering(ctx context.Context) error {
	page, err := l.records.ListRecords(ctx, l.did, market.OfferingNSID, 20, "")
	if err != nil {
		return err
	}
	if len(page.Records) == 0 {
		return l.ensureOffering(ctx)
	}
	rec := page.Records[0]
	offering, err := market.ParseOffering(rec.Value)
	if err != nil {
		return err
	}
	now := l.timestamp().UTC().Format(time.RFC3339)
	record, err := market.BuildOffering(l.endpointURL, []string{market.ComputeVMNSID}, offering.CreatedAt, now)
	if err != nil {
		return err
	}
	rkey := lastSegment(rec.URI)
	if _, err := l.records.PutRecord(ctx, l.did, market.OfferingNSID, rkey, record); err != nil {
		return err
	}
	l.logf().Info("the bidder offering is refreshed for the relay", "offering", rec.URI)
	return nil
}

func (l *marketLoop) beat(ctx context.Context) error {
	l.mu.Lock()
	offering := l.offeringURI
	l.mu.Unlock()
	if offering == "" {
		return nil
	}
	record, err := market.BuildHeartbeat(offering, l.timestamp().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if _, err := l.records.PutRecord(ctx, l.did, market.HeartbeatNSID, market.HeartbeatRKey, record); err != nil {
		return err
	}
	l.mu.Lock()
	l.statsV.Heartbeats++
	l.statsV.UpdatedAt = l.timestamp().UTC().Format(time.RFC3339)
	l.mu.Unlock()
	return nil
}

func (l *marketLoop) verifyVisibility(ctx context.Context) {
	if len(l.relays) == 0 || l.did == "" {
		return
	}
	l.wg.Go(func() {
		result := market.VerifyRelayVisibility(ctx, market.RelayVisibilityOptions{
			RelayURLs:  append([]string(nil), l.relays...),
			BidderDID:  l.did,
			Collection: market.OfferingNSID,
			Log:        func(level, msg string, fields map[string]any) { l.logf().Info(msg, "fields", fields) },
		})
		l.mu.Lock()
		l.statsV.RelayVisibility = &result
		l.statsV.UpdatedAt = l.timestamp().UTC().Format(time.RFC3339)
		l.mu.Unlock()
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
