package guestimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestrootfs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/progress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	KernelName   = "vmlinux"
	RootfsName   = "rootfs.ext4"
	ManifestName = "image.json"
	SHA256HexLen = 64
)

type FileRecord struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type Input struct {
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	VMM             string           `json:"vmm"`
	BootArgs        []string         `json:"bootArgs"`
	Vcpu            int              `json:"vcpu"`
	MemMiB          int              `json:"memMiB"`
	GuestInterface  string           `json:"guestInterface"`
	Target          string           `json:"target"`
	Hypervisor      string           `json:"hypervisor"`
	Arch            string           `json:"arch"`
	KernelVersion   string           `json:"kernelVersion"`
	Kernel          FileRecord       `json:"kernel"`
	Rootfs          FileRecord       `json:"rootfs"`
	RootfsLabel     string           `json:"rootfsLabel"`
	RootfsTarSHA256 string           `json:"rootfsTarSha256"`
	SourceDateEpoch int64            `json:"sourceDateEpoch"`
	Bake            guestrootfs.Bake `json:"bake"`
	Inputs          map[string]Input `json:"inputs"`
	Marker          string           `json:"marker,omitempty"`
}

type FilePin = versions.GuestImageFile

type Pin struct {
	Target     string
	Hypervisor string
	Arch       string
	Files      map[string]FilePin
}

func PinFor(target string) (Pin, error) {
	g, err := versions.GuestImages()
	if err != nil {
		return Pin{}, err
	}
	t, ok := g.Targets[target]
	if !ok {
		if plan, reserved := g.Reserved[target]; reserved {
			return Pin{}, fmt.Errorf("guestimage: %s is reserved for %s and has no image yet", target, plan)
		}
		names := make([]string, 0, len(g.Targets))
		for n := range g.Targets {
			names = append(names, n)
		}
		sort.Strings(names)
		return Pin{}, fmt.Errorf("guestimage: no guest image target %s; the pinned targets are %s",
			target, strings.Join(names, ", "))
	}
	return Pin{Target: target, Hypervisor: t.Hypervisor, Arch: t.Arch, Files: t.Outputs}, nil
}

type Store struct {
	Root   string
	Client *http.Client
	Out    io.Writer
	TTY    bool
}

type Result struct {
	Dir     string
	Fetched bool
}

func (s Store) Dir(pin Pin) string {
	return filepath.Join(s.Root, pin.Hypervisor, pin.Arch, pin.Files[ManifestName].SHA256)
}

func (p Pin) Valid() error {
	if p.Hypervisor == "" || p.Arch == "" {
		return fmt.Errorf("guestimage: the pin for %s names no hypervisor or arch", p.Target)
	}
	for _, name := range []string{KernelName, RootfsName, ManifestName} {
		f, ok := p.Files[name]
		if !ok || len(f.SHA256) != SHA256HexLen {
			return fmt.Errorf("guestimage: %s has no sha256 pin for %s in versions.json, so there is "+
				"nothing to verify a fetched copy against: build it with scripts/build-guest-image.sh "+
				"and record its outputs", p.Target, name)
		}
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

const VerifiedName = ".verified"

type verifiedFile struct {
	SHA256 string `json:"sha256"`

	Bytes int64 `json:"bytes"`

	ModNs int64 `json:"modNs"`
}

func readVerified(dir string) map[string]verifiedFile {
	raw, err := os.ReadFile(filepath.Join(dir, VerifiedName))
	if err != nil {
		return nil
	}
	var out map[string]verifiedFile
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func writeVerified(dir string, files map[string]verifiedFile) {
	raw, err := json.Marshal(files)
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, VerifiedName+".part")
	if os.WriteFile(tmp, raw, 0o644) != nil {
		return
	}
	if os.Rename(tmp, filepath.Join(dir, VerifiedName)) != nil {
		os.Remove(tmp)
	}
}

func (s Store) Verify(pin Pin) (string, error) {
	if err := pin.Valid(); err != nil {
		return "", err
	}
	dir := s.Dir(pin)
	held := readVerified(dir)
	files := map[string]verifiedFile{}
	changed := false
	for _, name := range []string{ManifestName, KernelName, RootfsName} {
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		want := pin.Files[name]
		if v, ok := held[name]; ok && v.SHA256 == want.SHA256 && v.Bytes == fi.Size() && v.ModNs == fi.ModTime().UnixNano() {
			files[name] = v
			continue
		}
		got, n, err := hashFile(path)
		if err != nil {
			return "", err
		}
		if got != want.SHA256 {
			return "", fmt.Errorf("guestimage: the stored %s of %s hashes to %s and is pinned at %s",
				name, pin.Target, got, want.SHA256)
		}
		files[name] = verifiedFile{SHA256: got, Bytes: n, ModNs: fi.ModTime().UnixNano()}
		changed = true
	}
	if changed {
		writeVerified(dir, files)
	}
	return dir, nil
}

func (s Store) open(ctx context.Context, source, name string) (io.ReadCloser, error) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("guestimage: the source %q: %w", source, err)
	}
	switch u.Scheme {
	case "file":
		return os.Open(filepath.Join(u.Path, name))
	case "http", "https":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(source, "/")+"/"+name, nil)
		if err != nil {
			return nil, err
		}
		client := s.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s answered %s", req.URL, resp.Status)
		}
		return resp.Body, nil
	default:
		return nil, fmt.Errorf("guestimage: the source %q is neither file:// nor http(s)://", source)
	}
}

func (s Store) Ensure(ctx context.Context, pin Pin, source string) (Result, error) {
	if err := pin.Valid(); err != nil {
		return Result{}, err
	}
	dir := s.Dir(pin)
	if _, err := os.Stat(dir); err == nil {
		if _, err := s.Verify(pin); err != nil {
			return Result{}, fmt.Errorf("%w -- the store holds this image under its pinned name and "+
				"its bytes no longer match, so it is refused rather than booted; remove %s to fetch "+
				"it again", err, dir)
		}
		return Result{Dir: dir}, nil
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return Result{}, fmt.Errorf("guestimage: %w", err)
	}
	tmp, err := os.MkdirTemp(s.Root, ".fetch-")
	if err != nil {
		return Result{}, fmt.Errorf("guestimage: %w", err)
	}
	defer os.RemoveAll(tmp)
	for _, name := range []string{ManifestName, KernelName, RootfsName} {
		if err := s.fetchOne(ctx, pin, source, name, filepath.Join(tmp, name)); err != nil {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Result{}, fmt.Errorf("guestimage: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Result{}, fmt.Errorf("guestimage: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			if _, err := s.Verify(pin); err == nil {
				return Result{Dir: dir}, nil
			}
		}
		return Result{}, fmt.Errorf("guestimage: placing %s: %w", dir, err)
	}
	stamped := map[string]verifiedFile{}
	for _, name := range []string{ManifestName, KernelName, RootfsName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return Result{Dir: dir, Fetched: true}, nil
		}
		stamped[name] = verifiedFile{SHA256: pin.Files[name].SHA256, Bytes: fi.Size(), ModNs: fi.ModTime().UnixNano()}
	}
	writeVerified(dir, stamped)
	return Result{Dir: dir, Fetched: true}, nil
}

func localSource(source string) bool {
	u, err := url.Parse(source)
	return err == nil && u.Scheme == "file"
}

func (s Store) fetchOne(ctx context.Context, pin Pin, source, name, dst string) error {
	in, err := s.open(ctx, source, name)
	if err != nil {
		return fmt.Errorf("guestimage: fetching %s of %s from %s: %w", name, pin.Target, source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("guestimage: %w", err)
	}
	want := pin.Files[name]
	h := sha256.New()
	pw := progress.New(name, want.Bytes, s.Out, s.TTY && !localSource(source), 0)
	defer pw.Finish()
	var src io.Reader = in
	if want.Bytes > 0 {
		src = io.LimitReader(in, want.Bytes+1)
	}
	n, err := io.Copy(io.MultiWriter(out, h, pw), src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("guestimage: fetching %s of %s: %w", name, pin.Target, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("guestimage: %s of %s from %s hashes to %s and is pinned at %s; "+
			"the image is refused", name, pin.Target, source, got, want.SHA256)
	}
	if want.Bytes > 0 && n != want.Bytes {
		return fmt.Errorf("guestimage: %s of %s is %d bytes and is pinned at %d", name, pin.Target, n, want.Bytes)
	}
	return os.Chmod(dst, 0o444)
}
