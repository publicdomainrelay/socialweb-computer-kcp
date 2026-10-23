package atprotorepo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	CapabilityRead = "repo.read"

	CapabilityWrite = "repo.write"

	CapabilitySubscribe = "repo.subscribe"
)

var (
	ErrNotFound = errors.New("atprotorepo: record not found")

	ErrOutOfScope = errors.New("atprotorepo: the collection is outside the granted scope")

	ErrNotBound = errors.New("atprotorepo: the repo has no bound backend")
)

type Record struct {
	URI string

	CID string

	Collection string

	RKey string

	Value json.RawMessage
}

type ListOptions struct {
	Limit int

	Cursor string

	Reverse bool
}

type ListPage struct {
	Records []Record

	Cursor string
}

type Op struct {
	Action string

	Collection string

	RKey string

	CID string
}

type Commit struct {
	Seq int64

	Repo string

	CID string

	Rev string

	Time time.Time

	Ops []Op
}

type SubscribeOptions struct {
	Collections []string

	Cursor *int64
}

type RecordReader interface {
	GetRecord(ctx context.Context, repo, collection, rkey string) (Record, error)

	ListRecords(ctx context.Context, repo, collection string, opts ListOptions) (ListPage, error)

	Resolve(ctx context.Context, ref commonv1.StrongRef) (Record, error)
}

type RecordWriter interface {
	CreateRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error)

	PutRecord(ctx context.Context, collection, rkey string, value json.RawMessage) (commonv1.StrongRef, error)

	DeleteRecord(ctx context.Context, collection, rkey string) error
}

type Subscriber interface {
	Subscribe(ctx context.Context, opts SubscribeOptions, fn func(Commit) error) error
}

type Repo interface {
	DID() string

	RecordReader

	RecordWriter

	Subscriber
}

func RepoName(did string) string {
	raw := strings.ToLower("repo-" + did)
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.') {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "repo"
	}
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-.")
	}
	return name
}
