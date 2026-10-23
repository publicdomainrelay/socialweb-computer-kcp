package kube

import (
	"context"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

type Event[T any] struct {
	Type   watch.EventType
	Object *T
}

func Watch[T any](ctx context.Context, r Resource, opts metav1.ListOptions) (<-chan Event[T], func(), error) {
	ri, err := r.iface()
	if err != nil {
		return nil, nil, err
	}
	w, err := ri.Watch(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan Event[T])
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			w.Stop()
		})
	}
	go func() {
		defer close(out)
		for ev := range w.ResultChan() {
			e := Event[T]{Type: ev.Type}
			if u, ok := ev.Object.(*unstructured.Unstructured); ok {
				v, err := Decode[T](u.Object)
				if err != nil {
					continue
				}
				e.Object = v
			} else if ev.Type != watch.Error {
				continue
			}
			select {
			case out <- e:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, stop, nil
}

type Cache[T any] struct {
	store cache.Store
	ctrl  cache.Controller
}

func NewCache[T any](r Resource, opts metav1.ListOptions, changed func()) (*Cache[T], error) {
	if _, err := r.iface(); err != nil {
		return nil, err
	}
	kick := func() {
		if changed != nil {
			changed()
		}
	}
	store, ctrl := cache.NewInformerWithOptions(cache.InformerOptions{ListerWatcher: r.ListWatch(opts), ObjectType: &unstructured.Unstructured{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { kick() },
			UpdateFunc: func(any, any) { kick() },
			DeleteFunc: func(any) { kick() },
		}})
	return &Cache[T]{store: store, ctrl: ctrl}, nil
}

func (c *Cache[T]) Run(ctx context.Context) { c.ctrl.RunWithContext(ctx) }

func (c *Cache[T]) HasSynced() bool { return c.ctrl.HasSynced() }

func (c *Cache[T]) List() []T {
	var out []T
	for _, item := range c.store.List() {
		u, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		v, err := Decode[T](u.Object)
		if err != nil {
			continue
		}
		out = append(out, *v)
	}
	return out
}
