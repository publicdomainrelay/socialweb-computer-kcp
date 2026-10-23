//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	eventsvc "github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

func TestMarketAccountAndTransportMatrix(t *testing.T) {
	accounts := []string{
		"managed-associated",
		"managed-unassociated",
		"existing-pds-account",
		"existing-oauth-browser",
		"existing-oauth-qr",
		"existing-oauth-delegated",
	}
	transports := []string{
		"XRPC",
		"Firehose subscribeRepos",
		"Firehose JetStream",
		"Both",
	}
	for _, account := range accounts {
		account := account
		t.Run(account, func(t *testing.T) {
			fixture := newMarketAccountFixture(t, account)
			for _, transport := range transports {
				transport := transport
				t.Run(transport, func(t *testing.T) {
					runMarketTransport(t, fixture, transport)
				})
			}
		})
	}
}

func runMarketTransport(t *testing.T, account *marketAccountFixture, transport string) {
	t.Helper()
	inbox := newMarketInbox()
	collection := marketFixtureCollection
	switch transport {
	case "XRPC":
		write := account.writeRecord(t, collection, "xrpc-"+marketRandomHex(t, 6))
		if !account.deliverWrite(t, inbox, write) {
			t.Fatalf("XRPC delivered an already-known object")
		}
		if inbox.Len() != 1 {
			t.Fatalf("XRPC created %d objects, want 1", inbox.Len())
		}
	case "Firehose subscribeRepos":
		account.deliverSubscribeRepos(t, inbox, func() marketRecordWrite {
			return account.writeRecord(t, collection, "sub-"+marketRandomHex(t, 6))
		})
		if inbox.Len() != 1 {
			t.Fatalf("subscribeRepos created %d objects, want 1", inbox.Len())
		}
	case "Firehose JetStream":
		jetstream := newMarketJetstreamFixture(t)
		watcher := startMarketEventStream(t, "jetstream", jetstream.URL(), []string{collection}, account.DID.String())
		write := account.writeRecord(t, collection, "jet-"+marketRandomHex(t, 6))
		jetstream.Publish(marketEventFor(write, time.Now().UnixMicro()))
		event := watcher.waitEvent(t, 15*time.Second)
		if !account.deliverEvent(t, inbox, event) {
			t.Fatalf("JetStream delivered an already-known object")
		}
		if inbox.Len() != 1 {
			t.Fatalf("JetStream created %d objects, want 1", inbox.Len())
		}
	case "Both":
		stream, err := account.Client.SubscribeRepos(context.Background(), nil)
		if err != nil {
			t.Fatalf("Both subscribeRepos: %v", err)
		}
		defer stream.Close()
		write := account.writeRecord(t, collection, "both-"+marketRandomHex(t, 6))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		event, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Both subscribeRepos next: %v", err)
		}
		if event.Repo.String() != account.DID.String() {
			t.Fatalf("Both subscribeRepos repo %s, want %s", event.Repo, account.DID)
		}
		if !account.deliverWrite(t, inbox, write) {
			t.Fatalf("Both XRPC delivered an already-known object")
		}
		if len(event.Ops) == 0 {
			t.Fatalf("Both subscribeRepos event has no path")
		}
		collectionName, rkey, ok := pathOfEvent(eventsvc.Event{Ops: event.Ops})
		if !ok {
			t.Fatalf("Both subscribeRepos event has no path")
		}
		value, cid := account.getRecord(t, collectionName, rkey)
		if inbox.Deliver(write.URI, cid, value) {
			t.Fatalf("Both delivered the same object twice")
		}
		if inbox.Len() != 1 {
			t.Fatalf("Both created %d objects, want 1", inbox.Len())
		}
		if inbox.Applies() < 2 {
			t.Fatalf("Both applied %d deliveries, want at least 2", inbox.Applies())
		}
	default:
		t.Fatalf("unknown transport %q", transport)
	}
}

func TestMarketDuplicateDeliveryCreatesOneObject(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	relay := newMarketRelayFixture(t, "json")
	watcher := startMarketEventStream(t, "relay", relay.URL(), []string{marketFixtureCollection}, account.DID.String())
	write := account.writeRecord(t, marketFixtureCollection, "duplicate-"+marketRandomHex(t, 6))
	event := marketEventFor(write, 1)
	inbox := newMarketInbox()
	relay.Publish(event)
	first := watcher.waitEvent(t, 15*time.Second)
	duplicate := event
	duplicate.Seq = 2
	relay.Publish(duplicate)
	second := watcher.waitEvent(t, 15*time.Second)
	account.deliverEvent(t, inbox, first)
	account.deliverEvent(t, inbox, second)
	account.deliverWrite(t, inbox, write)
	if inbox.Len() != 1 {
		t.Fatalf("duplicate XRPC and firehose delivery created %d objects, want 1", inbox.Len())
	}
	if inbox.Applies() != 3 {
		t.Fatalf("duplicate delivery applied %d times, want 3", inbox.Applies())
	}
}

func TestMarketRestartResumesCursorsAndDedupe(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	relay := newMarketRelayFixture(t, "json")
	relay.SetCloseAfter(1)
	watcher := startMarketEventStream(t, "relay", relay.URL(), []string{marketFixtureCollection}, account.DID.String())
	first := account.writeRecord(t, marketFixtureCollection, "restart-"+marketRandomHex(t, 6))
	second := account.writeRecord(t, marketFixtureCollection, "restart-"+marketRandomHex(t, 6))
	relay.Publish(marketEventFor(first, 1))
	relay.Publish(marketEventFor(second, 2))
	inbox := newMarketInbox()
	eventOne := watcher.waitEvent(t, 15*time.Second)
	eventTwo := watcher.waitEvent(t, 15*time.Second)
	account.deliverEvent(t, inbox, eventOne)
	account.deliverEvent(t, inbox, eventTwo)
	account.deliverEvent(t, inbox, eventTwo)
	if inbox.Len() != 2 {
		t.Fatalf("restart and resume created %d objects, want 2", inbox.Len())
	}
	cursors := relay.Cursors()
	reconnected := false
	for _, cursor := range cursors {
		if cursor == 1 {
			reconnected = true
		}
	}
	if !reconnected {
		t.Fatalf("relay reconnected with cursors %v, want a resume at 1", cursors)
	}
}

func TestMarketOutOfOrderAcceptConverges(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	bid := account.writeRecord(t, marketFixtureCollection, "ordered-bid-"+marketRandomHex(t, 6))
	receipt := account.writeRecord(t, marketFixtureReceipt, "ordered-receipt-"+marketRandomHex(t, 6))
	accept := account.writeRecord(t, marketFixtureAccept, "ordered-accept-"+marketRandomHex(t, 6))
	inbox := newMarketInbox()
	account.deliverWrite(t, inbox, accept)
	account.deliverWrite(t, inbox, bid)
	account.deliverWrite(t, inbox, receipt)
	account.deliverWrite(t, inbox, accept)
	if inbox.Len() != 3 {
		t.Fatalf("out-of-order accept,bid,receipt created %d objects, want 3", inbox.Len())
	}
	if inbox.Applies() != 4 {
		t.Fatalf("out-of-order delivery applied %d times, want 4", inbox.Applies())
	}
}

func TestMarketFirehoseFailsClosedWithoutIngress(t *testing.T) {
	watcher := startMarketEventStream(t, "relay", "", []string{marketFixtureCollection}, "")
	status := watcher.waitPhase(t, eventsv1alpha1.EventStreamPhaseFailed, 15*time.Second)
	if status.Phase != eventsv1alpha1.EventStreamPhaseFailed {
		t.Fatalf("firehose without ingress reached %s, want Failed", status.Phase)
	}
	if status.Cursor != 0 {
		t.Fatalf("firehose without ingress advanced the cursor to %d", status.Cursor)
	}
}

func TestMarketRemotePDSMissingAuthFailsClosed(t *testing.T) {
	pdsfx := newMarketPDSFixture(t, marketPDSOptions{})
	session := pdsfx.createSession(t)
	missing := newMarketRemoteFixture(t, pdsfx, session, false)
	resolver := &pdsclient.RepoResolver{Timeout: 10 * time.Second}
	_, err := resolver.Resolve(context.Background(), pdsclient.RepoBinding{
		Kind:      "RemotePDS",
		Name:      missing.Object.Name,
		DID:       missing.Object.Spec.DID,
		RemotePDS: &missing.Object,
	})
	if !errors.Is(err, pdsclient.ErrAuthenticationRequired) {
		t.Fatalf("RemotePDS without auth resolve error %v, want %v", err, pdsclient.ErrAuthenticationRequired)
	}
	withAuth := newMarketRemoteFixture(t, pdsfx, session, true)
	resolved := withAuth.resolve(t)
	info, err := resolved.PDS.Describe(context.Background())
	if err != nil {
		t.Fatalf("RemotePDS with auth describe: %v", err)
	}
	if info.DID.String() != pdsfx.DID.String() {
		t.Fatalf("RemotePDS described %s, want %s", info.DID, pdsfx.DID)
	}
}

func TestMarketRelayReconnectResumesCursor(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	relay := newMarketRelayFixture(t, "json")
	relay.SetCloseAfter(1)
	watcher := startMarketEventStream(t, "relay", relay.URL(), []string{marketFixtureCollection}, account.DID.String())
	first := account.writeRecord(t, marketFixtureCollection, "reconnect-"+marketRandomHex(t, 6))
	second := account.writeRecord(t, marketFixtureCollection, "reconnect-"+marketRandomHex(t, 6))
	relay.Publish(marketEventFor(first, 10))
	relay.Publish(marketEventFor(second, 11))
	watcher.waitEvent(t, 15*time.Second)
	watcher.waitEvent(t, 15*time.Second)
	resumed := false
	for _, cursor := range relay.Cursors() {
		if cursor == 10 {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("relay cursors %v never resumed at 10", relay.Cursors())
	}
}

func TestMarketAssociationRevocationRemovesApproval(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	subject := "did:plc:" + marketRandomHex(t, 12)
	path := account.deliverAssociationClaim(t, subject)
	if !account.recordExists(t, path) {
		t.Fatalf("association claim was not written")
	}
	account.deleteRecord(t, path)
	if account.recordExists(t, path) {
		t.Fatalf("revoked association claim still reads as present")
	}
}

func TestMarketExpiredSessionReportsSignInAgain(t *testing.T) {
	token := newMarketTokenServer(t, "refresh")
	key := marketFixtureDpopKey(t)
	store := &oauthclient.MemoryStore{}
	data := oauthclient.SessionData{
		AccountDID:              "did:plc:" + marketRandomHex(t, 12),
		SessionID:               "fixture",
		HostURL:                 "http://fixture.invalid",
		Scopes:                  []string{"atproto"},
		AccessToken:             marketJWT(time.Now().Add(-time.Hour)),
		RefreshToken:            "fixture-refresh",
		DPoPPrivateKeyMultibase: key,
		AuthServerTokenEndpoint: token.TokenURL(),
	}
	if err := store.Save(context.Background(), data); err != nil {
		t.Fatalf("expired session store: %v", err)
	}
	session, err := oauthclient.New(data, oauthclient.Options{Store: store, ClientID: "fixture"})
	if err != nil {
		t.Fatalf("expired session open: %v", err)
	}
	refreshed, err := session.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("refresh an expired session: %v", err)
	}
	if refreshed == data.AccessToken {
		t.Fatalf("refresh returned the expired access token")
	}

	revoked := newMarketTokenServer(t, "revoked")
	revokedData := data
	revokedData.SessionID = "revoked"
	revokedData.RefreshToken = "fixture-revoked"
	revokedData.AuthServerTokenEndpoint = revoked.TokenURL()
	revokedSession, err := oauthclient.New(revokedData, oauthclient.Options{Store: &oauthclient.MemoryStore{}, ClientID: "fixture"})
	if err != nil {
		t.Fatalf("revoked session open: %v", err)
	}
	if refreshed, err := revokedSession.AccessToken(context.Background()); !errors.Is(err, oauthclient.ErrSessionExpired) {
		t.Fatalf("revoked session refresh error %v (token %q), want %v", err, refreshed, oauthclient.ErrSessionExpired)
	}
}

func marketFixtureDpopKey(t *testing.T) string {
	t.Helper()
	key, err := marketGenerateP256()
	if err != nil {
		t.Fatalf("fixture P-256 key: %v", err)
	}
	return key
}

func TestMarketTransportFixtureFormats(t *testing.T) {
	account := newMarketAccountFixture(t, "managed-associated")
	write := account.writeRecord(t, marketFixtureCollection, "formats-"+marketRandomHex(t, 6))
	for _, format := range []string{"json", "cbor"} {
		format := format
		t.Run(format, func(t *testing.T) {
			relay := newMarketRelayFixture(t, format)
			if format == "json" {
				watcher := startMarketEventStream(t, "relay", relay.URL(), []string{marketFixtureCollection}, account.DID.String())
				relay.Publish(marketEventFor(write, 1))
				event := watcher.waitEvent(t, 15*time.Second)
				if event.Repo.String() != account.DID.String() {
					t.Fatalf("relay json event repo %s, want %s", event.Repo, account.DID)
				}
				return
			}
			client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: relay.URL(), Timeout: 10 * time.Second})
			if err != nil {
				t.Fatalf("relay cbor client: %v", err)
			}
			stream, err := client.SubscribeRepos(context.Background(), nil)
			if err != nil {
				t.Fatalf("relay cbor subscribe: %v", err)
			}
			defer stream.Close()
			relay.Publish(marketEventFor(write, 1))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			event, err := stream.Next(ctx)
			if err != nil {
				t.Fatalf("relay cbor next: %v", err)
			}
			if event.Repo.String() != account.DID.String() {
				t.Fatalf("relay cbor event repo %s, want %s", event.Repo, account.DID)
			}
		})
	}
}

func TestMarketPDSFixturePointableAtRealPDS(t *testing.T) {
	raw := marketFixtureURL("SOCIALWEB_IT_MARKET_PDS_URL", "")
	if raw == "" {
		t.Skip("SOCIALWEB_IT_MARKET_PDS_URL is not set: no real PDS to point the fixture at")
	}
	did, err := syntax.ParseDID(marketFixtureURL("SOCIALWEB_IT_MARKET_PDS_DID", ""))
	if err != nil {
		t.Fatalf("SOCIALWEB_IT_MARKET_PDS_DID: %v", err)
	}
	pdsfx := newMarketPDSFixture(t, marketPDSOptions{
		URL:      raw,
		DID:      did.String(),
		Handle:   marketFixtureURL("SOCIALWEB_IT_MARKET_PDS_HANDLE", ""),
		Password: marketFixtureURL("SOCIALWEB_IT_MARKET_PDS_PASSWORD", ""),
	})
	session := pdsfx.createSession(t)
	client := pdsfx.clientFor(session.AccessJWT)
	if _, err := client.Describe(context.Background()); err != nil {
		t.Fatalf("real PDS describe: %v", err)
	}
}
