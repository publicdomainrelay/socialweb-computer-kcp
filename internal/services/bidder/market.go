package bidder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	SubmitRFPPath = "/xrpc/" + market.SubmitRFPNSID

	SubmitBidPath = "/xrpc/" + market.SubmitBidNSID

	SubmitAcceptPath = "/xrpc/" + market.SubmitAcceptNSID

	SubmitEventPath = "/xrpc/" + market.SubmitEventNSID

	MaxSubmitBody = 2 << 20

	serviceAuthTokenLifetime = 5 * time.Minute

	serviceAuthRefreshMargin = 30 * time.Second
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
	if s.cfg.inboundXRPC() {
		mux.HandleFunc("POST "+SubmitRFPPath, s.handleSubmitRFP)
		mux.HandleFunc("POST "+SubmitBidPath, s.handleSubmitBid)
		mux.HandleFunc("POST "+SubmitAcceptPath, s.handleSubmitAccept)
		mux.HandleFunc("POST "+SubmitEventPath, s.handleSubmitEvent)
	}
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, r *http.Request) {
		httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET "+OAuthMetadataPath, s.oauthClientMetadata)
	mux.HandleFunc("GET /oauth/callback/{identity}", s.oauthCallback)
	return mux
}

func writeXRPCError(w http.ResponseWriter, status int, code, message string) {
	httpjson.Write(w, status, map[string]any{"error": code, "message": message})
}

type serviceCallError struct {
	Status int

	Err error
}

func (e *serviceCallError) HTTPStatus() int { return e.Status }

func (e *serviceCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func (e *serviceCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
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
	if s.bids == nil || s.records == nil || s.attestation == nil || s.did == "" {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder is not ready to bid")
		return
	}
	bid, err := s.enqueueBid(r.Context(), body.RFPURI, body.RFPCID, issuer)
	if err != nil {
		s.log.Warn("recording the bid intent failed", "rfp", body.RFPURI, "err", err)
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	s.kickBids()
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true, "name": bid.Name, "phase": string(v1alpha1.BidPhaseDeciding)})
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
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
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
	if s.records == nil || s.bids == nil || s.vms == nil || s.syncController == nil {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder is not ready to accept")
		return
	}
	payload := provisionPayload{RequesterDID: issuer}
	payload.Accept.URI, payload.Accept.CID = body.AcceptURI, body.AcceptCID
	name, err := s.ensureProvisionSync(r.Context(), payload)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "the provisioning intent could not be recorded: "+err.Error())
		return
	}
	s.kickStateSync()
	httpjson.Write(w, http.StatusAccepted, map[string]any{"ok": true, "id": name, "acceptUri": body.AcceptURI, "acceptCid": body.AcceptCID})
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
	if s.records == nil || s.vms == nil || s.syncController == nil {
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder is not ready to accept events")
		return
	}
	if err := s.applyMarketEvent(r.Context(), body.URI, body.CID, body.Record, issuer); err != nil {
		if errors.Is(err, errMarketEventForbidden) {
			writeXRPCError(w, http.StatusForbidden, "Forbidden", "not the accept author")
			return
		}
		if errors.Is(err, errMarketEventInvalid) {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
			return
		}
		s.log.Warn("recording the event intent failed", "event", body.URI, "err", err)
		writeXRPCError(w, http.StatusServiceUnavailable, "Unavailable", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

var errMarketEventForbidden = errors.New("bidder: not the accept author")

var errMarketEventInvalid = errors.New("bidder: the market event is invalid")

func (s *Service) applyMarketEvent(ctx context.Context, uri, cid string, record map[string]any, issuer string) error {
	if s.records == nil || s.vms == nil || s.syncController == nil {
		return errors.New("the bidder is not ready to accept events")
	}
	event, err := market.ParseEvent(record)
	if err != nil {
		return fmt.Errorf("%w: %v", errMarketEventInvalid, err)
	}
	if market.NSIDFromURI(event.Payload.URI) != market.ComputeEventsVMDeleteNSID {
		return nil
	}
	if _, ok := s.bidTerminatedByReceipt(ctx, event.Receipt.URI); ok {
		return nil
	}
	info, vm, found, err := s.localContractForReceipt(ctx, event.Receipt.URI)
	if err != nil {
		return fmt.Errorf("the contract could not be looked up: %w", err)
	}
	if found && info.acceptAuthor != "" && info.acceptAuthor != issuer {
		return errMarketEventForbidden
	}
	if found && vm != nil && vm.UID != "" {
		request := &commonv1.StrongRef{URI: event.Payload.URI, CID: event.Payload.CID}
		if err := s.ensureInboundTerminationSync(ctx, vm, request, issuer, uri, cid, record); err != nil {
			return fmt.Errorf("the termination intent could not be recorded: %w", err)
		}
	} else {
		if err := s.ensureReleaseSync(ctx, event.Receipt.URI, issuer, uri, cid, record); err != nil {
			return fmt.Errorf("the release intent could not be recorded: %w", err)
		}
	}
	s.kickStateSync()
	return nil
}

func (s *Service) oauthClientMetadata(w http.ResponseWriter, r *http.Request) {
	base := "https://" + r.Host
	httpjson.Write(w, http.StatusOK, map[string]any{
		"client_id":                  base + "/oauth-client-metadata.json",
		"client_name":                "socialweb-computer bidder",
		"redirect_uris":              []string{base + "/oauth/callback/bidder"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      atprotooauth.ScopeString(atprotooauth.BidderOAuthScope),
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
		"dpop_bound_access_tokens":   true,
	})

}

func (s *Service) oauthCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("identity")
	s.log.Info("oauth callback received", "identity", name, "state", r.URL.Query().Get("state"))
	handler, ok := oauthcallback.Lookup(name)
	s.log.Info("oauth callback lookup", "identity", name, "found", ok)
	if !ok {
		http.Error(w, "unknown identity", http.StatusNotFound)
		return
	}
	if err := handler(r.Context(), w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("signed in"))
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

func (s *Service) enqueueBid(ctx context.Context, rfpURI, rfpCID, issuer string) (*v1alpha1.Bid, error) {
	if s.bids == nil {
		return nil, errors.New("the bidder has no Bid store")
	}
	if s.did == "" {
		return nil, errors.New("the bidder has no DID")
	}
	now := s.cfg.Now().UTC()
	name := bidName(s.did, rfpURI, rfpCID)
	existing, err := s.bids.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Status.Phase == "" {
			status := decidingBidStatus(now, s.cfg.ComponentName)
			if err := s.bids.PatchStatus(ctx, name, status); err != nil {
				return nil, err
			}
			existing.Status = status
		}
		return existing, nil
	}
	bid := &v1alpha1.Bid{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BidSpec{
			Rfp:       commonv1.StrongRef{URI: rfpURI, CID: rfpCID},
			Requester: issuer,
			ExpiresAt: now.Add(s.cfg.bidTTL()).Format(time.RFC3339),
		},
	}
	if err := s.bids.Create(ctx, bid); err != nil {
		if isAlreadyExists(err) {
			again, getErr := s.bids.Get(ctx, name)
			if getErr != nil {
				return nil, getErr
			}
			if again != nil && again.Status.Phase == "" {
				status := decidingBidStatus(now, s.cfg.ComponentName)
				if err := s.bids.PatchStatus(ctx, name, status); err != nil {
					return nil, err
				}
				again.Status = status
			}
			return again, nil
		}
		return nil, err
	}
	status := decidingBidStatus(now, s.cfg.ComponentName)
	if err := s.bids.PatchStatus(ctx, name, status); err != nil {
		return nil, err
	}
	bid.Status = status
	return bid, nil
}

func decidingBidStatus(now time.Time, claimedBy string) v1alpha1.BidStatus {
	return v1alpha1.BidStatus{
		Phase:     v1alpha1.BidPhaseDeciding,
		ClaimedBy: claimedBy,
		Timings:   v1alpha1.BidTimings{RfpSeenAt: now.Format(time.RFC3339)},
	}
}

func (s *Service) submitBid(ctx context.Context, endpoint string, ref market.StrongRef) {
	raw := strings.TrimSpace(endpoint)
	endpoint = s.submitEventURL(ctx, raw)
	if endpoint == "" {
		s.log.Warn("submitting the bid failed: the endpoint does not resolve", "url", raw, "bid", ref.URI)
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
	host := hostPortOf(endpoint)
	client := &xrpc.Client{Host: endpoint, HTTP: s.cfg.Deps.ExternalHTTPClient(DefaultCallTimeout), Limit: 4096}
	if host == "" {
		s.log.Warn("the outbound XRPC call is unsigned: the endpoint has no hostname", "url", endpoint, "nsid", nsid)
	} else if _, tokenErr := s.serviceAuthToken(ctx, host, nsid); tokenErr != nil {
		s.log.Warn("the outbound XRPC call is unsigned: the PDS cannot mint a service-auth token", "url", endpoint, "nsid", nsid, "err", tokenErr)
	} else {
		client.Auth = &xrpc.ServiceAuth{Tokens: serviceAuthSource{s: s, host: host, lxm: nsid}}
	}
	_, err := xrpc.Procedure[any, struct{}](ctx, client, nsid, body)
	if xerr, ok := errors.AsType[*xrpc.Error](err); ok {
		return &serviceCallError{Status: xerr.Status, Err: xerr}
	}
	return err
}

type serviceAuthSource struct {
	s *Service

	host string

	lxm string
}

func (a serviceAuthSource) Get(ctx context.Context) (string, error) {
	return a.s.serviceAuthToken(ctx, a.host, a.lxm)
}

func (a serviceAuthSource) Invalidate() {
	a.s.invalidateServiceAuth(a.host, a.lxm)
}

func (s *Service) serviceAuthCache() *ttlcache.Cache[string, string] {
	s.serviceAuthOnce.Do(func() {
		s.serviceAuth = ttlcache.New[string, string](ttlcache.Options{})
	})
	return s.serviceAuth
}

func (s *Service) serviceAuthToken(ctx context.Context, host, lxm string) (string, error) {
	aud := market.ServiceDIDForHost(host)
	return s.serviceAuthCache().GetOrLoad(ctx, aud+"\x00"+lxm, func(ctx context.Context) (string, time.Duration, error) {
		token, expires, err := s.mintServiceAuth(ctx, aud, lxm, time.Now())
		if err != nil {
			return "", 0, err
		}
		ttl := time.Until(expires)
		if ttl <= 0 {
			ttl = -1
		}
		return token, ttl, nil
	})
}

func (s *Service) invalidateServiceAuth(host, lxm string) {
	s.serviceAuthCache().Delete(market.ServiceDIDForHost(host) + "\x00" + lxm)
}

func (s *Service) mintServiceAuth(ctx context.Context, aud, lxm string, now time.Time) (string, time.Time, error) {
	endpoint := s.pdsServiceAuthEndpoint()
	if endpoint == "" {
		return "", time.Time{}, errors.New("the bidder has no PDS URL")
	}
	expires := now.Add(serviceAuthTokenLifetime)
	if minter, ok := s.cfg.PDSTokens.(interface {
		ServiceAuth(ctx context.Context, audience, lxm string) (string, error)
	}); ok {
		callCtx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
		token, err := minter.ServiceAuth(callCtx, aud, lxm)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("getServiceAuth %s: %w", aud, err)
		}
		return token, serviceAuthCacheExpiry(token, expires), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	defer cancel()
	client := &xrpc.Client{Host: endpoint, HTTP: s.pdsHTTPClient(), Auth: s.pdsAuth()}
	token, err := xrpc.GetServiceAuth(callCtx, client, aud, lxm, expires)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("getServiceAuth %s: %w", aud, err)
	}
	return token, serviceAuthCacheExpiry(token, expires), nil
}

func (s *Service) pdsAuth() xrpc.Auth {
	var refresh xrpc.TokenFunc
	if refresher, ok := s.cfg.PDSTokens.(pdsclient.TokenRefresher); ok {
		refresh = refresher.RefreshAccessToken
	}
	tokens := xrpc.Refreshing(func(ctx context.Context) (string, error) {
		token, err := s.pdsAccessTokenForServiceAuth(ctx)
		if err != nil {
			return "", err
		}
		if token == "" {
			return "", errors.New("the bidder has no PDS access token")
		}
		return token, nil
	}, refresh)
	if s.cfg.PDSDPoP == nil {
		return &xrpc.Bearer{Tokens: tokens}
	}
	dpop := s.cfg.PDSDPoP
	return xrpc.StatusRetry(&xrpc.DPoP{Tokens: tokens, Prover: xrpc.DPoPProverFunc(func(ctx context.Context, method, target, accessToken, _ string) (xrpc.DPoPProof, error) {
		proof, err := dpop.DPoPProof(ctx, method, target, accessToken)
		return xrpc.DPoPProof{Proof: proof}, err
	})})
}

func serviceAuthCacheExpiry(token string, requested time.Time) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return requested.Add(-serviceAuthRefreshMargin)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return requested.Add(-serviceAuthRefreshMargin)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return requested.Add(-serviceAuthRefreshMargin)
	}
	expires := time.Unix(claims.Exp, 0).Add(-serviceAuthRefreshMargin)
	if expires.After(requested) {
		return requested.Add(-serviceAuthRefreshMargin)
	}
	return expires
}

func (s *Service) pdsAccessTokenForServiceAuth(ctx context.Context) (string, error) {
	if s.cfg.PDSTokens != nil {
		token, err := s.cfg.PDSTokens.AccessToken(ctx)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(token), nil
	}
	token, err := s.pdsAccessToken(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

func (s *Service) pdsServiceAuthEndpoint() string {
	raw := strings.TrimSpace(s.cfg.PDSURL)
	if raw == "" && s.cfg.RemotePDS != nil {
		raw = strings.TrimSpace(s.cfg.RemotePDS.Spec.URL)
	}
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimRight(raw, "/")
}

func (s *Service) pdsHTTPClient() *http.Client {
	if s.pdsInternal && s.cfg.Deps.Handlers != nil {
		return s.cfg.Deps.Handlers.HTTPClient()
	}
	return s.cfg.Deps.ExternalHTTPClient(DefaultCallTimeout)
}

func (s *Service) createReceipt(ctx context.Context, refs market.ReceiptRefs, accept market.ResolvedRecord, issuer string) (string, string, error) {
	if s.records == nil || s.attestation == nil {
		return "", "", errors.New("the bidder has no record store or attestation key")
	}
	value := make(map[string]any, len(accept.Value))
	maps.Copy(value, accept.Value)
	rkey := market.ReceiptRKey(refs.Accept.URI, refs.Accept.CID)
	existing, err := s.records.GetRecord(ctx, s.did, market.ReceiptNSID, rkey)
	if err == nil {
		return existing.URI, existing.CID, nil
	}
	if !errors.Is(err, market.ErrNotFound) {
		return "", "", err
	}
	receipt, err := market.CreateReceiptRecordAt(ctx, createOnlyWriter{records: s.records, did: s.did}, s.did, rkey, refs, market.ReceiptSubject{
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
	if market.ATURIAuthority(accept.Bid.URI) != s.did {
		return nil, fmt.Errorf("the accepted bid %s is not this bidder's", accept.Bid.URI)
	}
	bids, err := s.bids.List(ctx)
	if err != nil {
		return nil, err
	}
	rkey := lastSegment(accept.Bid.URI)
	for i := range bids {
		bid := &bids[i]
		if bid.Spec.Rfp.URI != accept.RFP.URI {
			continue
		}
		if bid.Name != rkey {
			continue
		}
		if bid.Status.Bid != nil && bid.Status.Bid.URI != "" && bid.Status.Bid.URI != accept.Bid.URI {
			continue
		}
		return bid, nil
	}
	return nil, fmt.Errorf("no bid of this bidder matches %s", accept.Bid.URI)
}

func (s *Service) localContractForReceipt(ctx context.Context, receiptURI string) (contractInfo, *v1alpha1.ComputeVM, bool, error) {
	if s.vms == nil {
		return contractInfo{}, nil, false, errors.New("the bidder has no ComputeVM store")
	}
	var cached contractInfo
	if value, ok := s.contracts.Load(receiptURI); ok {
		if info, isInfo := value.(contractInfo); isInfo {
			cached = info
		}
	}
	list, err := s.vms.List(ctx)
	if err != nil {
		return contractInfo{}, nil, false, err
	}
	if cached.computeVM != "" {
		for i := range list {
			if list[i].Name == cached.computeVM {
				return cached, &list[i], true, nil
			}
		}
	}
	for i := range list {
		vm := &list[i]
		contract := vm.Spec.Contract
		if contract == nil || contract.Receipt.URI != receiptURI {
			continue
		}
		info := contractInfo{computeVM: vm.Name, receiptURI: contract.Receipt.URI, receiptCID: contract.Receipt.CID,
			acceptURI: contract.Accept.URI, acceptCID: contract.Accept.CID, acceptAuthor: vm.Spec.Requester.Did,
			bidName: cached.bidName, submitEventURL: contract.SubmitEventURL}
		s.contracts.Store(receiptURI, info)
		return info, vm, true, nil
	}
	return contractInfo{}, nil, false, nil
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
		if terminated == nil || terminated.ReceiptURI != receiptURI {
			continue
		}
		condition := apimeta.FindStatusCondition(bids[i].Status.Conditions, BidConditionReleasing)
		if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "Released" {
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
		aud := "did:web:" + hostport.URLHostname(relay.IngressProxyHost)
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
		hosts, hostErr := networkdial.NewHostMap([]networkdial.Host{{Name: hostport.URLHostname(connectHost), Address: relay.DialAddress}})
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

type signerKeypair struct {
	signer *market.Signer

	did string
}

func (k signerKeypair) DID() string { return k.did }

func (k signerKeypair) Sign(data []byte) ([]byte, error) {
	return market.SignBytes(data, k.signer.PrivateKey)
}
