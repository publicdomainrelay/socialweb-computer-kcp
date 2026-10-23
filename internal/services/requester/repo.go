package requester

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

const (
	RecordRequestTimeout = 30 * time.Second
)

type RecordStore interface {
	market.RecordWriter

	market.RecordGetter

	market.RecordLister

	market.RecordResolver
}

type LocalRepo struct {
	store *repo.SQLiteStore

	handle repo.Repo

	did string

	remote market.RecordResolver

	clock *syntax.TIDClock
}

func NewLocalRepo(ctx context.Context, path, did string, key atcrypto.PrivateKey, remote market.RecordResolver) (*LocalRepo, error) {
	if path == "" {
		return nil, errors.New("requester: the local repo has no path")
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return nil, fmt.Errorf("requester: repo DID: %w", err)
	}
	store, err := repo.NewSQLiteStore(path)
	if err != nil {
		return nil, err
	}
	exists, err := store.RepoExists(ctx, parsed)
	if err != nil {
		store.Close()
		return nil, err
	}
	var handle repo.Repo
	if exists {
		handle, err = store.OpenRepo(ctx, parsed, key)
	} else {
		handle, err = store.CreateRepo(ctx, parsed, key)
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return &LocalRepo{store: store, handle: handle, did: did, remote: remote, clock: syntax.NewTIDClock(0)}, nil
}

func (r *LocalRepo) DID() string {
	if r == nil {
		return ""
	}
	return r.did
}

func (r *LocalRepo) CreateRecord(ctx context.Context, repoDID, collection string, record map[string]any) (market.StrongRef, error) {
	if r == nil || r.handle == nil {
		return market.StrongRef{}, errors.New("requester: the local repo is not open")
	}
	if repoDID != "" && repoDID != r.did {
		return market.StrongRef{}, fmt.Errorf("requester: the local repo is %s, not %s", r.did, repoDID)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("requester: %w", err)
	}
	rec, _, err := r.handle.CreateRecord(ctx, nsid, syntax.RecordKey(r.clock.Next()), record)
	if err != nil {
		return market.StrongRef{}, err
	}
	return market.NewStrongRef("at://"+r.did+"/"+collection+"/"+rec.Rkey.String(), rec.CID.String()), nil
}

func (r *LocalRepo) PutRecord(ctx context.Context, repoDID, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if r == nil || r.handle == nil {
		return market.StrongRef{}, errors.New("requester: the local repo is not open")
	}
	if repoDID != "" && repoDID != r.did {
		return market.StrongRef{}, fmt.Errorf("requester: the local repo is %s, not %s", r.did, repoDID)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("requester: %w", err)
	}
	rk, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return market.StrongRef{}, fmt.Errorf("requester: %w", err)
	}
	rec, _, err := r.handle.PutRecord(ctx, nsid, rk, record, nil)
	if err != nil {
		return market.StrongRef{}, err
	}
	return market.NewStrongRef("at://"+r.did+"/"+collection+"/"+rec.Rkey.String(), rec.CID.String()), nil
}

func (r *LocalRepo) DeleteRecord(ctx context.Context, repoDID, collection, rkey string) error {
	if r == nil || r.handle == nil {
		return errors.New("requester: the local repo is not open")
	}
	if repoDID != "" && repoDID != r.did {
		return fmt.Errorf("requester: the local repo is %s, not %s", r.did, repoDID)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return fmt.Errorf("requester: %w", err)
	}
	rk, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return fmt.Errorf("requester: %w", err)
	}
	_, err = r.handle.DeleteRecord(ctx, nsid, rk, nil)
	return err
}

func (r *LocalRepo) GetRecord(ctx context.Context, repoDID, collection, rkey string) (market.ResolvedRecord, error) {
	if r == nil {
		return market.ResolvedRecord{}, errors.New("requester: the local repo is not open")
	}
	if repoDID != "" && repoDID != r.did {
		return r.remoteGet(ctx, repoDID, collection, rkey)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return market.ResolvedRecord{}, fmt.Errorf("requester: %w", err)
	}
	rk, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return market.ResolvedRecord{}, fmt.Errorf("requester: %w", err)
	}
	rec, err := r.handle.GetRecord(ctx, nsid, rk)
	if err != nil {
		if errors.Is(err, repo.ErrRecordNotFound) {
			return market.ResolvedRecord{}, market.ErrNotFound
		}
		return market.ResolvedRecord{}, err
	}
	return market.ResolvedRecord{URI: "at://" + r.did + "/" + collection + "/" + rkey, CID: rec.CID.String(), Value: rec.Value}, nil
}

func (r *LocalRepo) ListRecords(ctx context.Context, repoDID, collection string, limit int, cursor string) (market.ListPage, error) {
	if r == nil {
		return market.ListPage{}, errors.New("requester: the local repo is not open")
	}
	if repoDID != "" && repoDID != r.did {
		if r.remote == nil {
			return market.ListPage{}, errors.New("requester: no remote record lister")
		}
		lister, ok := r.remote.(market.RecordLister)
		if !ok {
			return market.ListPage{}, errors.New("requester: the record resolver is not a lister")
		}
		return lister.ListRecords(ctx, repoDID, collection, limit, cursor)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return market.ListPage{}, fmt.Errorf("requester: %w", err)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	cursorKey := syntax.RecordKey("")
	if cursor != "" {
		cursorKey, err = syntax.ParseRecordKey(cursor)
		if err != nil {
			return market.ListPage{}, fmt.Errorf("requester: %w", err)
		}
	}
	records, next, err := r.handle.ListRecords(ctx, nsid, cursorKey, limit)
	if err != nil {
		return market.ListPage{}, err
	}
	page := market.ListPage{Records: make([]market.ListedRecord, 0, len(records))}
	for _, rec := range records {
		page.Records = append(page.Records, market.ListedRecord{
			URI:   "at://" + r.did + "/" + collection + "/" + rec.Rkey.String(),
			CID:   rec.CID.String(),
			Value: rec.Value,
		})
	}
	if next != nil {
		page.Cursor = next.String()
	}
	return page, nil
}

func (r *LocalRepo) Resolve(ctx context.Context, ref market.RecordRef) (market.ResolvedRecord, error) {
	if r == nil {
		return market.ResolvedRecord{}, errors.New("requester: the local repo is not open")
	}
	if market.ATURIAuthority(ref.URI) == r.did {
		uri, err := market.ParseATURI(ref.URI)
		if err != nil {
			return market.ResolvedRecord{}, err
		}
		found, err := r.GetRecord(ctx, r.did, uri.Collection().String(), uri.RecordKey().String())
		if err != nil {
			return market.ResolvedRecord{}, err
		}
		if ref.CID != "" && found.CID != ref.CID {
			return market.ResolvedRecord{}, fmt.Errorf("%w: %s has cid %s, not %s", market.ErrInvalidRef, ref.URI, found.CID, ref.CID)
		}
		return found, nil
	}
	if r.remote == nil {
		return market.ResolvedRecord{}, fmt.Errorf("%w: no remote resolver for %s", market.ErrNotFound, ref.URI)
	}
	return r.remote.Resolve(ctx, ref)
}

func (r *LocalRepo) remoteGet(ctx context.Context, repoDID, collection, rkey string) (market.ResolvedRecord, error) {
	if r.remote == nil {
		return market.ResolvedRecord{}, fmt.Errorf("%w: no remote resolver", market.ErrNotFound)
	}
	getter, ok := r.remote.(market.RecordGetter)
	if !ok {
		return r.remote.Resolve(ctx, market.RecordRef{URI: "at://" + repoDID + "/" + collection + "/" + rkey})
	}
	return getter.GetRecord(ctx, repoDID, collection, rkey)
}

func (r *LocalRepo) Close() error {
	if r == nil || r.store == nil {
		return nil
	}
	return r.store.Close()
}

type PDSStore struct {
	pds pdsclient.PDS

	did string

	remote market.RecordResolver
}

type PDSOptions struct {
	PDS pdsclient.PDS

	DID string

	Remote market.RecordResolver
}

func NewPDSStore(opts PDSOptions) (*PDSStore, error) {
	if opts.PDS == nil {
		return nil, errors.New("requester: the PDS store has no client")
	}
	return &PDSStore{pds: opts.PDS, did: opts.DID, remote: opts.Remote}, nil
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
	return fmt.Errorf("requester: %s: %w", op, err)
}

func encodeRecord(record map[string]any) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("requester: encoding a record: %w", err)
	}
	return raw, nil
}

func decodeRecordValue(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("%w: the record has no value", market.ErrNotFound)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("requester: decoding a record value: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("%w: the record has no value", market.ErrNotFound)
	}
	return value, nil
}

func (p *PDSStore) CreateRecord(ctx context.Context, repoDID, collection string, record map[string]any) (market.StrongRef, error) {
	if p == nil || p.pds == nil {
		return market.StrongRef{}, errors.New("requester: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repoDID, collection, "")
	if err != nil {
		return market.StrongRef{}, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	result, err := p.pds.CreateRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return market.StrongRef{}, pdsError("createRecord of "+repoDID+"/"+collection, err)
	}
	return market.NewStrongRef(result.URI.String(), result.CID.String()), nil
}

func (p *PDSStore) PutRecord(ctx context.Context, repoDID, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if p == nil || p.pds == nil {
		return market.StrongRef{}, errors.New("requester: the PDS store is not configured")
	}
	if rkey == "" {
		rkey = syntax.NewTIDNow(0).String()
	}
	path, err := pdsclient.ParseRepoPath(repoDID, collection, rkey)
	if err != nil {
		return market.StrongRef{}, err
	}
	raw, err := encodeRecord(record)
	if err != nil {
		return market.StrongRef{}, err
	}
	result, err := p.pds.PutRecord(ctx, path, raw, pdsclient.WriteOptions{})
	if err != nil {
		return market.StrongRef{}, pdsError("putRecord of "+repoDID+"/"+collection+"/"+rkey, err)
	}
	uri := result.URI.String()
	if uri == "" {
		uri = "at://" + repoDID + "/" + collection + "/" + rkey
	}
	if !result.CID.Defined() {
		return market.StrongRef{}, fmt.Errorf("requester: putRecord of %s returned no cid", uri)
	}
	return market.NewStrongRef(uri, result.CID.String()), nil
}

func (p *PDSStore) DeleteRecord(ctx context.Context, repoDID, collection, rkey string) error {
	if p == nil || p.pds == nil {
		return errors.New("requester: the PDS store is not configured")
	}
	path, err := pdsclient.ParseRepoPath(repoDID, collection, rkey)
	if err != nil {
		return err
	}
	if err := p.pds.DeleteRecord(ctx, path, pdsclient.WriteOptions{}); err != nil {
		return pdsError("deleteRecord of "+repoDID+"/"+collection+"/"+rkey, err)
	}
	return nil
}

func (p *PDSStore) GetRecord(ctx context.Context, repoDID, collection, rkey string) (market.ResolvedRecord, error) {
	if p == nil || p.pds == nil {
		return market.ResolvedRecord{}, errors.New("requester: the PDS store is not configured")
	}
	if repoDID != p.did {
		if reader, ok := p.remote.(market.RecordGetter); ok && p.remote != nil {
			return reader.GetRecord(ctx, repoDID, collection, rkey)
		}
	}
	path, err := pdsclient.ParseRepoPath(repoDID, collection, rkey)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	record, err := p.pds.GetRecord(ctx, path, nil)
	if err != nil {
		return market.ResolvedRecord{}, pdsError("getRecord of "+repoDID+"/"+collection+"/"+rkey, err)
	}
	value, err := decodeRecordValue(record.Value)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	uri := record.URI.String()
	if uri == "" {
		uri = "at://" + repoDID + "/" + collection + "/" + rkey
	}
	return market.ResolvedRecord{URI: uri, CID: record.CID.String(), Value: value}, nil
}

func (p *PDSStore) ListRecords(ctx context.Context, repoDID, collection string, limit int, cursor string) (market.ListPage, error) {
	if p == nil || p.pds == nil {
		return market.ListPage{}, errors.New("requester: the PDS store is not configured")
	}
	if repoDID != p.did {
		if reader, ok := p.remote.(market.RecordLister); ok && p.remote != nil {
			return reader.ListRecords(ctx, repoDID, collection, limit, cursor)
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	path, err := pdsclient.ParseRepoPath(repoDID, collection, "")
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
		return market.ListPage{}, pdsError("listRecords of "+repoDID+"/"+collection, err)
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
	uri, err := market.ParseATURI(ref.URI)
	if err != nil {
		return market.ResolvedRecord{}, err
	}
	if uri.Authority().String() == p.did {
		found, err := p.GetRecord(ctx, p.did, uri.Collection().String(), uri.RecordKey().String())
		if err != nil {
			return market.ResolvedRecord{}, err
		}
		if ref.CID != "" && found.CID != ref.CID {
			return market.ResolvedRecord{}, fmt.Errorf("%w: %s has cid %s, not %s", market.ErrInvalidRef, ref.URI, found.CID, ref.CID)
		}
		return found, nil
	}
	if p.remote == nil {
		return market.ResolvedRecord{}, fmt.Errorf("%w: no remote resolver for %s", market.ErrNotFound, ref.URI)
	}
	return p.remote.Resolve(ctx, ref)
}

func (p *PDSStore) Close() error {
	return nil
}

func repoPath(stateDir string) string {
	return filepath.Join(stateDir, "repo.sqlite")
}
