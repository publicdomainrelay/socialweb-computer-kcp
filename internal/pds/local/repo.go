package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

const backfillPage = 500

var errBehind = errors.New("pds: the subscription fell behind the sequencer")

type Repo struct {
	srv *atpds.Server

	did syntax.DID
}

func openRepo(ctx context.Context, srv *atpds.Server, raw string) (*Repo, error) {
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return nil, fmt.Errorf("pds: the repo did %q: %w", raw, err)
	}
	exists, err := srv.Repos().RepoExists(ctx, did)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: the Pds hosts no repo %s", atprotorepo.ErrNotFound, did)
	}
	return &Repo{srv: srv, did: did}, nil
}

func (r *Repo) DID() string { return r.did.String() }

func notFound(err error) error {
	if errors.Is(err, repo.ErrRecordNotFound) || errors.Is(err, repo.ErrRepoNotFound) {
		return fmt.Errorf("%w: %v", atprotorepo.ErrNotFound, err)
	}
	return err
}

func (r *Repo) target(raw string) (syntax.DID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return r.did, nil
	}
	return syntax.ParseDID(raw)
}

func toRecord(did syntax.DID, rec *repo.Record) (atprotorepo.Record, error) {
	value, err := json.Marshal(rec.Value)
	if err != nil {
		return atprotorepo.Record{}, fmt.Errorf("pds: encoding the record value: %w", err)
	}
	return atprotorepo.Record{URI: "at://" + did.String() + "/" + rec.Collection.String() + "/" + rec.Rkey.String(), CID: rec.CID.String(),
		Collection: rec.Collection.String(), RKey: rec.Rkey.String(), Value: value}, nil
}

func (r *Repo) GetRecord(ctx context.Context, repoDID, collection, rkey string) (atprotorepo.Record, error) {
	did, err := r.target(repoDID)
	if err != nil {
		return atprotorepo.Record{}, err
	}
	rec, err := r.srv.Repos().GetRecord(ctx, did, syntax.NSID(collection), syntax.RecordKey(rkey))
	if err != nil {
		return atprotorepo.Record{}, notFound(err)
	}
	return toRecord(did, rec)
}

func (r *Repo) ListRecords(ctx context.Context, repoDID, collection string, opts atprotorepo.ListOptions) (atprotorepo.ListPage, error) {
	if opts.Reverse {
		return atprotorepo.ListPage{}, errors.New("pds: the in-process repo lists records in key order only")
	}
	did, err := r.target(repoDID)
	if err != nil {
		return atprotorepo.ListPage{}, err
	}
	records, next, err := r.srv.Repos().ListRecords(ctx, did, syntax.NSID(collection), syntax.RecordKey(opts.Cursor), opts.Limit)
	if err != nil {
		return atprotorepo.ListPage{}, notFound(err)
	}
	page := atprotorepo.ListPage{Records: make([]atprotorepo.Record, 0, len(records))}
	for i := range records {
		rec, err := toRecord(did, &records[i])
		if err != nil {
			return atprotorepo.ListPage{}, err
		}
		page.Records = append(page.Records, rec)
	}
	if next != nil {
		page.Cursor = next.String()
	}
	return page, nil
}

func (r *Repo) Resolve(ctx context.Context, ref commonv1.StrongRef) (atprotorepo.Record, error) {
	uri, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return atprotorepo.Record{}, fmt.Errorf("pds: the record uri %q: %w", ref.URI, err)
	}
	rec, err := r.GetRecord(ctx, uri.Authority().String(), uri.Collection().String(), uri.RecordKey().String())
	if err != nil {
		return atprotorepo.Record{}, err
	}
	if ref.CID != "" && rec.CID != ref.CID {
		return atprotorepo.Record{}, fmt.Errorf("%w: %s holds %s, not %s", atprotorepo.ErrNotFound, ref.URI, rec.CID, ref.CID)
	}
	return rec, nil
}

func decodeValue(collection string, value json.RawMessage) (map[string]any, error) {
	var record map[string]any
	if err := json.Unmarshal(value, &record); err != nil {
		return nil, fmt.Errorf("pds: the record is not a JSON object: %w", err)
	}
	if record == nil {
		return nil, errors.New("pds: the record is empty")
	}
	if _, ok := record["$type"]; !ok {
		out := make(map[string]any, len(record)+1)
		out["$type"] = collection
		maps.Copy(out, record)
		record = out
	}
	return record, nil
}

func (r *Repo) open(ctx context.Context) (repo.Repo, error) {
	keys := r.srv.KeyStore()
	if keys == nil {
		return nil, repo.ErrReadOnly
	}
	key, err := keys.GetRepoKey(ctx, r.did)
	if err != nil {
		return nil, fmt.Errorf("pds: the signing key of %s: %w", r.did, err)
	}
	return r.srv.Repos().OpenRepo(ctx, r.did, key)
}

func (r *Repo) write(ctx context.Context, action repo.WriteAction, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return commonv1.StrongRef{}, fmt.Errorf("pds: the collection %q: %w", collection, err)
	}
	var key syntax.RecordKey
	if rkey != "" {
		if key, err = syntax.ParseRecordKey(rkey); err != nil {
			return commonv1.StrongRef{}, fmt.Errorf("pds: the rkey %q: %w", rkey, err)
		}
	}
	record, err := decodeValue(collection, value)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	handle, err := r.open(ctx)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	ev, err := handle.ApplyWrites(ctx, []repo.WriteOp{{Action: action, Collection: nsid, Rkey: key, Record: record}}, nil)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	if ev != nil {
		for _, op := range ev.Ops {
			if op.Action != repo.ActionDelete && op.CID != nil {
				return commonv1.StrongRef{URI: "at://" + r.did.String() + "/" + op.Path, CID: op.CID.String()}, nil
			}
		}
	}
	if key == "" {
		return commonv1.StrongRef{}, errors.New("pds: the write produced no record")
	}
	held, err := handle.GetRecord(ctx, nsid, key)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	return commonv1.StrongRef{URI: "at://" + r.did.String() + "/" + nsid.String() + "/" + key.String(), CID: held.CID.String()}, nil
}

func (r *Repo) CreateRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	return r.write(ctx, repo.ActionCreate, collection, rkey, value)
}

func (r *Repo) PutRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	if strings.TrimSpace(rkey) == "" {
		return commonv1.StrongRef{}, errors.New("pds: putRecord needs an rkey")
	}
	return r.write(ctx, repo.WriteAction("put"), collection, rkey, value)
}

func (r *Repo) DeleteRecord(ctx context.Context, collection, rkey string) error {
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return fmt.Errorf("pds: the collection %q: %w", collection, err)
	}
	key, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return fmt.Errorf("pds: the rkey %q: %w", rkey, err)
	}
	handle, err := r.open(ctx)
	if err != nil {
		return err
	}
	if _, err := handle.ApplyWrites(ctx, []repo.WriteOp{{Action: repo.ActionDelete, Collection: nsid, Rkey: key}}, nil); err != nil {
		return notFound(err)
	}
	return nil
}

func (r *Repo) Subscribe(ctx context.Context, opts atprotorepo.SubscribeOptions, fn func(atprotorepo.Commit) error) error {
	return follow(ctx, r.srv, opts.Cursor, func(ev *repo.CommitEvent) error {
		if ev.Repo != r.did {
			return nil
		}
		commit := toCommit(ev, opts.Collections)
		if len(commit.Ops) == 0 && len(ev.Ops) > 0 {
			return nil
		}
		return fn(commit)
	})
}

func follow(ctx context.Context, srv *atpds.Server, cursor *int64, fn func(*repo.CommitEvent) error) error {
	live, cancel := srv.Sequencer().Subscribe()
	defer cancel()
	last := int64(-1)
	if cursor != nil {
		last = *cursor
		for {
			events, err := srv.Repos().ListEvents(ctx, last, backfillPage)
			if err != nil {
				return err
			}
			for _, ev := range events {
				if ev.Seq <= last {
					continue
				}
				if err := fn(ev); err != nil {
					return err
				}
				last = ev.Seq
			}
			if len(events) < backfillPage {
				break
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-live:
			if !ok {
				return errBehind
			}
			if ev.Seq <= last {
				continue
			}
			if err := fn(ev); err != nil {
				return err
			}
			last = ev.Seq
		}
	}
}

func toCommit(ev *repo.CommitEvent, collections []string) atprotorepo.Commit {
	out := atprotorepo.Commit{Seq: ev.Seq, Repo: ev.Repo.String(), CID: ev.Commit.String(), Rev: ev.Rev.String(), Time: ev.Time}
	for _, op := range ev.Ops {
		collection, rkey, _ := strings.Cut(op.Path, "/")
		if len(collections) > 0 && !containsFold(collections, collection) {
			continue
		}
		next := atprotorepo.Op{Action: string(op.Action), Collection: collection, RKey: rkey}
		if op.CID != nil {
			next.CID = op.CID.String()
		}
		out.Ops = append(out.Ops, next)
	}
	return out
}

func containsFold(list []string, value string) bool {
	for _, entry := range list {
		if strings.EqualFold(strings.TrimSpace(entry), value) {
			return true
		}
	}
	return false
}
