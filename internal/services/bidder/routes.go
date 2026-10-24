package bidder

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/networkdial"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const (
	SubmitRFPPath = "/xrpc/" + market.SubmitRFPNSID

	SubmitBidPath = "/xrpc/" + market.SubmitBidNSID

	SubmitAcceptPath = "/xrpc/" + market.SubmitAcceptNSID

	SubmitEventPath = "/xrpc/" + market.SubmitEventNSID

	relayTokenLifetime = 5 * time.Minute
)

type submittedRecord struct {
	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`
}

func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	if s.inboundXRPC() {
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

func (s *Service) admit(w http.ResponseWriter, r *http.Request, lxm, recordURI string) (string, bool) {
	issuer, ok, status := s.authorizer.AuthorizeRequest(r, lxm, recordURI)
	if !ok {
		participant.WriteXRPCError(w, status, participant.AuthCode(status), "invalid service-auth token")
	}
	return issuer, ok
}

func (s *Service) suspended(w http.ResponseWriter) bool {
	if !s.spec.SuspendParticipation {
		return false
	}
	participant.WriteXRPCError(w, http.StatusServiceUnavailable, "Suspended", "the bidder's participation is suspended")
	return true
}

func (s *Service) ready(w http.ResponseWriter, what string) *core.Bidder {
	bidder := s.bidder.Load()
	if bidder == nil {
		participant.WriteXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the bidder is not ready to "+what)
	}
	return bidder
}

func (s *Service) handleSubmitRFP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RFPURI string `json:"rfpUri"`
		RFPCID string `json:"rfpCid"`
	}
	if err := participant.ReadSubmitBody(r, &body); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.RFPURI == "" || body.RFPCID == "" {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing rfpUri or rfpCid")
		return
	}
	issuer, ok := s.admit(w, r, market.SubmitRFPNSID, body.RFPURI)
	if !ok || s.suspended(w) {
		return
	}
	bidder := s.ready(w, "bid")
	if bidder == nil {
		return
	}
	name, err := bidder.SubmitRFP(r.Context(), body.RFPURI, body.RFPCID, issuer)
	if err != nil {
		s.log.Warn("recording the bid intent failed", "rfp", body.RFPURI, "err", err)
		participant.WriteXRPCError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true, "name": name, "phase": string(v1alpha1.BidPhaseDeciding)})
}

func (s *Service) handleSubmitBid(w http.ResponseWriter, r *http.Request) {
	var body submittedRecord
	if err := participant.ReadSubmitBody(r, &body); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.URI == "" || body.CID == "" || body.Record == nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing uri, cid, or record")
		return
	}
	if _, ok := s.admit(w, r, market.SubmitBidNSID, body.URI); !ok {
		return
	}
	if _, err := market.ParseBid(body.Record); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) handleSubmitAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AcceptURI string `json:"acceptUri"`
		AcceptCID string `json:"acceptCid"`
	}
	if err := participant.ReadSubmitBody(r, &body); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.AcceptURI == "" || body.AcceptCID == "" {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing acceptUri or acceptCid")
		return
	}
	issuer, ok := s.admit(w, r, market.SubmitAcceptNSID, body.AcceptURI)
	if !ok {
		return
	}
	bidder := s.ready(w, "accept")
	if bidder == nil {
		return
	}
	id, err := bidder.SubmitAccept(r.Context(), body.AcceptURI, body.AcceptCID, issuer)
	if err != nil {
		participant.WriteXRPCError(w, http.StatusInternalServerError, "InternalError", "the provisioning intent could not be recorded: "+err.Error())
		return
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"ok": true, "id": id, "acceptUri": body.AcceptURI, "acceptCid": body.AcceptCID})
}

func (s *Service) handleSubmitEvent(w http.ResponseWriter, r *http.Request) {
	var body submittedRecord
	if err := participant.ReadSubmitBody(r, &body); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if body.URI == "" || body.CID == "" || body.Record == nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", "missing uri, cid, or record")
		return
	}
	issuer, ok := s.admit(w, r, market.SubmitEventNSID, body.URI)
	if !ok {
		return
	}
	bidder := s.ready(w, "accept events")
	if bidder == nil {
		return
	}
	err := bidder.SubmitEvent(r.Context(), body.URI, body.CID, body.Record, issuer)
	switch {
	case err == nil:
		httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, core.ErrEventForbidden):
		participant.WriteXRPCError(w, http.StatusForbidden, "Forbidden", "not the accept author")
	case errors.Is(err, core.ErrEventInvalid):
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	default:
		s.log.Warn("recording the event intent failed", "event", body.URI, "err", err)
		participant.WriteXRPCError(w, http.StatusServiceUnavailable, "Unavailable", err.Error())
	}
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
	handler, ok := s.deps.OAuthCallbacks.Lookup(name)
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

func (s *Service) startRelay(ctx context.Context) error {
	ingressHost := s.network().IngressProxyHost
	if ingressHost == "" {
		return nil
	}
	if s.stateDir() == "" {
		return errors.New("the relay names no keypair and there is no state dir")
	}
	signer, err := market.LoadOrCreateSignerFile(s.stateDir() + "/market-relay.jwk")
	if err != nil {
		return err
	}
	did, err := signer.PublicDID()
	if err != nil {
		return err
	}
	aud := "did:web:" + hostport.URLHostname(ingressHost)
	config := ingress.SubscriberConfig{
		Label:   "market",
		Keypair: relayKeypair{signer: signer, did: did},
		GetServiceAuthToken: func(ctx context.Context, nsid string) (string, error) {
			return ingress.SignServiceAuth(signer.PrivateKey, did, aud, nsid, relayTokenLifetime)
		},
		IngressProxyHost: ingressHost,
		HandleRequest:    s.forwardRelay,
	}
	if dialAddress := ingressConnectHost(s.network()); dialAddress != "" {
		hosts, err := networkdial.NewHostMap([]networkdial.Host{{Name: hostport.URLHostname(ingressHost), Address: dialAddress}})
		if err != nil {
			return err
		}
		transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{Hosts: hosts})
		if err != nil {
			return err
		}
		config.HTTPClient = &http.Client{Transport: transport, Timeout: DefaultCallTimeout}
		var dialer *websocket.Dialer
		if dialer, err = networkdial.NewWebSocketDialerWithOptions(networkdial.DialOptions{Hosts: hosts}); err != nil {
			return err
		}
		config.Dialer = dialer
	}
	subscriber, err := ingress.NewSubscriber(ctx, config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.relay = subscriber
	s.mu.Unlock()
	return nil
}

func (s *Service) relayEndpoint() (component.Endpoint, bool) {
	s.mu.Lock()
	relay := s.relay
	s.mu.Unlock()
	host := s.network().IngressProxyHost
	if relay == nil || host == "" {
		return component.Endpoint{}, false
	}
	scheme := "https"
	if base := strings.TrimSpace(s.spec.Network.GuestTLSURL()); base != "" {
		parsed, err := url.Parse(base)
		if err != nil || parsed.Host == "" {
			return component.Endpoint{}, false
		}
		scheme, host = parsed.Scheme, parsed.Host
	} else if strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") {
		scheme = "http"
	}
	protocol := component.ProtocolHTTPS
	if scheme == "http" {
		protocol = component.ProtocolHTTP
	}
	return component.Endpoint{Name: "relay", Protocol: protocol, URL: scheme + "://" + relay.Subdomain() + "." + host}, true
}

func (s *Service) forwardRelay(ctx context.Context, request *ingress.Request) (*ingress.Response, error) {
	if request == nil {
		return nil, errors.New("the relay request is empty")
	}
	path := cmp.Or(request.Path, "/")
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

type relayKeypair struct {
	signer *market.Signer

	did string
}

func (k relayKeypair) DID() string { return k.did }

func (k relayKeypair) Sign(data []byte) ([]byte, error) {
	return market.SignBytes(data, k.signer.PrivateKey)
}
