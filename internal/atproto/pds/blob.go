package pds

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/ipfs/go-cid"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

type blobView struct {
	Type     string          `json:"$type"`
	Ref      lexutil.LexLink `json:"ref"`
	MimeType string          `json:"mimeType"`
	Size     int64           `json:"size"`
}

type uploadBlobOutput struct {
	Blob blobView `json:"blob"`
}

func (s *Server) handleUploadBlob(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	if s.blobs == nil {
		writeXRPCError(w, http.StatusNotImplemented, "Unsupported", "blob storage is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBlobSize+1)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "failed to read blob")
		return
	}
	if int64(len(data)) > s.maxBlobSize {
		writeXRPCError(w, http.StatusBadRequest, "BlobTooLarge", "blob exceeds maximum size")
		return
	}
	mimeType := strings.TrimSpace(r.Header.Get("Content-Type"))
	mimeType = cmp.Or(mimeType, "application/octet-stream")
	ref, err := s.blobs.PutBlob(ctx, info.DID, mimeType, data)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to store blob")
		return
	}
	httpjson.Write(w, http.StatusOK, uploadBlobOutput{
		Blob: blobView{
			Type:     "blob",
			Ref:      lexutil.LexLink(ref.CID),
			MimeType: ref.MimeType,
			Size:     ref.Size,
		},
	})

}

func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	did, err := syntax.ParseDID(query.Get("did"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "did is required")
		return
	}
	c, err := cid.Parse(query.Get("cid"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "valid cid is required")
		return
	}
	if s.blobs == nil {
		writeXRPCError(w, http.StatusNotImplemented, "Unsupported", "blob storage is not configured")
		return
	}
	data, mimeType, err := s.blobs.GetBlob(ctx, did, c)
	if err != nil {
		if errors.Is(err, ErrBlobNotFound) {
			writeXRPCError(w, http.StatusNotFound, "BlobNotFound", "blob not found")
			return
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to load blob")
		return
	}
	mimeType = cmp.Or(mimeType, "application/octet-stream")
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
