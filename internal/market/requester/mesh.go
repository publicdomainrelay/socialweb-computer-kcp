package requester

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ingress/webrtc"
)

const meshOfferSubhandle = "requester"

var DefaultEndpointKinds = []string{market.EndpointIroh, market.EndpointWebRTC, market.EndpointHTTPS}

type meshSignals struct {
	repo *market.RepoStore

	did string
}

var _ webrtc.MeshSignals = meshSignals{}

func (s meshSignals) Listen(ctx context.Context, address string) (webrtc.MeshListen, error) {
	ref, err := market.ParseWebRTCAddress(address)
	if err != nil {
		return webrtc.MeshListen{}, err
	}
	record, err := s.repo.GetRecord(ctx, ref.DID, market.IngressListenNSID, ref.Listen)
	if err != nil {
		return webrtc.MeshListen{}, err
	}
	key, _ := record.Value["key"].(string)
	fingerprint, _ := record.Value["fingerprint"].(string)
	if key == "" || fingerprint == "" {
		return webrtc.MeshListen{}, fmt.Errorf("%s has no {key, fingerprint}", record.URI)
	}
	return webrtc.MeshListen{Key: key, Fingerprint: fingerprint}, nil
}

func (s meshSignals) Offer(ctx context.Context, address, ciphertext string) (string, error) {
	ref, err := market.ParseWebRTCAddress(address)
	if err != nil {
		return "", err
	}
	session := syntax.NewTIDNow(0).String()
	record := map[string]any{"$type": market.IngressOfferNSID, "ciphertext": ciphertext, "subhandle": meshOfferSubhandle,
		"to": ref.Listen, "createdAt": market.NowISO()}
	_, err = s.repo.CreateRecordAt(ctx, s.did, market.IngressOfferNSID, ref.DID+"."+session, record)
	return session, err
}

func (s meshSignals) Answer(ctx context.Context, address, session string) (string, error) {
	ref, err := market.ParseWebRTCAddress(address)
	if err != nil {
		return "", err
	}
	record, err := s.repo.GetRecord(ctx, ref.DID, market.IngressAnswerNSID, s.did+"."+session)
	if errors.Is(err, market.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ciphertext, _ := record.Value["ciphertext"].(string)
	return ciphertext, nil
}

func (s meshSignals) Withdraw(ctx context.Context, address, session string) error {
	ref, err := market.ParseWebRTCAddress(address)
	if err != nil {
		return err
	}
	return s.repo.DeleteRecord(ctx, s.did, market.IngressOfferNSID, ref.DID+"."+session)
}

func (q *Requester) marketHTTP() *http.Client {
	if !slices.Contains(q.deps.EndpointKinds, market.EndpointWebRTC) {
		return q.deps.HTTP
	}
	base := q.deps.HTTP
	if base == nil {
		base = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	return webrtc.MeshClient(base, webrtc.NewMeshDialer(meshSignals{repo: q.repo, did: q.repoDID}, q.log))
}
