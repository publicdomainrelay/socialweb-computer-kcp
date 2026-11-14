package assets

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

type Options struct {
	Cache string
	Paths map[string]string
	Log   *slog.Logger
}

type Env interface {
	LookupEnv(key string) (string, bool)
}

type Listed struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	SHA256      string   `json:"sha256"`
	ZstdBytes   int64    `json:"zstdBytes"`
	Executables []string `json:"executables"`
	Embedded    bool     `json:"embedded"`
}

func platformPins(p string) ([]versions.Pin, error) {
	return PlatformPins(p)
}

func Listing() ([]Listed, error) { return ListingFor(Platform()) }

func ListingFor(platform string) ([]Listed, error) {
	pins, err := platformPins(platform)
	if err != nil {
		return nil, err
	}
	out := make([]Listed, 0, len(pins))
	for _, p := range pins {
		l := Listed{Name: p.Name, Version: p.Version, SHA256: p.TreeSHA256, ZstdBytes: p.ZstdBytes, Embedded: platform == Platform() && embeddedBlob(p.Name)}
		for e := range p.Executables {
			l.Executables = append(l.Executables, e)
		}
		sort.Strings(l.Executables)
		out = append(out, l)
	}
	return out, nil
}

func DevCache(env Env) (string, error) {
	if v, ok := env.LookupEnv("XDG_CACHE_HOME"); ok && v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "socialweb-computer", "runtime"), nil
	}
	if home, ok := env.LookupEnv("HOME"); ok && home != "" {
		return filepath.Join(home, ".cache", "socialweb-computer", "runtime"), nil
	}
	return "", errors.New("assets: neither asset-cache, XDG_CACHE_HOME nor HOME is set; the dev asset cache has no root")
}
