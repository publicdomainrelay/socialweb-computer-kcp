package scoped

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

func Route(inner atprotorepo.Repo, network atprotorepo.RecordReader) atprotorepo.Repo {
	if network == nil {
		return inner
	}
	return &routed{Repo: inner, network: network}
}

type routed struct {
	atprotorepo.Repo

	network atprotorepo.RecordReader
}

var (
	_ atprotorepo.SwapDeleter = (*routed)(nil)
	_ atprotorepo.SwapPutter  = (*routed)(nil)
)

func (r *routed) PutRecordSwap(ctx context.Context, collection, rkey string, value json.RawMessage, swapCID string) (commonv1.StrongRef, error) {
	inner, ok := r.Repo.(atprotorepo.SwapPutter)
	if !ok {
		return commonv1.StrongRef{}, errors.New("scoped: the routed repo cannot put a record by its cid")
	}
	return inner.PutRecordSwap(ctx, collection, rkey, value, swapCID)
}

func (r *routed) DeleteRecordSwap(ctx context.Context, collection, rkey, swapCID string) error {
	inner, ok := r.Repo.(atprotorepo.SwapDeleter)
	if !ok {
		return errors.New("scoped: the routed repo cannot delete a record by its cid")
	}
	return inner.DeleteRecordSwap(ctx, collection, rkey, swapCID)
}

func (r *routed) foreign(repo string) bool {
	repo = strings.TrimSpace(repo)
	return repo != "" && repo != r.DID()
}

func (r *routed) GetRecord(ctx context.Context, repo, collection, rkey string) (atprotorepo.Record, error) {
	if r.foreign(repo) {
		return r.network.GetRecord(ctx, repo, collection, rkey)
	}
	return r.Repo.GetRecord(ctx, repo, collection, rkey)
}

func (r *routed) ListRecords(ctx context.Context, repo, collection string, opts atprotorepo.ListOptions) (atprotorepo.ListPage, error) {
	if r.foreign(repo) {
		return r.network.ListRecords(ctx, repo, collection, opts)
	}
	return r.Repo.ListRecords(ctx, repo, collection, opts)
}

func (r *routed) Resolve(ctx context.Context, ref commonv1.StrongRef) (atprotorepo.Record, error) {
	if authority, ok := uriAuthority(ref.URI); ok && r.foreign(authority) {
		return r.network.Resolve(ctx, ref)
	}
	return r.Repo.Resolve(ctx, ref)
}

func uriAuthority(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(uri), "at://")
	if !ok {
		return "", false
	}
	authority, _, _ := strings.Cut(rest, "/")
	return authority, authority != ""
}
