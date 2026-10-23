package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component/ports"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/lease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/subnetallocator"
)

type Ports interface {
	Allocate(name string) (ports.Lease, error)

	Release(name string) error

	Leases() []ports.Lease
}

type Subnets interface {
	Allocate(req subnetallocator.Request) (subnetallocator.Lease, error)

	Release(cluster string) error

	Leases() []subnetallocator.Lease
}

type Front interface {
	Status(ctx context.Context) (apiserverfront.Status, error)
}

type Rolls = lease.Rolls

var (
	_ Ports = (*ports.Registry)(nil)

	_ Subnets = (*subnetallocator.Allocator)(nil)

	_ Front = apiserverfront.Client{}

	_ lease.Rolls = SlotFile{}
)

type Home struct {
	Workspace string

	Cluster string

	Purpose string
}

func Key(workspace, cluster, purpose, name string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{workspace, cluster, purpose} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(name)
	}
	return strings.Join(parts, "/")
}

func ParseKey(key string) (Home, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Home{}, false
	}
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	var h Home
	switch n := len(parts); {
	case n >= 3:
		h = Home{Workspace: strings.Join(parts[:n-2], "/"), Cluster: parts[n-2], Purpose: parts[n-1]}
	case n == 2:
		h = Home{Cluster: parts[0], Purpose: parts[1]}
	default:
		h = Home{Cluster: parts[0]}
	}
	if Key(h.Workspace, h.Cluster, h.Purpose, "") != key {
		return Home{}, false
	}
	return h, true
}

func (h Home) In(workspace string, clusters map[string]bool) bool {
	if h.Workspace != "" {
		return h.Workspace == workspace
	}
	return clusters[h.Cluster]
}

func MirrorName(kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return kind + "-" + hex.EncodeToString(sum[:])[:16]
}

func HeldBy(name, workspace, cluster string) bool {
	name = strings.TrimSpace(name)
	workspace = strings.TrimSpace(workspace)
	cluster = strings.TrimSpace(cluster)
	if name == "" || cluster == "" {
		return false
	}
	if name == cluster || strings.HasPrefix(name, cluster+"/") {
		return true
	}
	if workspace == "" {
		return false
	}
	key := workspace + "/" + cluster
	return name == key || strings.HasPrefix(name, key+"/")
}
