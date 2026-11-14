package bidder

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

func giveUpBidder(now *time.Time, out *bytes.Buffer) *Bidder {
	return &Bidder{log: slog.New(slog.NewJSONHandler(out, nil)), now: func() time.Time { return *now }}
}

// An accept that stays unresolvable is abandoned once: its reservation is released, the abandon is logged once, the intent ends
// Obsolete and a later attempt is silent and obsolete too.
func TestUnresolvedAcceptAbandonReleasesReservation(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	b := giveUpBidder(&now, &out)
	const accept, rfp = "at://did:plc:r/com.publicdomainrelay.temp.market.accept/a1", "at://did:plc:r/com.publicdomainrelay.temp.market.rfp/r1"
	b.reserved = map[string]reservation{rfp: {cpus: 4, mib: 1024, until: now.Add(AcceptReserveHold)}}
	b.rememberAcceptRFP(accept, rfp)
	request := participant.OutboxRequest{CreatedAt: now}
	notFound := fmt.Errorf("record: %w", market.ErrNotFound)

	for i := 1; i < AcceptGiveUpTries; i++ {
		err := b.unresolvedAccept(request, accept, notFound)
		if errors.Is(err, participant.ErrObsolete) {
			t.Fatalf("try %d: abandoned too early: %v", i, err)
		}
		now = now.Add(time.Second)
	}
	if _, held := b.reserved[rfp]; !held {
		t.Fatal("the reservation was released before the accept was abandoned")
	}
	now = now.Add(AcceptNotFoundGiveUpAfter)
	err := b.unresolvedAccept(request, accept, notFound)
	if !errors.Is(err, participant.ErrObsolete) {
		t.Fatalf("the accept was not abandoned after %d tries and %s: %v", AcceptGiveUpTries, AcceptNotFoundGiveUpAfter, err)
	}
	if _, held := b.reserved[rfp]; held {
		t.Fatal("the reservation of the abandoned accept is still held")
	}
	if !b.acceptGivenUp(accept) {
		t.Fatal("the abandoned accept is not remembered")
	}
	if n := strings.Count(out.String(), "bidder_accept_abandoned"); n != 1 {
		t.Fatalf("%d bidder_accept_abandoned lines, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), `"reservationReleased":true`) {
		t.Fatalf("the abandon line does not say the reservation was released:\n%s", out.String())
	}
}

// Transient errors (a PDS that does not answer) are kept for the longer bound.
func TestUnresolvedAcceptTransientWaitsLonger(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	b := giveUpBidder(&now, &out)
	const accept = "at://did:plc:r/com.publicdomainrelay.temp.market.accept/a2"
	request := participant.OutboxRequest{CreatedAt: now}
	down := errors.New("HTTP 503")
	for i := 0; i < AcceptGiveUpTries+2; i++ {
		now = now.Add(AcceptNotFoundGiveUpAfter)
		if err := b.unresolvedAccept(request, accept, down); errors.Is(err, participant.ErrObsolete) && now.Sub(request.CreatedAt) < AcceptUnreachableGiveUpAfter {
			t.Fatalf("a transient error was abandoned at %s, before %s", now.Sub(request.CreatedAt), AcceptUnreachableGiveUpAfter)
		}
	}
	now = now.Add(AcceptUnreachableGiveUpAfter)
	if err := b.unresolvedAccept(request, accept, down); !errors.Is(err, participant.ErrObsolete) {
		t.Fatalf("a transient error was not abandoned after %s: %v", AcceptUnreachableGiveUpAfter, err)
	}
	if !strings.Contains(out.String(), `"kind":"unreachable"`) {
		t.Fatalf("the abandon of a transient error is not reported as unreachable:\n%s", out.String())
	}
}
