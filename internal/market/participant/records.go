package participant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

type RepoConfig struct {
	DID string

	Directory identity.Directory

	HTTP *http.Client

	Open func(ctx context.Context) (atprotorepo.Repo, error)
}

func NewRepoStore(cfg RepoConfig) (*market.RepoStore, error) {
	if strings.TrimSpace(cfg.DID) == "" {
		return nil, errors.New("participant: the record store needs the account DID")
	}
	if cfg.Open == nil {
		return nil, errors.New("participant: the record store needs a repo handle")
	}
	reader := market.NewHTTPReader(cfg.Directory)
	if cfg.HTTP != nil {
		reader.Client = cfg.HTTP
	}
	return market.NewRepoStore(market.RepoStoreConfig{DID: cfg.DID, Foreign: reader, Open: cfg.Open}), nil
}

func EnsureRecordAt(ctx context.Context, records *market.RepoStore, did, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	if records == nil || did == "" {
		return market.StrongRef{}, errors.New("participant: no record store or DID")
	}
	if found, err := records.GetRecord(ctx, did, collection, rkey); err == nil {
		return market.NewStrongRef(found.URI, found.CID), nil
	} else if !errors.Is(err, market.ErrNotFound) {
		return market.StrongRef{}, err
	}
	ref, err := records.CreateRecordAt(ctx, did, collection, rkey, record)
	if err == nil {
		return ref, nil
	}
	if found, getErr := records.GetRecord(ctx, did, collection, rkey); getErr == nil {
		return market.NewStrongRef(found.URI, found.CID), nil
	}
	return market.StrongRef{}, err
}

type CreateOnly struct {
	Records *market.RepoStore
}

func (w CreateOnly) CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (market.StrongRef, error) {
	return w.Records.CreateRecord(ctx, repo, collection, record)
}

func (w CreateOnly) PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (market.StrongRef, error) {
	return EnsureRecordAt(ctx, w.Records, repo, collection, rkey, record)
}

func (w CreateOnly) DeleteRecord(ctx context.Context, repo, collection, rkey string) error {
	return w.Records.DeleteRecord(ctx, repo, collection, rkey)
}

func StructMap(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func LastSegment(uri string) string {
	if i := strings.LastIndexByte(uri, '/'); i >= 0 {
		return uri[i+1:]
	}
	return uri
}
