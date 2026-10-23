package runtime

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

func AwaitAs[T any](ctx context.Context, ri dynamic.ResourceInterface, name string, timeout time.Duration, done func(obj *T) (bool, error)) (*T, error) {
	var last *T
	_, err := Await(ctx, ri, name, timeout, func(u *unstructured.Unstructured) (bool, error) {
		if u == nil {
			return done(nil)
		}
		cur := new(T)
		if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cur); err != nil {
			return false, nil
		}
		last = cur
		return done(cur)
	})
	return last, err
}

func Present[T any](obj *T) (bool, error) { return obj != nil, nil }
