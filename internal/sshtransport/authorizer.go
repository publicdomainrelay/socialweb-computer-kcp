package sshtransport

import (
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
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
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

	maxRecordPages = 20
)

type PresentedKey struct {
	Algo string
	Key  string
}

type AuthorizedAccount struct {
	DID    string
	Handle string
	PDS    string
}

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
	if doc.Handle == "" {
		doc.Handle = doc.DID
	}
	doc.PDS = id.PDSEndpoint()
	return doc, nil
}

type RecordSource interface {
	ListRecords(ctx context.Context, pds, did, collection string, limit int, cursor string) ([]map[string]any, string, error)
}

type HTTPRecordSource struct {
	TrustedHosts []string

	MaxBytes int64

	Client *http.Client
}

func (s *HTTPRecordSource) ListRecords(ctx context.Context, pds, did, collection string, limit int, cursor string) ([]map[string]any, string, error) {
	if limit <= 0 {
		limit = 100
	}
	base := strings.TrimRight(pds, "/") + "/xrpc/com.atproto.repo.listRecords"
	u, err := url.Parse(base)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("repo", did)
	q.Set("collection", collection)
	q.Set("limit", fmt.Sprintf("%d", limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u.RawQuery = q.Encode()
	trusted := make(map[string]bool, len(s.TrustedHosts))
	for _, host := range s.TrustedHosts {
		trusted[host] = true
	}
	maxBytes := s.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	raw, err := GuardedFetch(ctx, s.Client, u.String(), maxBytes, trusted)
	if err != nil {
		return nil, "", err
	}
	var body struct {
		Records []struct {
			Value json.RawMessage `json:"value"`
		} `json:"records"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", err
	}
	out := make([]map[string]any, 0, len(body.Records))
	for _, rec := range body.Records {
		var value map[string]any
		if err := json.Unmarshal(rec.Value, &value); err != nil {
			continue
		}
		out = append(out, value)
	}
	return out, body.Cursor, nil
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
		return nil, fmt.Errorf("%d from %s", resp.StatusCode, target.Scheme+"://"+target.Host)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
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

	records []map[string]any

	lookupFailed bool

	unknownAccount bool

	expiresAt time.Time

	checkedAt time.Time
}

type PDSKeyAuthorizer struct {
	opts KeyAuthorizerOptions

	mu    sync.Mutex
	cache map[string]*cacheEntry
	order []string
}

func NewPDSKeyAuthorizer(opts KeyAuthorizerOptions) (*PDSKeyAuthorizer, error) {
	if opts.Resolver == nil {
		return nil, errors.New("sshtransport: key authorizer has no identity resolver")
	}
	if opts.Records == nil {
		return nil, errors.New("sshtransport: key authorizer has no record source")
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.PositiveCacheTTL <= 0 {
		opts.PositiveCacheTTL = DefaultPositiveCacheTTL
	}
	if opts.NegativeCacheTTL <= 0 {
		opts.NegativeCacheTTL = DefaultNegativeCacheTTL
	}
	if opts.MissRecheck <= 0 {
		opts.MissRecheck = DefaultMissRecheck
	}
	if opts.MaxCacheEntries <= 0 {
		opts.MaxCacheEntries = DefaultMaxCacheEntries
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &PDSKeyAuthorizer{
		opts:  opts,
		cache: map[string]*cacheEntry{},
	}, nil
}

func (a *PDSKeyAuthorizer) remember(username string, entry *cacheEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.cache[username]; exists {
		for i, name := range a.order {
			if name == username {
				a.order = append(a.order[:i], a.order[i+1:]...)
				break
			}
		}
	}
	a.order = append(a.order, username)
	a.cache[username] = entry
	for len(a.cache) > a.opts.MaxCacheEntries && len(a.order) > 0 {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.cache, oldest)
	}
}

func (a *PDSKeyAuthorizer) cached(username string) *cacheEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cache[username]
}

func (a *PDSKeyAuthorizer) empty(lookupFailed, unknownAccount bool) *cacheEntry {
	now := time.Now()
	return &cacheEntry{
		lookupFailed:   lookupFailed,
		unknownAccount: unknownAccount,
		checkedAt:      now,
		expiresAt:      now.Add(a.opts.NegativeCacheTTL),
	}
}

func (a *PDSKeyAuthorizer) resolveAccount(ctx context.Context, username string) (*AuthorizedAccount, bool, error) {
	if !strings.HasPrefix(username, "did:") && !strings.Contains(username, ".") {
		return nil, true, nil
	}
	if strings.HasPrefix(username, "did:web:") {
		rest := strings.TrimPrefix(username, "did:web:")
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
	if handle == "" {
		handle = did
	}
	return &AuthorizedAccount{DID: did, Handle: handle, PDS: doc.PDS}, false, nil
}

func (a *PDSKeyAuthorizer) listAllRecords(ctx context.Context, account *AuthorizedAccount) ([]map[string]any, error) {
	records := []map[string]any{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxRecordPages; page++ {
		found, next, err := a.opts.Records.ListRecords(ctx, account.PDS, account.DID, BadgeBlueKeysNSID, 100, cursor)
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
		if next == "" || seen[next] {
			break
		}
		seen[next] = true
		cursor = next
	}
	return records, nil
}

func (a *PDSKeyAuthorizer) load(ctx context.Context, username string) *cacheEntry {
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
	records, err := a.listAllRecords(ctx, account)
	if err != nil {
		a.opts.Log.Warn("authorize_error", "username", username, "err", err)
		return a.empty(true, false)
	}
	now := time.Now()
	return &cacheEntry{
		account:   account,
		records:   records,
		checkedAt: now,
		expiresAt: now.Add(a.opts.CacheTTL),
	}
}

func hasAnyAssociation(entry *cacheEntry, account *AuthorizedAccount) bool {
	for _, rec := range entry.records {
		if isRequesterAssociation(rec, account.DID) {
			return true
		}
	}
	return false
}

func entryMatches(entry *cacheEntry, key PresentedKey) bool {
	if entry.account == nil {
		return false
	}
	for _, rec := range entry.records {
		if isRequesterAssociation(rec, entry.account.DID) && sshKeyMatches(rec["keyId"], key) {
			return true
		}
	}
	return false
}

func (a *PDSKeyAuthorizer) Authorize(ctx context.Context, username string, key PresentedKey) (*AuthorizedAccount, error) {
	now := time.Now()
	entry := a.cached(username)
	if entry == nil || !now.Before(entry.expiresAt) {
		entry = a.load(ctx, username)
		a.remember(username, entry)
	}
	matched := entryMatches(entry, key)
	if !matched && now.Sub(entry.checkedAt) >= a.opts.MissRecheck {
		entry = a.load(ctx, username)
		a.remember(username, entry)
		matched = entryMatches(entry, key)
	}
	if matched && now.Sub(entry.checkedAt) >= a.opts.PositiveCacheTTL {
		entry = a.load(ctx, username)
		a.remember(username, entry)
		matched = entryMatches(entry, key)
	}
	if entry.lookupFailed {
		return nil, fmt.Errorf("association lookup failed for %s", username)
	}
	if entry.unknownAccount || (entry.account != nil && !hasAnyAssociation(entry, entry.account)) {
		reason := "no_associations"
		if entry.unknownAccount {
			reason = "no_account"
		}
		return nil, &NoKeyCouldMatchError{Username: username, Reason: reason}
	}
	if matched {
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
