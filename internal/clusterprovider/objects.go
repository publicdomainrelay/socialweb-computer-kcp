package clusterprovider

import (
	"context"
	"errors"
	"time"
)

var ErrAwaitTimeout = errors.New("clusterprovider: the condition did not hold in time")

type Objects[T any] interface {
	Get(ctx context.Context, name string) (*T, error)

	List(ctx context.Context, labelSelector string) ([]T, error)

	Create(ctx context.Context, obj *T) error

	Update(ctx context.Context, obj *T) error

	Delete(ctx context.Context, name string) error

	Await(ctx context.Context, name string, timeout time.Duration, done func(obj *T) (bool, error)) (*T, error)

	AwaitSelected(ctx context.Context, labelSelector string, timeout time.Duration, done func(objs []T) (bool, error)) error
}

func absent[T any](obj *T) (bool, error) { return obj == nil, nil }
