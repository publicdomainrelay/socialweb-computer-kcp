package logcap

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fswatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultCap   = 64 << 20
	DefaultEvery = time.Minute
	OldSuffix    = ".1"
)

func Rotate(path string, capBytes int64) (bool, error) {
	if capBytes <= 0 {
		return false, nil
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() <= capBytes {
		return false, nil
	}
	in, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer in.Close()
	if _, err := in.Seek(-capBytes, io.SeekEnd); err != nil {
		return false, err
	}
	out, err := os.OpenFile(path+OldSuffix, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return false, err
	}
	_, cerr := io.Copy(out, in)
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return false, cerr
	}
	return true, os.Truncate(path, 0)
}

type Config struct {
	Dirs  []string
	Files []string
	Cap   int64
	Every time.Duration
	Log   *slog.Logger
}

func (c Config) paths() []string {
	out := append([]string(nil), c.Files...)
	for _, d := range c.Dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".log") {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}

func (c Config) Pass() []string {
	capBytes := c.Cap
	capBytes = cmp.Or(capBytes, DefaultCap)
	var rotated []string
	for _, p := range c.paths() {
		ok, err := Rotate(p, capBytes)
		if err != nil && c.Log != nil {
			c.Log.Warn("a log file over its cap was not rotated", "file", p, "err", err.Error())
		}
		if ok {
			rotated = append(rotated, p)
		}
	}
	return rotated
}

func (c Config) watched() []string {
	dirs := append([]string(nil), c.Dirs...)
	for _, f := range c.Files {
		if d := filepath.Dir(f); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func (c Config) tracked(path string) bool {
	if slices.Contains(c.Files, path) {
		return true
	}
	return strings.HasSuffix(path, ".log") && slices.Contains(c.Dirs, filepath.Dir(path))
}

func (c Config) warn(msg string, args ...any) {
	if c.Log != nil {
		c.Log.Warn(msg, args...)
	}
}

func Keep(ctx context.Context, c Config) {
	pass := func() {
		if rotated := c.Pass(); len(rotated) > 0 && c.Log != nil {
			c.Log.Info("log files over their cap were rotated; each keeps its newest bytes in a .1 copy", "files", rotated)
		}
	}
	pass()
	w, err := fswatch.Shared().Subscribe()
	if err != nil {
		c.warn("log files are not watched; they are capped only at start", "err", err.Error())
		return
	}
	defer w.Close()
	for _, d := range c.watched() {
		if err := w.Add(d); err != nil {
			c.warn("a log directory is not watched", "dir", d, "err", err.Error())
		}
	}
	d := debounce.New(opt.Positive(c.Every, DefaultEvery), pass)
	defer d.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			if ev.Has(fsnotify.Write|fsnotify.Create) && c.tracked(ev.Name) {
				d.Mark()
			}
		case err, ok := <-w.Errors():
			if !ok {
				return
			}
			c.warn("watching the log files", "err", err.Error())
			d.Mark()
		}
	}
}
