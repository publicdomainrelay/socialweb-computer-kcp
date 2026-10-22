package oauthclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func DiscoverAuthServer(ctx context.Context, client *http.Client, pds string) (string, string, string, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	base := strings.TrimRight(pds, "/")
	var protected struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := GetJSON(ctx, client, base+"/.well-known/oauth-protected-resource", &protected); err != nil {
		return "", "", "", fmt.Errorf("oauthclient: the PDS oauth metadata: %w", err)
	}
	if len(protected.AuthorizationServers) == 0 || protected.AuthorizationServers[0] == "" {
		return "", "", "", errors.New("oauthclient: the PDS oauth metadata names no authorization server")
	}
	authURL, err := url.Parse(protected.AuthorizationServers[0])
	if err != nil || authURL.Host == "" || (authURL.Scheme != "http" && authURL.Scheme != "https") {
		return "", "", "", fmt.Errorf("oauthclient: the authorization server %q is not an absolute http or https URL", protected.AuthorizationServers[0])
	}
	authServer := strings.TrimRight(authURL.String(), "/")
	var metadata struct {
		TokenEndpoint      string `json:"token_endpoint"`
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := GetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", "", "", fmt.Errorf("oauthclient: the auth server metadata: %w", err)
	}
	if metadata.TokenEndpoint == "" {
		return "", "", "", errors.New("oauthclient: the auth server metadata names no token endpoint")
	}
	tokenURL, err := EndpointURL(authServer, metadata.TokenEndpoint)
	if err != nil {
		return "", "", "", err
	}
	revokeURL := ""
	if metadata.RevocationEndpoint != "" {
		revokeURL, err = EndpointURL(authServer, metadata.RevocationEndpoint)
		if err != nil {
			return "", "", "", err
		}
	}
	return authServer, tokenURL, revokeURL, nil
}

func EndpointURL(base, raw string) (string, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("oauthclient: the oauth endpoint %q: %w", raw, err)
	}
	baseURL, err := url.Parse(base + "/")
	if err != nil {
		return "", err
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.Host == "" || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return "", fmt.Errorf("oauthclient: the oauth endpoint %q is not an absolute http or https URL", raw)
	}
	return resolved.String(), nil
}

func GetJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", target, err)
	}
	return nil
}
