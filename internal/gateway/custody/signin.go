package custody

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/networkdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	DefaultRole = "user"

	DefaultTimeout = 15 * time.Second
)

const (
	agentMaxBody = 8 << 20
	agentPageMax = 100
)

var _ gateway.WebSignIn = (*SignIn)(nil)

const (
	identityCacheEntries = 4096

	identityCacheTTL = time.Minute

	identityCacheMargin = 5 * time.Second

	identityActiveWindow = 2 * time.Minute

	identityRefreshBackoffMin = time.Second

	identityRefreshBackoffMax = 30 * time.Second
)

type SignIn struct {
	client *custodyClient

	pdsHTTP *http.Client

	role string

	renewBelow time.Duration

	renewIncrement time.Duration

	allowInternalHTTP bool

	log *slog.Logger

	mu sync.Mutex

	identities *ttlcache.Cache[[sha256.Size]byte, *custodyIdentityEntry]

	wake chan struct{}

	stopCh chan struct{}

	closed bool

	refreshing bool

	wg sync.WaitGroup
}

type custodyIdentityEntry struct {
	user gatewayhttp.WebUser

	token string

	leaseUntil time.Time

	checkUntil time.Time

	renewable bool

	renewDue bool

	lastSeen time.Time

	failures int

	nextTry time.Time
}

type Config struct {
	Endpoint Endpoint

	Role string

	RenewBelow time.Duration

	RenewIncrement time.Duration

	Network *v1alpha1.MarketNetworkValue
}

type Deps struct {
	TLS *tls.Config

	Log *slog.Logger
}

func New(cfg Config, deps Deps) (*SignIn, error) {
	client, err := newCustodyClient(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SignIn{
		client:            client,
		pdsHTTP:           &http.Client{Timeout: 30 * time.Second, Transport: pdsTransport(cfg.Network, deps.TLS)},
		role:              cmp.Or(cfg.Role, DefaultRole),
		renewBelow:        cfg.RenewBelow,
		renewIncrement:    cfg.RenewIncrement,
		allowInternalHTTP: cfg.Network != nil,
		log:               log,
		identities:        ttlcache.New[[sha256.Size]byte, *custodyIdentityEntry](ttlcache.Options{MaxEntries: identityCacheEntries}),
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

func (s *SignIn) ClientMetadata(ctx context.Context) (json.RawMessage, error) {
	return s.client.clientMetadata(ctx)
}

func (s *SignIn) Start(ctx context.Context, identifier, redirectURI string) (gatewayhttp.SignInStart, error) {
	authorizationURL, state, err := s.client.start(ctx, s.role, identifier, redirectURI)
	if err != nil {
		return gatewayhttp.SignInStart{}, err
	}
	return gatewayhttp.SignInStart{AuthorizationURL: authorizationURL, State: state}, nil
}

func (s *SignIn) Complete(ctx context.Context, params gatewayhttp.SignInCompleteParams) (gatewayhttp.SignInGrant, error) {
	login, err := s.client.complete(ctx, params.State, params.Code, params.ISS)
	if err != nil {
		return gatewayhttp.SignInGrant{}, err
	}
	return gatewayhttp.SignInGrant{
		WebUser:      gatewayhttp.WebUser{DID: login.DID, Handle: login.Handle, PDS: login.PDS},
		Token:        login.Token,
		LeaseSeconds: login.LeaseSeconds,
	}, nil
}

func (s *SignIn) Identify(ctx context.Context, token string) (*gatewayhttp.WebUser, error) {
	if token == "" {
		return nil, nil
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	if entry, ok := s.identities.Get(key); ok {
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
	return &gatewayhttp.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS}, nil
}

func identityCacheWindow(now time.Time, ttlSeconds int64) (time.Time, time.Time) {
	if ttlSeconds <= 0 {
		return time.Time{}, time.Time{}
	}
	lease := time.Duration(ttlSeconds) * time.Second
	leaseUntil := now.Add(lease)
	window := min(lease, identityCacheTTL)
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

func (s *SignIn) renewalDue(entry *custodyIdentityEntry, now time.Time) bool {
	return entry.renewable && s.renewBelow > 0 && !entry.leaseUntil.Add(-s.renewBelow).After(now)
}

func (s *SignIn) dueAt(entry *custodyIdentityEntry, now time.Time) time.Time {
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

func (s *SignIn) storeIdentity(key [sha256.Size]byte, token string, identity *custodyIdentity, now time.Time) {
	leaseUntil, checkUntil := identityCacheWindow(now, identity.TTLSeconds)
	if leaseUntil.IsZero() {
		return
	}
	s.mu.Lock()
	s.identities.Prune()
	entry := &custodyIdentityEntry{
		user:       gatewayhttp.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS},
		token:      token,
		leaseUntil: leaseUntil,
		checkUntil: checkUntil,
		renewable:  identity.Renewable,
		lastSeen:   now,
	}
	entry.renewDue = s.renewalDue(entry, now)
	s.identities.SetTTL(key, entry, leaseUntil.Sub(now))
	s.wakeRefresherLocked()
	s.mu.Unlock()
}

func (s *SignIn) forgetToken(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	s.identities.Delete(sha256.Sum256([]byte(token)))
	s.mu.Unlock()
}

func (s *SignIn) forgetKey(key [sha256.Size]byte, token string) {
	s.mu.Lock()
	if entry, ok := s.identities.Get(key); ok && entry.token == token {
		s.identities.Delete(key)
	}
	s.mu.Unlock()
}

func (s *SignIn) ensureRefresherLocked() {
	if s.closed || s.refreshing {
		return
	}
	s.refreshing = true
	s.wg.Go(s.refreshLoop)
}

func (s *SignIn) wakeRefresherLocked() {
	if s.closed {
		return
	}
	s.ensureRefresherLocked()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *SignIn) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stopCh)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *SignIn) refreshLoop() {
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
		s.identities.Range(func(key [sha256.Size]byte, entry *custodyIdentityEntry) bool {
			if now.Sub(entry.lastSeen) > identityActiveWindow {
				return true
			}
			active = true
			due := s.dueAt(entry, now)
			if !due.After(now) {
				pickKey = key
				pick = entry
				return false
			}
			if next.IsZero() || due.Before(next) {
				next = due
			}
			return true
		})
		if pick == nil {
			if !active {
				s.refreshing = false
				s.mu.Unlock()
				return
			}
			delay := max(next.Sub(now), time.Millisecond)
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

func (s *SignIn) refreshIdentity(key [sha256.Size]byte, token string, renew bool) {
	timeout := s.client.endpoint.Timeout
	timeout = opt.Positive(timeout, DefaultTimeout)
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

func (s *SignIn) refreshFailed(key [sha256.Size]byte, token string, err error) {
	now := time.Now()
	var custody *custodyError
	invalid := errors.As(err, &custody) && (custody.Status == http.StatusForbidden || custody.Status == http.StatusUnauthorized)
	s.mu.Lock()
	entry, ok := s.identities.Get(key)
	if !ok || entry.token != token {
		s.mu.Unlock()
		return
	}
	if invalid || !now.Before(entry.leaseUntil) {
		s.identities.Delete(key)
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

func (s *SignIn) refreshRenewed(key [sha256.Size]byte, token string, leaseSeconds int64) {
	now := time.Now()
	leaseUntil, checkUntil := identityCacheWindow(now, leaseSeconds)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.identities.Get(key)
	if !ok || entry.token != token {
		return
	}
	if leaseUntil.IsZero() {
		s.identities.Delete(key)
		return
	}
	entry.leaseUntil = leaseUntil
	entry.checkUntil = checkUntil
	entry.failures = 0
	entry.nextTry = time.Time{}
	entry.renewDue = s.renewalDue(entry, now)
	s.identities.SetTTL(key, entry, leaseUntil.Sub(now))
}

func (s *SignIn) refreshIdentified(key [sha256.Size]byte, token string, identity *custodyIdentity) {
	now := time.Now()
	leaseUntil, checkUntil := identityCacheWindow(now, identity.TTLSeconds)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.identities.Get(key)
	if !ok || entry.token != token {
		return
	}
	if leaseUntil.IsZero() {
		s.identities.Delete(key)
		return
	}
	entry.user = gatewayhttp.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS}
	entry.leaseUntil = leaseUntil
	entry.checkUntil = checkUntil
	entry.renewable = identity.Renewable
	entry.failures = 0
	entry.nextTry = time.Time{}
	entry.renewDue = s.renewalDue(entry, now)
	s.identities.SetTTL(key, entry, leaseUntil.Sub(now))
}

func (s *SignIn) SignOut(ctx context.Context, caller *gatewayhttp.WebCaller) (gatewayhttp.SignOutResult, error) {
	if caller == nil {
		return gatewayhttp.SignOutResult{}, errors.New("gatewayapi: no caller to sign out")
	}
	s.forgetToken(caller.Token)
	revoked, revokeError, signOutErr := s.client.signOut(ctx, caller.Token, caller.DID)
	if err := s.client.revokeToken(ctx, caller.Token); err != nil {
		s.log.Warn("web_token_revoke_failed", "did", caller.DID, "err", err)
	}
	if signOutErr != nil {
		return gatewayhttp.SignOutResult{}, signOutErr
	}
	return gatewayhttp.SignOutResult{Revoked: revoked, RevokeError: revokeError}, nil
}

func (s *SignIn) AgentFor(caller *gatewayhttp.WebCaller) gatewayhttp.WebAccountAgent {
	if caller == nil {
		return nil
	}
	agent := &custodyAgent{client: s.client, http: s.pdsHTTP, caller: caller, allowInternalHTTP: s.allowInternalHTTP,
		renewIncrement: s.renewIncrement, log: s.log}
	agent.auth = &xrpc.DPoP{Tokens: &custodySession{agent: agent}, Prover: xrpc.DPoPProverFunc(agent.prove)}
	return agent
}

type custodyAgent struct {
	client *custodyClient

	http *http.Client

	caller *gatewayhttp.WebCaller

	allowInternalHTTP bool

	renewIncrement time.Duration

	log *slog.Logger

	auth *xrpc.DPoP
}

func (a *custodyAgent) prove(ctx context.Context, method, target, _, nonce string) (xrpc.DPoPProof, error) {
	proof, err := a.client.proof(ctx, a.caller.Token, a.caller.DID, method, target, nonce)
	if err != nil {
		return xrpc.DPoPProof{}, err
	}
	return xrpc.DPoPProof{AccessToken: proof.AccessToken, Proof: proof.DPoP}, nil
}

type custodySession struct {
	agent *custodyAgent

	stale atomic.Bool
}

func (s *custodySession) Get(ctx context.Context) (string, error) {
	a := s.agent
	if s.stale.Swap(false) {
		if _, err := a.client.renew(ctx, a.caller.Token, a.renewIncrement); err != nil {
			a.log.Debug("web_account_renew_failed", "did", a.caller.DID, "err", err.Error())
		}
		a.log.Debug("web_account_token_retry", "did", a.caller.DID)
	}
	return "", nil
}

func (s *custodySession) Invalidate() { s.stale.Store(true) }

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
	if u.Scheme == "http" && !LoopbackHost(u.Hostname()) && !(a.allowInternalHTTP && hostport.IsInternalKCP(u.Hostname())) {
		return nil, fmt.Errorf("gatewayapi: the account PDS URL %q is plain http on a non-loopback host", raw)
	}
	return u, nil
}

func (a *custodyAgent) xrpcClient() (*xrpc.Client, error) {
	base, err := a.pds()
	if err != nil {
		return nil, err
	}
	return &xrpc.Client{Host: base.Scheme + "://" + base.Host, HTTP: a.http, Auth: a.auth, Limit: agentMaxBody}, nil
}

func custodyXRPCError(method, nsid string, err error) error {
	if xerr, ok := errors.AsType[*xrpc.Error](err); ok {
		return &custodyError{Operation: method + " " + xrpc.Prefix + nsid, Status: xerr.Status, Message: xerr.Detail()}
	}
	return err
}

func custodyQuery[Out any](ctx context.Context, a *custodyAgent, nsid string, params url.Values) (Out, error) {
	client, err := a.xrpcClient()
	if err != nil {
		var out Out
		return out, err
	}
	out, err := xrpc.Query[Out](ctx, client, nsid, params)
	return out, custodyXRPCError(http.MethodGet, nsid, err)
}

func custodyProcedure[Out any](ctx context.Context, a *custodyAgent, nsid string, body any) (Out, error) {
	client, err := a.xrpcClient()
	if err != nil {
		var out Out
		return out, err
	}
	out, err := xrpc.Procedure[any, Out](ctx, client, nsid, body)
	return out, custodyXRPCError(http.MethodPost, nsid, err)
}

func (a *custodyAgent) GetServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	client, err := a.xrpcClient()
	if err != nil {
		return "", err
	}
	token, err := xrpc.GetServiceAuth(ctx, client, aud, lxm, time.Time{})
	return token, custodyXRPCError(http.MethodGet, xrpc.ServiceAuthNSID, err)
}

func (a *custodyAgent) ListRecords(ctx context.Context, did, collection string, limit int) ([]gatewayhttp.Record, error) {
	limit = opt.Positive(limit, 50)
	out := make([]gatewayhttp.Record, 0, limit)
	cursor := ""
	for len(out) < limit {
		page := min(limit-len(out), agentPageMax)
		query := url.Values{"repo": {did}, "collection": {collection}, "limit": {strconv.Itoa(page)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		payload, err := custodyQuery[struct {
			Records []struct {
				URI   string         `json:"uri"`
				CID   string         `json:"cid"`
				Value map[string]any `json:"value"`
			} `json:"records"`
			Cursor string `json:"cursor"`
		}](ctx, a, "com.atproto.repo.listRecords", query)
		if err != nil {
			return nil, err
		}
		for _, record := range payload.Records {
			out = append(out, gatewayhttp.Record{URI: record.URI, CID: record.CID, Value: record.Value})
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

func (a *custodyAgent) CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (gatewayhttp.RecordRef, error) {
	body := map[string]any{"repo": did, "collection": collection, "rkey": rkey, "record": record}
	payload, err := custodyProcedure[struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}](ctx, a, "com.atproto.repo.createRecord", body)
	if err != nil {
		return gatewayhttp.RecordRef{}, err
	}
	if payload.URI == "" {
		return gatewayhttp.RecordRef{}, errors.New("gatewayapi: createRecord returned no uri")
	}
	return gatewayhttp.RecordRef{URI: payload.URI, CID: payload.CID}, nil
}

func (a *custodyAgent) ApplyWrites(ctx context.Context, did string, writes []gatewayhttp.Write) error {
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
	_, err := custodyProcedure[struct{}](ctx, a, "com.atproto.repo.applyWrites", body)
	return err
}
