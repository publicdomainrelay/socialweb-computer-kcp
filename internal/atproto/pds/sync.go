package pds

import (
	"fmt"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

type latestCommitOutput struct {
	CID string `json:"cid"`
	Rev string `json:"rev"`
}

type repoStatusOutput struct {
	DID    string `json:"did"`
	Active bool   `json:"active"`
	Status string `json:"status,omitempty"`
	Rev    string `json:"rev,omitempty"`
}

func (s *Server) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	didRaw := r.URL.Query().Get("did")
	did, err := syntax.ParseDID(didRaw)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "did is required")
		return
	}
	var since *syntax.TID
	if rawSince := r.URL.Query().Get("since"); rawSince != "" {
		rev, err := syntax.ParseTID(rawSince)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid since")
			return
		}
		since = &rev
	}
	carBytes, err := s.repos.CAR(ctx, did, since)
	if err != nil {
		s.repoError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.ipld.car")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(carBytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(carBytes)
}

func (s *Server) handleGetLatestCommit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	did, err := syntax.ParseDID(r.URL.Query().Get("did"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "did is required")
		return
	}
	head, err := s.repos.Head(ctx, did)
	if err != nil {
		s.repoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, latestCommitOutput{CID: head.CID.String(), Rev: head.Rev.String()})
}

func (s *Server) handleGetRepoStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	did, err := syntax.ParseDID(r.URL.Query().Get("did"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "did is required")
		return
	}
	head, err := s.repos.Head(ctx, did)
	if err != nil {
		s.repoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, repoStatusOutput{DID: did.String(), Active: true, Rev: head.Rev.String()})
}

func (s *Server) handleSyncGetRecord(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	did, err := syntax.ParseDID(query.Get("did"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "did is required")
		return
	}
	collection, err := syntax.ParseNSID(query.Get("collection"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
		return
	}
	rkey, err := syntax.ParseRecordKey(query.Get("rkey"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid rkey")
		return
	}
	rec, err := s.repos.GetRecord(ctx, did, collection, rkey)
	if err != nil {
		s.repoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recordView{
		URI:   fmt.Sprintf("at://%s/%s/%s", did, collection, rkey),
		CID:   rec.CID.String(),
		Value: rec.Value,
	})
}
