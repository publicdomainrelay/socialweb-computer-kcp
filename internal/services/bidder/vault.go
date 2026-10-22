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
	v.until = time.Now().Add(VaultLoginFor)
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
	client, err := v.withClient(ctx)
	if err != nil {
		return err
	}
	err = client.PutKV(ctx, kvPath(mount, path), data)
	if isForbidden(err) {
		v.forget()
	}
	return err
}

func (v *openBaoVault) ReadKV(ctx context.Context, mount, path string) (map[string]any, error) {
	client, err := v.withClient(ctx)
	if err != nil {
		return nil, err
	}
	data, err := client.GetKV(ctx, kvPath(mount, path))
	if isForbidden(err) {
		v.forget()
	}
	return data, err
}

func kvPath(mount, path string) string {
	return strings.Trim(mount, "/") + "/data/" + strings.Trim(path, "/")
}

func isForbidden(err error) bool {
	var re *openbao.ResponseError
	return errors.As(err, &re) && re.Status == 403
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
