package bidder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/authretry"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	VaultLoginFor = 10 * time.Minute
)

type openBaoVault struct {
	logins *lease.Source[*openbao.Client]
}

func (v *openBaoVault) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	return authretry.Run(ctx, v.logins, func(ctx context.Context) error {
		client, err := v.logins.Get(ctx)
		if err != nil {
			return err
		}
		return client.PutKV(ctx, kvPath(mount, path), data)
	}, openbao.IsAuthError)
}

func (v *openBaoVault) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	return authretry.Do(ctx, v.logins, func(ctx context.Context) (map[string]any, error) {
		client, err := v.logins.Get(ctx)
		if err != nil {
			return nil, err
		}
		return client.GetKV(ctx, kvPath(mount, path))
	}, openbao.IsAuthError)
}

func kvPath(mount, path string) string {
	return strings.Trim(mount, "/") + "/data/" + strings.Trim(path, "/")
}

func kcpOpenBaoVault(store *kcpstore.Store, cluster, server, clientName string) (GuestVault, error) {
	if store == nil {
		return nil, errors.New("bidder: the guest vault needs a kcp store")
	}
	servers := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, OpenBaoServerGVR)
	return &openBaoVault{logins: lease.New(func(ctx context.Context) (*openbao.Client, time.Time, error) {
		obj, err := servers.Get(ctx, ref.New(cluster, "", server))
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("bidder: reading OpenBaoServer %s: %w", server, err)
		}
		client, _, err := openbaoctl.ClientLogin(ctx, obj.Status, clientName)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("bidder: logging in to OpenBaoServer %s as %s: %w", server, clientName, err)
		}
		until := time.Now().Add(VaultLoginFor)
		if at := client.RefreshAt(); !at.IsZero() && at.Before(until) {
			until = at
		}
		return client, until, nil
	})}, nil
}
