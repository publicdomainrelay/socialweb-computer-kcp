package pdsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/syntax"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/ipfs/go-cid"
)

type IndigoPDS struct {
	API *atclient.APIClient

	BaseURL string

	Subscribe func(ctx context.Context, cursor *int64) (RepoEventStream, error)
}

var _ PDS = (*IndigoPDS)(nil)

func NewIndigoPDS(baseURL string, client *http.Client, auth atclient.AuthMethod) (*IndigoPDS, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("%w: no PDS base URL", ErrInvalidConfig)
	}
	api := atclient.NewAPIClient(baseURL)
	if client != nil {
		api.Client = client
	}
	api.Auth = auth
	p := &IndigoPDS{API: api, BaseURL: baseURL}
	return p, nil
}

func indigoError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) {
		return &XRPCError{StatusCode: apiErr.StatusCode, Code: apiErr.Name, Message: apiErr.Message}
	}
	return err
}

func (p *IndigoPDS) Describe(ctx context.Context) (Info, error) {
	out, err := atproto.ServerDescribeServer(ctx, p.API)
	if err != nil {
		return Info{}, indigoError(err)
	}
	did, err := syntax.ParseDID(out.Did)
	if err != nil {
		return Info{}, fmt.Errorf("%w: the PDS did %q", ErrProtocol, out.Did)
	}
	info := Info{DID: did, AvailableUserDomains: out.AvailableUserDomains}
	return info, nil
}

func (p *IndigoPDS) GetRecord(ctx context.Context, path RepoPath, expected *cid.Cid) (Record, error) {
	params := map[string]any{"repo": path.Repo.String(), "collection": path.Collection.String(), "rkey": path.Rkey.String()}
	if expected != nil {
		params["cid"] = expected.String()
	}
	var out rawRecord
	if err := p.API.LexDo(ctx, lexutil.Query, "", "com.atproto.repo.getRecord", params, nil, &out); err != nil {
		return Record{}, indigoError(err)
	}
	return out.record()
}

type rawRecord struct {
	Uri   string          `json:"uri"`
	Cid   *string         `json:"cid,omitempty"`
	Value json.RawMessage `json:"value"`
}

func (r rawRecord) record() (Record, error) {
	parsedURI, err := syntax.ParseATURI(r.Uri)
	if err != nil {
		return Record{}, fmt.Errorf("%w: record uri %q", ErrProtocol, r.Uri)
	}
	var recordCID cid.Cid
	if r.Cid != nil && *r.Cid != "" {
		recordCID, err = cid.Decode(*r.Cid)
		if err != nil {
			return Record{}, fmt.Errorf("%w: record cid %q", ErrProtocol, *r.Cid)
		}
	}
	return Record{URI: parsedURI, CID: recordCID, Value: r.Value}, nil
}

type rawWriteOutput struct {
	Uri    string             `json:"uri"`
	Cid    string             `json:"cid"`
	Commit *rawCommitMetadata `json:"commit,omitempty"`
}

type rawCommitMetadata struct {
	Cid string `json:"cid"`
	Rev string `json:"rev"`
}

func (r rawWriteOutput) result() (WriteResult, error) {
	parsed, err := syntax.ParseATURI(r.Uri)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: record uri %q", ErrProtocol, r.Uri)
	}
	recordCID, err := cid.Decode(r.Cid)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: record cid %q", ErrProtocol, r.Cid)
	}
	result := WriteResult{URI: parsed, CID: recordCID}
	if r.Commit != nil {
		commitCID, err := cid.Decode(r.Commit.Cid)
		if err != nil {
			return WriteResult{}, fmt.Errorf("%w: commit cid %q", ErrProtocol, r.Commit.Cid)
		}
		rev, err := syntax.ParseTID(r.Commit.Rev)
		if err != nil {
			return WriteResult{}, fmt.Errorf("%w: commit rev %q", ErrProtocol, r.Commit.Rev)
		}
		result.Commit = &CommitRef{CID: commitCID, Rev: rev}
	}
	return result, nil
}

func (p *IndigoPDS) CreateRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	input := map[string]any{"repo": path.Repo.String(), "collection": path.Collection.String(),
		"rkey": path.Rkey.String(), "record": json.RawMessage(record), "validate": opts.Validate}
	if opts.SwapCommit != nil {
		input["swapCommit"] = opts.SwapCommit.String()
	}
	var out rawWriteOutput
	if err := p.API.LexDo(ctx, lexutil.Procedure, "application/json", "com.atproto.repo.createRecord", nil, input, &out); err != nil {
		return WriteResult{}, indigoError(err)
	}
	return out.result()
}

func (p *IndigoPDS) PutRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	input := map[string]any{"repo": path.Repo.String(), "collection": path.Collection.String(),
		"rkey": path.Rkey.String(), "record": json.RawMessage(record), "validate": opts.Validate}
	if opts.SwapCommit != nil {
		input["swapCommit"] = opts.SwapCommit.String()
	}
	if opts.SwapRecord != nil {
		input["swapRecord"] = opts.SwapRecord.String()
	}
	var out rawWriteOutput
	if err := p.API.LexDo(ctx, lexutil.Procedure, "application/json", "com.atproto.repo.putRecord", nil, input, &out); err != nil {
		return WriteResult{}, indigoError(err)
	}
	return out.result()
}

func (p *IndigoPDS) DeleteRecord(ctx context.Context, path RepoPath, opts WriteOptions) error {
	input := map[string]any{"repo": path.Repo.String(), "collection": path.Collection.String(), "rkey": path.Rkey.String()}
	if opts.SwapCommit != nil {
		input["swapCommit"] = opts.SwapCommit.String()
	}
	if opts.SwapRecord != nil {
		input["swapRecord"] = opts.SwapRecord.String()
	}
	var out struct{}
	return indigoError(p.API.LexDo(ctx, lexutil.Procedure, "application/json", "com.atproto.repo.deleteRecord", nil, input, &out))
}

func (p *IndigoPDS) ListRecords(ctx context.Context, req ListRecordsRequest) (ListRecordsResult, error) {
	params := map[string]any{"repo": req.Repo.String(), "collection": req.Collection.String()}
	if req.Cursor != "" {
		params["cursor"] = req.Cursor
	}
	if req.Limit > 0 {
		params["limit"] = req.Limit
	}
	if req.Reverse {
		params["reverse"] = true
	}
	var out struct {
		Cursor  *string     `json:"cursor,omitempty"`
		Records []rawRecord `json:"records"`
	}
	if err := p.API.LexDo(ctx, lexutil.Query, "", "com.atproto.repo.listRecords", params, nil, &out); err != nil {
		return ListRecordsResult{}, indigoError(err)
	}
	result := ListRecordsResult{Records: make([]Record, 0, len(out.Records))}
	if out.Cursor != nil {
		result.Cursor = *out.Cursor
	}
	for _, item := range out.Records {
		record, err := item.record()
		if err != nil {
			return ListRecordsResult{}, err
		}
		result.Records = append(result.Records, record)
	}
	return result, nil
}

func (p *IndigoPDS) GetRepo(ctx context.Context, did syntax.DID, since *syntax.TID) (io.ReadCloser, error) {
	cursor := ""
	if since != nil {
		cursor = since.String()
	}
	raw, err := atproto.SyncGetRepo(ctx, p.API, did.String(), cursor)
	if err != nil {
		return nil, indigoError(err)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (p *IndigoPDS) GetLatestCommit(ctx context.Context, did syntax.DID) (cid.Cid, syntax.TID, error) {
	out, err := atproto.SyncGetLatestCommit(ctx, p.API, did.String())
	if err != nil {
		return cid.Undef, "", indigoError(err)
	}
	commit, err := cid.Decode(out.Cid)
	if err != nil {
		return cid.Undef, "", fmt.Errorf("%w: commit cid %q", ErrProtocol, out.Cid)
	}
	rev, err := syntax.ParseTID(out.Rev)
	if err != nil {
		return cid.Undef, "", fmt.Errorf("%w: commit rev %q", ErrProtocol, out.Rev)
	}
	return commit, rev, nil
}

func (p *IndigoPDS) SubscribeRepos(ctx context.Context, cursor *int64) (RepoEventStream, error) {
	if p.Subscribe == nil {
		return nil, fmt.Errorf("%w: subscriptions are not configured", ErrInvalidConfig)
	}
	return p.Subscribe(ctx, cursor)
}

func (p *IndigoPDS) CreateSession(ctx context.Context, identifier, password string) (Session, error) {
	out, err := atproto.ServerCreateSession(ctx, p.API, &atproto.ServerCreateSession_Input{Identifier: identifier, Password: password})
	if err != nil {
		return Session{}, indigoError(err)
	}
	return sessionFromIndigo(out.Did, out.Handle, out.AccessJwt, out.RefreshJwt, out.Active)
}

func (p *IndigoPDS) CreateAccount(ctx context.Context, req CreateAccountRequest) (Session, error) {
	account := &atproto.ServerCreateAccount_Input{Handle: req.Identifier, Password: stringPtr(req.Password), Email: stringPtr(req.Email), InviteCode: stringPtr(req.InviteCode)}
	out, err := atproto.ServerCreateAccount(ctx, p.API, account)
	if err != nil {
		return Session{}, indigoError(err)
	}
	return sessionFromIndigo(out.Did, out.Handle, out.AccessJwt, out.RefreshJwt, nil)
}

func (p *IndigoPDS) GetSession(ctx context.Context) (Session, error) {
	out, err := atproto.ServerGetSession(ctx, p.API)
	if err != nil {
		return Session{}, indigoError(err)
	}
	return sessionFromIndigo(out.Did, out.Handle, "", "", out.Active)
}

func (p *IndigoPDS) DeleteSession(ctx context.Context) error {
	return indigoError(atproto.ServerDeleteSession(ctx, p.API))
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func sessionFromIndigo(didText, handle, access, refresh string, active *bool) (Session, error) {
	did, err := syntax.ParseDID(didText)
	if err != nil {
		return Session{}, fmt.Errorf("%w: session did %q", ErrProtocol, didText)
	}
	session := Session{DID: did, Handle: handle, AccessJWT: access, RefreshJWT: refresh}
	if active != nil {
		session.Active = *active
	}
	return session, nil
}
