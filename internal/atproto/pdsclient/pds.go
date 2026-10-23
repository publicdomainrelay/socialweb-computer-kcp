package pdsclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
)

var (
	ErrInvalidConfig          = errors.New("pdsclient: invalid configuration")
	ErrInvalidRef             = errors.New("pdsclient: invalid reference")
	ErrInvalidRequest         = errors.New("pdsclient: invalid request")
	ErrUnexpectedResponse     = errors.New("pdsclient: unexpected response")
	ErrAuthenticationRequired = errors.New("pdsclient: authentication required")
	ErrIdentityMismatch       = errors.New("pdsclient: identity mismatch")
	ErrUnavailable            = errors.New("pdsclient: unavailable")
	ErrClosed                 = errors.New("pdsclient: stream closed")
	ErrProtocol               = errors.New("pdsclient: protocol error")
)

type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

type TokenRefresher interface {
	RefreshAccessToken(ctx context.Context) (string, error)
}

type DPoPNonceSetter interface {
	SetDPoPNonce(ctx context.Context, host, nonce string) error
}

const TokenExpirySkew = 30 * time.Second

func TokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.ExpiresAt, 0), true
}

func TokenLive(token string) bool {
	expires, ok := TokenExpiry(token)
	if !ok {
		return false
	}
	return time.Now().Add(TokenExpirySkew).Before(expires)
}

type DPoPSource interface {
	DPoPProof(ctx context.Context, method, url, accessToken string) (string, error)
}

type TokenSourceFunc func(ctx context.Context) (string, error)

func (f TokenSourceFunc) AccessToken(ctx context.Context) (string, error) {
	if f == nil {
		return "", fmt.Errorf("%w: nil token source", ErrInvalidConfig)
	}
	return f(ctx)
}

type DPoPSourceFunc func(ctx context.Context, method, url, accessToken string) (string, error)

func (f DPoPSourceFunc) DPoPProof(ctx context.Context, method, url, accessToken string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("%w: nil dpop source", ErrInvalidConfig)
	}
	return f(ctx, method, url, accessToken)
}

type Info struct {
	DID                  syntax.DID
	AvailableUserDomains []string
	Version              string
}

type RepoPath struct {
	Repo       syntax.DID
	Collection syntax.NSID
	Rkey       syntax.RecordKey
}

type Record struct {
	URI   syntax.ATURI
	CID   cid.Cid
	Value json.RawMessage
}

type WriteOptions struct {
	SwapCommit *cid.Cid
	SwapRecord *cid.Cid
	Validate   *bool
}

type CommitRef struct {
	CID cid.Cid
	Rev syntax.TID
}

type WriteResult struct {
	URI    syntax.ATURI
	CID    cid.Cid
	Commit *CommitRef
}

type ListRecordsRequest struct {
	Repo       syntax.DID
	Collection syntax.NSID
	Cursor     string
	Limit      int
	Reverse    bool
}

type ListRecordsResult struct {
	Records []Record
	Cursor  string
}

type Session struct {
	DID        syntax.DID
	Handle     string
	Email      string
	AccessJWT  string
	RefreshJWT string
	Active     bool
}

type CreateAccountRequest struct {
	Identifier string
	Password   string
	Email      string
	InviteCode string
}

func (s Session) String() string {
	return fmt.Sprintf("pdsclient.Session{did:%s handle:%s}", s.DID, s.Handle)
}

type RepoOp struct {
	Action string
	Path   string
	CID    *cid.Cid
	Prev   *cid.Cid
}

type RepoEvent struct {
	Seq      int64
	Repo     syntax.DID
	Commit   cid.Cid
	Rev      syntax.TID
	Since    *syntax.TID
	PrevData *cid.Cid
	Ops      []RepoOp
	Blobs    []cid.Cid
	Blocks   []byte
	Time     time.Time
	TooBig   bool
}

type RepoEventStream interface {
	Next(ctx context.Context) (RepoEvent, error)
	Cursor() int64
	Close() error
}

type PDS interface {
	Describe(ctx context.Context) (Info, error)

	GetRecord(ctx context.Context, path RepoPath, expected *cid.Cid) (Record, error)
	CreateRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error)
	PutRecord(ctx context.Context, path RepoPath, record []byte, opts WriteOptions) (WriteResult, error)
	DeleteRecord(ctx context.Context, path RepoPath, opts WriteOptions) error
	ListRecords(ctx context.Context, req ListRecordsRequest) (ListRecordsResult, error)

	GetRepo(ctx context.Context, did syntax.DID, since *syntax.TID) (io.ReadCloser, error)
	GetLatestCommit(ctx context.Context, did syntax.DID) (cid.Cid, syntax.TID, error)
	SubscribeRepos(ctx context.Context, cursor *int64) (RepoEventStream, error)

	CreateSession(ctx context.Context, identifier, password string) (Session, error)
	CreateAccount(ctx context.Context, req CreateAccountRequest) (Session, error)
	GetSession(ctx context.Context) (Session, error)
	DeleteSession(ctx context.Context) error
}

type XRPCError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *XRPCError) Error() string {
	if e == nil {
		return "pdsclient: <nil>"
	}
	if e.Code == "" {
		return fmt.Sprintf("pdsclient: xrpc status %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("pdsclient: xrpc %s: %s (status %d)", e.Code, e.Message, e.StatusCode)
}
