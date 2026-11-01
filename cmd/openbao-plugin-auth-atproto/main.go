package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/plugin"
)

type backend struct {
	*framework.Backend
	lock     sync.Mutex
	sessions *sessionLocks
	nonces   *nonceCache
	revives  *reviveLimit
}

func factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := &backend{sessions: newSessionLocks(), nonces: newNonceCache(), revives: newReviveLimit()}
	b.Backend = &framework.Backend{
		Help:        "Signs users in with atproto OAuth as a confidential client and keeps their sessions: the DPoP key and the tokens never leave this mount.",
		BackendType: logical.TypeCredential,
		PathsSpecial: &logical.Paths{
			Unauthenticated: []string{"client-metadata", "login/start", "login"},
		},
		Paths: []*framework.Path{
			pathConfig(b),
			pathRotateClientKey(b),
			pathClientMetadata(b),
			pathRoleList(b),
			pathRole(b),
			pathLoginStart(b),
			pathLogin(b),
			pathSessionProof(b),
			pathSession(b),
		},
		AuthRenew: b.loginRenew,
	}
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

func main() {
	if err := plugin.ServeMultiplex(&plugin.ServeOpts{BackendFactoryFunc: factory}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
