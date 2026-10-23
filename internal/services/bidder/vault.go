package bidder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const (
	VaultLoginFor = 10 * time.Minute
)

type openBaoVault struct {
	fresh func(context.Context) (*openbao.Client, error)

	mu     sync.Mutex
	client *openbao.Client
	until  time.Time
}

func (v *openBaoVault) withClient(ctx context.Context) (*openbao.Client, error) {
	v.mu.Lock()
	if v.client != nil && time.Now().Before(v.until) {
		client := v.client
		v.mu.Unlock()
		return client, nil
	}
	v.mu.Unlock()
	client, err := v.fresh(ctx)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.client = client
	until := time.Now().Add(VaultLoginFor)
	if at := client.RefreshAt(); !at.IsZero() && at.Before(until) {
		until = at
	}
	v.until = until
	v.mu.Unlock()
	return client, nil
}

func (v *openBaoVault) forget() {
	v.mu.Lock()
	v.client = nil
	v.until = time.Time{}
	v.mu.Unlock()
}

func (v *openBaoVault) WriteKV(ctx context.Context, mount, path string, data map[string]any) error {
	err := v.writeKV(ctx, mount, path, data)
	if openbao.IsAuthError(err) {
		v.forget()
		err = v.writeKV(ctx, mount, path, data)
	}
	return err
}

func (v *openBaoVault) writeKV(ctx context.Context, mount, path string, data map[string]any) error {
	client, err := v.withClient(ctx)
	if err != nil {
		return err
	}
	return client.PutKV(ctx, kvPath(mount, path), data)
}

func (v *openBaoVault) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	data, err := v.readKV(ctx, mount, path)
	if openbao.IsAuthError(err) {
		v.forget()
		data, err = v.readKV(ctx, mount, path)
	}
	return data, err
}

func (v *openBaoVault) readKV(ctx context.Context, mount, path string) (map[string]any, error) {
	client, err := v.withClient(ctx)
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
	vault := &openBaoVault{}
	vault.fresh = func(ctx context.Context) (*openbao.Client, error) {
		obj, err := servers.Get(ctx, ref.New(cluster, "", server))
		if err != nil {
			return nil, fmt.Errorf("bidder: reading OpenBaoServer %s: %w", server, err)
		}
		client, _, err := openbaoctl.ClientLogin(ctx, obj.Status, clientName)
		if err != nil {
			return nil, fmt.Errorf("bidder: logging in to OpenBaoServer %s as %s: %w", server, clientName, err)
		}
		return client, nil
	}
	return vault, nil
}
