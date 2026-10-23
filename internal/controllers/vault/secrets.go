package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const SecretRenew = 5 * time.Minute

var ErrNoVault = errors.New("vault: the secret ref names no Vault, mount and path")

func SecretScope(ref commonv1.SecretRef) string {
	return strings.Trim(strings.TrimSpace(ref.Mount), "/") + "/" + strings.Trim(strings.TrimSpace(ref.Path), "/")
}

func secretTarget(from commonv1.ObjectRef, ref commonv1.SecretRef) (commonv1.ObjectRef, string, string, error) {
	mount, path := strings.Trim(strings.TrimSpace(ref.Mount), "/"), strings.Trim(strings.TrimSpace(ref.Path), "/")
	if ref.Vault == nil || strings.TrimSpace(ref.Vault.Name) == "" || mount == "" || path == "" {
		return commonv1.ObjectRef{}, "", "", ErrNoVault
	}
	to := *ref.Vault
	if strings.TrimSpace(to.Kind) == "" {
		to.Kind = VaultGroupKind.Kind
	}
	if strings.TrimSpace(to.Group) == "" {
		to.Group = VaultGroupKind.Group
	}
	to.Workspace = to.InWorkspace(from.Workspace)
	return to, mount, path, nil
}

func ReadSecret(ctx context.Context, b *bus.Bus, from commonv1.ObjectRef, ref commonv1.SecretRef) (map[string]any, error) {
	to, mount, path, err := secretTarget(from, ref)
	if err != nil {
		return nil, err
	}
	kv, err := bus.Resolve[contractvault.KV](ctx, b, from, to,
		bus.Capability{Name: contractvault.CapabilityRead, Scope: []string{mount + "/" + path}})
	if err != nil {
		return nil, fmt.Errorf("vault: %s reading %s/%s from %s: %w", from.String(), mount, path, to.String(), err)
	}
	return kv.Read(ctx, mount, path, 0)
}

func SecretValue(data map[string]any, key string, fallbacks ...string) string {
	keys := fallbacks
	if key = strings.TrimSpace(key); key != "" {
		keys = []string{key}
	}
	for _, k := range keys {
		if value, ok := data[k].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func WriteSecret(ctx context.Context, b *bus.Bus, from commonv1.ObjectRef, ref commonv1.SecretRef, data map[string]any) error {
	to, mount, path, err := secretTarget(from, ref)
	if err != nil {
		return err
	}
	kv, err := bus.Resolve[contractvault.KV](ctx, b, from, to,
		bus.Capability{Name: contractvault.CapabilityWrite, Scope: []string{mount + "/" + path}})
	if err != nil {
		return fmt.Errorf("vault: %s writing %s/%s to %s: %w", from.String(), mount, path, to.String(), err)
	}
	return kv.Write(ctx, mount, path, data)
}
