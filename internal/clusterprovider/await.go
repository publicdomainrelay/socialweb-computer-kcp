package clusterprovider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fswatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

func timedOut(err error) bool { return errors.Is(err, ErrAwaitTimeout) }

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
		w, err := fswatch.Shared().Subscribe(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("clusterprovider: watching the output of %s: %w", path, err)
		}
		defer w.Close()
		events, errs = w.Events(), w.Errors()
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
