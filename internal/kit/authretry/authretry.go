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

type InvalidateFunc func()

func (f InvalidateFunc) Invalidate() { f() }

type StatusError interface {
	error
	HTTPStatus() int
}

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

func Run(ctx context.Context, src Invalidator, call func(context.Context) error, isAuthErr func(error) bool) error {
	_, err := Do(ctx, src, func(ctx context.Context) (struct{}, error) { return struct{}{}, call(ctx) }, isAuthErr)
	return err
}

func Status(err error) int {
	var se StatusError
	if errors.As(err, &se) {
		return se.HTTPStatus()
	}
	return 0
}

func IsAuthErr(err error) bool {
	switch Status(err) {
	case http.StatusUnauthorized:
		return true
	case http.StatusBadRequest, http.StatusForbidden:
		return ExpiredToken(err.Error())
	}
	return false
}

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
