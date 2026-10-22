package market

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
)

var (
	ErrNotFound      = errors.New("market: record not found")
	ErrInvalidRecord = errors.New("market: invalid record")
	ErrInvalidRef    = errors.New("market: invalid strongRef")
	ErrUnauthorized  = errors.New("market: unauthorized")
	ErrForbidden     = errors.New("market: forbidden")
	ErrInvalidAuth   = errors.New("market: invalid service auth")
	ErrInvalidEvent  = errors.New("market: invalid event")
	ErrContractGraph = errors.New("market: contract graph")
)

type ContractGraphError struct {
	Reason string
}

func (e *ContractGraphError) Error() string {
	return "market: contract graph: " + e.Reason
}

func (e *ContractGraphError) Is(target error) bool {
	return target == ErrContractGraph
}

type StrongRef struct {
	Type string `json:"$type,omitempty"`
	URI  string `json:"uri"`
	CID  string `json:"cid"`
}

func NewStrongRef(uri, cid string) StrongRef {
	return StrongRef{Type: StrongRefNSID, URI: uri, CID: cid}
}

func (r StrongRef) Validate() error {
	if r.Type != "" && r.Type != StrongRefNSID {
		return fmt.Errorf("%w: unexpected $type %q", ErrInvalidRef, r.Type)
	}
	if _, err := ParseRecordRef(r.URI, r.CID); err != nil {
		return err
	}
	return nil
}

func (r StrongRef) Map() map[string]any {
	return map[string]any{
		"$type": StrongRefNSID,
		"uri":   r.URI,
		"cid":   r.CID,
	}
}

type RecordRef struct {
	URI string
	CID string
}

func ParseRecordRef(uri, cidStr string) (RecordRef, error) {
	aturi, err := syntax.ParseATURI(uri)
	if err != nil {
		return RecordRef{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	if aturi.Collection() == "" || aturi.RecordKey() == "" || aturi.Authority() == "" {
		return RecordRef{}, fmt.Errorf("%w: %q is not a record AT URI", ErrInvalidRef, uri)
	}
	if _, err := syntax.ParseCID(cidStr); err != nil {
		return RecordRef{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	c, err := cid.Parse(cidStr)
	if err != nil {
		return RecordRef{}, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	if !c.Defined() || c.Version() != 1 {
		return RecordRef{}, fmt.Errorf("%w: %q is not a CIDv1", ErrInvalidRef, cidStr)
	}
	return RecordRef{URI: uri, CID: cidStr}, nil
}

func RefsEqual(a, b RecordRef) bool {
	return a.URI == b.URI && a.CID == b.CID
}

func RefKey(r RecordRef) string {
	return r.URI + "#" + r.CID
}

func ATURIAuthority(uri string) string {
	return string(syntax.ATURI(uri).Authority())
}

func NSIDFromURI(uri string) string {
	return string(syntax.ATURI(uri).Collection())
}

func ParseATURI(uri string) (syntax.ATURI, error) {
	return syntax.ParseATURI(uri)
}

type ResolvedRecord struct {
	URI   string
	CID   string
	Value map[string]any
}

func (r ResolvedRecord) Ref() RecordRef {
	return RecordRef{URI: r.URI, CID: r.CID}
}

type ListedRecord struct {
	URI   string
	CID   string
	Value map[string]any
}

type ListPage struct {
	Records []ListedRecord
	Cursor  string
}

type RecordResolver interface {
	Resolve(ctx context.Context, ref RecordRef) (ResolvedRecord, error)
}

type RecordGetter interface {
	GetRecord(ctx context.Context, repo, collection, rkey string) (ResolvedRecord, error)
}

type RecordLister interface {
	ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (ListPage, error)
}

type RepoReader interface {
	RecordGetter
	RecordLister
}

type RecordWriter interface {
	CreateRecord(ctx context.Context, repo, collection string, record map[string]any) (StrongRef, error)
	PutRecord(ctx context.Context, repo, collection, rkey string, record map[string]any) (StrongRef, error)
	DeleteRecord(ctx context.Context, repo, collection, rkey string) error
}

type RelayLookup interface {
	ListReposByCollection(ctx context.Context, relayURL, collection string) ([]string, error)
}

type PDSResolver interface {
	PDSEndpoint(ctx context.Context, did string) (string, error)
}

type WatchEvent struct {
	DID        string
	Collection string
	RKey       string
	Operation  string
	CID        string
	Record     map[string]any
	Time       string
}

type Watcher interface {
	Watch(ctx context.Context, collections []string, handler func(WatchEvent) error) error
}

type LogFunc func(level, msg string, fields map[string]any)

func NoopLog(string, string, map[string]any) {}

func NowISO() string {
	return FormatTime(time.Now())
}

func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func ParseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func ValidServiceRef(value string) error {
	if strings.HasPrefix(value, "did:") {
		if i := strings.IndexByte(value, '#'); i >= 0 && i == len(value)-1 {
			return fmt.Errorf("%w: service ref %q has an empty fragment", ErrInvalidRecord, value)
		}
		return nil
	}
	u, err := url.Parse(value)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return nil
	}
	return fmt.Errorf("%w: service ref %q is neither a DID ref nor an HTTP URL", ErrInvalidRecord, value)
}

var hostKeyTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.@-]{0,63}$`)

func ValidHostKeyType(value string) bool {
	return hostKeyTypeRE.MatchString(value)
}
