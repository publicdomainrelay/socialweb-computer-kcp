package gatewayapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
)

const (
	agentMaxBody = 8 << 20
	agentPageMax = 100
)

const (
	identityCacheEntries = 4096

	identityCacheTTL = time.Minute

	identityCacheMargin = 5 * time.Second

	identityActiveWindow = 2 * time.Minute

	identityRefreshBackoffMin = time.Second

	identityRefreshBackoffMax = 30 * time.Second
)

type custodySignIn struct {
	client *custodyClient

	pdsHTTP *http.Client

	role string

	renewBelow time.Duration

	renewIncrement time.Duration

	allowInternalHTTP bool

	log *slog.Logger

	mu sync.Mutex

	identities map[[sha256.Size]byte]*custodyIdentityEntry

	wake chan struct{}

	stopCh chan struct{}

	closed bool

	refreshing bool
}

type custodyIdentityEntry struct {
	user coreapi.WebUser

	token string

	leaseUntil time.Time

	checkUntil time.Time

	renewable bool

	renewDue bool

	lastSeen time.Time

	failures int

	nextTry time.Time
}

type custodySignInOptions struct {
	Endpoint custodyEndpoint

	Role string

	RenewBelow time.Duration

	RenewIncrement time.Duration

	Network *v1alpha1.MarketNetworkValue

	TLSConfig *tls.Config

	Log *slog.Logger
}

func newCustodySignIn(opts custodySignInOptions) (*custodySignIn, error) {
	client, err := newCustodyClient(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	if opts.Role == "" {
		opts.Role = DefaultAtprotoRole
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &custodySignIn{
		client:            client,
		pdsHTTP:           &http.Client{Timeout: 30 * time.Second, Transport: pdsTransport(opts.Network, opts.TLSConfig)},
		role:              opts.Role,
		renewBelow:        opts.RenewBelow,
		renewIncrement:    opts.RenewIncrement,
		allowInternalHTTP: opts.Network != nil,
		log:               opts.Log,
		identities:        map[[sha256.Size]byte]*custodyIdentityEntry{},
		wake:              make(chan struct{}, 1),
		stopCh:            make(chan struct{}),
	}, nil
}

func pdsTransport(network *v1alpha1.MarketNetworkValue, tlsConfig *tls.Config) *http.Transport {
	if network != nil {
		pairs := make(map[string]string, len(network.Hosts))
		for _, host := range network.Hosts {
			pairs[host.Name] = host.Address
		}
		if hosts, err := networkdial.HostMapFromPairs(pairs); err == nil {
			if transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{
				Hosts:          hosts,
				TLSConfig:      tlsConfig,
				AllowSystemDNS: true,
			}); err == nil {
				transport.Proxy = nil
				return transport
			}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

func (s *custodySignIn) ClientMetadata(ctx context.Context) (json.RawMessage, error) {
	return s.client.clientMetadata(ctx)
}

func (s *custodySignIn) Start(ctx context.Context, identifier, redirectURI string) (coreapi.SignInStart, error) {
	authorizationURL, state, err := s.client.start(ctx, s.role, identifier, redirectURI)
	if err != nil {
		return coreapi.SignInStart{}, err
	}
	return coreapi.SignInStart{AuthorizationURL: authorizationURL, State: state}, nil
}

func (s *custodySignIn) Complete(ctx context.Context, params coreapi.SignInCompleteParams) (coreapi.SignInGrant, error) {
	login, err := s.client.complete(ctx, params.State, params.Code, params.ISS)
	if err != nil {
		return coreapi.SignInGrant{}, err
	}
	return coreapi.SignInGrant{
		WebUser:      coreapi.WebUser{DID: login.DID, Handle: login.Handle, PDS: login.PDS},
		Token:        login.Token,
		LeaseSeconds: login.LeaseSeconds,
	}, nil
}

func (s *custodySignIn) Identify(ctx context.Context, token string) (*coreapi.WebUser, error) {
	if token == "" {
		return nil, nil
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	entry := s.identities[key]
	if entry != nil && now.Before(entry.leaseUntil) {
		user := entry.user
		due := !entry.checkUntil.IsZero() && !entry.checkUntil.After(now)
		if s.renewalDue(entry, now) {
			entry.renewDue = true
			due = true
		}
		entry.lastSeen = now
		if due {
			s.wakeRefresherLocked()
		}
		s.mu.Unlock()
		return &user, nil
	}
	if entry != nil {
		delete(s.identities, key)
	}
	s.mu.Unlock()
	identity, err := s.client.identify(ctx, token)
	if err != nil {
		var custody *custodyError
		if errors.As(err, &custody) && custody.Status == http.StatusForbidden {
			return nil, nil
		}
		return nil, err
	}
	if identity == nil {
		return nil, nil
	}
	s.storeIdentity(key, token, identity, time.Now())
	return &coreapi.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS}, nil
}

func identityCacheWindow(now time.Time, ttlSeconds int64) (time.Time, time.Time) {
	if ttlSeconds <= 0 {
		return time.Time{}, time.Time{}
	}
	lease := time.Duration(ttlSeconds) * time.Second
	leaseUntil := now.Add(lease)
	window := lease
	if window > identityCacheTTL {
		window = identityCacheTTL
	}
	margin := identityCacheMargin
	if quarter := lease / 4; quarter < margin {
		margin = quarter
	}
	window -= margin
	if window <= 0 {
		return leaseUntil, time.Time{}
	}
	return leaseUntil, now.Add(window)
}

func (s *custodySignIn) renewalDue(entry *custodyIdentityEntry, now time.Time) bool {
	return entry.renewable && s.renewBelow > 0 && !entry.leaseUntil.Add(-s.renewBelow).After(now)
}

func (s *custodySignIn) dueAt(entry *custodyIdentityEntry, now time.Time) time.Time {
	if entry.renewDue {
		return now
	}
	if entry.nextTry.After(now) {
		return entry.nextTry
	}
	next := entry.leaseUntil
	if !entry.checkUntil.IsZero() && entry.checkUntil.Before(next) {
		next = entry.checkUntil
	}
	if entry.renewable && s.renewBelow > 0 {
		if horizon := entry.leaseUntil.Add(-s.renewBelow); horizon.Before(next) {
			next = horizon
		}
	}
	if !next.After(now) {
		return now
	}
	return next
}

func (s *custodySignIn) storeIdentity(key [sha256.Size]byte, token string, identity *custodyIdentity, now time.Time) {
	leaseUntil, checkUntil := identityCacheWindow(now, identity.TTLSeconds)
	if leaseUntil.IsZero() {
		return
	}
	s.mu.Lock()
	s.pruneLocked(now)
	if len(s.identities) >= identityCacheEntries {
		s.evictLocked()
	}
	entry := &custodyIdentityEntry{
		user:       coreapi.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS},
		token:      token,
		leaseUntil: leaseUntil,
		checkUntil: checkUntil,
		renewable:  identity.Renewable,
		lastSeen:   now,
	}
	entry.renewDue = s.renewalDue(entry, now)
	s.identities[key] = entry
	s.wakeRefresherLocked()
	s.mu.Unlock()
}

func (s *custodySignIn) pruneLocked(now time.Time) {
	for key, entry := range s.identities {
		if !now.Before(entry.leaseUntil) {
			delete(s.identities, key)
		}
	}
}

func (s *custodySignIn) evictLocked() {
	var oldestKey [sha256.Size]byte
	var oldest time.Time
	found := false
	for key, entry := range s.identities {
		if !found || entry.leaseUntil.Before(oldest) {
			oldestKey = key
			oldest = entry.leaseUntil
			found = true
		}
	}
	if found {
		delete(s.identities, oldestKey)
	}
}

func (s *custodySignIn) forgetToken(token string) {
	if token == "" {
		return
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	delete(s.identities, key)
	s.mu.Unlock()
}

func (s *custodySignIn) forgetKey(key [sha256.Size]byte, token string) {
	s.mu.Lock()
	entry := s.identities[key]
	if entry != nil && entry.token == token {
		delete(s.identities, key)
	}
	s.mu.Unlock()
}

func (s *custodySignIn) ensureRefresherLocked() {
	if s.closed || s.refreshing {
		return
	}
	s.refreshing = true
	go s.refreshLoop()
}

func (s *custodySignIn) wakeRefresherLocked() {
	if s.closed {
		return
	}
	s.ensureRefresherLocked()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *custodySignIn) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stopCh)
	}
	s.mu.Unlock()
}

func (s *custodySignIn) refreshLoop() {
	for {
		s.mu.Lock()
		if s.closed {
			s.refreshing = false
			s.mu.Unlock()
			return
		}
		now := time.Now()
		var pickKey [sha256.Size]byte
		var pick *custodyIdentityEntry
		var next time.Time
		active := false
		for key, entry := range s.identities {
			if !now.Before(entry.leaseUntil) {
				delete(s.identities, key)
				continue
			}
			if now.Sub(entry.lastSeen) > identityActiveWindow {
				continue
			}
			active = true
			due := s.dueAt(entry, now)
			if !due.After(now) {
				pickKey = key
				pick = entry
				break
			}
			if next.IsZero() || due.Before(next) {
				next = due
			}
		}
		if pick == nil {
			if !active {
				s.refreshing = false
				s.mu.Unlock()
				return
			}
			delay := next.Sub(now)
			if delay < time.Millisecond {
				delay = time.Millisecond
			}
			s.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-s.stopCh:
				timer.Stop()
				s.mu.Lock()
				s.refreshing = false
				s.mu.Unlock()
				return
			case <-s.wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		token := pick.token
		renew := pick.renewDue || s.renewalDue(pick, now)
		s.mu.Unlock()
		s.refreshIdentity(pickKey, token, renew)
	}
}

func (s *custodySignIn) refreshIdentity(key [sha256.Size]byte, token string, renew bool) {
	timeout := s.client.endpoint.Timeout
	if timeout <= 0 {
		timeout = DefaultCustodyTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if renew {
		leaseSeconds, err := s.client.renew(ctx, token, s.renewIncrement)
		if err != nil {
			s.refreshFailed(key, token, err)
			return
		}
		if leaseSeconds <= 0 {
			s.refreshFailed(key, token, errors.New("gatewayapi: the custody renew returned no lease"))
			return
		}
		s.refreshRenewed(key, token, leaseSeconds)
		return
	}
	identity, err := s.client.identify(ctx, token)
	if err != nil {
		s.refreshFailed(key, token, err)
		return
	}
	if identity == nil {
		s.forgetKey(key, token)
		return
	}
	s.refreshIdentified(key, token, identity)
}

func (s *custodySignIn) refreshFailed(key [sha256.Size]byte, token string, err error) {
	now := time.Now()
	var custody *custodyError
	invalid := errors.As(err, &custody) && (custody.Status == http.StatusForbidden || custody.Status == http.StatusUnauthorized)
	s.mu.Lock()
	entry := s.identities[key]
	if entry == nil || entry.token != token {
		s.mu.Unlock()
		return
	}
	if invalid || !now.Before(entry.leaseUntil) {
		delete(s.identities, key)
		s.mu.Unlock()
		return
	}
	entry.failures++
	delay := identityRefreshBackoff(entry.failures)
	if remaining := entry.leaseUntil.Sub(now); delay > remaining/2 {
		delay = remaining / 2
	}
	if delay < identityRefreshBackoffMin {
		delay = identityRefreshBackoffMin
	}
	entry.nextTry = now.Add(delay)
	did := entry.user.DID
	s.mu.Unlock()
	s.log.Warn("web_token_refresh_failed", "did", did, "err", err)
}

func identityRefreshBackoff(failures int) time.Duration {
	delay := identityRefreshBackoffMin
	for i := 1; i < failures; i++ {
		if delay >= identityRefreshBackoffMax/2 {
			return identityRefreshBackoffMax
		}
		delay *= 2
	}
	if delay > identityRefreshBackoffMax {
		delay = identityRefreshBackoffMax
	}
	return delay
}

func (s *custodySignIn) refreshRenewed(key [sha256.Size]byte, token string, leaseSeconds int64) {
	now := time.Now()
	leaseUntil, checkUntil := identityCacheWindow(now, leaseSeconds)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.identities[key]
	if entry == nil || entry.token != token {
		return
	}
	if leaseUntil.IsZero() {
		delete(s.identities, key)
		return
	}
	entry.leaseUntil = leaseUntil
	entry.checkUntil = checkUntil
	entry.failures = 0
	entry.nextTry = time.Time{}
	entry.renewDue = s.renewalDue(entry, now)
}

func (s *custodySignIn) refreshIdentified(key [sha256.Size]byte, token string, identity *custodyIdentity) {
	now := time.Now()
	leaseUntil, checkUntil := identityCacheWindow(now, identity.TTLSeconds)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.identities[key]
	if entry == nil || entry.token != token {
		return
	}
	if leaseUntil.IsZero() {
		delete(s.identities, key)
		return
	}
	entry.user = coreapi.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS}
	entry.leaseUntil = leaseUntil
	entry.checkUntil = checkUntil
	entry.renewable = identity.Renewable
	entry.failures = 0
	entry.nextTry = time.Time{}
	entry.renewDue = s.renewalDue(entry, now)
}

func (s *custodySignIn) SignOut(ctx context.Context, caller *coreapi.WebCaller) (coreapi.SignOutResult, error) {
	if caller == nil {
		return coreapi.SignOutResult{}, errors.New("gatewayapi: no caller to sign out")
	}
	s.forgetToken(caller.Token)
	revoked, revokeError, signOutErr := s.client.signOut(ctx, caller.Token, caller.DID)
	if err := s.client.revokeToken(ctx, caller.Token); err != nil {
		s.log.Warn("web_token_revoke_failed", "did", caller.DID, "err", err)
	}
	if signOutErr != nil {
		return coreapi.SignOutResult{}, signOutErr
	}
	return coreapi.SignOutResult{Revoked: revoked, RevokeError: revokeError}, nil
}

func (s *custodySignIn) AgentFor(caller *coreapi.WebCaller) coreapi.WebAccountAgent {
	if caller == nil {
		return nil
	}
	return &custodyAgent{client: s.client, http: s.pdsHTTP, caller: caller, allowInternalHTTP: s.allowInternalHTTP,
		log: s.log, nonces: map[string]string{}}
}

type custodyAgent struct {
	client *custodyClient

	http *http.Client

	caller *coreapi.WebCaller

	allowInternalHTTP bool

	log *slog.Logger

	mu sync.Mutex

	nonces map[string]string
}

func (a *custodyAgent) nonce(origin string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nonces[origin]
}

func (a *custodyAgent) setNonce(origin, nonce string) {
	if nonce == "" {
		return
	}
	a.mu.Lock()
	a.nonces[origin] = nonce
	a.mu.Unlock()
}

func (a *custodyAgent) pds() (*url.URL, error) {
	raw := strings.TrimSpace(a.caller.PDS)
	if raw == "" {
		return nil, errors.New("gatewayapi: the account has no PDS URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the account PDS URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("gatewayapi: the account PDS URL %q is not absolute http or https", raw)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) && !(a.allowInternalHTTP && internalServiceHost(u.Hostname())) {
		return nil, fmt.Errorf("gatewayapi: the account PDS URL %q is plain http on a non-loopback host", raw)
	}
	return u, nil
}

func internalServiceHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "svc.kcp.local" || strings.HasSuffix(host, ".svc.kcp.local")
}

func (a *custodyAgent) request(ctx context.Context, method, target string, body any) ([]byte, error) {
	base, err := a.pds()
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	full := base.ResolveReference(ref)
	origin := full.Scheme + "://" + full.Host
	var encoded []byte
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: encoding %s %s: %w", method, full.Path, err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := a.client.proof(ctx, a.caller.Token, a.caller.DID, method, full.String(), a.nonce(origin))
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if encoded != nil {
			reader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, full.String(), reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "DPoP "+proof.AccessToken)
		req.Header.Set("DPoP", proof.DPoP)
		req.Header.Set("Accept", "application/json")
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: %s %s: %w", method, full.Path, err)
		}
		raw, readErr := readLimited(resp.Body, agentMaxBody)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("gatewayapi: reading %s %s: %w", method, full.Path, readErr)
		}
		a.setNonce(origin, resp.Header.Get("DPoP-Nonce"))
		if attempt == 0 && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) &&
			resp.Header.Get("DPoP-Nonce") != "" {
			a.log.Debug("web_account_nonce_retry", "did", a.caller.DID, "path", full.Path)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &custodyError{Operation: method + " " + full.Path, Status: resp.StatusCode, Message: xrpcError(raw)}
		}
		return raw, nil
	}
	return nil, errors.New("gatewayapi: the PDS kept asking for a new DPoP nonce")
}

func readLimited(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("the body is too large")
	}
	return raw, nil
}

func xrpcError(raw []byte) string {
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		switch {
		case payload.Error != "" && payload.Message != "":
			return payload.Error + ": " + payload.Message
		case payload.Error != "":
			return payload.Error
		case payload.Message != "":
			return payload.Message
		}
	}
	return truncate(string(raw), 300)
}

func (a *custodyAgent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	query := url.Values{"aud": {aud}}
	if lxm != "" {
		query.Set("lxm", lxm)
	}
	raw, err := a.request(ctx, http.MethodGet, "/xrpc/com.atproto.server.getServiceAuth?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("gatewayapi: decoding getServiceAuth: %w", err)
	}
	if payload.Token == "" {
		return "", errors.New("gatewayapi: getServiceAuth returned no token")
	}
	return payload.Token, nil
}

func (a *custodyAgent) ListRecords(ctx context.Context, did, collection string, limit int) ([]coreapi.Record, error) {
	if limit <= 0 {
		limit = 50
	}
	out := make([]coreapi.Record, 0, limit)
	cursor := ""
	for len(out) < limit {
		page := limit - len(out)
		if page > agentPageMax {
			page = agentPageMax
		}
		query := url.Values{"repo": {did}, "collection": {collection}, "limit": {strconv.Itoa(page)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		raw, err := a.request(ctx, http.MethodGet, "/xrpc/com.atproto.repo.listRecords?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Records []struct {
				URI   string         `json:"uri"`
				CID   string         `json:"cid"`
				Value map[string]any `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("gatewayapi: decoding listRecords: %w", err)
		}
		for _, record := range payload.Records {
			out = append(out, coreapi.Record{URI: record.URI, CID: record.CID, Value: record.Value})
			if len(out) >= limit {
				break
			}
		}
		if payload.Cursor == "" || len(payload.Records) == 0 {
			break
		}
		cursor = payload.Cursor
	}
	return out, nil
}

func (a *custodyAgent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (coreapi.RecordRef, error) {
	body := map[string]any{"repo": did, "collection": collection, "rkey": rkey, "record": record}
	raw, err := a.request(ctx, http.MethodPost, "/xrpc/com.atproto.repo.createRecord", body)
	if err != nil {
		return coreapi.RecordRef{}, err
	}
	var payload struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return coreapi.RecordRef{}, fmt.Errorf("gatewayapi: decoding createRecord: %w", err)
	}
	if payload.URI == "" {
		return coreapi.RecordRef{}, errors.New("gatewayapi: createRecord returned no uri")
	}
	return coreapi.RecordRef{URI: payload.URI, CID: payload.CID}, nil
}

func (a *custodyAgent) ApplyWrites(ctx context.Context, did string, writes []coreapi.Write) error {
	encoded := make([]map[string]any, 0, len(writes))
	for _, write := range writes {
		entry := map[string]any{
			"$type":      "com.atproto.repo.applyWrites#" + write.Action,
			"collection": write.Collection,
			"rkey":       write.Rkey,
		}
		if write.Action != "delete" {
			entry["value"] = write.Record
		}
		encoded = append(encoded, entry)
	}
	body := map[string]any{"repo": did, "writes": encoded}
	_, err := a.request(ctx, http.MethodPost, "/xrpc/com.atproto.repo.applyWrites", body)
	return err
}
