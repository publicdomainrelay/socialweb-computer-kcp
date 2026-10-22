package gatewayapi

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
	"strconv"
	"strings"
	"sync"
	"time"

	coreapi "github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
)

const (
	agentMaxBody = 8 << 20
	agentPageMax = 100
)

type custodySignIn struct {
	client *custodyClient

	pdsHTTP *http.Client

	role string

	renewBelow time.Duration

	renewIncrement time.Duration

	log *slog.Logger
}

type custodySignInOptions struct {
	Endpoint custodyEndpoint

	Role string

	RenewBelow time.Duration

	RenewIncrement time.Duration

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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &custodySignIn{
		client:         client,
		pdsHTTP:        &http.Client{Timeout: 30 * time.Second, Transport: transport},
		role:           opts.Role,
		renewBelow:     opts.RenewBelow,
		renewIncrement: opts.RenewIncrement,
		log:            opts.Log,
	}, nil
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
	if identity.Renewable && s.renewBelow > 0 && time.Duration(identity.TTLSeconds)*time.Second < s.renewBelow {
		if _, err := s.client.renew(ctx, token, s.renewIncrement); err != nil {
			s.log.Warn("web_token_renew_failed", "did", identity.DID, "err", err)
		}
	}
	return &coreapi.WebUser{DID: identity.DID, Handle: identity.Handle, PDS: identity.PDS}, nil
}

func (s *custodySignIn) SignOut(ctx context.Context, caller *coreapi.WebCaller) (coreapi.SignOutResult, error) {
	if caller == nil {
		return coreapi.SignOutResult{}, errors.New("gatewayapi: no caller to sign out")
	}
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
	return &custodyAgent{client: s.client, http: s.pdsHTTP, caller: caller, log: s.log, nonces: map[string]string{}}
}

type custodyAgent struct {
	client *custodyClient

	http *http.Client

	caller *coreapi.WebCaller

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
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("gatewayapi: the account PDS URL %q is plain http on a non-loopback host", raw)
	}
	return u, nil
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
