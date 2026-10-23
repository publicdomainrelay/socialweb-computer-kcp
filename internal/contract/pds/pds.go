package pds

import (
	"context"
	"io"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	CapabilityAccount = "pds.account"

	CapabilitySession = "pds.session"
)

type Info struct {
	DID string

	AvailableUserDomains []string

	Version string
}

type AccountRequest struct {
	Handle string

	Email string

	Credentials commonv1.SecretRef
}

type Account struct {
	DID string

	Handle string

	Email string

	Active bool
}

type SessionRequest struct {
	Identifier string

	Credentials commonv1.SecretRef
}

type Session struct {
	DID string

	Handle string

	Active bool
}

type Commit struct {
	CID string

	Rev string
}

type Op struct {
	Action string

	Path string

	CID string

	Prev string
}

type Event struct {
	Seq int64

	Repo string

	Commit string

	Rev string

	Since string

	Time time.Time

	Ops []Op

	TooBig bool
}

type Accounts interface {
	Describe(ctx context.Context) (Info, error)

	CreateAccount(ctx context.Context, req AccountRequest) (Account, error)

	Account(ctx context.Context, did string) (Account, error)
}

type Sessions interface {
	SignIn(ctx context.Context, req SessionRequest) (Session, error)

	Session(ctx context.Context, did string) (Session, error)

	SignOut(ctx context.Context, did string) error
}

type Sync interface {
	LatestCommit(ctx context.Context, did string) (Commit, error)

	GetRepo(ctx context.Context, did, since string) (io.ReadCloser, error)

	SubscribeRepos(ctx context.Context, cursor *int64, fn func(Event) error) error
}
