package vault

import (
	"context"
	"time"
)

const (
	CapabilityRead = "vault.read"

	CapabilityWrite = "vault.write"

	CapabilityLease = "vault.lease"

	CapabilityPKIIssue = "vault.pki.issue"
)

type KV interface {
	Read(ctx context.Context, mount, path string, version int) (map[string]any, error)

	Write(ctx context.Context, mount, path string, data map[string]any) error

	Delete(ctx context.Context, mount, path string) error
}

type LeaseRequest struct {
	Role string

	TTL time.Duration
}

type Lease struct {
	ID string

	Role string

	Renewable bool

	ExpiresAt time.Time
}

type Leases interface {
	Acquire(ctx context.Context, req LeaseRequest) (Lease, error)

	Renew(ctx context.Context, id string, increment time.Duration) (Lease, error)

	Revoke(ctx context.Context, id string) error
}

type PKI interface {
	Issue(ctx context.Context, mount, role string, req CertRequest) (Cert, error)

	CAChain(ctx context.Context, mount string) (string, error)
}

type Trust interface {
	CABundle(ctx context.Context) ([]byte, error)

	Watch(fn func(bundle []byte)) (cancel func())
}

type Keys interface {
	RotateKey(ctx context.Context, name string) error
}
