package pds

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

type getServiceAuthRequest struct {
	Aud string `json:"aud"`
	Lxm string `json:"lxm"`
	Exp int64  `json:"exp"`
}

type getServiceAuthOutput struct {
	Token string `json:"token"`
}

func (s *Server) handleGetServiceAuth(w http.ResponseWriter, r *http.Request, info *authInfo) {
	if info.Service {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "service auth tokens cannot mint service auth tokens")
		return
	}
	if s.keys == nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "repo key store is not configured")
		return
	}
	var req getServiceAuthRequest
	query := r.URL.Query()
	req.Aud = query.Get("aud")
	req.Lxm = query.Get("lxm")
	if rawExp := query.Get("exp"); rawExp != "" {
		exp, err := strconv.ParseInt(rawExp, 10, 64)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "exp must be a unix timestamp")
			return
		}
		req.Exp = exp
	}
	if r.Method == http.MethodPost && r.ContentLength != 0 {
		if err := s.decodeJSON(w, r, &req); err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
			return
		}
	}
	if req.Aud == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "aud is required")
		return
	}
	var lexMethod *syntax.NSID
	if req.Lxm != "" {
		parsed, err := syntax.ParseNSID(req.Lxm)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid lxm")
			return
		}
		lexMethod = &parsed
	}
	ttl := serviceauth.MaxTokenLifetime
	if req.Exp != 0 {
		remaining := time.Until(time.Unix(req.Exp, 0))
		if remaining <= 0 {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "exp is in the past")
			return
		}
		if remaining < ttl {
			ttl = remaining
		}
	}
	key, err := s.keys.GetRepoKey(r.Context(), info.DID)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to load signing key")
		return
	}
	token, err := auth.SignServiceAuth(info.DID, req.Aud, ttl, lexMethod, key)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to sign service auth token")
		return
	}
	httpjson.Write(w, http.StatusOK, getServiceAuthOutput{Token: token})
}
