package sshtransport

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	BadgeBlueKeysNSID = "com.publicdomainrelay.temp.badgeBlueKeys"

	RequesterAssociateService = "requester_associate"

	DefaultCacheTTL = 5 * time.Minute

	DefaultPositiveCacheTTL = 30 * time.Second

	DefaultNegativeCacheTTL = 30 * time.Second

	DefaultMissRecheck = 5 * time.Second

	DefaultMaxCacheEntries = 1024

	DefaultMaxResponseBytes = 1 << 20

	DefaultFetchTimeout = 15 * time.Second

	maxErrorBodyBytes = 4096
)

var ErrRecordNotFound = errors.New("sshtransport: record not found")

type PresentedKey struct {
	Algo string
	Key  string
}

type AuthorizedAccount = sshcontract.Account

type KeyAuthorizer interface {
	Authorize(ctx context.Context, username string, key PresentedKey) (*AuthorizedAccount, error)
}

type NoKeyCouldMatchError struct {
	Username string
	Reason   string
}

func (e *NoKeyCouldMatchError) Error() string {
	return fmt.Sprintf("sshtransport: %s has no key that could be associated with it (%s)", e.Username, e.Reason)
}

type DIDDocument struct {
	DID    string
	Handle string
	PDS    string
}

type IdentityResolver interface {
	ResolveHandle(ctx context.Context, handle string) (string, error)

	ResolveDID(ctx context.Context, did string) (*DIDDocument, error)
}

type DirectoryResolver struct {
	Directory identity.Directory
}

func (r DirectoryResolver) ResolveHandle(ctx context.Context, handle string) (string, error) {
	if r.Directory == nil {
		return "", errors.New("sshtransport: no identity directory")
	}
	h, err := syntax.ParseHandle(handle)
	if err != nil {
		return "", err
	}
	id, err := r.Directory.LookupHandle(ctx, h)
	if err != nil {
		return "", err
	}
	return id.DID.String(), nil
}

func (r DirectoryResolver) ResolveDID(ctx context.Context, did string) (*DIDDocument, error) {
	if r.Directory == nil {
		return nil, errors.New("sshtransport: no identity directory")
	}
	d, err := syntax.ParseDID(did)
	if err != nil {
		return nil, err
	}
	id, err := r.Directory.LookupDID(ctx, d)
	if err != nil {
		return nil, err
	}
	doc := &DIDDocument{DID: id.DID.String()}
	if len(id.AlsoKnownAs) > 0 {
		doc.Handle = strings.TrimPrefix(id.AlsoKnownAs[0], "at://")
	}
	doc.Handle = cmp.Or(doc.Handle, doc.DID)
	doc.PDS = id.PDSEndpoint()
	return doc, nil
}

type RecordSource interface {
	GetRecord(ctx context.Context, pds, did, collection, rkey string) (map[string]any, error)
}

type HTTPRecordSource struct {
	TrustedHosts []string

	Trust func(host string) bool

	MaxBytes int64

	Client *http.Client
}

func (s *HTTPRecordSource) GetRecord(ctx context.Context, pds, did, collection, rkey string) (map[string]any, error) {
	base := strings.TrimRight(pds, "/") + "/xrpc/com.atproto.repo.getRecord"
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("repo", did)
	q.Set("collection", collection)
	q.Set("rkey", rkey)
	u.RawQuery = q.Encode()
	trusted := make(map[string]bool, len(s.TrustedHosts))
	host := strings.ToLower(u.Hostname())
	for _, name := range s.TrustedHosts {
		name = strings.ToLower(strings.TrimSpace(name))
		trusted[name] = true
		if name != "" && strings.HasSuffix(host, "."+name) {
			trusted[host] = true
		}
	}
	if s.Trust != nil && host != "" && s.Trust(host) {
		trusted[host] = true
	}
	maxBytes := s.MaxBytes
	maxBytes = opt.Positive(maxBytes, DefaultMaxResponseBytes)
	raw, err := GuardedFetch(ctx, s.Client, u.String(), maxBytes, trusted)
	if err != nil {
		var status *FetchStatusError
		if errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Code == "RecordNotFound" || status.Code == "RepoNotFound") {
			return nil, fmt.Errorf("%w: %v", ErrRecordNotFound, err)
		}
		return nil, err
	}
	var body struct {
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body.Value, nil
}

type FetchStatusError struct {
	Status int

	Code string

	Origin string
}

func (e *FetchStatusError) Error() string {
	return fmt.Sprintf("%d from %s", e.Status, e.Origin)
}

func IsPrivateAddress(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		a, b := v4[0], v4[1]
		if a == 0 || a == 10 || a == 127 {
			return true
		}
		if a == 169 && b == 254 {
			return true
		}
		if a == 172 && b >= 16 && b <= 31 {
			return true
		}
		if a == 192 && b == 168 {
			return true
		}
		if a == 100 && b >= 64 && b <= 127 {
			return true
		}
		return a >= 224
	}
	v6 := ip.To16()
	if v6 == nil {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return true
	}
	return v6[0]&0xfe == 0xfc
}

func ResolvesToPublicAddress(ctx context.Context, hostname string) bool {
	host := strings.Trim(hostname, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return !IsPrivateAddress(host)
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, DefaultFetchTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(cctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if IsPrivateAddress(addr.IP.String()) {
			return false
		}
	}
	return true
}

func GuardedFetch(ctx context.Context, client *http.Client, rawURL string, maxBytes int64, trustedHosts map[string]bool) ([]byte, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := target.Hostname()
	loopback := target.Scheme == "http" &&
		(host == "127.0.0.1" || host == "::1" || host == "localhost")
	if target.Scheme != "https" && !loopback && !trustedHosts[host] {
		return nil, fmt.Errorf("sshtransport: refused to fetch %s", target.Scheme+"://"+target.Host)
	}
	if !loopback && !trustedHosts[host] && !ResolvesToPublicAddress(ctx, host) {
		return nil, fmt.Errorf("sshtransport: refused to fetch %s", target.Host)
	}
	cctx, cancel := context.WithTimeout(ctx, DefaultFetchTimeout)
	defer cancel()
	fetch := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if client != nil {
		copied := *client
		copied.CheckRedirect = fetch.CheckRedirect
		fetch = &copied
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := fetch.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("%s tried to redirect the fetch", target.Scheme+"://"+target.Host)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		failure := &FetchStatusError{Status: resp.StatusCode, Origin: target.Scheme + "://" + target.Host}
		if raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes)); err == nil {
			var body struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(raw, &body) == nil {
				failure.Code = body.Error
			}
		}
		return nil, failure
	}
	maxBytes = opt.Positive(maxBytes, DefaultMaxResponseBytes)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%s exceeded %d bytes", target.Scheme+"://"+target.Host, maxBytes)
	}
	return raw, nil
}

type KeyAuthorizerOptions struct {
	Resolver IdentityResolver

	Records RecordSource

	CacheTTL time.Duration

	PositiveCacheTTL time.Duration

	NegativeCacheTTL time.Duration

	MissRecheck time.Duration

	MaxCacheEntries int

	Log *slog.Logger
}

type cacheEntry struct {
	account *AuthorizedAccount

	matched bool

	lookupFailed bool

	unknownAccount bool

	ttl time.Duration

	checkedAt time.Time
}

type PDSKeyAuthorizer struct {
	opts KeyAuthorizerOptions

	cache *ttlcache.Cache[string, *cacheEntry]
}

func NewPDSKeyAuthorizer(opts KeyAuthorizerOptions) (*PDSKeyAuthorizer, error) {
	if opts.Resolver == nil {
		return nil, errors.New("sshtransport: key authorizer has no identity resolver")
	}
	if opts.Records == nil {
		return nil, errors.New("sshtransport: key authorizer has no record source")
	}
	opts.CacheTTL = opt.Positive(opts.CacheTTL, DefaultCacheTTL)
	opts.PositiveCacheTTL = opt.Positive(opts.PositiveCacheTTL, DefaultPositiveCacheTTL)
	opts.NegativeCacheTTL = opt.Positive(opts.NegativeCacheTTL, DefaultNegativeCacheTTL)
	opts.MissRecheck = opt.Positive(opts.MissRecheck, DefaultMissRecheck)
	opts.MaxCacheEntries = opt.Positive(opts.MaxCacheEntries, DefaultMaxCacheEntries)
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &PDSKeyAuthorizer{
		opts:  opts,
		cache: ttlcache.New[string, *cacheEntry](ttlcache.Options{MaxEntries: opts.MaxCacheEntries}),
	}, nil
}

func (a *PDSKeyAuthorizer) remember(id string, entry *cacheEntry) {
	a.cache.SetTTL(id, entry, entry.ttl)
}

func (a *PDSKeyAuthorizer) empty(lookupFailed, unknownAccount bool) *cacheEntry {
	now := time.Now()
	return &cacheEntry{
		lookupFailed:   lookupFailed,
		unknownAccount: unknownAccount,
		checkedAt:      now,
		ttl:            a.opts.NegativeCacheTTL,
	}
}

func (a *PDSKeyAuthorizer) resolveAccount(ctx context.Context, username string) (*AuthorizedAccount, bool, error) {
	if !strings.HasPrefix(username, "did:") && !strings.Contains(username, ".") {
		return nil, true, nil
	}
	if after, ok := strings.CutPrefix(username, "did:web:"); ok {
		rest := after
		host, _, _ := strings.Cut(rest, ":")
		decoded, err := url.PathUnescape(host)
		if err != nil || decoded == "" || !ResolvesToPublicAddress(ctx, decoded) {
			return nil, false, errors.New("sshtransport: the did:web host does not resolve publicly")
		}
	}
	did := username
	if !strings.HasPrefix(username, "did:") {
		resolved, err := a.opts.Resolver.ResolveHandle(ctx, username)
		if err != nil {
			return nil, false, err
		}
		did = resolved
	}
	doc, err := a.opts.Resolver.ResolveDID(ctx, did)
	if err != nil {
		return nil, false, err
	}
	if doc == nil || doc.PDS == "" {
		return nil, false, errors.New("sshtransport: the identity names no PDS")
	}
	handle := doc.Handle
	handle = cmp.Or(handle, did)
	return &AuthorizedAccount{DID: did, Handle: handle, PDS: doc.PDS}, false, nil
}

func (a *PDSKeyAuthorizer) load(ctx context.Context, username string, key PresentedKey) *cacheEntry {
	account, unknown, err := a.resolveAccount(ctx, username)
	if err != nil {
		a.opts.Log.Warn("authorize_error", "username", username, "err", err)
		return a.empty(true, false)
	}
	if unknown {
		return a.empty(false, true)
	}
	if account == nil {
		return a.empty(true, false)
	}
	rkey := keyRkey(account.DID, parsedPublicKey{algo: key.Algo, key: key.Key})
	record, err := a.opts.Records.GetRecord(ctx, account.PDS, account.DID, BadgeBlueKeysNSID, rkey)
	if err != nil && !errors.Is(err, ErrRecordNotFound) {
		a.opts.Log.Warn("authorize_error", "username", username, "rkey", rkey, "err", err)
		return a.empty(true, false)
	}
	return &cacheEntry{
		account:   account,
		matched:   err == nil && isRequesterAssociation(record, account.DID) && sshKeyMatches(record["keyId"], key),
		checkedAt: time.Now(),
		ttl:       a.opts.CacheTTL,
	}
}

func (a *PDSKeyAuthorizer) Authorize(ctx context.Context, username string, key PresentedKey) (*AuthorizedAccount, error) {
	now := time.Now()
	id := username + "\x00" + key.Algo + " " + key.Key
	entry, ok := a.cache.Get(id)
	if !ok {
		entry = a.load(ctx, username, key)
		a.remember(id, entry)
	}
	if !entry.matched && now.Sub(entry.checkedAt) >= a.opts.MissRecheck {
		entry = a.load(ctx, username, key)
		a.remember(id, entry)
	}
	if entry.matched && now.Sub(entry.checkedAt) >= a.opts.PositiveCacheTTL {
		entry = a.load(ctx, username, key)
		a.remember(id, entry)
	}
	if entry.lookupFailed {
		return nil, fmt.Errorf("association lookup failed for %s", username)
	}
	if entry.unknownAccount {
		return nil, &NoKeyCouldMatchError{Username: username, Reason: "no_account"}
	}
	if entry.matched {
		return entry.account, nil
	}
	return nil, nil
}

func isRequesterAssociation(record map[string]any, accountDID string) bool {
	if record == nil {
		return false
	}
	challenge, _ := record["challenge"].(string)
	service, _ := record["service"].(string)
	return challenge == accountDID && service == RequesterAssociateService
}

func SplitSSHPublicKey(line string) (PresentedKey, bool) {
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return PresentedKey{}, false
	}
	return PresentedKey{Algo: parts[0], Key: parts[1]}, true
}

func sshKeyMatches(stored any, presented PresentedKey) bool {
	text, ok := stored.(string)
	if !ok {
		return false
	}
	parsed, ok := SplitSSHPublicKey(text)
	if !ok {
		return false
	}
	return parsed.Algo == presented.Algo && parsed.Key == presented.Key
}

type PDSOverrideResolver struct {
	Inner IdentityResolver

	PDS map[string]string
}

var _ IdentityResolver = PDSOverrideResolver{}

func (r PDSOverrideResolver) ResolveHandle(ctx context.Context, handle string) (string, error) {
	return r.Inner.ResolveHandle(ctx, handle)
}

func (r PDSOverrideResolver) ResolveDID(ctx context.Context, did string) (*DIDDocument, error) {
	doc, err := r.Inner.ResolveDID(ctx, did)
	if err != nil {
		return nil, err
	}
	if pds := strings.TrimSpace(r.PDS[doc.DID]); pds != "" {
		doc.PDS = pds
	}
	return doc, nil
}
