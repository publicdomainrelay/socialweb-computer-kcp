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

const proveNamespace = "prove-sshd"

type backend struct {
	*framework.Backend
	lock sync.Mutex
}

func factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := &backend{}
	b.Backend = &framework.Backend{
		Help:        "Logs a guest in, and issues its OIDC workload identity tokens, by an sshsig made with its bound ed25519 ssh host key.",
		BackendType: logical.TypeCredential,
		PathsSpecial: &logical.Paths{
			Unauthenticated: []string{"bind", "challenge", "login", "wid/token", "wid/.well-known/*"},
		},
		Paths: []*framework.Path{
			pathRoleList(b),
			pathRole(b),
			pathRoleWID(b),
			pathBind(b),
			pathChallenge(b),
			pathLogin(b),
			pathWIDConfig(b),
			pathWIDRotate(b),
			pathWIDToken(b),
			pathWIDDiscovery(b),
			pathWIDKeys(b),
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
