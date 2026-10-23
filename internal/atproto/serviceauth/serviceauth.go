package serviceauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotoauth"
)

const MaxTokenLifetime = atprotoauth.MaxServiceAuthLifetime

var (
	ErrMalformed = atprotoauth.ErrMalformed

	ErrExpired = atprotoauth.ErrExpired

	ErrBadSignature = atprotoauth.ErrBadSignature

	ErrNoKey = atprotoauth.ErrNoKey

	ErrBadAudience = atprotoauth.ErrBadAudience

	ErrBadLxm = atprotoauth.ErrBadLxm

	ErrBadIssuer = atprotoauth.ErrBadIssuer

	ErrLifetimeTooLong = atprotoauth.ErrLifetimeTooLong

	ErrInvalidIssuer = errors.New("serviceauth: invalid issuer DID")

	ErrInvalidAudience = errors.New("serviceauth: invalid audience")

	ErrInvalidLxm = errors.New("serviceauth: invalid lexicon method")

	ErrInvalidLifetime = errors.New("serviceauth: invalid token lifetime")

	ErrInvalidKey = errors.New("serviceauth: invalid signing key")

	ErrNoDirectory = errors.New("serviceauth: no identity directory")
)

type Verifier struct {
	Audiences []string
	Directory identity.Directory
}

func Sign(issuer syntax.DID, audience string, lexMethod syntax.NSID, key atcrypto.PrivateKey, ttl time.Duration) (string, error) {
	if _, err := syntax.ParseDID(issuer.String()); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidIssuer, err)
	}
	if audience == "" {
		return "", ErrInvalidAudience
	}
	nsid, err := syntax.ParseNSID(lexMethod.String())
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidLxm, err)
	}
	if ttl <= 0 || ttl > MaxTokenLifetime {
		return "", fmt.Errorf("%w: must be positive and at most %s", ErrInvalidLifetime, MaxTokenLifetime)
	}
	if key == nil {
		return "", ErrInvalidKey
	}
	return auth.SignServiceAuth(issuer, audience, ttl, &nsid, key)
}

func (v *Verifier) Verify(ctx context.Context, token string, lexMethod syntax.NSID) (syntax.DID, error) {
	if v == nil || len(v.Audiences) == 0 {
		return "", fmt.Errorf("%w: no audience is configured", ErrBadAudience)
	}
	if v.Directory == nil {
		return "", ErrNoDirectory
	}
	if _, err := syntax.ParseNSID(lexMethod.String()); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadLxm, err)
	}
	rawDID, err := atprotoauth.Verify(ctx, token, lexMethod.String(), v.Audiences, v.Directory)
	if err != nil {
		return "", err
	}
	did, err := syntax.ParseDID(rawDID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return did, nil
}

func (v *Verifier) VerifyRequest(r *http.Request) (syntax.DID, error) {
	scheme, token, ok := SplitAuthorization(r.Header.Get("Authorization"))
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%w: missing bearer token", ErrMalformed)
	}
	lexMethod, ok := LXMFromPath(r.URL.Path)
	if !ok {
		return "", fmt.Errorf("%w: path carries no lexicon method", ErrBadLxm)
	}
	return v.Verify(r.Context(), token, lexMethod)
}

func LXMFromPath(path string) (syntax.NSID, bool) {
	parts := strings.Split(path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "xrpc" {
			continue
		}
		lexMethod, err := syntax.ParseNSID(parts[i+1])
		if err == nil {
			return lexMethod, true
		}
	}
	return "", false
}

type didContextKey struct{}

func DIDFromContext(ctx context.Context) (syntax.DID, bool) {
	did, ok := ctx.Value(didContextKey{}).(syntax.DID)
	return did, ok
}

func (v *Verifier) Require(next http.Handler) http.Handler {
	return v.middleware(next, true)
}

func (v *Verifier) Optional(next http.Handler) http.Handler {
	return v.middleware(next, false)
}

func (v *Verifier) middleware(next http.Handler, mandatory bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization == "" {
			if mandatory {
				writeUnauthorized(w, "atproto service auth required")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if scheme, _, ok := SplitAuthorization(authorization); !ok || !strings.EqualFold(scheme, "Bearer") {
			if mandatory {
				writeUnauthorized(w, "atproto service auth required, but missing or incorrect formatting")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !mandatory {
			if _, ok := LXMFromPath(r.URL.Path); !ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		did, err := v.VerifyRequest(r)
		if err != nil {
			writeUnauthorized(w, fmt.Sprintf("invalid service auth: %s", err))
			return
		}
		ctx := context.WithValue(r.Context(), didContextKey{}, did)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func SplitAuthorization(header string) (string, string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "Unauthorized",
		"message": message,
	})
}
