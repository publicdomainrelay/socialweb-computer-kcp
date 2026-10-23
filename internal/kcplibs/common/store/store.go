package store

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

func Unchanged(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

type Reader[T any] interface {
	Get(ctx context.Context, r ref.Ref) (*T, error)

	List(ctx context.Context, logicalCluster string) ([]T, error)
}

type Writer[T any] interface {
	Create(ctx context.Context, logicalCluster string, obj *T) error

	Delete(ctx context.Context, r ref.Ref) error

	PatchStatus(ctx context.Context, r ref.Ref, patch []byte) error

	RemoveFinalizer(ctx context.Context, r ref.Ref, finalizer string) error
}

type Resource[T any] interface {
	Reader[T]

	Writer[T]
}

type TokenMinter interface {
	MintServiceAccountToken(ctx context.Context, logicalCluster, namespace, name string, ttl time.Duration) (string, error)
}
