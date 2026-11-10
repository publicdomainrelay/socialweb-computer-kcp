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

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

func PushAuthorization(ctx context.Context, client *http.Client, endpoint string, key atcrypto.PrivateKeyExportable, form url.Values) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	nonce := ""
	for attempt := range 2 {
		proof, err := DPoPProof(key, http.MethodPost, endpoint, "", nonce)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		if next := resp.Header.Get("DPoP-Nonce"); attempt == 0 && resp.StatusCode == http.StatusBadRequest && next != "" &&
			strings.Contains(string(raw), "use_dpop_nonce") {
			nonce = next
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("oauthclient: the pushed authorization request answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var out struct {
			RequestURI string `json:"request_uri"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.RequestURI == "" {
			return "", errors.New("oauthclient: the pushed authorization request returned no request_uri")
		}
		return out.RequestURI, nil
	}
	return "", errors.New("oauthclient: the pushed authorization request kept asking for a new DPoP nonce")
}
