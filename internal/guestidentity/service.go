package guestidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultHostKeyAttempts = 24

	DefaultHostKeyRetry = 5 * time.Second

	oidcDocumentRefresh = 5 * time.Minute

	oidcDocumentRetry = time.Second

	oidcDocumentRetryMax = 30 * time.Second
)

type VMStore = computevm.GuestStore

type OnNetworkReport struct {
	AcceptURI string

	AcceptCID string

	Address string

	CreatedAt string
}

type GuestOptions struct {
	Handle string

	Identity *Client

	Verifier *Verifier

	VMs VMStore

	Log *slog.Logger

	LoginLimits *TokenBuckets

	TokenLimits *TokenBuckets

	PublicLimits *TokenBuckets

	Now func() time.Time

	HostKeyAttempts int

	HostKeyRetry time.Duration
}

type Guest struct {
	handle   string
	identity *Client
	verifier *Verifier
	recorder *computevm.GuestRecord
	log      *slog.Logger
	now      func() time.Time

	loginLimits  *TokenBuckets
	tokenLimits  *TokenBuckets
	publicLimits *TokenBuckets

	hostKeyAttempts int
	hostKeyRetry    time.Duration

	discovery oidcCache
	keys      oidcCache

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func NewGuest(ctx context.Context, opts GuestOptions) (*Guest, error) {
	switch {
	case opts.Handle == "":
		return nil, errors.New("guestidentity: the guest control plane names no bidder handle")
	case opts.Identity == nil:
		return nil, errors.New("guestidentity: the guest control plane has no OpenBao identity client")
	case opts.Verifier == nil:
		return nil, errors.New("guestidentity: the guest control plane has no token verifier")
	case opts.VMs == nil:
		return nil, errors.New("guestidentity: the guest control plane has no ComputeVM store")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LoginLimits == nil {
		opts.LoginLimits = LoginRateLimits()
	}
	if opts.TokenLimits == nil {
		opts.TokenLimits = OIDCTokenRateLimits()
	}
	if opts.PublicLimits == nil {
		opts.PublicLimits = PublicRateLimits()
	}
	opts.HostKeyAttempts = opt.Positive(opts.HostKeyAttempts, DefaultHostKeyAttempts)
	opts.HostKeyRetry = opt.Positive(opts.HostKeyRetry, DefaultHostKeyRetry)
	root, cancel := context.WithCancel(ctx)
	g := &Guest{
		handle: opts.Handle, identity: opts.Identity, verifier: opts.Verifier,
		recorder: &computevm.GuestRecord{Store: opts.VMs, Handle: "", Log: opts.Log, Now: opts.Now},
		log:      opts.Log, now: opts.Now,
		loginLimits: opts.LoginLimits, tokenLimits: opts.TokenLimits, publicLimits: opts.PublicLimits,
		hostKeyAttempts: opts.HostKeyAttempts, hostKeyRetry: opts.HostKeyRetry,
		ctx: root, cancel: cancel,
	}
	g.wg.Go(func() {
		g.refreshOIDCDocuments()
	})
	return g, nil
}

func IdentityKeys(identity *Client) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		if identity == nil {
			return nil, errors.New("guestidentity: the verifier has no OpenBao identity client")
		}
		reply, err := identity.Keys(ctx)
		if err != nil {
			return nil, err
		}
		if !reply.OK() {
			return nil, fmt.Errorf("guestidentity: the OIDC keys returned HTTP %d", reply.Status)
		}
		return reply.Body, nil
	}
}

func (g *Guest) Close() {
	g.once.Do(func() {
		g.cancel()
		g.wg.Wait()
	})
}

func (g *Guest) LoginStep(ctx context.Context, step string, input map[string]any) Reply {
	parsed, err := ParseLoginStep(step)
	if err != nil {
		return jsonReply(404, map[string]any{"errors": []string{"no such step"}})
	}
	body, err := LoginBody(parsed, input)
	if err != nil {
		return jsonReply(400, map[string]any{"errors": []string{err.Error()}})
	}
	if !g.loginLimits.Take(body["role"], g.now()) {
		return jsonReply(429, map[string]any{"errors": []string{"too many requests"}})
	}
	reply, err := g.identity.Step(ctx, parsed, body)
	if err != nil {
		g.log.Warn("the guest login step failed", "step", parsed, "role", body["role"], "err", err.Error())
		return jsonReply(502, map[string]any{"errors": []string{err.Error()}})
	}
	g.log.Info("guest login step", "step", parsed, "role", body["role"], "status", reply.Status)
	if parsed == LoginBind && reply.OK() {
		g.recordBoundHostKeys(body["role"])
	}
	return normalized(reply)
}

func (g *Guest) OIDCToken(ctx context.Context, role, vaultToken string) Reply {
	if !ValidRole(role) {
		return jsonReply(404, map[string]any{"errors": []string{"no such role"}})
	}
	if vaultToken == "" || len(vaultToken) > 512 {
		return jsonReply(403, map[string]any{"errors": []string{"missing token"}})
	}
	key := vaultToken
	if len(key) > 24 {
		key = key[len(key)-24:]
	}
	if !g.tokenLimits.Take(key, g.now()) {
		return jsonReply(429, map[string]any{"errors": []string{"too many requests"}})
	}
	reply, err := g.identity.Token(ctx, role, vaultToken)
	if err != nil {
		g.log.Warn("the guest OIDC token exchange failed", "role", role, "err", err.Error())
		return jsonReply(502, map[string]any{"errors": []string{err.Error()}})
	}
	if reply.OK() {
		if err := g.refreshOIDCDocument(ctx, "keys", &g.keys, g.identity.Keys); err == nil {
			g.log.Info("the guest OIDC keys are published after a token exchange", "role", role)
		}
	}
	g.log.Info("guest OIDC token", "role", role, "status", reply.Status)
	return normalized(reply)
}

func (g *Guest) Discovery(ctx context.Context) Reply {
	if !g.publicLimits.Take("discovery", g.now()) {
		return jsonReply(429, map[string]any{"errors": []string{"too many requests"}})
	}
	return g.serveOIDCDocument(ctx, "discovery", &g.discovery, g.identity.Discovery)
}

func (g *Guest) Keys(ctx context.Context) Reply {
	if !g.publicLimits.Take("keys", g.now()) {
		return jsonReply(429, map[string]any{"errors": []string{"too many requests"}})
	}
	return g.serveOIDCDocument(ctx, "keys", &g.keys, g.identity.Keys)
}

func (g *Guest) OnNetwork(ctx context.Context, authorization string, report OnNetworkReport) Reply {
	token, ok := strings.CutPrefix(strings.TrimSpace(authorization), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return jsonReply(401, map[string]any{"error": "AuthenticationRequired"})
	}
	claims, err := g.verifier.Verify(ctx, strings.TrimSpace(token))
	if err != nil {
		g.log.Warn("guest.onNetwork: the token is refused", "err", err.Error())
		return jsonReply(401, map[string]any{"error": "InvalidToken"})
	}
	if report.AcceptURI == "" || report.AcceptCID == "" || strings.TrimSpace(report.Address) == "" {
		return jsonReply(400, map[string]any{"error": "InvalidRequest", "message": "acceptUri, acceptCid and address are required"})
	}
	err = g.recorder.OnNetwork(ctx, claims.ComputeVM, report.AcceptURI, report.AcceptCID, report.Address, report.CreatedAt)
	switch {
	case err == nil:
		g.log.Info("guest.onNetwork recorded on the ComputeVM", "computevm", claims.ComputeVM,
			"address", strings.TrimSpace(report.Address))
		return jsonReply(200, map[string]any{"ok": true, "name": claims.ComputeVM})
	case errors.Is(err, computevm.ErrUnknownGuest):
		g.log.Warn("guest.onNetwork: no live ComputeVM holds this token", "computevm", claims.ComputeVM, "acceptUri", report.AcceptURI)
		return jsonReply(404, map[string]any{"error": "UnknownGuest"})
	case errors.Is(err, computevm.ErrUnknownAccept):
		g.log.Warn("guest.onNetwork: the accept is not this guest's", "computevm", claims.ComputeVM, "acceptUri", report.AcceptURI)
		return jsonReply(404, map[string]any{"error": "UnknownAccept"})
	case errors.Is(err, computevm.ErrReportRefused):
		return jsonReply(400, map[string]any{"error": "InvalidRequest", "message": err.Error()})
	default:
		g.log.Warn("guest.onNetwork: the report could not be recorded", "computevm", claims.ComputeVM, "err", err.Error())
		return jsonReply(503, map[string]any{"error": "Unavailable", "message": "the report could not be recorded on the ComputeVM"})
	}
}

func (g *Guest) RecordBoundHostKey(ctx context.Context, computevmName string) (bool, error) {
	o, err := g.recorder.Live(ctx, computevmName)
	if errors.Is(err, computevm.ErrUnknownGuest) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if held := computevm.GuestReportOf(o); held != nil && len(held.HostKeys) > 0 {
		return true, nil
	}
	line, err := g.identity.BoundHostKey(ctx, computevmName)
	if err != nil {
		return false, err
	}
	if line == "" {
		return false, nil
	}
	key, err := computevm.ParseHostKey(line)
	if err != nil {
		return false, err
	}
	if err := g.recorder.HostKeys(ctx, computevmName, []v1alpha1.ComputeVMHostKey{key}); err != nil {
		if errors.Is(err, computevm.ErrUnknownGuest) {
			return false, nil
		}
		return false, err
	}
	g.log.Info("the guest's bound host key is recorded", "computevm", computevmName, "type", key.Type)
	return true, nil
}

func (g *Guest) recordBoundHostKeys(computevmName string) {
	g.wg.Go(func() {
		for attempt := 0; attempt < g.hostKeyAttempts; attempt++ {
			done, err := g.RecordBoundHostKey(g.ctx, computevmName)
			if done {
				return
			}
			if err != nil {
				g.log.Warn("recording the guest's bound host key failed", "computevm", computevmName, "err", err.Error())
			}
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(g.hostKeyRetry):
			}
		}
	})
}

type oidcCache struct {
	mu      sync.RWMutex
	reply   Reply
	held    bool
	fetchMu sync.Mutex
}

func (c *oidcCache) get() (Reply, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reply, c.held
}

func (c *oidcCache) put(reply Reply) {
	c.mu.Lock()
	c.reply = reply
	c.held = true
	c.mu.Unlock()
}

func (g *Guest) serveOIDCDocument(ctx context.Context, name string, cache *oidcCache, fetch func(context.Context) (Reply, error)) Reply {
	if reply, ok := cache.get(); ok {
		return normalized(reply)
	}
	cache.fetchMu.Lock()
	defer cache.fetchMu.Unlock()
	if reply, ok := cache.get(); ok {
		return normalized(reply)
	}
	reply, err := fetch(ctx)
	if err != nil {
		g.log.Warn("the guest OIDC document fetch failed", "document", name, "err", err.Error())
		return jsonReply(502, map[string]any{"errors": []string{err.Error()}})
	}
	if reply.OK() && (name != "keys" || oidcKeysPublished(reply)) {
		cache.put(normalized(reply))
	}
	if name == "keys" && !oidcKeysPublished(reply) {
		g.log.Warn("the guest OIDC keys document is empty; retrying in the background", "document", name)
		return jsonReply(502, map[string]any{"errors": []string{"no OIDC key is published yet"}})
	}
	return normalized(reply)
}

func oidcKeysPublished(reply Reply) bool {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(reply.Body, &doc); err != nil {
		return false
	}
	return len(doc.Keys) > 0
}

func (g *Guest) refreshOIDCDocuments() {
	delay := time.Duration(0)
	for {
		if delay > 0 {
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		if g.ctx.Err() != nil {
			return
		}
		if err := g.identity.refreshEndpoint(g.ctx); err != nil {
			g.log.Warn("resolving the OpenBao endpoint for the guest OIDC documents failed", "err", err.Error())
		}
		discoveryErr := g.refreshOIDCDocument(g.ctx, "discovery", &g.discovery, g.identity.Discovery)
		keysErr := g.refreshOIDCDocument(g.ctx, "keys", &g.keys, g.identity.Keys)
		if discoveryErr == nil && keysErr == nil {
			delay = oidcDocumentRefresh
			continue
		}
		switch {
		case delay <= 0 || delay >= oidcDocumentRefresh:
			delay = oidcDocumentRetry
		case delay >= oidcDocumentRetryMax:
			delay = oidcDocumentRetryMax
		default:
			delay *= 2
			if delay > oidcDocumentRetryMax {
				delay = oidcDocumentRetryMax
			}
		}
	}
}

func (g *Guest) refreshOIDCDocument(ctx context.Context, name string, cache *oidcCache, fetch func(context.Context) (Reply, error)) error {
	reply, err := fetch(ctx)
	if err != nil {
		g.oidcRefreshWarning(name, cache, err)
		return err
	}
	if !reply.OK() {
		err := fmt.Errorf("the guest OIDC %s document returned HTTP %d", name, reply.Status)
		g.oidcRefreshWarning(name, cache, err)
		return err
	}
	if name == "keys" && !oidcKeysPublished(reply) {
		err := errors.New("the guest OIDC keys document is empty")
		g.oidcRefreshWarning(name, cache, err)
		return err
	}
	cache.put(normalized(reply))
	return nil
}

func (g *Guest) oidcRefreshWarning(name string, cache *oidcCache, err error) {
	if _, held := cache.get(); held {
		g.log.Warn("refreshing the guest OIDC document failed; serving the last known document", "document", name, "err", err.Error())
		return
	}
	g.log.Warn("refreshing the guest OIDC document failed; no document is cached yet", "document", name, "err", err.Error())
}

func jsonReply(status int, body any) Reply {
	raw, err := json.Marshal(body)
	if err != nil {
		return Reply{Status: 500, Body: json.RawMessage(`{"error":"InternalError"}`)}
	}
	return Reply{Status: status, Body: raw}
}

func normalized(reply Reply) Reply {
	if len(reply.Body) == 0 {
		reply.Body = json.RawMessage(`{}`)
	}
	return reply
}
