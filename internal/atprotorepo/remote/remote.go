package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

const DefaultTimeout = 30 * time.Second

type Config struct {
	DID string

	Transport pds.Transport

	Auth xrpc.Auth

	Timeout time.Duration
}

type Repo struct {
	pds pdsclient.PDS

	did syntax.DID
}

var (
	_ atprotorepo.Repo         = (*Repo)(nil)
	_ atprotorepo.RecordReader = (*Repo)(nil)
	_ atprotorepo.RecordWriter = (*Repo)(nil)
	_ atprotorepo.Subscriber   = (*Repo)(nil)
)

func New(cfg Config) (*Repo, error) {
	did, err := syntax.ParseDID(strings.TrimSpace(cfg.DID))
	if err != nil {
		return nil, fmt.Errorf("remote: the repo did %q: %w", cfg.DID, err)
	}
	if cfg.Transport == nil {
		return nil, errors.New("remote: the repo has no PDS transport")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: cfg.Transport.Address(), HTTPClient: cfg.Transport.Client(),
		TLS: cfg.Transport.TLS(), Auth: cfg.Auth, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return Wrap(client, did), nil
}

func Wrap(client pdsclient.PDS, did syntax.DID) *Repo {
	return &Repo{pds: client, did: did}
}

func (r *Repo) DID() string { return r.did.String() }

func notFound(err error) error {
	var failure *pdsclient.XRPCError
	if errors.As(err, &failure) && (failure.StatusCode == http.StatusNotFound || failure.Code == "RecordNotFound" || failure.Code == "RepoNotFound") {
		return fmt.Errorf("%w: %v", atprotorepo.ErrNotFound, err)
	}
	return err
}

func (r *Repo) path(repo, collection, rkey string) (pdsclient.RepoPath, error) {
	if strings.TrimSpace(repo) == "" {
		repo = r.did.String()
	}
	return pdsclient.ParseRepoPath(repo, collection, rkey)
}

func toRecord(rec pdsclient.Record) atprotorepo.Record {
	return atprotorepo.Record{URI: rec.URI.String(), CID: rec.CID.String(), Collection: rec.URI.Collection().String(),
		RKey: rec.URI.RecordKey().String(), Value: rec.Value}
}

func (r *Repo) GetRecord(ctx context.Context, repo, collection, rkey string) (atprotorepo.Record, error) {
	path, err := r.path(repo, collection, rkey)
	if err != nil {
		return atprotorepo.Record{}, err
	}
	rec, err := r.pds.GetRecord(ctx, path, nil)
	if err != nil {
		return atprotorepo.Record{}, notFound(err)
	}
	return toRecord(rec), nil
}

func (r *Repo) ListRecords(ctx context.Context, repo, collection string, opts atprotorepo.ListOptions) (atprotorepo.ListPage, error) {
	path, err := r.path(repo, collection, "")
	if err != nil {
		return atprotorepo.ListPage{}, err
	}
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out, err := r.pds.ListRecords(ctx, pdsclient.ListRecordsRequest{Repo: path.Repo, Collection: path.Collection, Cursor: opts.Cursor,
		Limit: limit, Reverse: opts.Reverse})
	if err != nil {
		return atprotorepo.ListPage{}, notFound(err)
	}
	page := atprotorepo.ListPage{Cursor: out.Cursor, Records: make([]atprotorepo.Record, 0, len(out.Records))}
	for _, rec := range out.Records {
		page.Records = append(page.Records, toRecord(rec))
	}
	return page, nil
}

func (r *Repo) Resolve(ctx context.Context, ref commonv1.StrongRef) (atprotorepo.Record, error) {
	uri, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return atprotorepo.Record{}, fmt.Errorf("remote: the record uri %q: %w", ref.URI, err)
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

func (r *Repo) CreateRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	path, err := r.path("", collection, rkey)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	out, err := r.pds.CreateRecord(ctx, path, value, pdsclient.WriteOptions{})
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	return commonv1.StrongRef{URI: out.URI.String(), CID: out.CID.String()}, nil
}

func (r *Repo) PutRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error) {
	if strings.TrimSpace(rkey) == "" {
		return commonv1.StrongRef{}, errors.New("remote: putRecord needs an rkey")
	}
	path, err := r.path("", collection, rkey)
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	out, err := r.pds.PutRecord(ctx, path, value, pdsclient.WriteOptions{})
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	return commonv1.StrongRef{URI: out.URI.String(), CID: out.CID.String()}, nil
}

func (r *Repo) DeleteRecord(ctx context.Context, collection, rkey string) error {
	path, err := r.path("", collection, rkey)
	if err != nil {
		return err
	}
	if err := r.pds.DeleteRecord(ctx, path, pdsclient.WriteOptions{}); err != nil {
		return notFound(err)
	}
	return nil
}

func (r *Repo) Subscribe(ctx context.Context, opts atprotorepo.SubscribeOptions, fn func(atprotorepo.Commit) error) error {
	stream, err := r.pds.SubscribeRepos(ctx, opts.Cursor)
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		ev, err := stream.Next(ctx)
		if err != nil {
			return err
		}
		if ev.Repo != r.did {
			continue
		}
		commit := Commit(ev, opts.Collections)
		if len(commit.Ops) == 0 && len(ev.Ops) > 0 {
			continue
		}
		if err := fn(commit); err != nil {
			return err
		}
	}
}

func Commit(ev pdsclient.RepoEvent, collections []string) atprotorepo.Commit {
	out := atprotorepo.Commit{Seq: ev.Seq, Repo: ev.Repo.String(), CID: ev.Commit.String(), Rev: ev.Rev.String(), Time: ev.Time}
	for _, op := range ev.Ops {
		collection, rkey, _ := strings.Cut(op.Path, "/")
		if len(collections) > 0 && !contains(collections, collection) {
			continue
		}
		next := atprotorepo.Op{Action: op.Action, Collection: collection, RKey: rkey}
		if op.CID != nil {
			next.CID = op.CID.String()
		}
		out.Ops = append(out.Ops, next)
	}
	return out
}

func contains(list []string, value string) bool {
	for _, entry := range list {
		if strings.EqualFold(strings.TrimSpace(entry), value) {
			return true
		}
	}
	return false
}
