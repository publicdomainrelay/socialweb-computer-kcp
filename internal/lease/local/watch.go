package local

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

func Watch(ctx context.Context, paths ...string) (<-chan struct{}, error) {
	if len(paths) == 0 {
		return nil, errors.New("local: a watch needs at least one file")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("local: opening a file watch: %w", err)
	}
	files := make(map[string]bool, len(paths))
	dirs := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = filepath.Clean(p)
		files[p] = true
		dir := filepath.Dir(p)
		if dirs[dir] {
			continue
		}
		if err := w.Add(dir); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("local: watching %s: %w", dir, err)
		}
		dirs[dir] = true
	}
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !files[filepath.Clean(ev.Name)] || ev.Op == fsnotify.Chmod {
					continue
				}
				select {
				case out <- struct{}{}:
				default:
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return out, nil
}
