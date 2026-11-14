package bidder

import (
	"errors"
	"fmt"
	"time"

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
