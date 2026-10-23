package xrpc

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ServiceAuthNSID = "com.atproto.server.getServiceAuth"

var ErrNoServiceAuthToken = errors.New("xrpc: getServiceAuth returned no token")

type Minter interface {
	ServiceAuth(ctx context.Context, aud, lxm string) (string, error)
}

type MinterFunc func(ctx context.Context, aud, lxm string) (string, error)

func (f MinterFunc) ServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return f(ctx, aud, lxm)
}

func GetServiceAuth(ctx context.Context, c *Client, aud, lxm string, exp time.Time) (string, error) {
	if aud == "" {
		return "", errors.New("xrpc: getServiceAuth needs an audience")
	}
	params := url.Values{"aud": {aud}}
	if lxm != "" {
		params.Set("lxm", lxm)
	}
	if !exp.IsZero() {
		params.Set("exp", strconv.FormatInt(exp.Unix(), 10))
	}
	out, err := Query[struct {
		Token string `json:"token"`
	}](ctx, c, ServiceAuthNSID, params)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.Token)
	if token == "" {
		return "", ErrNoServiceAuthToken
	}
	return token, nil
}

type PDSMinter struct {
	Client *Client
}

func (m PDSMinter) ServiceAuth(ctx context.Context, aud, lxm string) (string, error) {
	return GetServiceAuth(ctx, m.Client, aud, lxm, time.Time{})
}
