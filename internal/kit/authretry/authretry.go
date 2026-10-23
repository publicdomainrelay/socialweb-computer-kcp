package authretry

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type Invalidator interface {
	Invalidate()
}

type StatusError interface {
	error
	HTTPStatus() int
}

// Do runs call, and once more after src.Invalidate when isAuthErr reports the
// first error as an auth failure. A nil isAuthErr means IsAuthErr.
func Do[T any](ctx context.Context, src Invalidator, call func(context.Context) (T, error), isAuthErr func(error) bool) (T, error) {
	if isAuthErr == nil {
		isAuthErr = IsAuthErr
	}
	value, err := call(ctx)
	if err == nil || ctx.Err() != nil || !isAuthErr(err) {
		return value, err
	}
	if src != nil {
		src.Invalidate()
	}
	return call(ctx)
}

func Status(err error) int {
	var se StatusError
	if errors.As(err, &se) {
		return se.HTTPStatus()
	}
	return 0
}

// IsAuthErr reports a 401, or a 400 or 403 whose message names an expired or invalid token.
func IsAuthErr(err error) bool {
	switch Status(err) {
	case http.StatusUnauthorized:
		return true
	case http.StatusBadRequest, http.StatusForbidden:
		return ExpiredToken(err.Error())
	}
	return false
}

// IsAuthOrForbidden reports a 401 or any 403, for servers such as OpenBao that answer an expired token with 403.
func IsAuthOrForbidden(err error) bool {
	switch Status(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

func IsAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func ExpiredToken(message string) bool {
	m := strings.ToLower(message)
	for _, needle := range [...]string{"expiredtoken", "invalidtoken", "invalid_token", "token expired", "token has expired", "expired token", "invalid token", "token is expired"} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}
