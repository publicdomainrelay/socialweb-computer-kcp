package repo

import (
	"context"
	"errors"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigorepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/ipfs/go-cid"
)

const (
	MaxWrites      = 200
	DefaultList    = 50
	MaxList        = 100
	DefaultCommits = 100
	MaxCommits     = 1000
)

var (
	ErrRepoNotFound    = errors.New("repo not found")
	ErrRepoExists      = errors.New("repo already exists")
	ErrCommitNotFound  = errors.New("commit not found")
	ErrRecordNotFound  = errors.New("record not found")
	ErrRecordExists    = errors.New("record already exists")
	ErrInvalidSwap     = errors.New("invalid swap")
	ErrInvalidRequest  = errors.New("invalid request")
	ErrReadOnly        = errors.New("repo has no signing key")
	ErrCorruptRepo     = errors.New("repo is corrupt")
	ErrUnsupportedRepo = errors.New("unsupported repo")
)

type Commit = indigorepo.Commit

type WriteAction string

const (
	ActionCreate WriteAction = "create"
	ActionUpdate WriteAction = "update"
	ActionDelete WriteAction = "delete"

	actionPut WriteAction = "put"
)

type WriteOp struct {
	Action     WriteAction
	Collection syntax.NSID
	Rkey       syntax.RecordKey
	Record     map[string]any
	Prev       *cid.Cid
}

type CommitOp struct {
	Action WriteAction
	Path   string
	CID    *cid.Cid
	Prev   *cid.Cid
}

type CommitEvent struct {
	Repo     syntax.DID
	Seq      int64
	Commit   cid.Cid
	Rev      syntax.TID
	Since    *syntax.TID
	Data     cid.Cid
	PrevData *cid.Cid
	Blocks   []byte
	Ops      []CommitOp
	Blobs    []cid.Cid
	Time     time.Time
}

func (ev *CommitEvent) CommitMessage() *comatproto.SyncSubscribeRepos_Commit {
	ops := make([]*comatproto.SyncSubscribeRepos_RepoOp, 0, len(ev.Ops))
	for _, op := range ev.Ops {
		rop := &comatproto.SyncSubscribeRepos_RepoOp{
			Action: string(op.Action),
			Path:   op.Path,
		}
		if op.CID != nil {
			c := lexutil.LexLink(*op.CID)
			rop.Cid = &c
		}
		if op.Prev != nil {
			p := lexutil.LexLink(*op.Prev)
			rop.Prev = &p
		}
		ops = append(ops, rop)
	}
	var since *string
	if ev.Since != nil {
		s := ev.Since.String()
		since = &s
	}
	var prevData *lexutil.LexLink
	if ev.PrevData != nil {
		p := lexutil.LexLink(*ev.PrevData)
		prevData = &p
	}
	blobs := make([]lexutil.LexLink, 0, len(ev.Blobs))
	for _, b := range ev.Blobs {
		blobs = append(blobs, lexutil.LexLink(b))
	}
	return &comatproto.SyncSubscribeRepos_Commit{
		Blobs:    blobs,
		Blocks:   lexutil.LexBytes(ev.Blocks),
		Commit:   lexutil.LexLink(ev.Commit),
		Ops:      ops,
		PrevData: prevData,
		Repo:     ev.Repo.String(),
		Rev:      ev.Rev.String(),
		Seq:      ev.Seq,
		Since:    since,
		Time:     ev.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

type Record struct {
	Collection syntax.NSID
	Rkey       syntax.RecordKey
	CID        cid.Cid
	Bytes      []byte
	Value      map[string]any
}

type CommitRecord struct {
	DID    syntax.DID
	CID    cid.Cid
	Rev    syntax.TID
	Data   cid.Cid
	Prev   *cid.Cid
	Signed *Commit
	Seq    int64
	Time   time.Time
}

type EventSink interface {
	OnCommit(ctx context.Context, ev *CommitEvent) error
}

type EventSinkFunc func(ctx context.Context, ev *CommitEvent) error

func (f EventSinkFunc) OnCommit(ctx context.Context, ev *CommitEvent) error {
	return f(ctx, ev)
}

type RecordStore interface {
	GetRecord(ctx context.Context, did syntax.DID, collection syntax.NSID, rkey syntax.RecordKey) (*Record, error)
	ListRecords(ctx context.Context, did syntax.DID, collection syntax.NSID, cursor syntax.RecordKey, limit int) ([]Record, *syntax.RecordKey, error)
}

type CommitStore interface {
	Head(ctx context.Context, did syntax.DID) (*CommitRecord, error)
	GetCommit(ctx context.Context, did syntax.DID, rev syntax.TID) (*CommitRecord, error)
	ListCommits(ctx context.Context, did syntax.DID, since *syntax.TID, limit int) ([]CommitRecord, error)
	CAR(ctx context.Context, did syntax.DID, since *syntax.TID) ([]byte, error)
	CommitCAR(ctx context.Context, did syntax.DID, commit cid.Cid) ([]byte, error)
}

type RepoStore interface {
	RecordStore
	CommitStore

	CreateRepo(ctx context.Context, did syntax.DID, key atcrypto.PrivateKey) (Repo, error)
	OpenRepo(ctx context.Context, did syntax.DID, key atcrypto.PrivateKey) (Repo, error)
	DeleteRepo(ctx context.Context, did syntax.DID) error
	RepoExists(ctx context.Context, did syntax.DID) (bool, error)
	ListRepos(ctx context.Context) ([]syntax.DID, error)
	ListEvents(ctx context.Context, since int64, limit int) ([]*CommitEvent, error)
	SetEventSink(sink EventSink)
	Close() error
}

type Repo interface {
	DID() syntax.DID
	Head(ctx context.Context) (*CommitRecord, error)
	GetRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey) (*Record, error)
	ListRecords(ctx context.Context, collection syntax.NSID, cursor syntax.RecordKey, limit int) ([]Record, *syntax.RecordKey, error)
	Walk(ctx context.Context, prefix string, fn func(path string, c cid.Cid) error) error
	ApplyWrites(ctx context.Context, writes []WriteOp, swapCommit *cid.Cid) (*CommitEvent, error)
	CreateRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, record map[string]any) (*Record, *CommitEvent, error)
	PutRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, record map[string]any, swapRecord *cid.Cid) (*Record, *CommitEvent, error)
	DeleteRecord(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey, swapRecord *cid.Cid) (*CommitEvent, error)
	GetCommit(ctx context.Context, rev syntax.TID) (*CommitRecord, error)
	Commits(ctx context.Context, since *syntax.TID, limit int) ([]CommitRecord, error)
	CAR(ctx context.Context, since *syntax.TID) ([]byte, error)
	CommitCAR(ctx context.Context, commit cid.Cid) ([]byte, error)
	Proof(ctx context.Context, collection syntax.NSID, rkey syntax.RecordKey) ([]byte, error)
	Diff(ctx context.Context, since syntax.TID) ([]CommitOp, error)
	Verify(ctx context.Context) error
}
