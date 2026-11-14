package guestimage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	FetchPrefix   = ".fetch-"
	FetchKeep     = time.Hour
	BinaryTimeout = 30 * time.Second

	RequestedName = ".requested"
	UsedName      = ".used"

	DefaultBudget  int64 = 16 << 30
	DefaultMinFree int64 = 8 << 30
	UsedGrace            = 10 * time.Minute
)

var digestName = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Names() ([]string, error) {
	g, err := versions.GuestImages()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range g.Targets {
		m, ok := t.Outputs[ManifestName]
		if !ok || !digestName.MatchString(m.SHA256) || t.Hypervisor == "" || t.Arch == "" {
			continue
		}
		out = append(out, filepath.Join(t.Hypervisor, t.Arch, m.SHA256))
	}
	sort.Strings(out)
	return out, nil
}

type binaryVersion struct {
	GuestImages []string `json:"guestImages"`
}

func NamedByBinary(ctx context.Context, binary string) ([]string, error) {
	if _, err := os.Stat(binary); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, BinaryTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("guestimage: %s version --json: %w", binary, err)
	}
	var v binaryVersion
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("guestimage: %s version --json: %w", binary, err)
	}
	return v.GuestImages, nil
}

type GCKept struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

type GCReport struct {
	Root    string   `json:"root"`
	Removed []string `json:"removed"`
	Kept    []GCKept `json:"kept"`
}

type requestedImage struct {
	path string
	size int64
	last time.Time
}

func (s Store) Touch(dir string) {
	path := filepath.Join(dir, UsedName)
	now := time.Now()
	if os.Chtimes(path, now, now) == nil {
		return
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
}

func imageBytes(dir string) int64 {
	var n int64
	for _, name := range []string{ManifestName, KernelName, RootfsName} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func lastUsed(dir string, fallback time.Time) time.Time {
	if fi, err := os.Stat(filepath.Join(dir, UsedName)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime()
	}
	return fallback
}

func (s Store) evictRequested(rep *GCReport, held []requestedImage, now time.Time) error {
	sort.Slice(held, func(i, j int) bool { return held[i].last.Before(held[j].last) })
	var total int64
	for _, h := range held {
		total += h.size
	}
	free, freeKnown := freeBytes(s.Root)
	for _, h := range held {
		over := s.Budget > 0 && total > s.Budget
		pressure := freeKnown && s.MinFree > 0 && free < s.MinFree
		switch {
		case !over && !pressure:
			rep.Kept = append(rep.Kept, GCKept{Dir: h.path, Reason: "a requested image, kept by last use within the budget"})
		case now.Sub(h.last) < UsedGrace:
			rep.Kept = append(rep.Kept, GCKept{Dir: h.path, Reason: "a requested image used within the last " + UsedGrace.String()})
		default:
			if err := os.RemoveAll(h.path); err != nil {
				return err
			}
			rep.Removed = append(rep.Removed, h.path)
			total -= h.size
			free += h.size
		}
	}
	return nil
}

func (s Store) Collect(keep map[string]string, now time.Time) (GCReport, error) {
	rep := GCReport{Root: s.Root, Removed: []string{}, Kept: []GCKept{}}
	var held []requestedImage
	if !filepath.IsAbs(s.Root) {
		return rep, fmt.Errorf("guestimage: the image store root %q is not absolute", s.Root)
	}
	top, err := os.ReadDir(s.Root)
	if os.IsNotExist(err) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	for _, h := range top {
		dir := filepath.Join(s.Root, h.Name())
		if h.IsDir() && strings.HasPrefix(h.Name(), FetchPrefix) {
			if fi, err := h.Info(); err == nil && now.Sub(fi.ModTime()) > FetchKeep {
				if err := os.RemoveAll(dir); err != nil {
					return rep, err
				}
				rep.Removed = append(rep.Removed, dir)
			}
			continue
		}
		if !h.IsDir() {
			continue
		}
		arches, err := os.ReadDir(dir)
		if err != nil {
			return rep, err
		}
		for _, a := range arches {
			if !a.IsDir() {
				continue
			}
			adir := filepath.Join(dir, a.Name())
			images, err := os.ReadDir(adir)
			if err != nil {
				return rep, err
			}
			for _, img := range images {
				if !img.IsDir() || !digestName.MatchString(img.Name()) {
					continue
				}
				path := filepath.Join(adir, img.Name())
				if why, ok := keep[img.Name()]; ok {
					rep.Kept = append(rep.Kept, GCKept{Dir: path, Reason: why})
					continue
				}
				if _, err := os.Stat(filepath.Join(path, RequestedName)); err == nil {
					held = append(held, requestedImage{path: path, size: imageBytes(path), last: lastUsed(path, now)})
					continue
				}
				if err := os.RemoveAll(path); err != nil {
					return rep, err
				}
				rep.Removed = append(rep.Removed, path)
			}
		}
	}
	if err := s.evictRequested(&rep, held, now); err != nil {
		return rep, err
	}
	return rep, nil
}
