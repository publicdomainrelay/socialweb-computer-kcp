package bidder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	MeshOfferWindow = 2 * time.Minute

	MeshAnswerTTL = 5 * time.Minute
)

type Mesh interface {
	Recipient() string

	Fingerprint() string

	Answer(ctx context.Context, ciphertext string) (string, error)
}

func (b *Bidder) meshEndpoint() (market.Endpoint, bool) {
	if b.mesh == nil {
		return market.Endpoint{}, false
	}
	ref, _ := json.Marshal(map[string]string{"did": b.cfg.DID, "listen": market.MarketListenRKey})
	return market.Endpoint{Kind: market.EndpointWebRTC, Address: base64.RawURLEncoding.EncodeToString(ref)}, true
}

func (b *Bidder) offeredEndpoints(listening bool) []market.Endpoint {
	out := append([]market.Endpoint(nil), b.cfg.Endpoints...)
	if endpoint, ok := b.meshEndpoint(); ok && listening {
		out = append(out, endpoint)
	}
	return out
}

func (b *Bidder) ensureListen(ctx context.Context) bool {
	if b.mesh == nil {
		return false
	}
	key, fingerprint := b.mesh.Recipient(), b.mesh.Fingerprint()
	if prior, err := b.records.GetRecord(ctx, b.cfg.DID, market.IngressListenNSID, market.MarketListenRKey); err == nil &&
		prior.Value["key"] == key && prior.Value["fingerprint"] == fingerprint {
		return true
	}
	record := map[string]any{"$type": market.IngressListenNSID, "key": key, "fingerprint": fingerprint, "createdAt": market.NowISO()}
	if _, err := b.records.PutRecord(ctx, b.cfg.DID, market.IngressListenNSID, market.MarketListenRKey, record); err != nil {
		b.log.Warn("bidder_mesh_listen_failed", "err", err.Error())
		return false
	}
	b.log.Info("bidder_mesh_listen_written", "fingerprint", fingerprint)
	return true
}

func (b *Bidder) answerOffer(ctx context.Context, requesterDID, uri string, record map[string]any) {
	rkey := uri[strings.LastIndex(uri, "/")+1:]
	did, tid, _ := strings.Cut(rkey, ".")
	drop := func(reason string) { b.log.Info("bidder_mesh_offer_dropped", "offer", uri, "reason", reason) }
	switch {
	case b.mesh == nil:
		drop("this bidder serves no webrtc")
		return
	case did != b.cfg.DID || tid == "" || market.ATURIAuthority(uri) != requesterDID:
		drop("not addressed to this bidder")
		return
	}
	created, _ := record["createdAt"].(string)
	if at, err := market.ParseTime(created); err != nil || time.Since(at) > MeshOfferWindow || time.Until(at) > MeshOfferWindow {
		drop("stale or undated")
		return
	}
	ciphertext, _ := record["ciphertext"].(string)
	answer, err := b.mesh.Answer(ctx, ciphertext)
	if err != nil {
		drop(err.Error())
		return
	}
	answerKey := requesterDID + "." + tid
	value := map[string]any{"$type": market.IngressAnswerNSID, "ciphertext": answer, "createdAt": market.NowISO()}
	if _, err := b.records.PutRecord(ctx, b.cfg.DID, market.IngressAnswerNSID, answerKey, value); err != nil {
		drop("writing the answer: " + err.Error())
		return
	}
	b.log.Info("bidder_mesh_answered", "offer", uri, "answer", answerKey)
	time.AfterFunc(MeshAnswerTTL, func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.records.DeleteRecord(cctx, b.cfg.DID, market.IngressAnswerNSID, answerKey)
	})
}
