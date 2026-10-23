package market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

type Foreign interface {
	RecordGetter
	RecordLister
	RecordResolver
}

type RepoStoreConfig struct {
	DID string

	Open func(ctx context.Context) (atprotorepo.Repo, error)

	Foreign Foreign
}

type RepoStore struct {
	did string

	open func(ctx context.Context) (atprotorepo.Repo, error)

	foreign Foreign
}

var (
	_ RecordWriter   = (*RepoStore)(nil)
	_ RecordGetter   = (*RepoStore)(nil)
	_ RecordLister   = (*RepoStore)(nil)
	_ RecordResolver = (*RepoStore)(nil)
)

func NewRepoStore(cfg RepoStoreConfig) *RepoStore {
	return &RepoStore{did: strings.TrimSpace(cfg.DID), open: cfg.Open, foreign: cfg.Foreign}
}

func (s *RepoStore) DID() string { return s.did }

func (s *RepoStore) repo(ctx context.Context, did string) (atprotorepo.Repo, error) {
	if s.open == nil {
		return nil, errors.New("market: the repo store has no repo handle")
	}
	if did != "" && did != s.did {
		return nil, fmt.Errorf("%w: the repo store writes %s, not %s", ErrInvalidRef, s.did, did)
	}
	return s.open(ctx)
}

func (s *RepoStore) local(repo string) bool {
	return s.foreign == nil || repo == "" || repo == s.did
}

func mapError(err error) error {
	if errors.Is(err, atprotorepo.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func encodeValue(record map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("market: encoding a record: %w", err)
	}
	return raw, nil
}

func decodeValue(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("%w: the record has no value", ErrNotFound)
	}
	var value map[string]any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, fmt.Errorf("market: decoding a record value: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("%w: the record has no value", ErrNotFound)
	}
	return value, nil
}

func (s *RepoStore) write(ctx context.Context, repoDID, collection, rkey string, record map[string]any, create bool) (StrongRef, error) {
	repo, err := s.repo(ctx, repoDID)
	if err != nil {
		return StrongRef{}, err
	}
	raw, err := encodeValue(record)
	if err != nil {
		return StrongRef{}, err
	}
	var ref commonv1.StrongRef
	if create {
		ref, err = repo.CreateRecord(ctx, collection, rkey, raw)
	} else {
		ref, err = repo.PutRecord(ctx, collection, rkey, raw)
	}
	if err != nil {
		return StrongRef{}, fmt.Errorf("market: writing %s/%s/%s: %w", s.did, collection, rkey, err)
	}
	return NewStrongRef(ref.URI, ref.CID), nil
}

func (s *RepoStore) CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (StrongRef, error) {
	return s.write(ctx, repo, collection, "", record, true)
}

func (s *RepoStore) CreateRecordAt(ctx context.Context, repo, collection, rkey string, record map[string]any) (StrongRef, error) {
	return s.write(ctx, repo, collection, rkey, record, true)
}

func (s *RepoStore) PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (StrongRef, error) {
	if rkey == "" {
		rkey = syntax.NewTIDNow(0).String()
	}
	return s.write(ctx, repo, collection, rkey, record, false)
}

func (s *RepoStore) DeleteRecord(ctx context.Context, repoDID, collection, rkey string) error {
	repo, err := s.repo(ctx, repoDID)
	if err != nil {
		return err
	}
	return mapError(repo.DeleteRecord(ctx, collection, rkey))
}

func (s *RepoStore) GetRecord(ctx context.Context, repoDID, collection, rkey string) (ResolvedRecord, error) {
	if !s.local(repoDID) {
		return s.foreign.GetRecord(ctx, repoDID, collection, rkey)
	}
	repo, err := s.repo(ctx, "")
	if err != nil {
		return ResolvedRecord{}, err
	}
	rec, err := repo.GetRecord(ctx, repoDID, collection, rkey)
	if err != nil {
		return ResolvedRecord{}, mapError(err)
	}
	value, err := decodeValue(rec.Value)
	if err != nil {
		return ResolvedRecord{}, err
	}
	return ResolvedRecord{URI: rec.URI, CID: rec.CID, Value: value}, nil
}

func (s *RepoStore) ListRecords(ctx context.Context, repoDID, collection string, limit int, cursor string) (ListPage, error) {
	if !s.local(repoDID) {
		return s.foreign.ListRecords(ctx, repoDID, collection, limit, cursor)
	}
	repo, err := s.repo(ctx, "")
	if err != nil {
		return ListPage{}, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page, err := repo.ListRecords(ctx, repoDID, collection, atprotorepo.ListOptions{Limit: limit, Cursor: cursor})
	if err != nil {
		return ListPage{}, mapError(err)
	}
	out := ListPage{Cursor: page.Cursor, Records: make([]ListedRecord, 0, len(page.Records))}
	for _, rec := range page.Records {
		value, err := decodeValue(rec.Value)
		if err != nil {
			return ListPage{}, err
		}
		out.Records = append(out.Records, ListedRecord{URI: rec.URI, CID: rec.CID, Value: value})
	}
	return out, nil
}

func (s *RepoStore) Resolve(ctx context.Context, ref RecordRef) (ResolvedRecord, error) {
	aturi, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return ResolvedRecord{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	repo, collection, rkey := aturi.Authority().String(), aturi.Collection().String(), aturi.RecordKey().String()
	if repo == "" || collection == "" || rkey == "" {
		return ResolvedRecord{}, fmt.Errorf("%w: %q is not a record AT URI", ErrInvalidRef, ref.URI)
	}
	if !s.local(repo) {
		return s.foreign.Resolve(ctx, ref)
	}
	rec, err := s.GetRecord(ctx, repo, collection, rkey)
	if err != nil {
		return ResolvedRecord{}, err
	}
	if rec.CID == "" {
		rec.CID = ref.CID
	}
	return rec, nil
}
