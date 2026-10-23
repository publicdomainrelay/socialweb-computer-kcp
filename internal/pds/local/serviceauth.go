package local

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

var _ pds.ServiceAuth = (*ServiceAuth)(nil)

type ServiceAuth struct {
	srv *atpds.Server

	scope []string
}

func NewServiceAuth(srv *atpds.Server, scope []string) *ServiceAuth {
	return &ServiceAuth{srv: srv, scope: scope}
}

func (s *ServiceAuth) ServiceAuth(ctx context.Context, raw, audience, lxm string, expires time.Time) (string, error) {
	if !inScope(s.scope, raw) {
		return "", fmt.Errorf("pds: the account %s is outside the granted scope %v", raw, s.scope)
	}
	did, err := syntax.ParseDID(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("pds: the account did %q: %w", raw, err)
	}
	nsid, err := syntax.ParseNSID(strings.TrimSpace(lxm))
	if err != nil {
		return "", fmt.Errorf("pds: the lexicon method %q: %w", lxm, err)
	}
	keys := s.srv.KeyStore()
	if keys == nil {
		return "", fmt.Errorf("%w: the Pds holds no repo keys", pds.ErrUnsupported)
	}
	key, err := keys.GetRepoKey(ctx, did)
	if err != nil {
		return "", fmt.Errorf("%w: the repo key of %s: %v", pds.ErrNotFound, did, err)
	}
	ttl := serviceauth.MaxTokenLifetime
	if !expires.IsZero() {
		if remaining := time.Until(expires); remaining > 0 && remaining < ttl {
			ttl = remaining
		}
	}
	return serviceauth.Sign(did, audience, nsid, key, ttl)
}
