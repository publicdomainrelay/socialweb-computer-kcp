package bidder

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

const (
	RecordRequestLimit = 2 << 20

	RecordReplyLimit = 8 << 20

	RecordRequestTimeout = 30 * time.Second
)

type RecordStore interface {
	market.RecordWriter

	CreateRecordAt(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error)

	market.RecordGetter

	market.RecordLister

	market.RecordResolver
}

type PDSOptions struct {
	PDS pdsclient.PDS

	DID string
}

type PDSStore struct {
	pds pdsclient.PDS

	did string
}

func NewPDSStore(opts PDSOptions) (*PDSStore, error) {
	if opts.PDS == nil {
		return nil, errors.New("bidder: the PDS store has no PDS client")
	}
	return &PDSStore{pds: opts.PDS, did: opts.DID}, nil
}

func (p *PDSStore) DID() string {
	if p == nil {
		return ""
	}
	return p.did
}

func pdsError(op string, err error) error {
	var failure *pdsclient.XRPCError
	if errors.As(err, &failure) && (failure.StatusCode == 404 || failure.Code == "RecordNotFound") {
		return fmt.Errorf("%w: %s: %s", market.ErrNotFound, op, failure.Message)
	}
	return fmt.Errorf("bidder: %s: %w", op, err)
}

func encodeRecord(record map[string]any) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("bidder: encoding a record: %w", err)
	}
	return raw, nil
}

func decodeRecordValue(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("%w: the record has no value", market.ErrNotFound)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("bidder: decoding a record value: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("%w: the record has no value", market.ErrNotFound)
	}
	return value, nil
}

func (p *PDSStore) CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (market.StrongRef, error) {
	if p == nil || p.pds == nil {
		return market.StrongRef{}, errors.New("bidder: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, "")
	if err != nil {
		return market.StrongRef{}, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	result, err := p.pds.CreateRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return market.StrongRef{}, pdsError("createRecord of "+repo+"/"+collection, err)
	}
	return market.NewStrongRef(result.URI.String(), result.CID.String()), nil
}

func (p *PDSStore) CreateRecordAt(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if p == nil || p.pds == nil {
		return market.StrongRef{}, errors.New("bidder: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, rkey)
	if err != nil {
		return market.StrongRef{}, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	result, err := p.pds.CreateRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return market.StrongRef{}, pdsError("createRecord of "+repo+"/"+collection+"/"+rkey, err)
	}
	return market.NewStrongRef(result.URI.String(), result.CID.String()), nil
}

func (p *PDSStore) PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if p == nil || p.pds == nil {
		return market.StrongRef{}, errors.New("bidder: the PDS store is not configured")
	}
	if rkey == "" {
		rkey = syntax.NewTIDNow(0).String()
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, rkey)
	if err != nil {
		return market.StrongRef{}, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	result, err := p.pds.PutRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return market.StrongRef{}, pdsError("putRecord of "+repo+"/"+collection+"/"+rkey, err)
	}
	uri := result.URI.String()
	uri = cmp.Or(uri, "at://"+repo+"/"+collection+"/"+rkey)
	if !result.CID.Defined() {
		return market.StrongRef{}, fmt.Errorf("bidder: putRecord of %s returned no cid", uri)
	}
	return market.NewStrongRef(uri, result.CID.String()), nil
}

func (p *PDSStore) DeleteRecord(ctx context.Context, repo, collection, rkey string) error {
	if p == nil || p.pds == nil {
		return errors.New("bidder: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, rkey)
	if err != nil {
		return err
	}
	if err := p.pds.DeleteRecord(ctx, path, pdsclient.WriteOptions{}); err != nil {
		return pdsError("deleteRecord of "+repo+"/"+collection+"/"+rkey, err)
	}
	return nil
}

func (p *PDSStore) GetRecord(ctx context.Context, repo, collection, rkey string) (market.ResolvedRecord, error) {
	if p == nil || p.pds == nil {
		return market.ResolvedRecord{}, errors.New("bidder: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, rkey)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	record, err := p.pds.GetRecord(ctx, path, nil)
	if err != nil {
		return market.ResolvedRecord{}, pdsError("getRecord of "+repo+"/"+collection+"/"+rkey, err)
	}
	value, err := decodeRecordValue(record.Value)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	uri := record.URI.String()
	uri = cmp.Or(uri, "at://"+repo+"/"+collection+"/"+rkey)
	return market.ResolvedRecord{URI: uri, CID: record.CID.String(), Value: value}, nil
}

func (p *PDSStore) ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (market.ListPage, error) {
	if p == nil || p.pds == nil {
		return market.ListPage{}, errors.New("bidder: the PDS store is not configured")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	path, err := pdsclient.ParseRepoPath(repo, collection, "")
	if err != nil {
		return market.ListPage{}, err
	}
	result, err := p.pds.ListRecords(ctx, pdsclient.ListRecordsRequest{
		Repo:       path.Repo,
		Collection: path.Collection,
		Cursor:     cursor,
		Limit:      limit,
	})
	if err != nil {
		return market.ListPage{}, pdsError("listRecords of "+repo+"/"+collection, err)
	}
	page := market.ListPage{Cursor: result.Cursor, Records: make([]market.ListedRecord, 0, len(result.Records))}
	for _, record := range result.Records {
		value, err := decodeRecordValue(record.Value)
		if err != nil {
			return market.ListPage{}, err
		}
		page.Records = append(page.Records, market.ListedRecord{URI: record.URI.String(), CID: record.CID.String(), Value: value})
	}
	return page, nil
}

func (p *PDSStore) Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error) {
	aturi, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %v", market.ErrInvalidRef, err)
	}
	repo := string(aturi.Authority())
	collection := string(aturi.Collection())
	rkey := string(aturi.RecordKey())
	if repo == "" || collection == "" || rkey == "" {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %q is not a record AT URI", market.ErrInvalidRef, ref.URI)
	}
	rec, err := p.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	rec.CID = cmp.Or(rec.CID, ref.CID)
	return rec, nil
}

type routedStore struct {
	did string

	local *PDSStore

	remote *market.HTTPReader
}

func NewRoutedStore(did string, local *PDSStore, remote *market.HTTPReader) *routedStore {
	return &routedStore{did: did, local: local, remote: remote}
}

func (r *routedStore) CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (market.StrongRef, error) {
	return r.local.CreateRecord(ctx, repo, collection, record)
}

func (r *routedStore) CreateRecordAt(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	return r.local.CreateRecordAt(ctx, repo, collection, rkey, record)
}

func (r *routedStore) PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	return r.local.PutRecord(ctx, repo, collection, rkey, record)
}

func (r *routedStore) DeleteRecord(ctx context.Context, repo, collection, rkey string) error {
	return r.local.DeleteRecord(ctx, repo, collection, rkey)
}

func (r *routedStore) GetRecord(ctx context.Context, repo, collection, rkey string) (market.ResolvedRecord, error) {
	if repo == r.did || r.remote == nil {
		return r.local.GetRecord(ctx, repo, collection, rkey)
	}
	return r.remote.GetRecord(ctx, repo, collection, rkey)
}

func (r *routedStore) ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (market.ListPage, error) {
	if repo == r.did || r.remote == nil {
		return r.local.ListRecords(ctx, repo, collection, limit, cursor)
	}
	return r.remote.ListRecords(ctx, repo, collection, limit, cursor)
}

func (r *routedStore) Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error) {
	aturi, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %v", market.ErrInvalidRef, err)
	}
	repo := string(aturi.Authority())
	collection := string(aturi.Collection())
	rkey := string(aturi.RecordKey())
	if repo == "" || collection == "" || rkey == "" {
		return market.ResolvedRecord{}, fmt.Errorf("%w: %q is not a record AT URI", market.ErrInvalidRef, ref.URI)
	}
	rec, err := r.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	rec.CID = cmp.Or(rec.CID, ref.CID)
	return rec, nil
}
