//go:build !release

package assets

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func Release() bool { return false }

func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func Ready() error { return nil }

func Resolve(opts Options, executable string) (Exec, error) {
	p := opts.Paths[executable]
	if p == "" {
		return Exec{}, fmt.Errorf("assets: this dev build embeds no assets; give %s by its explicit path option (never PATH)", executable)
	}
	if !filepath.IsAbs(p) {
		return Exec{}, fmt.Errorf("assets: %s path %q is not absolute; a dev build never searches PATH", executable, p)
	}
	st, err := os.Stat(p)
	if err != nil {
		return Exec{}, fmt.Errorf("assets: %s: %w", executable, err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return Exec{}, fmt.Errorf("assets: %s path %s is not an executable file", executable, p)
	}
	e := Exec{Name: executable, Path: p}
	if pins, err := platformPins(Platform()); err == nil {
		if pin, ok := PinFor(pins, executable); ok {
			e.Asset, e.Version = pin.Name, pin.Version
		}
	}
	return e, nil
}

func embeddedBlob(name string) bool { return false }

func embeddedBuilt(asset string) (Built, bool, error) { return Built{}, false, nil }
