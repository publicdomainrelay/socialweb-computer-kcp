package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	accessKeys = []string{"accessJwt", "accessToken", "access_token", "jwt", "token", "value"}

	refreshKeys = []string{"refreshJwt", "refreshToken", "refresh_token", "jwt", "token", "value"}
)

type Resolve func(ctx context.Context, ref string) (map[string]any, error)

type leaseTokens struct {
	source *lease.Source[string]
}

var (
	_ pdsclient.TokenSource    = leaseTokens{}
	_ pdsclient.TokenRefresher = leaseTokens{}
)

func (t leaseTokens) AccessToken(ctx context.Context) (string, error) { return t.source.Get(ctx) }

func (t leaseTokens) RefreshAccessToken(ctx context.Context) (string, error) {
	t.source.Invalidate()
	return t.source.Get(ctx)
}

func Credentials(ctx context.Context, auth pdsv1alpha1.RemotePDSAuth, resolve Resolve) (xrpc.Auth, error) {
	accessRef := strings.TrimSpace(auth.AccessTokenRef)
	refreshRef := strings.TrimSpace(auth.RefreshTokenRef)
	if accessRef == "" && refreshRef == "" {
		accessRef = strings.TrimSpace(auth.AccountRef)
	}
	keyRef := strings.TrimSpace(auth.DPoPKeyRef)
	if accessRef == "" && refreshRef == "" && keyRef == "" {
		return nil, nil
	}
	if resolve == nil {
		return nil, errors.New("remote: the RemotePDS auth names refs but no secret resolver is configured")
	}
	tokens := leaseTokens{source: lease.New(func(ctx context.Context) (string, time.Time, error) {
		token, err := firstToken(ctx, resolve, accessRef, accessKeys)
		if err == nil && token == "" {
			token, err = firstToken(ctx, resolve, refreshRef, refreshKeys)
		}
		if err != nil {
			return "", time.Time{}, err
		}
		if token == "" {
			return "", time.Time{}, fmt.Errorf("%w: the RemotePDS auth refs hold no token", pdsclient.ErrAuthenticationRequired)
		}
		expires, ok := pdsclient.TokenExpiry(token)
		if !ok {
			return token, time.Time{}, nil
		}
		return token, expires.Add(-pdsclient.TokenExpirySkew), nil
	})}
	var dpop pdsclient.DPoPSource
	if keyRef != "" {
		data, err := resolve(ctx, keyRef)
		if err != nil {
			return nil, fmt.Errorf("remote: resolving the dpop key ref %s: %w", keyRef, err)
		}
		if dpop, err = pdsclient.DPoPSourceFromData(data); err != nil {
			return nil, err
		}
	}
	return pdsclient.TokenAuth(tokens, dpop), nil
}

func firstToken(ctx context.Context, resolve Resolve, ref string, keys []string) (string, error) {
	if ref == "" {
		return "", nil
	}
	data, err := resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("remote: resolving the token ref %s: %w", ref, err)
	}
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}
