//go:build release

package assets

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func Release() bool { return true }

func Platform() string { return platform }

func Ready() error {
	pins, err := platformPins(platform)
	if err != nil {
		return err
	}
	if missing := MissingBlobs(pins, blobs, blobDir); len(missing) > 0 {
		return fmt.Errorf("release build for %s is missing embedded assets %s; it refuses to start (stage them with scripts/stage-assets.sh, then rebuild with -tags release)",
			platform, strings.Join(missing, ", "))
	}
	return nil
}

func Resolve(opts Options, executable string) (Exec, error) {
	if err := Ready(); err != nil {
		return Exec{}, err
	}
	pins, err := platformPins(platform)
	if err != nil {
		return Exec{}, err
	}
	pin, ok := PinFor(pins, executable)
	if !ok {
		return Exec{}, fmt.Errorf("assets: no embedded asset carries %q on %s", executable, platform)
	}
	if opts.Cache == "" {
		return Exec{}, fmt.Errorf("assets: asset-cache is unset; install points it at <state>/runtime, dev at the per-user cache")
	}
	return Store{Root: opts.Cache, Log: opts.Log}.Resolve(pin, FSBlobs(blobs, blobDir), executable)
}

type Verified struct {
	Asset       string            `json:"asset"`
	Version     string            `json:"version"`
	TreeSHA256  string            `json:"treeSHA256"`
	Executables map[string]string `json:"executables"`
}

func VerifyEmbedded(cache string) ([]Verified, error) {
	if err := Ready(); err != nil {
		return nil, err
	}
	pins, err := platformPins(platform)
	if err != nil {
		return nil, err
	}
	out := make([]Verified, 0, len(pins))
	for _, p := range pins {
		v := Verified{Asset: p.Name, Version: p.Version, TreeSHA256: p.TreeSHA256, Executables: map[string]string{}}
		for exe := range p.Executables {
			e, err := Resolve(Options{Cache: cache}, exe)
			if err != nil {
				return out, fmt.Errorf("%s/%s: %w", p.Name, exe, err)
			}
			st, err := os.Stat(e.Path)
			if err != nil || st.Mode().Perm()&0o111 == 0 {
				return out, fmt.Errorf("%s/%s at %s is not an executable file: %v", p.Name, exe, e.Path, err)
			}
			if e.SHA256 != p.TreeSHA256 {
				return out, fmt.Errorf("%s/%s resolved tree %s, pin %s", p.Name, exe, e.SHA256, p.TreeSHA256)
			}
			v.Executables[exe] = e.Path
		}
		got, err := TreeDigest(filepath.Join(cache, p.TreeSHA256))
		if err != nil || got != p.TreeSHA256 {
			return out, fmt.Errorf("%s: extracted tree %s, pin %s (%v)", p.Name, got, p.TreeSHA256, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func embeddedBlob(name string) bool {
	_, err := fs.Stat(blobs, blobDir+"/"+BlobName(name))
	return err == nil
}
