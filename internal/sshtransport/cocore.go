package sshtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	DevicePairStartNSID = "dev.cocore.devicePair.start"

	DevicePairPollNSID = "dev.cocore.devicePair.poll"

	CocoreConfigPathVM = "/root/.pi/agent/cocore-config.json"

	DefaultCocorePairingTTL = 10 * time.Minute
)

type CocoreToken struct {
	Token string `json:"token"`

	APIBase string `json:"apiBase,omitempty"`

	AccountDID string `json:"accountDid,omitempty"`

	PairedAt string `json:"pairedAt"`
}

type CocoreTokenStore interface {
	Get(ctx context.Context, did string) (*CocoreToken, error)

	Set(ctx context.Context, did string, token *CocoreToken) error
}

type FileCocoreTokenStore struct {
	Path string

	Log *slog.Logger

	mu sync.Mutex

	loaded bool

	tokens map[string]CocoreToken
}

func NewFileCocoreTokenStore(path string, log *slog.Logger) *FileCocoreTokenStore {
	if log == nil {
		log = slog.Default()
	}
	return &FileCocoreTokenStore{Path: path, Log: log, tokens: map[string]CocoreToken{}}
}

func (s *FileCocoreTokenStore) loadLocked() error {
	if s.loaded {
		return nil
	}
	if s.Path == "" {
		return errors.New("sshtransport: no cocore token store path")
	}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.tokens); err != nil {
		quarantine := s.Path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
		if renameErr := os.Rename(s.Path, quarantine); renameErr != nil {
			return err
		}
		s.Log.Error("cocore_token_store_corrupt", "path", s.Path, "quarantine", quarantine, "err", err)
		s.tokens = map[string]CocoreToken{}
	}
	s.loaded = true
	return nil
}

func (s *FileCocoreTokenStore) Get(ctx context.Context, did string) (*CocoreToken, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	token, ok := s.tokens[did]
	if !ok {
		return nil, nil
	}
	return &token, nil
}

func (s *FileCocoreTokenStore) Set(ctx context.Context, did string, token *CocoreToken) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.tokens[did] = *token
	raw, err := json.Marshal(s.tokens)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	temp := s.Path + ".tmp-" + randomHex(6)
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, s.Path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

type CocorePairingStarted struct {
	PairID string `json:"pairId"`

	UserCode string `json:"userCode"`

	VerificationURI string `json:"verificationUri"`

	IntervalSecs int `json:"intervalSecs"`
}

type CocorePairingOptions struct {
	APIBaseURL string

	Tokens CocoreTokenStore

	AppName string

	AppDID string

	KeyName string

	ReturnURL string

	TTL time.Duration

	Client *http.Client

	Log *slog.Logger
}

type pendingPair struct {
	did string

	deviceID string
}

type CocorePairing struct {
	opts CocorePairingOptions

	base string

	log *slog.Logger

	pending *ttlcache.Cache[string, pendingPair]
}

func NewCocorePairing(opts CocorePairingOptions) (*CocorePairing, error) {
	if opts.APIBaseURL == "" {
		return nil, errors.New("sshtransport: cocore pairing has no API base URL")
	}
	if opts.Tokens == nil {
		return nil, errors.New("sshtransport: cocore pairing has no token store")
	}
	opts.TTL = opt.Positive(opts.TTL, DefaultCocorePairingTTL)
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &CocorePairing{
		opts:    opts,
		base:    strings.TrimRight(opts.APIBaseURL, "/"),
		log:     opts.Log,
		pending: ttlcache.New[string, pendingPair](ttlcache.Options{}),
	}, nil
}

func (p *CocorePairing) client() *http.Client {
	if p.opts.Client != nil {
		return p.opts.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (p *CocorePairing) do(ctx context.Context, method, target string, body any) (map[string]any, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var out map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out, resp.StatusCode, nil
}

func (p *CocorePairing) Start(ctx context.Context, did string) (CocorePairingStarted, error) {
	body, status, err := p.do(ctx, http.MethodPost, p.base+"/xrpc/"+DevicePairStartNSID, map[string]any{
		"appName":   p.opts.AppName,
		"appDid":    p.opts.AppDID,
		"keyName":   p.opts.KeyName,
		"returnUrl": p.opts.ReturnURL,
	})
	if err != nil {
		return CocorePairingStarted{}, fmt.Errorf("cocore pairing could not start: %w", err)
	}
	if status < 200 || status > 299 || body == nil {
		detail := ""
		if body != nil {
			if message, ok := body["message"].(string); ok && message != "" {
				detail = message
			} else if code, ok := body["error"].(string); ok && code != "" {
				detail = code
			}
		}
		if detail == "" {
			detail = "HTTP " + strconv.Itoa(status)
		}
		return CocorePairingStarted{}, errors.New("cocore pairing could not start: " + detail)
	}
	deviceID, _ := body["deviceId"].(string)
	userCode, _ := body["userCode"].(string)
	verificationURI, _ := body["verificationUri"].(string)
	if deviceID == "" || verificationURI == "" {
		return CocorePairingStarted{}, errors.New("cocore pairing response carried no deviceId or verificationUri")
	}
	expires := p.opts.TTL
	if seconds, ok := numberValue(body["expiresInSecs"]); ok && seconds > 0 {
		expires = time.Duration(seconds * float64(time.Second))
	}
	interval := 5
	if seconds, ok := numberValue(body["pollIntervalSecs"]); ok && seconds > 0 {
		interval = int(seconds)
	}
	pairID := randomHex(16)
	p.pending.SetTTL(pairID, pendingPair{did: did, deviceID: deviceID}, expires)
	p.log.Info("cocore_pairing_started", "did", did, "hasUserCode", userCode != "")
	return CocorePairingStarted{
		PairID: pairID, UserCode: userCode, VerificationURI: verificationURI, IntervalSecs: interval,
	}, nil
}

func (p *CocorePairing) Poll(ctx context.Context, pairID string) (string, error) {
	entry, ok := p.pending.Get(pairID)
	if !ok {
		return "expired", nil
	}
	target := p.base + "/xrpc/" + DevicePairPollNSID + "?deviceId=" + url.QueryEscape(entry.deviceID)
	body, status, err := p.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		p.log.Warn("cocore_pairing_poll_failed", "err", err)
		return "pending", nil
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		p.pending.Delete(pairID)
		return "expired", nil
	}
	if status < 200 || status > 299 || body == nil {
		p.log.Warn("cocore_pairing_poll_failed", "status", status)
		return "pending", nil
	}
	state, _ := body["status"].(string)
	switch state {
	case "denied":
		p.pending.Delete(pairID)
		p.log.Info("cocore_pairing_denied", "did", entry.did)
		return "denied", nil
	case "expired":
		p.pending.Delete(pairID)
		return "expired", nil
	}
	session, _ := body["session"].(map[string]any)
	if session == nil {
		return "pending", nil
	}
	token, ok := sessionKey(session)
	if !ok {
		keys := make([]string, 0, len(session))
		for key := range session {
			keys = append(keys, key)
		}
		p.log.Warn("cocore_pairing_no_key_in_session", "keys", strings.Join(keys, ","))
		return "pending", nil
	}
	stored := &CocoreToken{
		Token:    token,
		PairedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if apiBase, ok := session["apiBase"].(string); ok {
		stored.APIBase = apiBase
	}
	if did, ok := session["did"].(string); ok {
		stored.AccountDID = did
	}
	if err := p.opts.Tokens.Set(ctx, entry.did, stored); err != nil {
		return "", err
	}
	p.pending.Delete(pairID)
	p.log.Info("cocore_paired", "did", entry.did)
	return "complete", nil
}

func (p *CocorePairing) Token(ctx context.Context, did string) (*CocoreToken, error) {
	return p.opts.Tokens.Get(ctx, did)
}

func sessionKey(session map[string]any) (string, bool) {
	for _, name := range []string{"apiKey", "token", "secret", "key"} {
		if value, ok := session[name].(string); ok && value != "" {
			return value, true
		}
	}
	return "", false
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

type GuestSecret struct {
	Path string

	Value string
}

func CocoreSecretEntry(token string) GuestSecret {
	raw, _ := json.Marshal(map[string]string{"apiKey": token})
	return GuestSecret{Path: CocoreConfigPathVM, Value: string(raw)}
}

type CocoreOptions struct {
	Pairing *CocorePairing

	Auth Authenticator

	Log *slog.Logger
}

type Cocore struct {
	opts CocoreOptions

	log *slog.Logger
}

func NewCocoreAPI(opts CocoreOptions) (*Cocore, error) {
	if opts.Pairing == nil {
		return nil, errors.New("sshtransport: cocore API has no pairing")
	}
	if opts.Auth == nil {
		return nil, errors.New("sshtransport: cocore API has no authenticator")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Cocore{opts: opts, log: opts.Log}, nil
}

func (c *Cocore) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /cocore/status", c.status)
	mux.HandleFunc("POST /cocore/pair/start", c.start)
	mux.HandleFunc("POST /cocore/pair/poll", c.poll)
}

func (c *Cocore) Handler() http.Handler {
	mux := http.NewServeMux()
	c.Register(mux)
	return mux
}

func (c *Cocore) caller(w http.ResponseWriter, r *http.Request) (*gatewayhttp.WebCaller, bool) {
	who, err := c.opts.Auth.Authenticate(r)
	if err != nil {
		c.log.Error("cocore_identify_failed", "err", err)
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"error": "the vault is not answering"})
		return nil, false
	}
	if who == nil {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return nil, false
	}
	return who, true
}

func (c *Cocore) status(w http.ResponseWriter, r *http.Request) {
	who, ok := c.caller(w, r)
	if !ok {
		return
	}
	token, err := c.opts.Pairing.Token(r.Context(), who.DID)
	if err != nil {
		c.log.Error("cocore_status_failed", "did", who.DID, "err", err)
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var accountDID any
	var pairedAt any
	if token != nil {
		if token.AccountDID != "" {
			accountDID = token.AccountDID
		}
		pairedAt = token.PairedAt
	}
	httpjson.Write(w, http.StatusOK, struct {
		Paired     bool `json:"paired"`
		AccountDID any  `json:"accountDid"`
		PairedAt   any  `json:"pairedAt"`
	}{token != nil, accountDID, pairedAt})

}

func (c *Cocore) start(w http.ResponseWriter, r *http.Request) {
	who, ok := c.caller(w, r)
	if !ok {
		return
	}
	started, err := c.opts.Pairing.Start(r.Context(), who.DID)
	if err != nil {
		c.log.Error("cocore_pairing_start_failed", "did", who.DID, "err", err)
		httpjson.Write(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	httpjson.Write(w, http.StatusOK, started)
}

func (c *Cocore) poll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PairID any `json:"pairId"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err == nil {
		_ = json.Unmarshal(raw, &body)
	}
	pairID, _ := body.PairID.(string)
	if pairID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "expected a pairId"})
		return
	}
	status, err := c.opts.Pairing.Poll(r.Context(), pairID)
	if err != nil {
		c.log.Error("cocore_pairing_poll_failed", "err", err)
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"status": status})
}
