package assets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	tmpPrefix   = ".tmp-"
	stalePrefix = ".stale-"
	lockPrefix  = ".lock-"
	blobSuffix  = ".tar.zst"
)

var digestName = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Exec struct {
	Name    string   `json:"name"`
	Asset   string   `json:"asset"`
	Version string   `json:"version"`
	SHA256  string   `json:"sha256"`
	Path    string   `json:"path"`
	Env     []string `json:"env,omitempty"`
}

type BlobSource func(name string) (io.ReadCloser, error)

type Store struct {
	Root string
	Log  *slog.Logger
}

func (s Store) Dir(pin versions.Pin) string { return filepath.Join(s.Root, pin.TreeSHA256) }

func (s Store) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s Store) Ensure(pin versions.Pin, open BlobSource) (string, error) {
	if !digestName.MatchString(pin.TreeSHA256) {
		return "", fmt.Errorf("assets: %s on %s has no tree sha256 (a versions.json pin, or for an asset built from this repository the build record beside its blob)", pin.Name, pin.Platform)
	}
	if !filepath.IsAbs(s.Root) {
		return "", fmt.Errorf("assets: the extraction root %q is not absolute", s.Root)
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return "", err
	}
	dst := s.Dir(pin)
	if got, err := TreeDigest(dst); err == nil && got == pin.TreeSHA256 {
		touch(dst)
		return dst, nil
	}
	unlock, err := lockPath(filepath.Join(s.Root, lockPrefix+pin.TreeSHA256))
	if err != nil {
		return "", err
	}
	defer unlock()
	got, err := TreeDigest(dst)
	switch {
	case err == nil && got == pin.TreeSHA256:
		touch(dst)
		return dst, nil
	case err == nil:
		s.log().Warn("extracted asset no longer matches its pin; replacing it", "asset", pin.Name, "dir", dst, "pin", pin.TreeSHA256, "got", got)
	case !errors.Is(err, fs.ErrNotExist):
		s.log().Warn("extracted asset unreadable; replacing it", "asset", pin.Name, "dir", dst, "err", err.Error())
	}
	tmp := filepath.Join(s.Root, tmpPrefix+randomSuffix())
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return "", err
	}
	if err := s.unpack(pin, open, tmp); err != nil {
		removeTree(tmp)
		return "", err
	}
	if _, err := os.Lstat(dst); err == nil {
		stale := filepath.Join(s.Root, stalePrefix+randomSuffix())
		if err := os.Rename(dst, stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			removeTree(tmp)
			return "", fmt.Errorf("assets: moving the mismatched %s aside: %w", dst, err)
		}
		defer removeTree(stale)
	}
	if err := os.Rename(tmp, dst); err != nil {
		removeTree(tmp)
		if again, derr := TreeDigest(dst); derr == nil && again == pin.TreeSHA256 {
			return dst, nil
		}
		return "", fmt.Errorf("assets: placing %s: %w", dst, err)
	}
	return dst, nil
}

func (s Store) unpack(pin versions.Pin, open BlobSource, tmp string) error {
	rc, err := open(pin.Name)
	if err != nil {
		return fmt.Errorf("assets: %s: the blob for %s is missing from this build: %w", pin.Platform, pin.Name, err)
	}
	defer rc.Close()
	zr, err := zstd.NewReader(rc, zstd.WithDecoderMaxWindow(1<<30), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := extractTar(zr, tmp); err != nil {
		return fmt.Errorf("assets: unpacking %s: %w", pin.Name, err)
	}
	got, err := TreeDigest(tmp)
	if err != nil {
		return err
	}
	if got != pin.TreeSHA256 {
		return fmt.Errorf("assets: the embedded blob of %s unpacks to tree %s, but its pin (versions.json, or the build record of a built asset) is %s", pin.Name, got, pin.TreeSHA256)
	}
	return nil
}

func (s Store) Resolve(pin versions.Pin, open BlobSource, executable string) (Exec, error) {
	rel, ok := pin.Executables[executable]
	if !ok {
		return Exec{}, fmt.Errorf("assets: asset %s carries no executable %q", pin.Name, executable)
	}
	dir, err := s.Ensure(pin, open)
	if err != nil {
		return Exec{}, err
	}
	e := Exec{Name: executable, Asset: pin.Name, Version: pin.Version, SHA256: pin.TreeSHA256,
		Path: filepath.Join(dir, filepath.FromSlash(rel))}
	if pin.LibraryPath != "" {
		e.Env = []string{"LD_LIBRARY_PATH=" + filepath.Join(dir, filepath.FromSlash(pin.LibraryPath))}
	}
	return e, nil
}

func PinFor(pins []versions.Pin, executable string) (versions.Pin, bool) {
	for _, p := range pins {
		if _, ok := p.Executables[executable]; ok {
			return p, true
		}
	}
	return versions.Pin{}, false
}

func BlobName(asset string) string { return asset + blobSuffix }

func FSBlobs(fsys fs.FS, dir string) BlobSource {
	return func(name string) (io.ReadCloser, error) {
		return fsys.Open(strings.TrimPrefix(dir+"/"+BlobName(name), "/"))
	}
}

func MissingBlobs(pins []versions.Pin, fsys fs.FS, dir string) []string {
	var missing []string
	for _, p := range pins {
		st, err := fs.Stat(fsys, strings.TrimPrefix(dir+"/"+BlobName(p.Name), "/"))
		if err != nil || st.Size() == 0 {
			missing = append(missing, p.Name)
		}
	}
	return missing
}

func touch(dir string) {
	now := time.Now()
	os.Chtimes(dir, now, now)
}

func randomSuffix() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
