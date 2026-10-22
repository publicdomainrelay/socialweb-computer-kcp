package ingress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/didkey"
)

var serviceAuthMethods = []string{"ES256K", "ES256"}

func NewDirectory(plcURL string) identity.Directory {
	if plcURL == "" {
		return identity.DefaultDirectory()
	}
	base := &identity.BaseDirectory{
		PLCURL:                 strings.TrimSuffix(plcURL, "/"),
		HTTPClient:             http.Client{Timeout: 10 * time.Second},
		PLCClient:              &http.Client{Timeout: 10 * time.Second},
		SkipHandleVerification: true,
	}
	return identity.NewCacheDirectory(base, 100_000, time.Hour, 2*time.Minute, 5*time.Minute)
}

func BearerToken(authHeader, queryToken string) (string, error) {
	if authHeader != "" {
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			return "", errors.New("Authorization header must be Bearer <token>")
		}
		return parts[1], nil
	}
	if queryToken != "" {
		return queryToken, nil
	}
	return "", errors.New("missing Authorization header")
}

func VerifyServiceAuth(ctx context.Context, token, aud, lxm string, dir identity.Directory) (string, error) {
	if token == "" {
		return "", errors.New("missing Authorization header")
	}
	claims := jwt.MapClaims{}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(serviceAuthMethods),
		jwt.WithAudience(aud),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5 * time.Second),
	}
	keyFunc := func(t *jwt.Token) (any, error) {
		iss, err := claims.GetIssuer()
		if err != nil {
			return nil, fmt.Errorf("missing iss claim: %w", err)
		}
		return resolveSigningKey(ctx, iss, dir)
	}
	parsed, err := jwt.ParseWithClaims(token, claims, keyFunc, opts...)
	if err != nil && errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		insecure := jwt.MapClaims{}
		if _, _, uerr := jwt.NewParser().ParseUnverified(token, insecure); uerr == nil {
			if iss, ierr := insecure.GetIssuer(); ierr == nil && dir != nil {
				if did, derr := syntax.ParseDID(iss); derr == nil {
					_ = dir.Purge(ctx, did.AtIdentifier())
				}
			}
			claims = jwt.MapClaims{}
			parsed, err = jwt.ParseWithClaims(token, claims, keyFunc, opts...)
		}
	}
	if err != nil {
		return "", err
	}
	if !parsed.Valid {
		return "", errors.New("service auth token is not valid")
	}
	gotLXM, _ := claims["lxm"].(string)
	if gotLXM != lxm {
		return "", fmt.Errorf("lxm mismatch: expected %s, got %s", lxm, gotLXM)
	}
	iss, err := claims.GetIssuer()
	if err != nil {
		return "", err
	}
	return iss, nil
}

func resolveSigningKey(ctx context.Context, did string, dir identity.Directory) (atcrypto.PublicKey, error) {
	if strings.HasPrefix(did, didkey.Prefix) {
		return didkey.ParsePublic(did)
	}
	if dir == nil {
		return nil, fmt.Errorf("cannot resolve signing key for %s", did)
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return nil, err
	}
	ident, err := dir.LookupDID(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("resolving DID (%s): %w", did, err)
	}
	return ident.PublicKey()
}

func SignServiceAuth(priv atcrypto.PrivateKey, iss, aud, lxm string, ttl time.Duration) (string, error) {
	nsid, err := syntax.ParseNSID(lxm)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return auth.SignServiceAuth(syntax.DID(iss), aud, ttl, &nsid, priv)
}
