package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

var ErrAwaitTimeout = errors.New("kube: the condition did not hold in time")

func Absent[T any](obj *T) (bool, error) { return obj == nil, nil }

func Present[T any](obj *T) (bool, error) { return obj != nil, nil }

func (r Resource) ListWatch(opts metav1.ListOptions) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			ri, err := r.iface()
			if err != nil {
				return nil, err
			}
			o.LabelSelector, o.FieldSelector = opts.LabelSelector, opts.FieldSelector
			return ri.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			ri, err := r.iface()
			if err != nil {
				return nil, err
			}
			o.LabelSelector, o.FieldSelector = opts.LabelSelector, opts.FieldSelector
			return ri.Watch(ctx, o)
		},
	}
}

func expired(parent context.Context, err error, what string, timeout time.Duration) error {
	if err != nil && (wait.Interrupted(err) || errors.Is(err, watchtools.ErrWatchClosed)) {
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("%w: %s within %s", ErrAwaitTimeout, what, timeout)
	}
	return err
}

func Await[T any](ctx context.Context, r Resource, name string, timeout time.Duration, done func(obj *T) (bool, error)) (*T, error) {
	if _, err := r.iface(); err != nil {
		return nil, err
	}
	if done == nil {
		return nil, errors.New("kube: await needs a condition")
	}
	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	lw := r.ListWatch(metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()})
	var mu sync.Mutex
	var last *T
	check := func(u *unstructured.Unstructured) (bool, error) {
		if u == nil {
			mu.Lock()
			last = nil
			mu.Unlock()
			return done(nil)
		}
		cur, err := Decode[T](u.Object)
		if err != nil {
			return false, nil
		}
		mu.Lock()
		last = cur
		mu.Unlock()
		return done(cur)
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
	return seen, expired(parent, err, name, timeout)
}

func AwaitList[T any](ctx context.Context, r Resource, opts metav1.ListOptions, timeout time.Duration, done func(items []T) (bool, error)) ([]T, error) {
	if _, err := r.iface(); err != nil {
		return nil, err
	}
	if done == nil {
		return nil, errors.New("kube: await needs a condition")
	}
	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var mu sync.Mutex
	var store cache.Store
	var last []T
	check := func() (bool, error) {
		mu.Lock()
		s := store
		mu.Unlock()
		if s == nil {
			return false, nil
		}
		var objs []*unstructured.Unstructured
		for _, item := range s.List() {
			if u, ok := item.(*unstructured.Unstructured); ok {
				objs = append(objs, u)
			}
		}
		sort.Slice(objs, func(i, j int) bool {
			if objs[i].GetNamespace() != objs[j].GetNamespace() {
				return objs[i].GetNamespace() < objs[j].GetNamespace()
			}
			return objs[i].GetName() < objs[j].GetName()
		})
		items := make([]T, 0, len(objs))
		for _, u := range objs {
			v, err := Decode[T](u.Object)
			if err != nil {
				continue
			}
			items = append(items, *v)
		}
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
	_, err := watchtools.UntilWithSync(ctx, r.ListWatch(opts), &unstructured.Unstructured{}, pre, func(watch.Event) (bool, error) {
		return check()
	})
	mu.Lock()
	seen := last
	mu.Unlock()
	return seen, expired(parent, err, "the list of "+r.gvr.Resource, timeout)
}

type ListCondition func(items []any) (bool, error)

func AwaitListWatch(ctx context.Context, lw cache.ListerWatcher, objType kruntime.Object, timeout time.Duration, done ListCondition) error {
	if lw == nil {
		return errors.New("kube: await needs a list watch")
	}
	if done == nil {
		return errors.New("kube: await needs a condition")
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
	timedOut := func() error {
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("%w within %s", ErrAwaitTimeout, timeout)
	}
	if !cache.WaitForCacheSync(ctx.Done(), ctrl.HasSynced) {
		return timedOut()
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
			return timedOut()
		case <-changed:
		}
	}
}

func AwaitSelected[T any](ctx context.Context, r Resource, labelSelector string, timeout time.Duration, done func(items []T) (bool, error)) error {
	if _, err := r.iface(); err != nil {
		return err
	}
	return AwaitListWatch(ctx, r.ListWatch(metav1.ListOptions{LabelSelector: labelSelector}), &unstructured.Unstructured{}, timeout, func(items []any) (bool, error) {
		out := make([]T, 0, len(items))
		for _, item := range items {
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
		return done(out)
	})
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

func FollowResource[T any](ctx context.Context, r Resource, opts metav1.ListOptions, notify func(obj *T, deleted bool)) {
	if notify == nil {
		return
	}
	if _, err := r.iface(); err != nil {
		return
	}
	emit := func(obj any, deleted bool) {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		v, err := Decode[T](u.Object)
		if err != nil {
			return
		}
		notify(v, deleted)
	}
	_, ctrl := cache.NewInformerWithOptions(cache.InformerOptions{ListerWatcher: r.ListWatch(opts), ObjectType: &unstructured.Unstructured{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { emit(obj, false) },
			UpdateFunc: func(_, obj any) { emit(obj, false) },
			DeleteFunc: func(obj any) { emit(obj, true) },
		}})
	ctrl.RunWithContext(ctx)
}
