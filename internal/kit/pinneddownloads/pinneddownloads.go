package pinneddownloads

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
)

type PinnedDownload struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type entry struct {
	meta  PinnedDownload
	bytes []byte
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
		name := strings.TrimSuffix(sidecar, SidecarSuffix)
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("%s: pins %q, which is not a download name", filepath.Join(dir, sidecar), name)
		}
		lineBytes, err := os.ReadFile(filepath.Join(dir, sidecar))
		if err != nil {
			return nil, err
		}
		match := sha256sumLinePattern.FindStringSubmatch(strings.TrimSpace(string(lineBytes)))
		if match == nil {
			return nil, fmt.Errorf("%s is not one sha256sum line", filepath.Join(dir, sidecar))
		}
		if match[2] != name {
			return nil, fmt.Errorf("%s names %s, not %s", filepath.Join(dir, sidecar), match[2], name)
		}
		bytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		got := sha256sum.Hex(bytes)
		if got != match[1] {
			return nil, fmt.Errorf("%s has sha256 %s, but %s pins %s", filepath.Join(dir, name), got, sidecar, match[1])
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

func (s *Set) Get(sha256Hex, name string) []byte {
	if s == nil {
		return nil
	}
	byName, ok := s.index[sha256Hex]
	if !ok {
		return nil
	}
	idx, ok := byName[name]
	if !ok {
		return nil
	}
	return s.entries[idx].bytes
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
	bytes := h.set.Get(sha256Hex, name)
	if bytes == nil {
		httpjson.Write(w, http.StatusNotFound, struct {
			Error   string `json:"error"`
			Message string `json:"message,omitempty"`
		}{"NotFound", "no download with that sha256 and name"})
		return
	}
	h.log.Info("pinned_download_served", "name", name, "sha256", sha256Hex, "bytes", len(bytes))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(bytes)))
	w.Header().Set("ETag", `"`+sha256Hex+`"`)
	w.Header().Set("Cache-Control", CacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
}
