package pinneddownloads

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
)

const (
	SidecarSuffix = ".sha256"

	CacheControl = "public, max-age=31536000, immutable"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	sha256sumLinePattern = regexp.MustCompile(`^([0-9a-f]{64}) [ *](\S+)$`)

	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type PinnedDownload struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type entry struct {
	meta  PinnedDownload
	bytes []byte
	path  string
}

type Set struct {
	entries []entry
	index   map[string]map[string]int
}

func Load(dir string) (*Set, error) {
	names, err := fileNames(dir)
	if err != nil {
		return nil, err
	}
	out := &Set{index: map[string]map[string]int{}}
	for _, sidecar := range names {
		if !strings.HasSuffix(sidecar, SidecarSuffix) {
			continue
		}
		name, pinned, err := readSidecar(dir, sidecar)
		if err != nil {
			return nil, err
		}
		bytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		got := sha256sum.Hex(bytes)
		if got != pinned {
			return nil, fmt.Errorf("%s has sha256 %s, but %s pins %s", filepath.Join(dir, name), got, sidecar, pinned)
		}
		idx := len(out.entries)
		out.entries = append(out.entries, entry{
			meta:  PinnedDownload{Name: name, Sha256: got, Size: int64(len(bytes))},
			bytes: bytes,
		})
		if out.index[got] == nil {
			out.index[got] = map[string]int{}
		}
		out.index[got][name] = idx
	}
	return out, nil
}

func readSidecar(dir, sidecar string) (string, string, error) {
	name := strings.TrimSuffix(sidecar, SidecarSuffix)
	if !namePattern.MatchString(name) {
		return "", "", fmt.Errorf("%s: pins %q, which is not a download name", filepath.Join(dir, sidecar), name)
	}
	lineBytes, err := os.ReadFile(filepath.Join(dir, sidecar))
	if err != nil {
		return "", "", err
	}
	match := sha256sumLinePattern.FindStringSubmatch(strings.TrimSpace(string(lineBytes)))
	if match == nil {
		return "", "", fmt.Errorf("%s is not one sha256sum line", filepath.Join(dir, sidecar))
	}
	if match[2] != name {
		return "", "", fmt.Errorf("%s names %s, not %s", filepath.Join(dir, sidecar), match[2], name)
	}
	return name, match[1], nil
}

func LoadPins(dir string, required []string) (map[string]string, error) {
	names, err := fileNames(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, sidecar := range names {
		if !strings.HasSuffix(sidecar, SidecarSuffix) {
			continue
		}
		name, pinned, err := readSidecar(dir, sidecar)
		if err != nil {
			return nil, err
		}
		out[name] = pinned
	}
	var missing []string
	for _, name := range required {
		if _, ok := out[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required pinned downloads are missing from %s: %s", dir, strings.Join(missing, ", "))
	}
	return out, nil
}

func LoadRequired(dir string, required []string) (*Set, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	set, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if missing := set.Missing(required); len(missing) > 0 {
		return nil, fmt.Errorf("required pinned downloads are missing from %s: %s", dir, strings.Join(missing, ", "))
	}
	return set, nil
}

func fileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Set) List() []PinnedDownload {
	if s == nil {
		return nil
	}
	out := make([]PinnedDownload, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.meta)
	}
	return out
}

func (s *Set) Missing(required []string) []string {
	have := map[string]bool{}
	for _, d := range s.List() {
		have[d.Name] = true
	}
	var missing []string
	for _, name := range required {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func (s *Set) WithFile(name, sha256Hex, path string, size int64) (*Set, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("%q is not a download name", name)
	}
	if !sha256Pattern.MatchString(sha256Hex) {
		return nil, fmt.Errorf("%s: sha256 %q is not 64 lowercase hex characters", name, sha256Hex)
	}
	out := &Set{index: map[string]map[string]int{}}
	if s != nil {
		out.entries = slices.Clone(s.entries)
		for sum, byName := range s.index {
			out.index[sum] = maps.Clone(byName)
		}
	}
	if _, ok := out.index[sha256Hex][name]; ok {
		return out, nil
	}
	if out.index[sha256Hex] == nil {
		out.index[sha256Hex] = map[string]int{}
	}
	out.index[sha256Hex][name] = len(out.entries)
	out.entries = append(out.entries, entry{meta: PinnedDownload{Name: name, Sha256: sha256Hex, Size: size}, path: path})
	return out, nil
}

func (s *Set) lookup(sha256Hex, name string) *entry {
	if s == nil {
		return nil
	}
	idx, ok := s.index[sha256Hex][name]
	if !ok {
		return nil
	}
	return &s.entries[idx]
}

func (s *Set) Get(sha256Hex, name string) []byte {
	if e := s.lookup(sha256Hex, name); e != nil {
		return e.bytes
	}
	return nil
}

type Handler struct {
	set *Set
	log *slog.Logger
}

func NewHandler(set *Set, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{set: set, log: log}
}

func (h *Handler) Register(mux *http.ServeMux, pattern string) {
	mux.Handle(pattern, h)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sha256Hex := r.PathValue("sha256")
	name := r.PathValue("name")
	e := h.set.lookup(sha256Hex, name)
	if e == nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+sha256Hex+`"`)
	w.Header().Set("Cache-Control", CacheControl)
	if e.path != "" {
		h.serveFile(w, r, e)
		return
	}
	h.log.Info("pinned_download_served", "name", name, "sha256", sha256Hex, "bytes", len(e.bytes))
	w.Header().Set("Content-Length", strconv.Itoa(len(e.bytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.bytes)
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, e *entry) {
	f, err := os.Open(e.path)
	if err != nil {
		h.log.Warn("pinned_download_unreadable", "name", e.meta.Name, "path", e.path, "err", err.Error())
		notFound(w)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != e.meta.Size {
		h.log.Warn("pinned_download_changed", "name", e.meta.Name, "path", e.path, "size", e.meta.Size)
		notFound(w)
		return
	}
	h.log.Info("pinned_download_served", "name", e.meta.Name, "sha256", e.meta.Sha256, "bytes", e.meta.Size, "path", e.path)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func notFound(w http.ResponseWriter) {
	httpjson.Write(w, http.StatusNotFound, struct {
		Error   string `json:"error"`
		Message string `json:"message,omitempty"`
	}{"NotFound", "no download with that sha256 and name"})
}
