package oauthclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

type PendingAuth struct {
	State string

	CodeVerifier string

	RedirectURI string

	ClientID string

	HostURL string

	Issuer string

	AuthServerURL string

	TokenEndpoint string

	Scopes []string

	DPoPPrivateKeyMultibase string
}

type StateStore interface {
	Put(ctx context.Context, pending PendingAuth) error

	Take(ctx context.Context, state string) (PendingAuth, bool, error)
}

type MemoryStateStore struct {
	mu sync.Mutex

	pending map[string]PendingAuth
}

func (m *MemoryStateStore) Put(ctx context.Context, pending PendingAuth) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pending.State == "" {
		return errors.New("oauthclient: the oauth callback state is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = map[string]PendingAuth{}
	}
	m.pending[pending.State] = pending
	return nil
}

func (m *MemoryStateStore) Take(ctx context.Context, state string) (PendingAuth, bool, error) {
	if err := ctx.Err(); err != nil {
		return PendingAuth{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.pending[state]
	if ok {
		delete(m.pending, state)
	}
	return pending, ok, nil
}

type CallbackRequest struct {
	Code string

	State string

	Issuer string

	Error string

	ErrorDescription string
}

func ParseCallback(r *http.Request) (CallbackRequest, error) {
	if r == nil {
		return CallbackRequest{}, errors.New("oauthclient: no oauth callback request")
	}
	query := r.URL.Query()
	callback := CallbackRequest{
		Code:             query.Get("code"),
		State:            query.Get("state"),
		Issuer:           query.Get("iss"),
		Error:            query.Get("error"),
		ErrorDescription: query.Get("error_description"),
	}
	if callback.Error != "" {
		if callback.ErrorDescription != "" {
			return callback, fmt.Errorf("oauthclient: the oauth callback failed: %s: %s", callback.Error, callback.ErrorDescription)
		}
		return callback, fmt.Errorf("oauthclient: the oauth callback failed: %s", callback.Error)
	}
	if callback.Code == "" {
		return callback, errors.New("oauthclient: the oauth callback names no code")
	}
	if callback.State == "" {
		return callback, errors.New("oauthclient: the oauth callback names no state")
	}
	return callback, nil
}

func NewCallbackState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func GeneratePKCE() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func ExchangeCallback(ctx context.Context, store StateStore, r *http.Request, client *http.Client) (SessionData, error) {
	callback, err := ParseCallback(r)
	if err != nil {
		return SessionData{}, err
	}
	if store == nil {
		return SessionData{}, errors.New("oauthclient: no oauth callback state store")
	}
	pending, ok, err := store.Take(ctx, callback.State)
	if err != nil {
		return SessionData{}, err
	}
	if !ok {
		return SessionData{}, ErrStateNotFound
	}
	if pending.Issuer != "" && callback.Issuer != "" && pending.Issuer != callback.Issuer {
		return SessionData{}, fmt.Errorf("oauthclient: the oauth callback issuer %q is not %q", callback.Issuer, pending.Issuer)
	}
	return ExchangeCode(ctx, client, pending, callback.Code)
}

func ExchangeCode(ctx context.Context, client *http.Client, pending PendingAuth, code string) (SessionData, error) {
	if code == "" {
		return SessionData{}, errors.New("oauthclient: no oauth authorization code")
	}
	if pending.TokenEndpoint == "" {
		return SessionData{}, errors.New("oauthclient: the pending oauth request names no token endpoint")
	}
	if pending.HostURL == "" {
		return SessionData{}, errors.New("oauthclient: the pending oauth request names no PDS")
	}
	key, err := atcrypto.ParsePrivateMultibase(pending.DPoPPrivateKeyMultibase)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the oauth DPoP key: %w", err)
	}
	if _, ok := key.(*atcrypto.PrivateKeyP256); !ok {
		return SessionData{}, fmt.Errorf("oauthclient: the oauth DPoP key type %T is not P-256", key)
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {pending.ClientID},
		"redirect_uri":  {pending.RedirectURI},
		"code_verifier": {pending.CodeVerifier},
	}
	var resp *http.Response
	nonce := ""
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := DPoPProof(key, http.MethodPost, pending.TokenEndpoint, "", nonce)
		if err != nil {
			return SessionData{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, pending.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return SessionData{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err = client.Do(req)
		if err != nil {
			return SessionData{}, err
		}
		nextNonce := resp.Header.Get("DPoP-Nonce")
		if attempt == 0 && resp.StatusCode == http.StatusBadRequest && nextNonce != "" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
			resp.Body.Close()
			if strings.Contains(string(raw), "use_dpop_nonce") {
				nonce = nextNonce
				continue
			}
			return SessionData{}, fmt.Errorf("oauthclient: the code exchange answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		break
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return SessionData{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SessionData{}, fmt.Errorf("oauthclient: the code exchange answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		Subject      string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: decoding the code exchange: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return SessionData{}, errors.New("oauthclient: the code exchange returned no access or refresh token")
	}
	did, err := syntax.ParseDID(out.Subject)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the code exchange subject: %w", err)
	}
	host := strings.TrimRight(pending.HostURL, "/")
	if nextNonce := resp.Header.Get("DPoP-Nonce"); nextNonce != "" {
		nonce = nextNonce
	}
	scopes := pending.Scopes
	if out.Scope != "" {
		scopes = strings.Fields(out.Scope)
	}
	return SessionData{
		AccountDID:              did.String(),
		SessionID:               SessionID(did.String(), host),
		HostURL:                 host,
		ClientID:                pending.ClientID,
		AuthServerURL:           pending.AuthServerURL,
		AuthServerTokenEndpoint: pending.TokenEndpoint,
		Scopes:                  append([]string(nil), scopes...),
		AccessToken:             out.AccessToken,
		RefreshToken:            out.RefreshToken,
		DPoPAuthServerNonce:     nonce,
		DPoPPrivateKeyMultibase: key.Multibase(),
	}, nil
}
