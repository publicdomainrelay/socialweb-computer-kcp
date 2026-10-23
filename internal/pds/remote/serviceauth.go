package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

var _ pds.ServiceAuth = (*ServiceAuth)(nil)

type ServiceAuth struct {
	host *Host

	scope []string
}

func (h *Host) ServiceAuth(scope []string) *ServiceAuth {
	return &ServiceAuth{host: h, scope: scope}
}

func (s *ServiceAuth) ServiceAuth(ctx context.Context, did, audience, lxm string, expires time.Time) (string, error) {
	if err := inScope(s.scope, did); err != nil {
		return "", err
	}
	client, ok := s.host.held(did)
	if !ok {
		return "", fmt.Errorf("%w: no session is held for %s", pds.ErrNotFound, did)
	}
	return client.ServiceAuth(ctx, audience, lxm, expires)
}
