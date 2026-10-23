package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

type ListCondition func(items []any) (bool, error)

func AwaitListWatch(ctx context.Context, lw cache.ListerWatcher, objType kruntime.Object, timeout time.Duration, done ListCondition) error {
	if lw == nil {
		return errors.New("runtime: await needs a list watch")
	}
	if done == nil {
		return errors.New("runtime: await needs a condition")
	}
	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	changed := make(chan struct{}, 1)
	kick := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	store, ctrl := cache.NewInformerWithOptions(cache.InformerOptions{ListerWatcher: lw, ObjectType: objType,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { kick() },
			UpdateFunc: func(any, any) { kick() },
			DeleteFunc: func(any) { kick() },
		}})
	go ctrl.RunWithContext(ctx)
	expired := func() error {
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("%w within %s", ErrAwaitTimeout, timeout)
	}
	if !cache.WaitForCacheSync(ctx.Done(), ctrl.HasSynced) {
		return expired()
	}
	for {
		ok, err := done(store.List())
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return expired()
		case <-changed:
		}
	}
}

func AwaitSelected(ctx context.Context, ri dynamic.ResourceInterface, labelSelector string, timeout time.Duration, done func(objs []*unstructured.Unstructured) (bool, error)) error {
	if ri == nil {
		return errors.New("runtime: await needs a resource client")
	}
	return AwaitListWatch(ctx, SelectedListWatch(ri, labelSelector), &unstructured.Unstructured{}, timeout, func(items []any) (bool, error) {
		objs := make([]*unstructured.Unstructured, 0, len(items))
		for _, item := range items {
			if u, ok := item.(*unstructured.Unstructured); ok {
				objs = append(objs, u)
			}
		}
		return done(objs)
	})
}

func SelectedListWatch(ri dynamic.ResourceInterface, labelSelector string) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.LabelSelector = labelSelector
			return ri.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = labelSelector
			return ri.Watch(ctx, o)
		},
	}
}

func Follow(ctx context.Context, lw cache.ListerWatcher, objType kruntime.Object, notify func(obj any)) {
	if lw == nil || notify == nil {
		return
	}
	gone := func(obj any) {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		notify(obj)
	}
	_, ctrl := cache.NewInformerWithOptions(cache.InformerOptions{ListerWatcher: lw, ObjectType: objType,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    notify,
			UpdateFunc: func(_, obj any) { notify(obj) },
			DeleteFunc: gone,
		}})
	ctrl.RunWithContext(ctx)
}
