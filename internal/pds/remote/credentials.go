package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
)

var (
	accessKeys = []string{"accessJwt", "accessToken", "access_token", "jwt", "token", "value"}

	refreshKeys = []string{"refreshJwt", "refreshToken", "refresh_token", "jwt", "token", "value"}
)

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

func Credentials(ctx context.Context, auth pdsv1alpha1.RemotePDSAuth, secrets Secrets) (xrpc.Auth, error) {
	accessRef, refreshRef, keyRef := usable(auth.AccessTokenRef), usable(auth.RefreshTokenRef), usable(auth.DPoPKeyRef)
	if accessRef == nil && refreshRef == nil && keyRef == nil {
		return nil, nil
	}
	if secrets == nil {
		return nil, errors.New("remote: the RemotePDS auth names refs but no secret resolver is configured")
	}
	tokens := leaseTokens{source: lease.New(func(ctx context.Context) (string, time.Time, error) {
		token, err := firstToken(ctx, secrets, accessRef, accessKeys)
		if err == nil && token == "" {
			token, err = firstToken(ctx, secrets, refreshRef, refreshKeys)
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
	if keyRef != nil {
		data, err := secrets(ctx, *keyRef)
		if err != nil {
			return nil, fmt.Errorf("remote: resolving the dpop key ref %s: %w", refName(*keyRef), err)
		}
		if dpop, err = pdsclient.DPoPSourceFromData(data); err != nil {
			return nil, err
		}
	}
	return pdsclient.TokenAuth(tokens, dpop), nil
}

func usable(ref *commonv1.SecretRef) *commonv1.SecretRef {
	if ref == nil || strings.Trim(strings.TrimSpace(ref.Path), "/") == "" {
		return nil
	}
	return ref
}

func refName(ref commonv1.SecretRef) string {
	return strings.Trim(strings.TrimSpace(ref.Mount)+"/"+strings.TrimSpace(ref.Path), "/")
}

func firstToken(ctx context.Context, secrets Secrets, ref *commonv1.SecretRef, keys []string) (string, error) {
	if ref == nil {
		return "", nil
	}
	data, err := secrets(ctx, *ref)
	if err != nil {
		return "", fmt.Errorf("remote: resolving the token ref %s: %w", refName(*ref), err)
	}
	if key := strings.TrimSpace(ref.Key); key != "" {
		keys = []string{key}
	}
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}
