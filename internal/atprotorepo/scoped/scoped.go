package scoped

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

const all = "*"

func Covers(scope []string, collection string) bool {
	if len(scope) == 0 {
		return true
	}
	collection = strings.TrimSpace(collection)
	for _, entry := range scope {
		entry = strings.TrimSpace(entry)
		if entry == all || entry == collection {
			return true
		}
		if prefix, ok := strings.CutSuffix(entry, all); ok && strings.HasPrefix(collection, prefix) {
			return true
		}
	}
	return false
}

type Repo struct {
	inner atprotorepo.Repo

	scope []string
}

var _ atprotorepo.Repo = (*Repo)(nil)

func New(inner atprotorepo.Repo, scope []string) *Repo {
	return &Repo{inner: inner, scope: slices.Clone(scope)}
}

func (r *Repo) DID() string { return r.inner.DID() }

func (r *Repo) GetRecord(ctx context.Context, repo, collection, rkey string) (atprotorepo.Record, error) {
	return r.inner.GetRecord(ctx, repo, collection, rkey)
}

func (r *Repo) ListRecords(ctx context.Context, repo, collection string, opts atprotorepo.ListOptions) (atprotorepo.ListPage, error) {
	return r.inner.ListRecords(ctx, repo, collection, opts)
}

func (r *Repo) Resolve(ctx context.Context, ref commonv1.StrongRef) (atprotorepo.Record, error) {
	return r.inner.Resolve(ctx, ref)
}

func (r *Repo) allowed(collection string) error {
	if Covers(r.scope, collection) {
		return nil
	}
	return fmt.Errorf("%w: %s is not in %v", atprotorepo.ErrOutOfScope, collection, r.scope)
}

func (r *Repo) CreateRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	if err := r.allowed(collection); err != nil {
		return commonv1.StrongRef{}, err
	}
	return r.inner.CreateRecord(ctx, collection, rkey, value)
}

func (r *Repo) PutRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	if err := r.allowed(collection); err != nil {
		return commonv1.StrongRef{}, err
	}
	return r.inner.PutRecord(ctx, collection, rkey, value)
}

func (r *Repo) DeleteRecord(ctx context.Context, collection, rkey string) error {
	if err := r.allowed(collection); err != nil {
		return err
	}
	return r.inner.DeleteRecord(ctx, collection, rkey)
}

func (r *Repo) Subscribe(ctx context.Context, opts atprotorepo.SubscribeOptions, fn func(atprotorepo.Commit) error) error {
	for _, collection := range opts.Collections {
		if err := r.allowed(collection); err != nil {
			return err
		}
	}
	scope := r.scope
	return r.inner.Subscribe(ctx, opts, func(commit atprotorepo.Commit) error {
		ops := commit.Ops[:0:0]
		for _, op := range commit.Ops {
			if Covers(scope, op.Collection) {
				ops = append(ops, op)
			}
		}
		if len(ops) == 0 && len(commit.Ops) > 0 {
			return nil
		}
		commit.Ops = ops
		return fn(commit)
	})
}
