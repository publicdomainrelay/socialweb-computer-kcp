package kineetcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tenantstore"
)

const DefaultTimeout = 15 * time.Second

const registryPrefix = "/registry/"

type Config struct {
	Timeout time.Duration
}

type Store struct {
	timeout time.Duration
}

var _ tenantstore.Eraser = (*Store)(nil)

func New(cfg Config) *Store {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Store{timeout: cfg.Timeout}
}

func ServiceAccountKey(sa tenantstore.ServiceAccount) string {
	return registryPrefix + "serviceaccounts/" + sa.Namespace + "/" + sa.Name
}

func TokenSecretPrefix(sa tenantstore.ServiceAccount) string {
	return registryPrefix + "secrets/" + sa.Namespace + "/" + sa.Name + "-token-"
}

func (s *Store) EraseServiceAccount(ctx context.Context, endpoint string, sa tenantstore.ServiceAccount) (tenantstore.Erased, error) {
	if endpoint == "" {
		return tenantstore.Erased{}, errors.New("kineetcd: no datastore endpoint to erase the ServiceAccount from")
	}
	if sa.Namespace == "" || sa.Name == "" || strings.Contains(sa.Namespace+sa.Name, "/") {
		return tenantstore.Erased{}, fmt.Errorf("kineetcd: %q/%q does not name one ServiceAccount", sa.Namespace, sa.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: s.timeout,
		Context:     ctx,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		return tenantstore.Erased{}, fmt.Errorf("kineetcd: dial the datastore at %s: %w", endpoint, err)
	}
	defer cli.Close()

	var out tenantstore.Erased
	prefix := TokenSecretPrefix(sa)
	secrets, err := cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return out, fmt.Errorf("kineetcd: list the token Secrets of %s/%s at %s: %w", sa.Namespace, sa.Name, endpoint, err)
	}
	for _, kv := range secrets.Kvs {
		erased, err := erase(ctx, cli, string(kv.Key), kv.ModRevision)
		if err != nil {
			return out, err
		}
		if erased {
			out.Secrets = append(out.Secrets, strings.TrimPrefix(string(kv.Key), registryPrefix+"secrets/"+sa.Namespace+"/"))
		}
	}

	key := ServiceAccountKey(sa)
	account, err := cli.Get(ctx, key, clientv3.WithKeysOnly())
	if err != nil {
		return out, fmt.Errorf("kineetcd: read %s at %s: %w", key, endpoint, err)
	}
	for _, kv := range account.Kvs {
		erased, err := erase(ctx, cli, string(kv.Key), kv.ModRevision)
		if err != nil {
			return out, err
		}
		out.ServiceAccount = out.ServiceAccount || erased
	}
	return out, nil
}

func erase(ctx context.Context, cli *clientv3.Client, key string, rev int64) (bool, error) {
	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
		Then(clientv3.OpDelete(key)).
		Else(clientv3.OpGet(key, clientv3.WithKeysOnly())).
		Commit()
	if err != nil {
		return false, fmt.Errorf("kineetcd: delete %s at revision %d: %w", key, rev, err)
	}
	if resp.Succeeded {
		return true, nil
	}
	for _, r := range resp.Responses {
		if rng := r.GetResponseRange(); rng != nil && len(rng.Kvs) == 0 {
			return false, nil
		}
	}
	return false, fmt.Errorf("kineetcd: %s changed after revision %d while it was being deleted", key, rev)
}
