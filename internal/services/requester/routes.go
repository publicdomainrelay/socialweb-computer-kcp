package requester

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	atpmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
	role "github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
)

const healthPath = "/xrpc/_health"

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, s.health)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.getRecord", s.repoGetRecord)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.listRecords", s.repoListRecords)
	mux.HandleFunc("GET /xrpc/com.atproto.repo.describeRepo", s.repoDescribe)
	mux.HandleFunc("GET /xrpc/com.atproto.server.describeServer", s.serverDescribe)
	if s.cfg.InboundXRPC() {
		mux.HandleFunc("POST /xrpc/"+atpmarket.SubmitBidNSID, s.submitBid)
		mux.HandleFunc("POST /xrpc/"+atpmarket.SubmitEventNSID, s.submitEvent)
	}
	mux.HandleFunc("GET "+role.SecretsRoute+"/{contract}", s.getSecrets)
	mux.HandleFunc("GET /oauth-client-metadata.json", s.oauthClientMetadata)
	mux.HandleFunc("GET /oauth/callback/{identity}", s.oauthCallback)
	return mux
}

func writeRequestError(w http.ResponseWriter, err error) {
	if failure, ok := errors.AsType[*role.RequestError](err); ok {
		participant.WriteXRPCError(w, failure.Status, failure.Code, failure.Message)
		return
	}
	participant.WriteXRPCError(w, http.StatusInternalServerError, "InternalError", err.Error())
}

func (s *Service) unavailable(w http.ResponseWriter) {
	participant.WriteXRPCError(w, http.StatusServiceUnavailable, "Unavailable", "the requester is starting")
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	core := s.healthy()
	if core == nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true, "did": core.DID()})
}

func (s *Service) repoGetRecord(w http.ResponseWriter, r *http.Request) {
	core := s.role()
	if core == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	repoDID := cmp.Or(query.Get("repo"), core.RepoDID())
	record, err := core.Records().GetRecord(r.Context(), repoDID, query.Get("collection"), query.Get("rkey"))
	if err != nil {
		status := http.StatusNotFound
		if !errors.Is(err, atpmarket.ErrNotFound) {
			status = http.StatusBadGateway
		}
		httpjson.Write(w, status, map[string]any{"error": "RecordNotFound", "message": err.Error()})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"uri": record.URI, "cid": record.CID, "value": record.Value})
}

func (s *Service) repoListRecords(w http.ResponseWriter, r *http.Request) {
	core := s.role()
	if core == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	repoDID := cmp.Or(query.Get("repo"), core.RepoDID())
	limit := 50
	if raw := query.Get("limit"); raw != "" {
		fmt.Sscanf(raw, "%d", &limit)
	}
	page, err := core.Records().ListRecords(r.Context(), repoDID, query.Get("collection"), limit, query.Get("cursor"))
	if err != nil {
		httpjson.Write(w, http.StatusBadGateway, map[string]any{"error": "RepoError", "message": err.Error()})
		return
	}
	records := make([]any, 0, len(page.Records))
	for _, record := range page.Records {
		records = append(records, map[string]any{"uri": record.URI, "cid": record.CID, "value": record.Value})
	}
	body := map[string]any{"records": records}
	if page.Cursor != "" {
		body["cursor"] = page.Cursor
	}
	httpjson.Write(w, http.StatusOK, body)
}

func (s *Service) repoDescribe(w http.ResponseWriter, r *http.Request) {
	core := s.role()
	if core == nil {
		s.unavailable(w)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"did": core.RepoDID(), "handle": core.Handle(), "collections": []string{}})
}

func (s *Service) serverDescribe(w http.ResponseWriter, r *http.Request) {
	core := s.role()
	if core == nil {
		s.unavailable(w)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"did": core.RepoDID(), "version": "socialweb-computer"})
}

func (s *Service) submitBid(w http.ResponseWriter, r *http.Request) {
	s.submitRecord(w, r, atpmarket.SubmitBidNSID, atpmarket.DefaultMarketServiceID, (*role.Requester).SubmitBid)
}

func (s *Service) submitEvent(w http.ResponseWriter, r *http.Request) {
	s.submitRecord(w, r, atpmarket.SubmitEventNSID, atpmarket.DefaultComputeEventServiceID, (*role.Requester).SubmitEvent)
}

type submitFunc func(core *role.Requester, ctx context.Context, issuer string, body role.SubmittedRecord) error

func (s *Service) submitRecord(w http.ResponseWriter, r *http.Request, nsid, serviceID string, submit submitFunc) {
	core := s.role()
	if core == nil {
		s.unavailable(w)
		return
	}
	issuer, err := core.AuthorizeCallback(r.Context(), r.Host, r.Header.Get("Authorization"), nsid, serviceID)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	var body role.SubmittedRecord
	if err := participant.ReadSubmitBody(r, &body); err != nil {
		participant.WriteXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := submit(core, r.Context(), issuer, body); err != nil {
		writeRequestError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Service) getSecrets(w http.ResponseWriter, r *http.Request) {
	core := s.role()
	if core == nil {
		http.Error(w, `{"error":"NotReady"}`, http.StatusServiceUnavailable)
		return
	}
	entries, err := core.Secrets(r.Context(), r.PathValue("contract"), r.Header.Get("Authorization"))
	if err != nil {
		if failure, ok := errors.AsType[*role.RequestError](err); ok {
			http.Error(w, `{"error":"`+failure.Code+`"}`, failure.Status)
			return
		}
		http.Error(w, `{"error":"InternalError"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (s *Service) oauthClientMetadata(w http.ResponseWriter, r *http.Request) {
	base := "https://" + r.Host
	httpjson.Write(w, http.StatusOK, map[string]any{
		"client_id":                  base + "/oauth-client-metadata.json",
		"client_name":                "socialweb-computer requester",
		"redirect_uris":              []string{base + "/oauth/callback/requester"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      atprotooauth.ScopeString(atprotooauth.RequesterOAuthScope),
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
		"dpop_bound_access_tokens":   true,
	})
}

func (s *Service) oauthCallback(w http.ResponseWriter, r *http.Request) {
	handler, ok := oauthcallback.Lookup(r.PathValue("identity"))
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
