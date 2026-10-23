package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

var ErrAwaitTimeout = errors.New("runtime: the condition did not hold in time")

type AwaitCondition func(obj *unstructured.Unstructured) (bool, error)

func Await(ctx context.Context, ri dynamic.ResourceInterface, name string, timeout time.Duration, done AwaitCondition) (*unstructured.Unstructured, error) {
	if ri == nil {
		return nil, errors.New("runtime: await needs a resource client")
	}
	if done == nil {
		return nil, errors.New("runtime: await needs a condition")
	}
	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.FieldSelector = selector
			return ri.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = selector
			return ri.Watch(ctx, o)
		},
	}
	var mu sync.Mutex
	var last *unstructured.Unstructured
	check := func(obj *unstructured.Unstructured) (bool, error) {
		mu.Lock()
		last = obj
		mu.Unlock()
		return done(obj)
	}
	pre := func(store cache.Store) (bool, error) {
		for _, item := range store.List() {
			if u, ok := item.(*unstructured.Unstructured); ok && u.GetName() == name {
				return check(u)
			}
		}
		return check(nil)
	}
	_, err := watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, pre, func(ev watch.Event) (bool, error) {
		switch ev.Type {
		case watch.Deleted:
			return check(nil)
		case watch.Added, watch.Modified:
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				return false, nil
			}
			return check(u)
		}
		return false, nil
	})
	mu.Lock()
	seen := last
	mu.Unlock()
	if err != nil && (wait.Interrupted(err) || errors.Is(err, watchtools.ErrWatchClosed)) {
		if parent.Err() != nil {
			return seen, parent.Err()
		}
		return seen, fmt.Errorf("%w: %s within %s", ErrAwaitTimeout, name, timeout)
	}
	return seen, err
}

func Absent(obj *unstructured.Unstructured) (bool, error) { return obj == nil, nil }
