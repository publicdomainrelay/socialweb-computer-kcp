package oauthclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var ErrNoRevocationEndpoint = errors.New("oauthclient: the authorization server names no revocation endpoint")

type Revocation struct {
	Endpoint string

	Revoked []string
}

func (s *Session) Revoke(ctx context.Context) (Revocation, error) {
	if s == nil {
		return Revocation{}, errors.New("oauthclient: no oauth session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if s.data.AuthServerRevocationEndpoint == "" {
		if _, _, revokeEndpoint, err := DiscoverAuthServer(ctx, client, s.hostURL); err == nil {
			s.data.AuthServerRevocationEndpoint = revokeEndpoint
		}
	}
	out := Revocation{Endpoint: s.data.AuthServerRevocationEndpoint}
	if out.Endpoint == "" {
		return out, ErrNoRevocationEndpoint
	}
	var errs []error
	for _, token := range []struct{ value, hint string }{{s.data.RefreshToken, "refresh_token"}, {s.data.AccessToken, "access_token"}} {
		if token.value == "" {
			continue
		}
		if err := s.revokeLocked(ctx, client, out.Endpoint, token.value, token.hint); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Revoked = append(out.Revoked, token.hint)
	}
	return out, errors.Join(errs...)
}

func (s *Session) revokeLocked(ctx context.Context, client *http.Client, endpoint, token, hint string) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}}
	if s.data.ClientID != "" {
		form.Set("client_id", s.data.ClientID)
	}
	for attempt := range 2 {
		proof, err := DPoPProof(s.key, http.MethodPost, endpoint, "", s.data.DPoPAuthServerNonce)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("oauthclient: revoking the %s: %w", hint, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
		resp.Body.Close()
		if nextNonce := resp.Header.Get("DPoP-Nonce"); nextNonce != "" {
			s.data.DPoPAuthServerNonce = nextNonce
			if attempt == 0 && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized) &&
				strings.Contains(string(raw), "use_dpop_nonce") {
				continue
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("oauthclient: revoking the %s answered HTTP %d", hint, resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("oauthclient: revoking the %s kept asking for a new DPoP nonce", hint)
}
