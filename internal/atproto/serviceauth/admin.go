package serviceauth

import (
	"crypto/subtle"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/auth"
)

func Admin(passwords []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		handler := auth.AdminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		}, passwords)
		return http.HandlerFunc(handler)
	}
}

func IsAdmin(r *http.Request, passwords []string) bool {
	username, password, ok := r.BasicAuth()
	if !ok || username != "admin" {
		return false
	}
	for _, candidate := range passwords {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(password)) == 1 {
			return true
		}
	}
	return false
}
