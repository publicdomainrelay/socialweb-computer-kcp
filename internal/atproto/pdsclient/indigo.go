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
	cidText := ""
	if expected != nil {
		cidText = expected.String()
	}
	out, err := atproto.RepoGetRecord(ctx, p.API, cidText, path.Collection.String(), path.Repo.String(), path.Rkey.String())
	if err != nil {
		return Record{}, indigoError(err)
	}
	return recordFromIndigo(out.Uri, out.Cid, out.Value)
}

func recordFromIndigo(uri string, cidText *string, value *lexutil.LexiconTypeDecoder) (Record, error) {
	parsedURI, err := syntax.ParseATURI(uri)
	if err != nil {
		return Record{}, fmt.Errorf("%w: record uri %q", ErrProtocol, uri)
	}
	var recordCID cid.Cid
	if cidText != nil && *cidText != "" {
		recordCID, err = cid.Decode(*cidText)
		if err != nil {
			return Record{}, fmt.Errorf("%w: record cid %q", ErrProtocol, *cidText)
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Record{}, fmt.Errorf("%w: encoding the record value: %v", ErrProtocol, err)
	}
	return Record{URI: parsedURI, CID: recordCID, Value: raw}, nil
}

func (p *IndigoPDS) CreateRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	value, err := recordValue(record)
	if err != nil {
		return WriteResult{}, err
	}
	input := &atproto.RepoCreateRecord_Input{Collection: path.Collection.String(), Repo: path.Repo.String(),
		Rkey: stringPtr(path.Rkey.String()), Record: value, Validate: opts.Validate, SwapCommit: cidText(opts.SwapCommit)}
	out, err := atproto.RepoCreateRecord(ctx, p.API, input)
	if err != nil {
		return WriteResult{}, indigoError(err)
	}
	return writeResult(out.Uri, out.Cid, out.Commit)
}

func (p *IndigoPDS) PutRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error) {
	value, err := recordValue(record)
	if err != nil {
		return WriteResult{}, err
	}
	input := &atproto.RepoPutRecord_Input{Collection: path.Collection.String(), Repo: path.Repo.String(),
		Rkey: path.Rkey.String(), Record: value, Validate: opts.Validate, SwapCommit: cidText(opts.SwapCommit), SwapRecord: cidText(opts.SwapRecord)}
	out, err := atproto.RepoPutRecord(ctx, p.API, input)
	if err != nil {
		return WriteResult{}, indigoError(err)
	}
	return writeResult(out.Uri, out.Cid, out.Commit)
}

func (p *IndigoPDS) DeleteRecord(ctx context.Context, path RepoPath, opts WriteOptions) error {
	input := &atproto.RepoDeleteRecord_Input{Collection: path.Collection.String(), Repo: path.Repo.String(),
		Rkey: path.Rkey.String(), SwapCommit: cidText(opts.SwapCommit), SwapRecord: cidText(opts.SwapRecord)}
	_, err := atproto.RepoDeleteRecord(ctx, p.API, input)
	return indigoError(err)
}

func (p *IndigoPDS) ListRecords(ctx context.Context, req ListRecordsRequest) (ListRecordsResult, error) {
	out, err := atproto.RepoListRecords(ctx, p.API, req.Collection.String(), req.Cursor, int64(req.Limit), req.Repo.String(), req.Reverse)
	if err != nil {
		return ListRecordsResult{}, indigoError(err)
	}
	result := ListRecordsResult{Records: make([]Record, 0, len(out.Records))}
	if out.Cursor != nil {
		result.Cursor = *out.Cursor
	}
	for _, item := range out.Records {
		record, err := recordFromIndigo(item.Uri, &item.Cid, item.Value)
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

func recordValue(raw []byte) (*lexutil.LexiconTypeDecoder, error) {
	var value lexutil.LexiconTypeDecoder
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: decoding the record: %v", ErrInvalidRequest, err)
	}
	return &value, nil
}

func cidText(value *cid.Cid) *string {
	if value == nil {
		return nil
	}
	text := value.String()
	return &text
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func writeResult(uri, cidText string, commit *atproto.RepoDefs_CommitMeta) (WriteResult, error) {
	parsed, err := syntax.ParseATURI(uri)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: record uri %q", ErrProtocol, uri)
	}
	recordCID, err := cid.Decode(cidText)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: record cid %q", ErrProtocol, cidText)
	}
	result := WriteResult{URI: parsed, CID: recordCID}
	if commit != nil {
		commitCID, err := cid.Decode(commit.Cid)
		if err != nil {
			return WriteResult{}, fmt.Errorf("%w: commit cid %q", ErrProtocol, commit.Cid)
		}
		rev, err := syntax.ParseTID(commit.Rev)
		if err != nil {
			return WriteResult{}, fmt.Errorf("%w: commit rev %q", ErrProtocol, commit.Rev)
		}
		result.Commit = &CommitRef{CID: commitCID, Rev: rev}
	}
	return result, nil
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
