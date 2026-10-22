package pds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

var errForbidden = errors.New("pds: forbidden")

type recordView struct {
	URI   string         `json:"uri"`
	CID   string         `json:"cid"`
	Value map[string]any `json:"value"`
}

type listRecordsOutput struct {
	Records []recordView `json:"records"`
	Cursor  string       `json:"cursor,omitempty"`
}

type describeRepoOutput struct {
	DID             string   `json:"did"`
	Handle          string   `json:"handle"`
	HandleIsCorrect bool     `json:"handleIsCorrect"`
	Collections     []string `json:"collections"`
	DIDDoc          any      `json:"didDoc,omitempty"`
}

type commitView struct {
	CID string `json:"cid"`
	Rev string `json:"rev"`
}

type createRecordOutput struct {
	URI    string      `json:"uri"`
	CID    string      `json:"cid"`
	Commit *commitView `json:"commit,omitempty"`
}

type createWriteRequest struct {
	Repo       string         `json:"repo,omitempty"`
	Collection string         `json:"collection"`
	Rkey       string         `json:"rkey,omitempty"`
	Record     map[string]any `json:"record"`
	SwapCommit *string        `json:"swapCommit,omitempty"`
	SwapRecord *string        `json:"swapRecord,omitempty"`
}

type putWriteRequest struct {
	Repo       string         `json:"repo,omitempty"`
	Collection string         `json:"collection"`
	Rkey       string         `json:"rkey"`
	Record     map[string]any `json:"record"`
	SwapCommit *string        `json:"swapCommit,omitempty"`
	SwapRecord *string        `json:"swapRecord,omitempty"`
}

type deleteWriteRequest struct {
	Repo       string  `json:"repo,omitempty"`
	Collection string  `json:"collection"`
	Rkey       string  `json:"rkey"`
	SwapCommit *string `json:"swapCommit,omitempty"`
	SwapRecord *string `json:"swapRecord,omitempty"`
}

type applyWrite struct {
	Type       string         `json:"$type"`
	Collection string         `json:"collection"`
	Rkey       string         `json:"rkey,omitempty"`
	Value      map[string]any `json:"value,omitempty"`
	SwapRecord *string        `json:"swapRecord,omitempty"`
}

type applyWritesRequest struct {
	Repo       string       `json:"repo,omitempty"`
	Writes     []applyWrite `json:"writes"`
	SwapCommit *string      `json:"swapCommit,omitempty"`
}

type applyWritesOutput struct {
	Commit  commitView `json:"commit"`
	Results []any      `json:"results"`
}

type createWriteResult struct {
	Type             string `json:"$type"`
	URI              string `json:"uri"`
	CID              string `json:"cid"`
	ValidationStatus string `json:"validationStatus,omitempty"`
}

type updateWriteResult struct {
	Type             string `json:"$type"`
	URI              string `json:"uri"`
	CID              string `json:"cid"`
	ValidationStatus string `json:"validationStatus,omitempty"`
}

type deleteWriteResult struct {
	Type string `json:"$type"`
}

func (s *Server) handleGetRecord(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	rawRepo := query.Get("repo")
	if rawRepo == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "repo is required")
		return
	}
	did, err := s.resolveRepo(ctx, rawRepo)
	if err != nil {
		s.repoError(w, err)
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
	if rawCID := query.Get("cid"); rawCID != "" {
		want, err := cid.Parse(rawCID)
		if err != nil || !want.Equals(rec.CID) {
			writeXRPCError(w, http.StatusBadRequest, "RecordNotFound", "record cid does not match")
			return
		}
	}
	writeJSON(w, http.StatusOK, recordView{
		URI:   fmt.Sprintf("at://%s/%s/%s", did, collection, rkey),
		CID:   rec.CID.String(),
		Value: rec.Value,
	})
}

func (s *Server) handleListRecords(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	rawRepo := query.Get("repo")
	if rawRepo == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "repo is required")
		return
	}
	did, err := s.resolveRepo(ctx, rawRepo)
	if err != nil {
		s.repoError(w, err)
		return
	}
	collection, err := syntax.ParseNSID(query.Get("collection"))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
		return
	}
	limit := repo.DefaultList
	if rawLimit := query.Get("limit"); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > repo.MaxList {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	var cursor syntax.RecordKey
	if rawCursor := query.Get("cursor"); rawCursor != "" {
		cursor, err = syntax.ParseRecordKey(rawCursor)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid cursor")
			return
		}
	}
	reverse := false
	if rawReverse := query.Get("reverse"); rawReverse != "" {
		reverse, err = strconv.ParseBool(rawReverse)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid reverse")
			return
		}
	}
	var records []repo.Record
	var next *syntax.RecordKey
	if reverse {
		records, next, err = s.listRecordsReverse(ctx, did, collection, cursor, limit)
	} else {
		records, next, err = s.repos.ListRecords(ctx, did, collection, cursor, limit)
	}
	if err != nil {
		s.repoError(w, err)
		return
	}
	out := listRecordsOutput{Records: make([]recordView, 0, len(records))}
	for _, rec := range records {
		out.Records = append(out.Records, recordView{
			URI:   fmt.Sprintf("at://%s/%s/%s", did, collection, rec.Rkey),
			CID:   rec.CID.String(),
			Value: rec.Value,
		})
	}
	if next != nil {
		out.Cursor = next.String()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listRecordsReverse(ctx context.Context, did syntax.DID, collection syntax.NSID, cursor syntax.RecordKey, limit int) ([]repo.Record, *syntax.RecordKey, error) {
	var pageCursor syntax.RecordKey
	all := make([]repo.Record, 0, limit)
	for {
		records, next, err := s.repos.ListRecords(ctx, did, collection, pageCursor, repo.MaxList)
		if err != nil {
			return nil, nil, err
		}
		stop := false
		for _, rec := range records {
			if cursor != "" && rec.Rkey.String() >= cursor.String() {
				stop = true
				continue
			}
			all = append(all, rec)
		}
		if stop || next == nil {
			break
		}
		pageCursor = *next
		if cursor != "" && pageCursor.String() >= cursor.String() {
			break
		}
	}
	if len(all) == 0 {
		return nil, nil, nil
	}
	start := len(all) - limit
	if start < 0 {
		start = 0
	}
	window := all[start:]
	out := make([]repo.Record, len(window))
	for i := range window {
		out[len(window)-1-i] = window[i]
	}
	var next *syntax.RecordKey
	if start > 0 {
		rk := all[start].Rkey
		next = &rk
	}
	return out, next, nil
}

func (s *Server) handleDescribeRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawRepo := r.URL.Query().Get("repo")
	if rawRepo == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "repo is required")
		return
	}
	did, err := s.resolveRepo(ctx, rawRepo)
	if err != nil {
		s.repoError(w, err)
		return
	}
	handle, err := s.repos.OpenRepo(ctx, did, nil)
	if err != nil {
		s.repoError(w, err)
		return
	}
	collections := map[string]bool{}
	err = handle.Walk(ctx, "", func(path string, _ cid.Cid) error {
		i := strings.IndexByte(path, '/')
		if i <= 0 {
			return nil
		}
		nsid := path[:i]
		if _, err := syntax.ParseNSID(nsid); err == nil {
			collections[nsid] = true
		}
		return nil
	})
	if err != nil {
		s.repoError(w, err)
		return
	}
	names := make([]string, 0, len(collections))
	for name := range collections {
		names = append(names, name)
	}
	sort.Strings(names)
	handleName := s.accountHandle(ctx, did)
	writeJSON(w, http.StatusOK, describeRepoOutput{
		DID:             did.String(),
		Handle:          handleName,
		HandleIsCorrect: s.handleIsCorrect(ctx, did, handleName),
		Collections:     names,
		DIDDoc:          s.didDocument(ctx, did),
	})
}

func (s *Server) handleCreateRecord(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	var req createWriteRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	collection, err := syntax.ParseNSID(req.Collection)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
		return
	}
	if req.Record == nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "record is required")
		return
	}
	var rkey syntax.RecordKey
	if req.Rkey != "" {
		rkey, err = syntax.ParseRecordKey(req.Rkey)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid rkey")
			return
		}
	}
	swapCommit, err := parseCIDPtr(req.SwapCommit)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapCommit")
		return
	}
	did, handle, err := s.writeRepo(ctx, info, req.Repo)
	if err != nil {
		s.writeRepoError(w, err)
		return
	}
	ev, err := handle.ApplyWrites(ctx, []repo.WriteOp{{
		Action:     repo.ActionCreate,
		Collection: collection,
		Rkey:       rkey,
		Record:     recordType(req.Record, collection.String()),
	}}, swapCommit)
	if err != nil {
		s.repoError(w, err)
		return
	}
	op, ok := firstRecordOp(ev)
	if !ok || op.CID == nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "write produced no record")
		return
	}
	writeJSON(w, http.StatusOK, createRecordOutput{
		URI:    uriForPath(did, op.Path),
		CID:    op.CID.String(),
		Commit: commitFromEvent(ev),
	})
}

func (s *Server) handlePutRecord(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	var req putWriteRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	collection, err := syntax.ParseNSID(req.Collection)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
		return
	}
	rkey, err := syntax.ParseRecordKey(req.Rkey)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid rkey")
		return
	}
	if req.Record == nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "record is required")
		return
	}
	swapCommit, err := parseCIDPtr(req.SwapCommit)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapCommit")
		return
	}
	swapRecord, err := parseCIDPtr(req.SwapRecord)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapRecord")
		return
	}
	did, handle, err := s.writeRepo(ctx, info, req.Repo)
	if err != nil {
		s.writeRepoError(w, err)
		return
	}
	ev, err := handle.ApplyWrites(ctx, []repo.WriteOp{{
		Action:     repo.WriteAction("put"),
		Collection: collection,
		Rkey:       rkey,
		Record:     recordType(req.Record, collection.String()),
		Prev:       swapRecord,
	}}, swapCommit)
	if err != nil {
		s.repoError(w, err)
		return
	}
	op, ok := firstRecordOp(ev)
	if !ok || op.CID == nil {
		if rec, err := handle.GetRecord(ctx, collection, rkey); err == nil {
			writeJSON(w, http.StatusOK, createRecordOutput{
				URI:    fmt.Sprintf("at://%s/%s/%s", did, collection, rkey),
				CID:    rec.CID.String(),
				Commit: commitFromEvent(ev),
			})
			return
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "write produced no record")
		return
	}
	writeJSON(w, http.StatusOK, createRecordOutput{
		URI:    uriForPath(did, op.Path),
		CID:    op.CID.String(),
		Commit: commitFromEvent(ev),
	})
}

func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	var req deleteWriteRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	collection, err := syntax.ParseNSID(req.Collection)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
		return
	}
	rkey, err := syntax.ParseRecordKey(req.Rkey)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid rkey")
		return
	}
	swapCommit, err := parseCIDPtr(req.SwapCommit)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapCommit")
		return
	}
	swapRecord, err := parseCIDPtr(req.SwapRecord)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapRecord")
		return
	}
	_, handle, err := s.writeRepo(ctx, info, req.Repo)
	if err != nil {
		s.writeRepoError(w, err)
		return
	}
	if _, err := handle.ApplyWrites(ctx, []repo.WriteOp{{
		Action:     repo.ActionDelete,
		Collection: collection,
		Rkey:       rkey,
		Prev:       swapRecord,
	}}, swapCommit); err != nil {
		s.repoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleApplyWrites(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	var req applyWritesRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	if len(req.Writes) == 0 {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "writes is required")
		return
	}
	if len(req.Writes) > repo.MaxWrites {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "too many writes")
		return
	}
	swapCommit, err := parseCIDPtr(req.SwapCommit)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapCommit")
		return
	}
	writes := make([]repo.WriteOp, 0, len(req.Writes))
	inputActions := make([]repo.WriteAction, 0, len(req.Writes))
	inputRkeys := make([]syntax.RecordKey, 0, len(req.Writes))
	for _, write := range req.Writes {
		kind := write.Type
		if i := strings.LastIndexByte(kind, '#'); i >= 0 {
			kind = kind[i+1:]
		}
		collection, err := syntax.ParseNSID(write.Collection)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid collection")
			return
		}
		var rkey syntax.RecordKey
		if write.Rkey != "" {
			rkey, err = syntax.ParseRecordKey(write.Rkey)
			if err != nil {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid rkey")
				return
			}
		}
		var action repo.WriteAction
		switch kind {
		case "create":
			action = repo.ActionCreate
			if write.Value == nil {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "create write requires value")
				return
			}
		case "update":
			action = repo.ActionUpdate
			if rkey == "" {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "update write requires rkey")
				return
			}
			if write.Value == nil {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "update write requires value")
				return
			}
		case "delete":
			action = repo.ActionDelete
			if rkey == "" {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "delete write requires rkey")
				return
			}
			if write.Value != nil {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "delete write must not include value")
				return
			}
		default:
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "unknown write type")
			return
		}
		var prev *cid.Cid
		if write.SwapRecord != nil {
			prev, err = parseCIDPtr(write.SwapRecord)
			if err != nil {
				writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid swapRecord")
				return
			}
		}
		op := repo.WriteOp{
			Action:     action,
			Collection: collection,
			Rkey:       rkey,
			Prev:       prev,
		}
		if action != repo.ActionDelete {
			op.Record = recordType(write.Value, collection.String())
		}
		writes = append(writes, op)
		inputActions = append(inputActions, action)
		inputRkeys = append(inputRkeys, rkey)
	}
	did, handle, err := s.writeRepo(ctx, info, req.Repo)
	if err != nil {
		s.writeRepoError(w, err)
		return
	}
	ev, err := handle.ApplyWrites(ctx, writes, swapCommit)
	if err != nil {
		s.repoError(w, err)
		return
	}
	results, err := s.applyWriteResults(ctx, did, handle, writes, inputActions, inputRkeys, ev)
	if err != nil {
		s.repoError(w, err)
		return
	}
	out := applyWritesOutput{Results: results}
	if commit := commitFromEvent(ev); commit != nil {
		out.Commit = *commit
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) applyWriteResults(ctx context.Context, did syntax.DID, handle repo.Repo, writes []repo.WriteOp, actions []repo.WriteAction, rkeys []syntax.RecordKey, ev *repo.CommitEvent) ([]any, error) {
	used := make([]bool, len(ev.Ops))
	results := make([]any, len(writes))
	for i, action := range actions {
		if action == repo.ActionDelete {
			results[i] = deleteWriteResult{Type: "com.atproto.repo.applyWrites#deleteResult"}
			continue
		}
		path := writes[i].Collection.String() + "/" + rkeys[i].String()
		found := -1
		for j := range ev.Ops {
			if used[j] || ev.Ops[j].Action == repo.ActionDelete {
				continue
			}
			if rkeys[i] != "" {
				if ev.Ops[j].Path == path {
					found = j
					break
				}
			} else if ev.Ops[j].Action == repo.ActionCreate && strings.HasPrefix(ev.Ops[j].Path, writes[i].Collection.String()+"/") {
				found = j
				break
			}
		}
		resultPath := path
		var resultCID *cid.Cid
		resultAction := action
		if found >= 0 {
			used[found] = true
			resultPath = ev.Ops[found].Path
			resultCID = ev.Ops[found].CID
			resultAction = ev.Ops[found].Action
		} else if rkeys[i] != "" {
			rec, err := handle.GetRecord(ctx, writes[i].Collection, rkeys[i])
			if err != nil {
				return nil, err
			}
			resultCID = &rec.CID
		} else {
			return nil, fmt.Errorf("%w: create produced no record op", repo.ErrInvalidRequest)
		}
		cidString := ""
		if resultCID != nil {
			cidString = resultCID.String()
		}
		uri := uriForPath(did, resultPath)
		if resultAction == repo.ActionCreate {
			results[i] = createWriteResult{
				Type:             "com.atproto.repo.applyWrites#createResult",
				URI:              uri,
				CID:              cidString,
				ValidationStatus: "valid",
			}
		} else {
			results[i] = updateWriteResult{
				Type:             "com.atproto.repo.applyWrites#updateResult",
				URI:              uri,
				CID:              cidString,
				ValidationStatus: "valid",
			}
		}
	}
	return results, nil
}

func (s *Server) writeRepo(ctx context.Context, info *authInfo, raw string) (syntax.DID, repo.Repo, error) {
	if raw == "" {
		raw = info.DID.String()
	}
	did, err := s.resolveRepo(ctx, raw)
	if err != nil {
		return "", nil, err
	}
	if did != info.DID {
		return "", nil, errForbidden
	}
	handle, err := s.openRepo(ctx, did)
	if err != nil {
		return "", nil, err
	}
	return did, handle, nil
}

func (s *Server) writeRepoError(w http.ResponseWriter, err error) {
	if errors.Is(err, errForbidden) {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "cannot write to another account")
		return
	}
	s.repoError(w, err)
}

func firstRecordOp(ev *repo.CommitEvent) (*repo.CommitOp, bool) {
	if ev == nil {
		return nil, false
	}
	for i := range ev.Ops {
		op := &ev.Ops[i]
		if op.Action != repo.ActionDelete {
			return op, true
		}
	}
	return nil, false
}

func commitFromEvent(ev *repo.CommitEvent) *commitView {
	if ev == nil {
		return nil
	}
	return &commitView{CID: ev.Commit.String(), Rev: ev.Rev.String()}
}

func uriForPath(did syntax.DID, path string) string {
	return "at://" + did.String() + "/" + path
}

func parseCIDPtr(raw *string) (*cid.Cid, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	c, err := cid.Parse(*raw)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
