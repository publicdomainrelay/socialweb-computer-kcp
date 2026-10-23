package pdsclient

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (b *basePDS) ServiceAuth(ctx context.Context, audience, lxm string, expires time.Time) (string, error) {
	if strings.TrimSpace(audience) == "" {
		return "", fmt.Errorf("%w: getServiceAuth needs an audience", ErrUnexpectedResponse)
	}
	auth, err := b.client.credential()
	if err != nil {
		return "", err
	}
	query := url.Values{"aud": {audience}}
	if lxm != "" {
		query.Set("lxm", lxm)
	}
	if !expires.IsZero() {
		query.Set("exp", strconv.FormatInt(expires.Unix(), 10))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := b.client.getJSON(ctx, "com.atproto.server.getServiceAuth", query, auth, &out); err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.Token)
	if token == "" {
		return "", fmt.Errorf("%w: getServiceAuth returned no token", ErrUnexpectedResponse)
	}
	return token, nil
}
