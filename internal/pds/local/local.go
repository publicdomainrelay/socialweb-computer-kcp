package local

import (
	"context"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

const scopeAll = "*"

var (
	_ atprotorepo.Repo = (*Repo)(nil)
	_ pds.Accounts     = (*Accounts)(nil)
	_ pds.Sessions     = (*Sessions)(nil)
	_ pds.Sync         = (*Sync)(nil)
)

type Secrets func(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error)

func NewRepo(ctx context.Context, srv *atpds.Server, scope []string) (*Repo, error) {
	did, err := pds.AccountDID(scope)
	if err != nil {
		return nil, err
	}
	return openRepo(ctx, srv, did)
}

func NewAccounts(srv *atpds.Server, secrets Secrets, scope []string) *Accounts {
	return &Accounts{srv: srv, secrets: secrets, scope: scope}
}

func NewSessions(srv *atpds.Server, secrets Secrets, scope []string) *Sessions {
	return &Sessions{srv: srv, secrets: secrets, scope: scope}
}

func NewSync(srv *atpds.Server) *Sync {
	return &Sync{srv: srv}
}

func inScope(scope []string, did string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, entry := range scope {
		entry = strings.TrimSpace(entry)
		if entry == scopeAll || entry == did {
			return true
		}
	}
	return false
}
