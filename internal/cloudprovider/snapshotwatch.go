package cloudprovider

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fswatch"
)

func WatchSnapshot(ctx context.Context, path string) (<-chan struct{}, error) {
	if path == "" {
		return nil, ErrNoSnapshotPath
	}
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	w, err := fswatch.Shared().Subscribe(dir)
	if err != nil {
		return nil, fmt.Errorf("cloudprovider: watching %s, the directory of the instance snapshot: %w", dir, err)
	}
	out := make(chan struct{}, 1)
	signal := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	go func() {
		defer close(out)
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events():
				if !ok {
					return
				}
				name := filepath.Clean(ev.Name)
				if name == dir && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
					signal()
					return
				}
				if name != path || ev.Op == fsnotify.Chmod {
					continue
				}
				signal()
			case _, ok := <-w.Errors():
				if !ok {
					return
				}
				signal()
			}
		}
	}()
	return out, nil
}
