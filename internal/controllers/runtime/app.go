package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

type AwaitListCondition func(items []*unstructured.Unstructured) (bool, error)

func AwaitList(ctx context.Context, ri dynamic.ResourceInterface, opts metav1.ListOptions, timeout time.Duration, done AwaitListCondition) ([]*unstructured.Unstructured, error) {
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
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.LabelSelector, o.FieldSelector = opts.LabelSelector, opts.FieldSelector
			return ri.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector, o.FieldSelector = opts.LabelSelector, opts.FieldSelector
			return ri.Watch(ctx, o)
		},
	}
	var mu sync.Mutex
	var store cache.Store
	var last []*unstructured.Unstructured
	check := func() (bool, error) {
		mu.Lock()
		s := store
		mu.Unlock()
		if s == nil {
			return false, nil
		}
		var items []*unstructured.Unstructured
		for _, item := range s.List() {
			if u, ok := item.(*unstructured.Unstructured); ok {
				items = append(items, u)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].GetNamespace() != items[j].GetNamespace() {
				return items[i].GetNamespace() < items[j].GetNamespace()
			}
			return items[i].GetName() < items[j].GetName()
		})
		mu.Lock()
		last = items
		mu.Unlock()
		return done(items)
	}
	pre := func(s cache.Store) (bool, error) {
		mu.Lock()
		store = s
		mu.Unlock()
		return check()
	}
	_, err := watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, pre, func(watch.Event) (bool, error) {
		return check()
	})
	mu.Lock()
	seen := last
	mu.Unlock()
	if err != nil && (wait.Interrupted(err) || errors.Is(err, watchtools.ErrWatchClosed)) {
		if parent.Err() != nil {
			return seen, parent.Err()
		}
		return seen, fmt.Errorf("%w: the list within %s", ErrAwaitTimeout, timeout)
	}
	return seen, err
}

func BackoffDelay(base, max time.Duration, failures int) time.Duration {
	return newBackoff(base, max).delay(failures)
}
