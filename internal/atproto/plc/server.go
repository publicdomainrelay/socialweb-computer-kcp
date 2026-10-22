package plc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/didkey"
)

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

type Options struct {
	Store           Store
	Version         string
	VerifySignature func(ctx context.Context, did string, data, sig []byte) (bool, error)
}

type Server struct {
	store     Store
	version   string
	verifySig func(ctx context.Context, did string, data, sig []byte) (bool, error)
	mu        sync.Mutex
	handler   http.Handler
}

func NewServer(opts Options) *Server {
	store := opts.Store
	if store == nil {
		store = NewMemoryStore()
	}
	version := opts.Version
	if version == "" {
		version = "0.1.0"
	}
	s := &Server{store: store, version: version, verifySig: opts.VerifySignature}
	if s.verifySig == nil {
		s.verifySig = func(ctx context.Context, did string, data, sig []byte) (bool, error) {
			err := didkey.Verify(did, data, sig)
			if err != nil {
				return false, err
			}
			return true, nil
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /export", s.handleExport)
	mux.HandleFunc("GET /{did}", s.handleResolve)
	mux.HandleFunc("POST /{did}", s.handleSubmit)
	mux.HandleFunc("GET /{did}/log", s.handleLog)
	mux.HandleFunc("GET /{did}/log/audit", s.handleAudit)
	s.handler = withCORS(mux)
	return s
}

func (s *Server) Store() Store { return s.store }

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Submit(ctx context.Context, did string, raw map[string]any) (*LogEntry, error) {
	if _, ok := raw["type"]; !ok {
		return nil, errorf(400, "Operation must be an object")
	}
	if msg := ValidateStructure(raw); msg != "" {
		return nil, errorf(400, "%s", msg)
	}
	op := NewOperationFromObject(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	audit, err := s.store.GetAuditLog(ctx, did)
	if err != nil {
		return nil, err
	}
	current, err := s.store.GetCurrentOps(ctx, did)
	if err != nil {
		return nil, err
	}

	switch op.Type {
	case TypeCreate:
		if len(audit) > 0 {
			return nil, errorf(400, "Genesis operation requires empty log")
		}
	case TypeOperation, TypeTombstone:
		if op.Prev == nil && len(audit) > 0 {
			return nil, errorf(400, "Genesis operation requires empty log")
		}
		if err := validatePrevChain(op.Prev, current); err != nil {
			return nil, err
		}
		if len(current) > 0 {
			head := current[len(current)-1].Operation
			if head.Type == TypeTombstone {
				return nil, errorf(410, "DID not available: %s", did)
			}
			if head.Type == TypeOperation {
				if !s.verifySignatures(ctx, op, head.RotationKeys) {
					return nil, errorf(400, "Invalid Signature")
				}
			}
		} else {
			if !s.verifySignatures(ctx, op, op.RotationKeys) {
				return nil, errorf(400, "Invalid Signature")
			}
			if op.Type == TypeOperation {
				derived, err := OperationDID(op)
				if err != nil {
					return nil, errorf(400, "Failed to compute operation CID")
				}
				if derived != did {
					return nil, errorf(400, "DID does not match operation")
				}
			}
		}
	default:
		return nil, errorf(400, "Unknown operation type: %s", op.Type)
	}

	cid, err := OperationCID(op)
	if err != nil {
		return nil, errorf(400, "Failed to compute operation CID")
	}

	if op.Type == TypeOperation && op.Prev != nil && len(current) > 0 {
		prevIdx := -1
		for i := range current {
			if current[i].CID == *op.Prev {
				prevIdx = i
				break
			}
		}
		if prevIdx >= 0 && prevIdx < len(current)-1 {
			toNullify := make([]string, 0, len(current)-prevIdx-1)
			for _, e := range current[prevIdx+1:] {
				toNullify = append(toNullify, e.CID)
			}
			if err := s.store.NullifyOps(ctx, did, toNullify); err != nil {
				return nil, err
			}
		}
	}

	entry := LogEntry{
		DID:       did,
		Operation: *op,
		CID:       cid,
		Nullified: false,
		CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if err := s.store.InsertOp(ctx, entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (s *Server) Resolve(ctx context.Context, did string) (*DIDDocument, error) {
	ops, err := s.store.GetCurrentOps(ctx, did)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, errorf(404, "DID not registered: %s", did)
	}
	if ops[len(ops)-1].Operation.Type == TypeTombstone {
		return nil, errorf(410, "DID not available: %s", did)
	}
	doc := ResolveDocument(did, ops)
	if doc == nil {
		return nil, errorf(410, "DID not available: %s", did)
	}
	return doc, nil
}

func (s *Server) Log(ctx context.Context, did string) ([]Operation, error) {
	ops, err := s.store.GetCurrentOps(ctx, did)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, errorf(404, "DID not registered: %s", did)
	}
	out := make([]Operation, len(ops))
	for i, e := range ops {
		out[i] = e.Operation
	}
	return out, nil
}

func (s *Server) AuditLog(ctx context.Context, did string) ([]LogEntry, error) {
	ops, err := s.store.GetAuditLog(ctx, did)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, errorf(404, "DID not registered: %s", did)
	}
	return ops, nil
}

func (s *Server) Export(ctx context.Context, after *time.Time, count int) ([]LogEntry, error) {
	return s.store.ExportLogs(ctx, after, count)
}

func (s *Server) verifySignatures(ctx context.Context, op *Operation, keys []string) bool {
	sig, err := decodeSignature(op.Sig)
	if err != nil {
		return false
	}
	unsigned, err := atdata.MarshalCBOR(op.UnsignedObject())
	if err != nil {
		return false
	}
	for _, key := range keys {
		ok, err := s.verifySig(ctx, key, unsigned, sig)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func validatePrevChain(prev *string, current []LogEntry) *Error {
	if prev == nil {
		if len(current) > 0 {
			return errorf(400, "Genesis operation requires empty log")
		}
		return nil
	}
	for _, e := range current {
		if e.CID == *prev {
			return nil
		}
	}
	return errorf(400, "prev CID not found in current chain: %s", *prev)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after *time.Time
	if v := q.Get("after"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid Query Parameter: after"})
			return
		}
		after = &t
	}
	count := -1
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid Query Parameter: count"})
			return
		}
		count = n
		if count > 1000 {
			count = 1000
		}
	}
	entries, err := s.store.ExportLogs(r.Context(), after, count)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	doc, err := s.Resolve(r.Context(), r.PathValue("did"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid JSON body"})
		return
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid JSON body"})
		return
	}
	raw, ok := decoded.(map[string]any)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Operation must be an object"})
		return
	}
	entry, err := s.Submit(r.Context(), r.PathValue("did"), raw)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	ops, err := s.Log(r.Context(), r.PathValue("did"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ops)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.AuditLog(r.Context(), r.PathValue("did"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	var pe *Error
	if errors.As(err, &pe) {
		writeJSON(w, pe.Status, map[string]string{"message": pe.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Internal Server Error"})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
