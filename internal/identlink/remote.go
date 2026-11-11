package identlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/identity"
	indigorepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	DocTTL         = 5 * time.Minute
	ErrorTTL       = 5 * time.Second
	maxRecordCAR   = 2 << 20
	defaultTimeout = 15 * time.Second
)

type Source interface {
	ResolveDID(ctx context.Context, did string) (*Doc, error)
	GetRecord(ctx context.Context, repo, collection, rkey string) (map[string]any, error)
	Forget(did string)
}

type resolved struct {
	ident *identity.Identity
	err   error
}

type Remote struct {
	dir  identity.Directory
	http *http.Client
	docs *ttlcache.Cache[string, resolved]
}

var _ Source = (*Remote)(nil)

func NewRemote(dir identity.Directory, client *http.Client) *Remote {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Remote{dir: dir, http: client, docs: ttlcache.New[string, resolved](ttlcache.Options{TTL: DocTTL, MaxEntries: 4096})}
}

func PLCDirectory(plcURL string, client *http.Client) identity.Directory {
	dir := &identity.BaseDirectory{PLCURL: strings.TrimRight(strings.TrimSpace(plcURL), "/"), SkipHandleVerification: true}
	if client != nil {
		dir.HTTPClient = *client
	}
	return dir
}

func (r *Remote) Forget(did string) {
	r.docs.Delete(did)
}

func (r *Remote) identity(ctx context.Context, did string) (*identity.Identity, error) {
	if got, ok := r.docs.Get(did); ok {
		return got.ident, got.err
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		r.docs.Set(did, resolved{})
		return nil, nil
	}
	ident, err := r.dir.LookupDID(ctx, parsed)
	switch {
	case errors.Is(err, identity.ErrDIDNotFound), err != nil && strings.HasSuffix(err.Error(), "status 410"):
		r.docs.Set(did, resolved{})
		return nil, nil
	case err != nil:
		if ctx.Err() == nil {
			r.docs.SetTTL(did, resolved{err: err}, ErrorTTL)
		}
		return nil, err
	case ident.DID != parsed:
		err = fmt.Errorf("the DID document of %s names %s", did, ident.DID)
		r.docs.SetTTL(did, resolved{err: err}, ErrorTTL)
		return nil, err
	}
	r.docs.Set(did, resolved{ident: ident})
	return ident, nil
}

func (r *Remote) ResolveDID(ctx context.Context, did string) (*Doc, error) {
	ident, err := r.identity(ctx, did)
	if ident == nil {
		return nil, err
	}
	return &Doc{AlsoKnownAs: ident.AlsoKnownAs}, nil
}

func (r *Remote) GetRecord(ctx context.Context, did, collection, rkey string) (map[string]any, error) {
	ident, err := r.identity(ctx, did)
	if ident == nil {
		return nil, err
	}
	pds := strings.TrimRight(ident.PDSEndpoint(), "/")
	if pds == "" {
		return nil, fmt.Errorf("%s has no atproto PDS", did)
	}
	pub, err := ident.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("the signing key of %s: %w", did, err)
	}
	query := url.Values{"did": {did}, "collection": {collection}, "rkey": {rkey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pds+"/xrpc/com.atproto.sync.getRecord?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.ipld.car")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRecordCAR+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var xrpc struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &xrpc)
		switch xrpc.Error {
		case "RecordNotFound", "RepoNotFound", "RepoDeactivated", "RepoTakendown", "RepoSuspended":
			return nil, nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("sync.getRecord %s/%s/%s: HTTP %d %s", did, collection, rkey, resp.StatusCode, xrpc.Error)
	}
	if len(body) > maxRecordCAR {
		return nil, fmt.Errorf("sync.getRecord %s/%s/%s: the CAR is larger than %d bytes", did, collection, rkey, maxRecordCAR)
	}
	return verifyRecordCAR(ctx, body, did, pub, collection, rkey)
}

func verifyRecordCAR(ctx context.Context, car []byte, did string, pub atcrypto.PublicKey, collection, rkey string) (map[string]any, error) {
	_, blocks, err := repo.ReadCARBytes(ctx, car)
	if err != nil {
		return nil, err
	}
	for _, blk := range blocks {
		sum, err := blk.Cid().Prefix().Sum(blk.RawData())
		if err != nil || !sum.Equals(blk.Cid()) {
			return nil, fmt.Errorf("the CAR block %s does not match its CID", blk.Cid())
		}
	}
	commit, tree, err := repo.VerifyCommitCAR(ctx, car, pub)
	if err != nil {
		return nil, err
	}
	if commit.DID != did {
		return nil, fmt.Errorf("the commit is of %s, not %s", commit.DID, did)
	}
	nsid, err := syntax.ParseNSID(collection)
	if err != nil {
		return nil, err
	}
	key, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return nil, err
	}
	data, _, err := tree.GetRecordBytes(ctx, nsid, key)
	if errors.Is(err, indigorepo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return atdata.UnmarshalCBOR(data)
}
