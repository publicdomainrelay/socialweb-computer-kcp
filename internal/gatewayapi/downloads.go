package gatewayapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	SidecarSuffix = ".sha256"

	PinnedDownloadCacheControl = "public, max-age=31536000, immutable"
)

var (
	downloadNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	sha256sumLinePattern = regexp.MustCompile(`^([0-9a-f]{64}) [ *](\S+)$`)
)

type PinnedDownload struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type pinnedEntry struct {
	meta  PinnedDownload
	bytes []byte
}

type PinnedDownloads struct {
	entries []pinnedEntry
	index   map[string]map[string]int
}

func LoadPinnedDownloads(dir string) (*PinnedDownloads, error) {
	names, err := fileNames(dir)
	if err != nil {
		return nil, err
	}
	out := &PinnedDownloads{index: map[string]map[string]int{}}
	for _, sidecar := range names {
		if !strings.HasSuffix(sidecar, SidecarSuffix) {
			continue
		}
		name := strings.TrimSuffix(sidecar, SidecarSuffix)
		if !downloadNamePattern.MatchString(name) {
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
		sum := sha256.Sum256(bytes)
		got := hex.EncodeToString(sum[:])
		if got != match[1] {
			return nil, fmt.Errorf("%s has sha256 %s, but %s pins %s", filepath.Join(dir, name), got, sidecar, match[1])
		}
		idx := len(out.entries)
		out.entries = append(out.entries, pinnedEntry{
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

func fileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (p *PinnedDownloads) List() []PinnedDownload {
	if p == nil {
		return nil
	}
	out := make([]PinnedDownload, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.meta)
	}
	return out
}

func (p *PinnedDownloads) Get(sha256Hex, name string) []byte {
	if p == nil {
		return nil
	}
	byName, ok := p.index[sha256Hex]
	if !ok {
		return nil
	}
	idx, ok := byName[name]
	if !ok {
		return nil
	}
	return p.entries[idx].bytes
}

type DownloadsOptions struct {
	Downloads *PinnedDownloads

	Log *slog.Logger
}

type Downloads struct {
	downloads *PinnedDownloads
	log       *slog.Logger
}

func NewDownloads(opts DownloadsOptions) *Downloads {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Downloads{downloads: opts.Downloads, log: log}
}

func (d *Downloads) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /downloads/{sha256}/{name}", d.serve)
}

func (d *Downloads) Handler() http.Handler {
	mux := http.NewServeMux()
	d.Register(mux)
	return mux
}

func (d *Downloads) serve(w http.ResponseWriter, r *http.Request) {
	sha256Hex := r.PathValue("sha256")
	name := r.PathValue("name")
	bytes := d.downloads.Get(sha256Hex, name)
	if bytes == nil {
		writeFailure(w, &apiFailure{Status: http.StatusNotFound, Error: "NotFound",
			Message: "no download with that sha256 and name"})
		return
	}
	d.log.Info("pinned_download_served", "name", name, "sha256", sha256Hex, "bytes", len(bytes))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bytes)))
	w.Header().Set("ETag", `"`+sha256Hex+`"`)
	w.Header().Set("Cache-Control", PinnedDownloadCacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
}
