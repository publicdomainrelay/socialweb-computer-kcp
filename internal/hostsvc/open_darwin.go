//go:build darwin

package hostsvc

import (
	"context"
	"fmt"
)

func Open(ctx context.Context, cfg Config) (Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Backend == BackendProc {
		return openProc(ctx, cfg)
	}
	return nil, fmt.Errorf("%w: the %s backend is unsupported on darwin; see plan 0012", ErrUnsupported, cfg.Backend)
}
