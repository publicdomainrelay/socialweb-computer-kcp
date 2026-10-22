package bidder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
)

const (
	SubmitRFPPath = "/xrpc/" + market.SubmitRFPNSID

	SubmitBidPath = "/xrpc/" + market.SubmitBidNSID

	SubmitAcceptPath = "/xrpc/" + market.SubmitAcceptNSID

	SubmitEventPath = "/xrpc/" + market.SubmitEventNSID

	submitAcceptProvisionTimeout = 3 * time.Minute

	MaxSubmitBody = 2 << 20
)

type contractInfo struct {
	bidName string

	computeVM string

	receiptURI string

	receiptCID string

	acceptURI string

	acceptCID string

	acceptAuthor string

	submitEventURL string
}

func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+SubmitRFPPath, s.handleSubmitRFP)
	mux.HandleFunc("POST "+SubmitBidPath, s.handleSubmitBid)
	mux.HandleFunc("POST "+SubmitAcceptPath, s.handleSubmitAccept)
	mux.HandleFunc("POST "+SubmitEventPath, s.handleSubmitEvent)
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET "+OAuthMetadataPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"client_name": "Compute Provider (bidder)"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"error":"InternalError"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeXRPCError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

func readSubmitBody(r *http.Request, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxSubmitBody+1))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the body is not JSON: %w", err)
	}
	return nil
}

func (s *Service) verifyRecord(ctx context.Context, record market.ResolvedRecord) error {
	opts := market.VerifyRecordOptions{KeysForDID: market.NewDirectoryKeyLister(s.dir)}
	if s.records != nil {
		opts.KeyBoundByBadgeBlueKey = func(ctx context.Context, did, key string) (bool, error) {
			return market.KeyBoundByBadgeBlueKey(ctx, s.records, did, key), nil
		}
	}
	if !market.VerifyRecordSignatures(ctx, record.Value, market.ATURIAuthority(record.URI), opts) {
		return errors.New("record is missing a valid badge.blue signature")
	}
	return nil
}

func (s *Service) handleSubmitRFP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RFPURI string `json:"rfpUri"`
		RFPCID string `json:"rfpCid"`
	}
	if err := readSubmitBody(r, &body); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.RFPURI == "" || body.RFPCID == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing rfpUri or rfpCid")
		return
	}
	issuer, ok, status := s.authorizeRequest(r, market.SubmitRFPNSID, body.RFPURI)
	if !ok {
		writeXRPCError(w, status, authCode(status), "invalid service-auth token")
		return
	}
	if s.records == nil {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder has no market record store")
		return
	}
	resolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: body.RFPURI, CID: body.RFPCID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	rfp, err := market.ParseRFP(resolved.Value)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := s.verifyRecord(r.Context(), resolved); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if scope := s.policy.Scope(r.Context(), PolicyInput{SelfDid: s.did, CounterpartyDid: issuer}); !scope.Allow {
		s.log.Info("the scope policy declined the RFP", "rfp", body.RFPURI, "reason", scope.Reason)
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "scope filter declined"})
		return
	}
	decision := s.policy.EvaluatePolicies(r.Context(), rfp.Policies, PolicyInput{
		Perspective:      PolicyPerspectiveBidder,
		SelfDid:          s.did,
		SubjectDid:       s.did,
		CounterpartyDid:  issuer,
		RootRequesterDid: issuer,
		RFP:              &PolicyStrongRef{URI: body.RFPURI, CID: body.RFPCID},
		Mode:             PolicyModeFull,
	})
	if !decision.Allow {
		s.log.Warn("policy rejected the RFP", "rfp", body.RFPURI, "reason", decision.Reason, "run", decision.Run)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": policyError(decision), "violations": decision.Violations})
		return
	}
	bid, created, err := s.createBid(r.Context(), rfp, resolved, body.RFPURI, body.RFPCID, issuer)
	if err != nil {
		s.log.Warn("creating the bid failed", "rfp", body.RFPURI, "err", err)
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if !created {
		response := map[string]any{"ok": true, "duplicate": true}
		s.attachBidResponse(r.Context(), response, bid)
		writeJSON(w, http.StatusOK, response)
		return
	}
	response := map[string]any{"ok": true}
	s.attachBidResponse(r.Context(), response, bid)
	if rfp.SubmitBid != "" {
		response["submitBid"] = rfp.SubmitBid
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Service) attachBidResponse(ctx context.Context, response map[string]any, bid *v1alpha1.Bid) {
	if bid == nil || bid.Status.Bid == nil {
		return
	}
	ref := bid.Status.Bid
	response["bidUri"] = ref.Uri
	response["bidCid"] = ref.Cid
	record, err := s.records.GetRecord(ctx, s.did, market.BidNSID, lastSegment(ref.Uri))
	if err != nil {
		s.log.Warn("reading the published bid record failed", "bid", ref.Uri, "err", err)
		return
	}
	response["bid"] = map[string]any{"uri": ref.Uri, "cid": ref.Cid, "record": record.Value}
}

func (s *Service) handleSubmitBid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URI    string         `json:"uri"`
		CID    string         `json:"cid"`
		Record map[string]any `json:"record"`
	}
	if err := readSubmitBody(r, &body); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.URI == "" || body.CID == "" || body.Record == nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing uri, cid, or record")
		return
	}
	if _, ok, status := s.authorizeRequest(r, market.SubmitBidNSID, body.URI); !ok {
		writeXRPCError(w, status, authCode(status), "invalid service-auth token")
		return
	}
	if _, err := market.ParseBid(body.Record); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := s.verifyRecord(r.Context(), market.ResolvedRecord{URI: body.URI, CID: body.CID, Value: body.Record}); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleSubmitAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AcceptURI string `json:"acceptUri"`
		AcceptCID string `json:"acceptCid"`
	}
	if err := readSubmitBody(r, &body); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.AcceptURI == "" || body.AcceptCID == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing acceptUri or acceptCid")
		return
	}
	issuer, ok, status := s.authorizeRequest(r, market.SubmitAcceptNSID, body.AcceptURI)
	if !ok {
		writeXRPCError(w, status, authCode(status), "invalid service-auth token")
		return
	}
	unlock := s.lockAccept(body.AcceptURI)
	defer unlock()
	if s.records == nil || s.bids == nil || s.vms == nil || s.provider == nil {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder is not ready to accept")
		return
	}
	resolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: body.AcceptURI, CID: body.AcceptCID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	accept, err := market.ParseAccept(resolved.Value)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := s.verifyRecord(r.Context(), resolved); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if scope := s.policy.Scope(r.Context(), PolicyInput{SelfDid: s.did, CounterpartyDid: issuer}); !scope.Allow {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "the scope policy declined the accept")
		return
	}
	if existing, found := s.contractForAccept(r.Context(), body.AcceptURI, body.AcceptCID); found {
		writeJSON(w, http.StatusOK, map[string]any{"id": lastSegment(existing.receiptURI), "uri": existing.receiptURI,
			"cid": existing.receiptCID, "submitEvent": s.publicURL(), "alreadyAccepted": true})
		return
	}
	bid, err := s.bidForAccept(r.Context(), accept)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if bid.Spec.Requester != "" && bid.Spec.Requester != issuer {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "not the requester that opened the contract")
		return
	}
	if terminated := bid.Status.Terminated; terminated != nil {
		if bid.Status.Accept == nil || bid.Status.Accept.Uri != body.AcceptURI || bid.Status.Accept.Cid != body.AcceptCID {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the bid's contract was released")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alreadyAccepted": true, "terminated": true,
			"id": lastSegment(terminated.ReceiptUri), "uri": terminated.ReceiptUri, "cid": terminated.ReceiptCid,
			"submitEvent": s.publicURL()})
		return
	}
	now := s.cfg.Now().UTC()
	if bidExpired(bid, now) || bid.Status.Phase == v1alpha1.BidPhaseExpired {
		if bid.Status.Phase == v1alpha1.BidPhaseDeciding || bid.Status.Phase == v1alpha1.BidPhasePublished {
			s.expireBid(r.Context(), bid, now)
		}
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the bid has expired")
		return
	}
	rfpResolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the accepted RFP does not resolve: "+err.Error())
		return
	}
	rfp, err := market.ParseRFP(rfpResolved.Value)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	payloadResolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the RFP payload does not resolve: "+err.Error())
		return
	}
	bidResolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the accepted bid does not resolve: "+err.Error())
		return
	}
	parsedBid, err := market.ParseBid(bidResolved.Value)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if parsedBid.Config == nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the bid names no bid config")
		return
	}
	configResolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the bid config does not resolve: "+err.Error())
		return
	}
	provisionCtx, cancelProvision := context.WithTimeout(context.WithoutCancel(r.Context()), submitAcceptProvisionTimeout)
	defer cancelProvision()
	provision, provisionErr := s.provider.Provision(provisionCtx, ProvisionRequest{
		RFPURI:         accept.RFP.URI,
		RFPCID:         accept.RFP.CID,
		VMURI:          rfp.Payload.URI,
		VMCID:          rfp.Payload.CID,
		AcceptURI:      body.AcceptURI,
		AcceptCID:      body.AcceptCID,
		BidURI:         accept.Bid.URI,
		BidCID:         accept.Bid.CID,
		RequesterDID:   issuer,
		SubmitEventURL: s.publicURL(),
		VMPayload:      payloadResolved,
		BidConfig:      market.StrongRef{URI: parsedBid.Config.URI, CID: parsedBid.Config.CID},
		BidConfigValue: configResolved.Value,
	})
	if provisionErr != nil {
		s.log.Warn("provisioning the guest failed", "accept", body.AcceptURI, "err", provisionErr)
		if provision.Name != "" {
			if destroyErr := s.provider.Destroy(context.WithoutCancel(r.Context()), provision.Name); destroyErr != nil {
				s.log.Warn("releasing the partially provisioned guest failed", "computevm", provision.Name, "err", destroyErr)
			}
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "provisioning the guest failed: "+statusError(provisionErr))
		return
	}
	receiptURI, receiptCID, err := s.createReceipt(provisionCtx, market.ReceiptRefs{
		RFP:         market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID},
		Bid:         market.RecordRef{URI: accept.Bid.URI, CID: accept.Bid.CID},
		Accept:      market.RecordRef{URI: body.AcceptURI, CID: body.AcceptCID},
		SubmitEvent: s.publicURL(),
	}, resolved, issuer)
	if err != nil {
		if destroyErr := s.provider.Destroy(context.WithoutCancel(r.Context()), provision.Name); destroyErr != nil {
			s.log.Warn("releasing the unreceipted guest failed", "computevm", provision.Name, "err", destroyErr)
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "the receipt could not be written: "+err.Error())
		return
	}
	info := contractInfo{
		bidName:        bid.Name,
		computeVM:      provision.Name,
		receiptURI:     receiptURI,
		receiptCID:     receiptCID,
		acceptURI:      body.AcceptURI,
		acceptCID:      body.AcceptCID,
		acceptAuthor:   issuer,
		submitEventURL: s.publicURL(),
	}
	statusValue := cloneBidStatus(bid.Status)
	statusValue.Accept = &v1alpha1.BidRecordRef{Uri: body.AcceptURI, Cid: body.AcceptCID}
	statusValue.Timings.AcceptedAt = now.Format(time.RFC3339)
	statusValue.Phase = v1alpha1.BidPhaseWon
	statusValue.ComputeVM = provision.Name
	setBidCondition(&statusValue, bid.Generation, v1alpha1.BidConditionComplete, metav1.ConditionTrue, "ContractAccepted", "the guest contract is accepted")
	contract := v1alpha1.ComputeVMContract{
		ReceiptUri:     receiptURI,
		ReceiptCid:     receiptCID,
		AcceptUri:      body.AcceptURI,
		AcceptCid:      body.AcceptCID,
		SubmitEventUrl: s.publicURL(),
		AcceptedAt:     now.Format(time.RFC3339),
	}
	if err := s.provider.BindContract(provisionCtx, provision.Name, contract); err != nil {
		s.log.Warn("recording the receipt on the ComputeVM failed", "computevm", provision.Name, "err", err)
	}
	if err := s.bids.PatchStatus(provisionCtx, bid.Name, statusValue); err != nil {
		s.log.Warn("updating the bid after accept failed", "bid", bid.Name, "err", err)
	}
	s.contracts.Store(receiptURI, info)
	writeJSON(w, http.StatusOK, map[string]any{"id": lastSegment(receiptURI), "uri": receiptURI, "cid": receiptCID, "submitEvent": s.publicURL()})
}

func (s *Service) handleSubmitEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URI    string         `json:"uri"`
		CID    string         `json:"cid"`
		Record map[string]any `json:"record"`
	}
	if err := readSubmitBody(r, &body); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.URI == "" || body.CID == "" || body.Record == nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing uri, cid, or record")
		return
	}
	issuer, ok, status := s.authorizeRequest(r, market.SubmitEventNSID, body.URI)
	if !ok {
		writeXRPCError(w, status, authCode(status), "invalid service-auth token")
		return
	}
	if s.records == nil {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder has no market record store")
		return
	}
	resolved, err := s.records.Resolve(r.Context(), market.RecordRef{URI: body.URI, CID: body.CID})
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	event, err := market.ParseEvent(resolved.Value)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := s.verifyRecord(r.Context(), resolved); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	payloadNSID := market.NSIDFromURI(event.Payload.URI)
	if payloadNSID != market.ComputeEventsVMDeleteNSID {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	info, found := s.contractForReceipt(r.Context(), event.Receipt.URI)
	if !found {
		if terminated, ok := s.bidTerminatedByReceipt(r.Context(), event.Receipt.URI); ok {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alreadyTerminated": true, "reason": terminated.Reason})
			return
		}
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "unknown receipt")
		return
	}
	if info.acceptAuthor != "" && info.acceptAuthor != issuer {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "not the accept author")
		return
	}
	unlock := s.lockAccept(info.acceptURI)
	defer unlock()
	now := s.cfg.Now().UTC()
	var releaseBid *v1alpha1.Bid
	if s.bids != nil {
		if bid, err := s.bids.Get(r.Context(), info.bidName); err == nil {
			releaseBid = bid
		}
	}
	if info.computeVM != "" && s.provider != nil {
		s.markBidReleasing(r.Context(), releaseBid, info, "ReleaseRequested", "the release of "+info.computeVM+" is requested")
		if err := s.provider.Destroy(r.Context(), info.computeVM); err != nil {
			s.log.Warn("deleting the guest on request failed", "computevm", info.computeVM, "err", err)
			s.markBidReleasing(r.Context(), releaseBid, info, "ReleaseFailed", statusError(err))
			writeXRPCError(w, http.StatusInternalServerError, "InternalError", "the guest release failed: "+statusError(err))
			return
		}
	}
	if s.bids != nil {
		if bid, err := s.bids.Get(r.Context(), info.bidName); err == nil && bid != nil {
			statusValue := cloneBidStatus(bid.Status)
			statusValue.Timings.FinishedAt = now.Format(time.RFC3339)
			statusValue.Terminated = &v1alpha1.BidTerminated{
				ReceiptUri:   info.receiptURI,
				ReceiptCid:   info.receiptCID,
				AcceptAuthor: info.acceptAuthor,
				Reason:       "deleted_on_request",
				At:           now.Format(time.RFC3339),
			}
			setBidCondition(&statusValue, bid.Generation, BidConditionReleasing, metav1.ConditionFalse, "Released", "the guest is released")
			if err := s.bids.PatchStatus(r.Context(), bid.Name, statusValue); err != nil {
				s.log.Warn("updating the terminated bid failed", "bid", bid.Name, "err", err)
			}
		}
	}
	s.contracts.Delete(info.receiptURI)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) authorizeRequest(r *http.Request, lxm, recordURI string) (string, bool, int) {
	if s.authorizer == nil {
		return "", false, http.StatusInternalServerError
	}
	issuer, err := s.authorizer.Authorize(r.Context(), r.Header.Get("Authorization"), lxm, recordURI)
	if err != nil {
		if errors.Is(err, market.ErrForbidden) {
			return "", false, http.StatusForbidden
		}
		return "", false, http.StatusUnauthorized
	}
	return issuer, true, 0
}

func authCode(status int) string {
	if status == http.StatusForbidden {
		return "Forbidden"
	}
	return "Unauthorized"
}

func policyError(decision PolicyDecision) string {
	for _, violation := range decision.Violations {
		if strings.Contains(strings.ToLower(violation), "sandbox-denied") {
			return "policy sandbox denied"
		}
	}
	return "policy rejected"
}

func lastSegment(uri string) string {
	if i := strings.LastIndexByte(uri, '/'); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

func bidName(did, rfpURI, rfpCID string) string {
	sum := sha256.Sum256([]byte(did + "\x00" + rfpURI + "#" + rfpCID))
	return "bid-" + hex.EncodeToString(sum[:])[:20]
}

func (s *Service) createBid(ctx context.Context, rfp *market.RFP, resolved market.ResolvedRecord, rfpURI, rfpCID, issuer string) (*v1alpha1.Bid, bool, error) {
	if s.bids == nil {
		return nil, false, errors.New("the bidder has no Bid store")
	}
	if s.did == "" {
		return nil, false, errors.New("the bidder has no DID")
	}
	now := s.cfg.Now().UTC()
	name := bidName(s.did, rfpURI, rfpCID)
	existing, err := s.bids.Get(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	attached := make([]v1alpha1.BidRecordRef, 0, len(rfp.Policies))
	for _, p := range rfp.Policies {
		attached = append(attached, v1alpha1.BidRecordRef{Uri: p.URI, Cid: p.CID})
	}
	payloadNSID := market.NSIDFromURI(rfp.Payload.URI)
	bid := &v1alpha1.Bid{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BidSpec{
			Rfp:         v1alpha1.BidRecordRef{Uri: rfpURI, Cid: rfpCID},
			Requester:   issuer,
			PayloadNsid: payloadNSID,
			Offer:       v1alpha1.BidOffer{Kind: "free", Cost: 0},
			Provider:    "kcp",
			Policy:      v1alpha1.BidPolicy{Attached: attached, Verdict: "allow"},
			ExpiresAt:   now.Add(DefaultBidTTL).Format(time.RFC3339),
		},
	}
	if err := s.bids.Create(ctx, bid); err != nil {
		if isAlreadyExists(err) {
			again, getErr := s.bids.Get(ctx, name)
			return again, false, getErr
		}
		return nil, false, err
	}
	status := v1alpha1.BidStatus{
		Phase:     v1alpha1.BidPhaseDeciding,
		ClaimedBy: s.cfg.ComponentName,
		Timings:   v1alpha1.BidTimings{RfpSeenAt: now.Format(time.RFC3339), DecidedAt: now.Format(time.RFC3339)},
	}
	if err := s.bids.PatchStatus(ctx, name, status); err != nil {
		s.log.Warn("recording the bid decision failed", "bid", name, "err", err)
	}
	bid.Status = status
	if err := s.publishBidRecord(ctx, bid, rfp, now, false); err != nil {
		return bid, true, err
	}
	return bid, true, nil
}

func (s *Service) submitBid(ctx context.Context, endpoint string, ref market.StrongRef) {
	if !isAbsoluteURL(endpoint) {
		return
	}
	record, err := s.records.GetRecord(ctx, market.ATURIAuthority(ref.URI), market.NSIDFromURI(ref.URI), lastSegment(ref.URI))
	if err != nil {
		s.log.Warn("submitting the bid failed: the bid record cannot be read", "bid", ref.URI, "err", err)
		return
	}
	body := map[string]any{"uri": ref.URI, "cid": ref.CID, "record": record.Value}
	err = s.callService(ctx, endpoint, market.SubmitBidNSID, market.SubmitBidNSID, body)
	if err != nil {
		s.log.Warn("submitting the bid to the requester failed", "url", endpoint, "err", err)
	}
}

func (s *Service) callService(ctx context.Context, endpoint, nsid, lxm string, body map[string]any) error {
	if s.cfg.CallService != nil {
		return s.cfg.CallService(ctx, endpoint, nsid, lxm, body)
	}
	return s.postXRPC(ctx, endpoint, nsid, body)
}

func (s *Service) postXRPC(ctx context.Context, endpoint, nsid string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/xrpc/"+nsid, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := externalHTTPClient(s.cfg.Deps, DefaultCallTimeout).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Service) createReceipt(ctx context.Context, refs market.ReceiptRefs, accept market.ResolvedRecord, issuer string) (string, string, error) {
	if s.records == nil || s.attestation == nil {
		return "", "", errors.New("the bidder has no record store or attestation key")
	}
	value := make(map[string]any, len(accept.Value))
	for key, item := range accept.Value {
		value[key] = item
	}
	receipt, err := market.CreateReceiptRecordAt(ctx, s.records, s.did, market.ReceiptRKey(refs.Accept.URI, refs.Accept.CID), refs, market.ReceiptSubject{
		AcceptRecord:        value,
		AcceptRepositoryDID: issuer,
	}, s.attestation)
	if err != nil {
		return "", "", err
	}
	return receipt.URI, receipt.CID, nil
}

func (s *Service) bidForAccept(ctx context.Context, accept *market.Accept) (*v1alpha1.Bid, error) {
	if s.bids == nil {
		return nil, errors.New("the bidder has no Bid store")
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range bids {
		bid := &bids[i]
		if bid.Spec.Rfp.Uri != accept.RFP.URI {
			continue
		}
		if bid.Status.Bid != nil && bid.Status.Bid.Uri != "" && bid.Status.Bid.Uri != accept.Bid.URI {
			continue
		}
		return bid, nil
	}
	return nil, fmt.Errorf("no bid of this bidder matches %s", accept.Bid.URI)
}

func (s *Service) contractForAccept(ctx context.Context, acceptURI, acceptCID string) (contractInfo, bool) {
	if found, ok := s.contractForAcceptMemory(acceptURI, acceptCID); ok {
		return found, true
	}
	if s.vms == nil {
		return contractInfo{}, false
	}
	list, err := s.vms.List(ctx)
	if err != nil {
		return contractInfo{}, false
	}
	for i := range list {
		vm := &list[i]
		contract := vm.Spec.Contract
		if contract == nil || contract.AcceptUri != acceptURI || contract.AcceptCid != acceptCID {
			continue
		}
		info := contractInfo{computeVM: vm.Name, receiptURI: contract.ReceiptUri, receiptCID: contract.ReceiptCid,
			acceptURI: acceptURI, acceptCID: acceptCID, acceptAuthor: vm.Spec.Requester.Did}
		s.contracts.Store(info.receiptURI, info)
		return info, true
	}
	return contractInfo{}, false
}

func (s *Service) contractForAcceptMemory(acceptURI, acceptCID string) (contractInfo, bool) {
	var found contractInfo
	ok := false
	s.contracts.Range(func(key, value any) bool {
		info, isInfo := value.(contractInfo)
		if !isInfo || info.acceptURI != acceptURI || info.acceptCID != acceptCID {
			return true
		}
		found = info
		ok = true
		return false
	})
	return found, ok
}

func (s *Service) contractForReceipt(ctx context.Context, receiptURI string) (contractInfo, bool) {
	if value, ok := s.contracts.Load(receiptURI); ok {
		if info, isInfo := value.(contractInfo); isInfo {
			return info, true
		}
	}
	if s.vms == nil {
		return contractInfo{}, false
	}
	list, err := s.vms.List(ctx)
	if err != nil {
		return contractInfo{}, false
	}
	for i := range list {
		vm := &list[i]
		contract := vm.Spec.Contract
		if contract == nil || contract.ReceiptUri != receiptURI {
			continue
		}
		info := contractInfo{computeVM: vm.Name, receiptURI: contract.ReceiptUri, receiptCID: contract.ReceiptCid,
			acceptURI: contract.AcceptUri, acceptCID: contract.AcceptCid, acceptAuthor: vm.Spec.Requester.Did}
		s.contracts.Store(receiptURI, info)
		return info, true
	}
	return s.contractForReceiptOfUnboundVM(ctx, receiptURI)
}

func (s *Service) contractForReceiptOfUnboundVM(ctx context.Context, receiptURI string) (contractInfo, bool) {
	if s.records == nil || s.vms == nil || s.did == "" {
		return contractInfo{}, false
	}
	if market.ATURIAuthority(receiptURI) != s.did {
		return contractInfo{}, false
	}
	resolved, err := s.records.Resolve(ctx, market.RecordRef{URI: receiptURI})
	if err != nil {
		return contractInfo{}, false
	}
	receipt, err := market.ParseReceipt(resolved.Value)
	if err != nil {
		return contractInfo{}, false
	}
	acceptKey := computevm.AcceptRef{URI: receipt.Accept.URI, CID: receipt.Accept.CID}.Key()
	if acceptKey == "" {
		return contractInfo{}, false
	}
	info := contractInfo{receiptURI: receiptURI, receiptCID: resolved.CID,
		acceptURI: receipt.Accept.URI, acceptCID: receipt.Accept.CID,
		acceptAuthor: market.ATURIAuthority(receipt.Accept.URI)}
	list, err := s.vms.List(ctx)
	if err != nil {
		return contractInfo{}, false
	}
	for i := range list {
		vm := &list[i]
		if computevm.AcceptKeyOf(vm) != acceptKey {
			continue
		}
		if vm.DeletionTimestamp != nil || computevm.Ended(vm) {
			continue
		}
		info.computeVM = vm.Name
		break
	}
	s.contracts.Store(receiptURI, info)
	return info, true
}

func (s *Service) bidTerminatedByReceipt(ctx context.Context, receiptURI string) (*v1alpha1.BidTerminated, bool) {
	if s.bids == nil {
		return nil, false
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, false
	}
	for i := range bids {
		terminated := bids[i].Status.Terminated
		if terminated != nil && terminated.ReceiptUri == receiptURI && bids[i].Status.Phase == v1alpha1.BidPhaseLost {
			return terminated, true
		}
	}
	return nil, false
}

func structMap(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) startRelay(ctx context.Context) error {
	if s.cfg.Relay == nil {
		return nil
	}
	relay := s.cfg.Relay
	var signer *market.Signer
	var did string
	if s.cfg.StateDir != "" {
		loaded, err := market.LoadOrCreateSignerFile(s.cfg.StateDir + "/market-relay.jwk")
		if err != nil {
			return err
		}
		signer = loaded
		if did, err = loaded.PublicDID(); err != nil {
			return err
		}
	}
	keypair := relay.Keypair
	if keypair == nil {
		if signer == nil {
			return errors.New("the relay names no keypair and there is no state dir")
		}
		keypair = signerKeypair{signer: signer, did: did}
	}
	getToken := relay.GetServiceAuthToken
	if getToken == nil {
		if signer == nil {
			return errors.New("the relay has no service-auth token source")
		}
		aud := "did:web:" + hostnameOnly(relay.IngressProxyHost)
		getToken = func(ctx context.Context, nsid string) (string, error) {
			return ingress.SignServiceAuth(signer.PrivateKey, did, aud, nsid, 5*time.Minute)
		}
	}
	label := relay.Label
	if label == "" {
		label = "market"
	}
	connectHost := relay.ConnectHost
	if connectHost == "" {
		connectHost = relay.IngressProxyHost
	}
	httpClient := relay.HTTPClient
	var dialer *websocket.Dialer
	if relay.DialAddress != "" && connectHost != "" {
		hosts, hostErr := networkdial.NewHostMap([]networkdial.Host{{Name: hostnameOnly(connectHost), Address: relay.DialAddress}})
		if hostErr != nil {
			return hostErr
		}
		transport, transportErr := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{Hosts: hosts})
		if transportErr != nil {
			return transportErr
		}
		if httpClient == nil {
			httpClient = &http.Client{Transport: transport, Timeout: DefaultCallTimeout}
		}
		if dialer, hostErr = networkdial.NewWebSocketDialerWithOptions(networkdial.DialOptions{Hosts: hosts}); hostErr != nil {
			return hostErr
		}
	}
	subscriber, err := ingress.NewSubscriber(ctx, ingress.SubscriberConfig{
		Label:               label,
		Keypair:             keypair,
		GetServiceAuthToken: getToken,
		IngressProxyHost:    connectHost,
		TLS:                 relay.TLS,
		HTTPClient:          httpClient,
		Dialer:              dialer,
		HandleRequest:       s.forwardRelay,
	})
	if err != nil {
		return err
	}
	s.relay = subscriber
	if endpoint, ok := s.relayEndpoint(); ok {
		s.addEndpoint(endpoint)
	}
	return nil
}

func (s *Service) relayEndpoint() (component.Endpoint, bool) {
	if s.relay == nil || s.cfg.Relay == nil {
		return component.Endpoint{}, false
	}
	host := s.cfg.Relay.IngressProxyHost
	if host == "" {
		return component.Endpoint{}, false
	}
	scheme := "https"
	if base := strings.TrimSpace(s.cfg.Relay.PublicURLBase); base != "" {
		parsed, err := url.Parse(base)
		if err != nil || parsed.Host == "" {
			return component.Endpoint{}, false
		}
		scheme, host = parsed.Scheme, parsed.Host
	} else if !s.cfg.Relay.TLS && (strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost")) {
		scheme = "http"
	}
	raw := scheme + "://" + s.relay.Subdomain() + "." + host
	protocol := component.ProtocolHTTPS
	if scheme == "http" {
		protocol = component.ProtocolHTTP
	}
	return component.Endpoint{Name: "relay", Protocol: protocol, URL: raw}, true
}

func (s *Service) forwardRelay(ctx context.Context, request *ingress.Request) (*ingress.Response, error) {
	if request == nil {
		return nil, errors.New("the relay request is empty")
	}
	path := request.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(request.Params) > 0 {
		values := url.Values{}
		for key, value := range request.Params {
			values.Set(key, value)
		}
		path += "?" + values.Encode()
	}
	body := request.RawBody
	if body == "" && len(request.Body) > 0 {
		body = string(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, "http://relay.local"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	s.mux.ServeHTTP(recorder, req)
	result := recorder.Result()
	defer result.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(result.Body, RecordReplyLimit))
	if err != nil {
		return nil, err
	}
	return &ingress.Response{Status: result.StatusCode, Body: json.RawMessage(raw), ContentType: result.Header.Get("Content-Type")}, nil
}

func hostnameOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if parsed, err := url.Parse("//" + raw); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return strings.Trim(raw, "[]")
}

type signerKeypair struct {
	signer *market.Signer

	did string
}

func (k signerKeypair) DID() string { return k.did }

func (k signerKeypair) Sign(data []byte) ([]byte, error) {
	return market.SignBytes(data, k.signer.PrivateKey)
}
