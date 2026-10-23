package postgresctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	PidFileName = "postmaster.pid"

	pidStatusLine = 7

	pidStatusReady = "ready"
)

func (h *Host) PidFile(o v1alpha1.Postgres) string {
	return filepath.Join(h.DataDir(o), PidFileName)
}

func (h *Host) Accepting(o v1alpha1.Postgres) bool {
	raw, err := os.ReadFile(h.PidFile(o))
	if err != nil {
		return false
	}
	lines := strings.Split(string(raw), "\n")
	return len(lines) > pidStatusLine && strings.TrimSpace(lines[pidStatusLine]) == pidStatusReady
}

func (h *Host) PidChanges(ctx context.Context, o v1alpha1.Postgres) (<-chan struct{}, error) {
	file := filepath.Clean(h.PidFile(o))
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("postgresctl: opening a watch on %s: %w", file, err)
	}
	if err := w.Add(filepath.Dir(file)); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("postgresctl: watching %s: %w", filepath.Dir(file), err)
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
				if filepath.Clean(ev.Name) != file || ev.Op == fsnotify.Chmod {
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
