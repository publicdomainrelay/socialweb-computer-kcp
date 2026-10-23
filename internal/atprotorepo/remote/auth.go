package remote

import (
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

type Auth struct {
	authenticator pds.Authenticator
}

var (
	_ xrpc.Auth       = (*Auth)(nil)
	_ xrpc.Challenger = (*Auth)(nil)
)

func NewAuth(authenticator pds.Authenticator) *Auth {
	return &Auth{authenticator: authenticator}
}

func (a *Auth) Apply(req *http.Request) error { return a.authenticator.Apply(req) }

func (a *Auth) Invalidate() { a.authenticator.Invalidate() }

func (a *Auth) Challenge(resp *http.Response) xrpc.Verdict {
	switch a.authenticator.Challenge(resp) {
	case pds.VerdictRetryNonce:
		return xrpc.RetryNonce
	case pds.VerdictRetryAuth:
		return xrpc.RetryAuth
	}
	return xrpc.Done
}
