package clusterkcp

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

type Objects[T any] struct {
	r kube.Resource
}

var (
	_ clusterprovider.Objects[v1alpha1.HostProcess]   = Objects[v1alpha1.HostProcess]{}
	_ clusterprovider.Objects[v1alpha1.Postgres]      = Objects[v1alpha1.Postgres]{}
	_ clusterprovider.Objects[v1alpha1.OpenBaoServer] = Objects[v1alpha1.OpenBaoServer]{}
)

func NewObjects[T any](r kube.Resource) Objects[T] { return Objects[T]{r: r} }

func (o Objects[T]) Get(ctx context.Context, name string) (*T, error) {
	return kube.Get[T](ctx, o.r, name)
}

func (o Objects[T]) List(ctx context.Context, labelSelector string) ([]T, error) {
	return kube.List[T](ctx, o.r, metav1.ListOptions{LabelSelector: labelSelector})
}

func (o Objects[T]) Create(ctx context.Context, obj *T) error {
	_, err := kube.Create[T](ctx, o.r, obj, metav1.CreateOptions{})
	return err
}

func (o Objects[T]) Update(ctx context.Context, obj *T) error {
	_, err := kube.Update[T](ctx, o.r, obj, metav1.UpdateOptions{})
	return err
}

func (o Objects[T]) Delete(ctx context.Context, name string) error {
	return o.r.Delete(ctx, name, metav1.DeleteOptions{})
}

func (o Objects[T]) Await(ctx context.Context, name string, timeout time.Duration, done func(obj *T) (bool, error)) (*T, error) {
	last, err := kube.Await[T](ctx, o.r, name, timeout, done)
	return last, translate(err)
}

func (o Objects[T]) AwaitSelected(ctx context.Context, labelSelector string, timeout time.Duration, done func(objs []T) (bool, error)) error {
	return translate(kube.AwaitSelected[T](ctx, o.r, labelSelector, timeout, done))
}

type awaitTimeout struct{ err error }

func (e awaitTimeout) Error() string { return e.err.Error() }

func (e awaitTimeout) Unwrap() []error { return []error{e.err, clusterprovider.ErrAwaitTimeout} }

func translate(err error) error {
	if err != nil && errors.Is(err, kube.ErrAwaitTimeout) {
		return awaitTimeout{err: err}
	}
	return err
}
