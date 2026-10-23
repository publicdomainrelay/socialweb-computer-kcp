package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

func awaitObject[T any](ctx context.Context, ri dynamic.ResourceInterface, name string, timeout time.Duration, done func(o *T) (bool, error)) (*T, error) {
	last, err := ctlruntime.Await(ctx, ri, name, timeout, func(u *unstructured.Unstructured) (bool, error) {
		if u == nil {
			return done(nil)
		}
		o, err := unstruct.From[T](u)
		if err != nil {
			return false, err
		}
		return done(&o)
	})
	if last == nil {
		return nil, err
	}
	o, derr := unstruct.From[T](last)
	if derr != nil {
		return nil, errors.Join(err, derr)
	}
	return &o, err
}

func awaitSelected[T any](ctx context.Context, ri dynamic.ResourceInterface, selector string, timeout time.Duration, done func(objs []T) (bool, error)) error {
	return ctlruntime.AwaitSelected(ctx, ri, selector, timeout, func(items []*unstructured.Unstructured) (bool, error) {
		objs := make([]T, 0, len(items))
		for _, u := range items {
			o, err := unstruct.From[T](u)
			if err != nil {
				return false, err
			}
			objs = append(objs, o)
		}
		return done(objs)
	})
}

func awaitTyped[T any](ctx context.Context, lw cache.ListerWatcher, proto kruntime.Object, timeout time.Duration, done func(objs []T) (bool, error)) error {
	return ctlruntime.AwaitListWatch(ctx, lw, proto, timeout, func(items []any) (bool, error) {
		objs := make([]T, 0, len(items))
		for _, item := range items {
			if o, ok := item.(T); ok {
				objs = append(objs, o)
			}
		}
		return done(objs)
	})
}

func timedOut(err error) bool { return errors.Is(err, ctlruntime.ErrAwaitTimeout) }

type logged interface {
	LogPath() string
}

func (m unitMember) LogPath() string { return m.path }

func untilOutput(ctx context.Context, m procgroup.Member, done func(out string) (bool, error), timeout func() error) error {
	path := ""
	if l, ok := m.(logged); ok {
		path = filepath.Clean(l.LogPath())
	}
	var events <-chan fsnotify.Event
	var errs <-chan error
	if path != "" && path != "." {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return fmt.Errorf("clusterprovider: watching the output of %s: %w", path, err)
		}
		defer w.Close()
		if err := w.Add(filepath.Dir(path)); err != nil {
			return fmt.Errorf("clusterprovider: watching the output of %s: %w", path, err)
		}
		events, errs = w.Events, w.Errors
	}
	for {
		ok, err := done(m.Output())
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		for wait := true; wait; {
			select {
			case <-ctx.Done():
				return timeout()
			case ev, open := <-events:
				if !open {
					events = nil
					continue
				}
				wait = filepath.Clean(ev.Name) != path || ev.Op == fsnotify.Chmod
			case _, open := <-errs:
				if !open {
					errs = nil
				}
			}
		}
	}
}
