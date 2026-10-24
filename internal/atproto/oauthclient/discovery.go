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

var ErrNoProtectedResource = errors.New("oauthclient: the origin serves no oauth protected-resource metadata")

type StatusError struct {
	URL string

	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d", e.URL, e.StatusCode)
}

type AuthServer struct {
	URL string

	Issuer string

	AuthorizationEndpoint string

	TokenEndpoint string

	RevocationEndpoint string

	Entryway bool
}

func DiscoverAuthServer(ctx context.Context, client *http.Client, pds string) (string, string, string, error) {
	as, err := discover(ctx, client, pds, false)
	if err != nil {
		return "", "", "", err
	}
	return as.URL, as.TokenEndpoint, as.RevocationEndpoint, nil
}

func DiscoverAuthorization(ctx context.Context, client *http.Client, origin string) (AuthServer, error) {
	return discover(ctx, client, origin, true)
}

func discover(ctx context.Context, client *http.Client, pds string, entryway bool) (AuthServer, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	base := strings.TrimRight(pds, "/")
	authServer, err := protectedResourceAuthServer(ctx, client, base)
	isEntryway := false
	if errors.Is(err, ErrNoProtectedResource) && entryway {
		origin, ok := originOf(base)
		if !ok {
			return AuthServer{}, err
		}
		authServer, isEntryway = origin, true
	} else if err != nil {
		return AuthServer{}, err
	}
	as, err := authServerMetadata(ctx, client, authServer)
	if err != nil {
		if isEntryway {
			return AuthServer{}, fmt.Errorf("oauthclient: %s serves neither protected-resource nor authorization server metadata: %w", base, err)
		}
		return AuthServer{}, err
	}
	if isEntryway && strings.TrimRight(as.Issuer, "/") != authServer {
		return AuthServer{}, fmt.Errorf("oauthclient: the entryway %s names the issuer %q, not itself", authServer, as.Issuer)
	}
	as.Entryway = isEntryway
	return as, nil
}

func protectedResourceAuthServer(ctx context.Context, client *http.Client, base string) (string, error) {
	var protected struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := GetJSON(ctx, client, base+"/.well-known/oauth-protected-resource", &protected); err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%w: %v", ErrNoProtectedResource, err)
		}
		return "", fmt.Errorf("oauthclient: the PDS oauth metadata: %w", err)
	}
	if len(protected.AuthorizationServers) == 0 || protected.AuthorizationServers[0] == "" {
		return "", errors.New("oauthclient: the PDS oauth metadata names no authorization server")
	}
	authURL, err := url.Parse(protected.AuthorizationServers[0])
	if err != nil || authURL.Host == "" || (authURL.Scheme != "http" && authURL.Scheme != "https") {
		return "", fmt.Errorf("oauthclient: the authorization server %q is not an absolute http or https URL", protected.AuthorizationServers[0])
	}
	return strings.TrimRight(authURL.String(), "/"), nil
}

func authServerMetadata(ctx context.Context, client *http.Client, authServer string) (AuthServer, error) {
	var metadata struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		RevocationEndpoint    string `json:"revocation_endpoint"`
	}
	if err := GetJSON(ctx, client, authServer+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return AuthServer{}, fmt.Errorf("oauthclient: the auth server metadata: %w", err)
	}
	if metadata.TokenEndpoint == "" {
		return AuthServer{}, errors.New("oauthclient: the auth server metadata names no token endpoint")
	}
	as := AuthServer{URL: authServer, Issuer: metadata.Issuer}
	var err error
	if as.TokenEndpoint, err = EndpointURL(authServer, metadata.TokenEndpoint); err != nil {
		return AuthServer{}, err
	}
	if metadata.AuthorizationEndpoint != "" {
		if as.AuthorizationEndpoint, err = EndpointURL(authServer, metadata.AuthorizationEndpoint); err != nil {
			return AuthServer{}, err
		}
	}
	if metadata.RevocationEndpoint != "" {
		if as.RevocationEndpoint, err = EndpointURL(authServer, metadata.RevocationEndpoint); err != nil {
			return AuthServer{}, err
		}
	}
	return as, nil
}

func originOf(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.Trim(parsed.Path, "/") != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
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
		return &StatusError{URL: target, StatusCode: resp.StatusCode}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", target, err)
	}
	return nil
}
