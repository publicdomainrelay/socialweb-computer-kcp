package bidder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

type openBaoVault struct {
	login func(ctx context.Context) (*openbao.Client, error)
}

func (v *openBaoVault) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	client, err := v.login(ctx)
	if err != nil {
		return err
	}
	return client.PutKV(ctx, kvPath(mount, path), data)
}

func (v *openBaoVault) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	client, err := v.login(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetKV(ctx, kvPath(mount, path))
}

func kvPath(mount, path string) string {
	return strings.Trim(mount, "/") + "/data/" + strings.Trim(path, "/")
}

func kcpOpenBaoVault(store *kcpstore.Store, cluster, server, clientName string) (GuestVault, error) {
	if store == nil {
		return nil, errors.New("bidder: the guest vault needs a kcp store")
	}
	servers := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, OpenBaoServerGVR)
	return &openBaoVault{login: func(ctx context.Context) (*openbao.Client, error) {
		obj, err := servers.Get(ctx, ref.New(cluster, "", server))
		if err != nil {
			return nil, fmt.Errorf("bidder: reading OpenBaoServer %s: %w", server, err)
		}
		client, _, err := openbaoctl.ClientLogin(ctx, obj.Status, clientName)
		if err != nil {
			return nil, fmt.Errorf("bidder: logging in to OpenBaoServer %s as %s: %w", server, clientName, err)
		}
		return client, nil
	}}, nil
}
