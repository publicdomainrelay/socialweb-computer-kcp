package statusstate

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const maxSourceBody = 8 << 20

func sourceBaseURL(raw, kind string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("statusstate: the %s base URL is invalid: %w", kind, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("statusstate: the %s base URL needs http or https", kind)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("statusstate: the %s base URL has no host", kind)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("statusstate: the %s base URL must not have a query or fragment", kind)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func httpClient(client *http.Client) *http.Client {
	if client == nil {
		return http.DefaultClient
	}
	return client
}
