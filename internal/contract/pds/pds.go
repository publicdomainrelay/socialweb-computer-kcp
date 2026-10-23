package pds

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	CapabilityAccount = "pds.account"

	CapabilitySession = "pds.session"

	CapabilitySync = "repo.read"

	CapabilityServiceAuth = "pds.serviceauth"
)

var (
	ErrNotFound = errors.New("pds: not found")

	ErrUnsupported = errors.New("pds: the backend does not support this call")
)

type Info struct {
	DID string

	AvailableUserDomains []string

	Version string

	URL string
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

	SigningKey string
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

type ServiceAuth interface {
	ServiceAuth(ctx context.Context, did, audience, lxm string, expires time.Time) (string, error)
}

type Sync interface {
	LatestCommit(ctx context.Context, did string) (Commit, error)

	GetRepo(ctx context.Context, did, since string) (io.ReadCloser, error)

	SubscribeRepos(ctx context.Context, cursor *int64, fn func(Event) error) error
}

type Verdict int

const (
	VerdictDone Verdict = iota

	VerdictRetryNonce

	VerdictRetryAuth
)

type Authenticator interface {
	DID() string

	Apply(req *http.Request) error

	Invalidate()

	Challenge(resp *http.Response) Verdict
}

type Transport interface {
	Address() string

	Client() *http.Client

	TLS() *tls.Config
}

func AccountDID(scope []string) (string, error) {
	var did string
	for _, entry := range scope {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if did != "" && entry != did {
			return "", fmt.Errorf("pds: a repo handle serves one account; the scope names %v", scope)
		}
		did = entry
	}
	if !strings.HasPrefix(did, "did:") {
		return "", fmt.Errorf("pds: a repo handle needs the account DID as its pds.account scope, got %v", scope)
	}
	return did, nil
}
