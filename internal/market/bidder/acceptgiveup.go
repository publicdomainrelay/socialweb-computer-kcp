package bidder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

type acceptTry struct {
	first time.Time

	tries int
}

// unresolvedAccept is what a provision intent does when its accept record cannot be read: record not found (the requester deleted
// the accept, or it never existed) or another error (the PDS did not answer). It is retried by the outbox's own backoff until
// AcceptGiveUpTries attempts have failed and the intent is old enough (AcceptNotFoundGiveUpAfter, AcceptUnreachableGiveUpAfter);
// then the accept is abandoned: remembered (never provisioned or retried again), its capacity reservation released, one
// bidder_accept_abandoned logged, and the intent ends Obsolete (the outbox does not retry it, across restarts too).
func (b *Bidder) unresolvedAccept(request participant.OutboxRequest, acceptURI string, cause error) error {
	notFound := errors.Is(cause, market.ErrNotFound)
	kind, bound := "unreachable", AcceptUnreachableGiveUpAfter
	if notFound {
		kind, bound = "notFound", AcceptNotFoundGiveUpAfter
	}
	now := b.now()
	b.acceptMu.Lock()
	if b.acceptTries == nil {
		b.acceptTries = map[string]*acceptTry{}
	}
	t := b.acceptTries[acceptURI]
	if t == nil {
		t = &acceptTry{first: now}
		b.acceptTries[acceptURI] = t
	}
	t.tries++
	first := t.first
	if !request.CreatedAt.IsZero() && request.CreatedAt.Before(first) {
		first = request.CreatedAt
	}
	tries, age := t.tries, now.Sub(first).Round(time.Second)
	giveUp := tries >= AcceptGiveUpTries && age >= bound
	var rfpURI string
	if giveUp {
		if b.acceptAbandoned == nil {
			b.acceptAbandoned = map[string]bool{}
		}
		b.acceptAbandoned[acceptURI] = true
		delete(b.acceptTries, acceptURI)
		rfpURI = b.acceptRFPs[acceptURI]
		delete(b.acceptRFPs, acceptURI)
	}
	b.acceptMu.Unlock()
	if !giveUp {
		// the outbox retries within seconds of each failure: the first tries and every 50th are logged, not each of hundreds
		if tries <= 3 || tries%50 == 0 {
			b.log.Info("bidder_accept_unresolved", "accept", acceptURI, "kind", kind, "tries", tries, "age", age.String(), "giveUpAfter", bound.String(), "err", cause.Error())
		}
		return &acceptUnresolvedError{cause: fmt.Errorf("bidder: resolving the accept: %w", cause)}
	}
	if rfpURI != "" {
		b.unreserve(rfpURI)
	}
	b.log.Warn("bidder_accept_abandoned", "accept", acceptURI, "kind", kind, "tries", tries, "age", age.String(), "reservationReleased", rfpURI != "", "err", cause.Error())
	return &acceptUnresolvedError{abandoned: true, cause: fmt.Errorf("bidder: the accept stayed unresolvable (%s) for %s over %d tries and is abandoned: %w: %w", kind, age, tries, cause, participant.ErrObsolete)}
}

// acceptUnresolvedError marks the errors unresolvedAccept already logged, so applyProvision does not log them again.
type acceptUnresolvedError struct {
	abandoned bool

	cause error
}

func (e *acceptUnresolvedError) Error() string { return e.cause.Error() }

func (e *acceptUnresolvedError) Unwrap() error { return e.cause }

func (b *Bidder) acceptGivenUp(acceptURI string) bool {
	b.acceptMu.Lock()
	defer b.acceptMu.Unlock()
	return b.acceptAbandoned[acceptURI]
}

func (b *Bidder) rememberAcceptRFP(acceptURI, rfpURI string) {
	b.acceptMu.Lock()
	defer b.acceptMu.Unlock()
	if b.acceptRFPs == nil {
		b.acceptRFPs = map[string]string{}
	}
	b.acceptRFPs[acceptURI] = rfpURI
}

func (b *Bidder) forgetAccept(acceptURI string) {
	b.acceptMu.Lock()
	defer b.acceptMu.Unlock()
	delete(b.acceptTries, acceptURI)
	delete(b.acceptRFPs, acceptURI)
}

// An RFP a bid is decided on can be deleted by its requester (a waiting round deletes its RFP) while the bid still resolves it:
// the bid is failed (RFPGone, one "a bid failed" line) after RFPGiveUpTries failed reads once its RFP was seen RFPNotFoundGiveUpAfter
// ago (replication lag is a minute or two; the bid window closes the bid at 10 min anyway). Until then the retry is one quiet line.
const (
	RFPGiveUpTries = 3

	RFPNotFoundGiveUpAfter = 2 * time.Minute
)

type rfpUnresolvedError struct {
	tries int

	cause error
}

func (e *rfpUnresolvedError) Error() string { return e.cause.Error() }

func (e *rfpUnresolvedError) Unwrap() error { return e.cause }

// rfpGone counts a not-found read of the bid's RFP; true when the bid is given up (failed) now.
func (b *Bidder) rfpGone(ctx context.Context, bid *v1alpha1.Bid, now time.Time, cause error) (bool, int) {
	key := "rfp:" + bid.Name
	b.acceptMu.Lock()
	if b.acceptTries == nil {
		b.acceptTries = map[string]*acceptTry{}
	}
	t := b.acceptTries[key]
	if t == nil {
		t = &acceptTry{first: now}
		b.acceptTries[key] = t
	}
	t.tries++
	tries, first := t.tries, t.first
	if seen, err := time.Parse(time.RFC3339, bid.Status.Timings.RfpSeenAt); err == nil && seen.Before(first) {
		first = seen
	}
	giveUp := tries >= RFPGiveUpTries && now.Sub(first) >= RFPNotFoundGiveUpAfter
	if giveUp {
		delete(b.acceptTries, key)
	}
	b.acceptMu.Unlock()
	if !giveUp {
		return false, tries
	}
	b.failBid(ctx, bid, "RFPGone", fmt.Sprintf("the RFP is gone (deleted by its requester): not found over %d reads in %s: %s", tries, now.Sub(first).Round(time.Second), cause), now)
	return true, tries
}
