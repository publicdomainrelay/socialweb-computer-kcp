package guestrootfs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
)

const RootfsLabel = "cloudimg-rootfs"

const dpkgStatusPath = "/var/lib/dpkg/status"

type File struct {
	Path string
	Mode int64
	Body []byte
}

type Spec struct {
	SourceDateEpoch int64
	Files           []File
	Remove          []string
	RequirePackages []string
	Runs            map[string]string
	Assets          map[string]string
}

type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}

type Bake struct {
	Packages []string          `json:"packages"`
	Files    map[string]string `json:"files"`
	Assets   map[string]string `json:"assets"`
	Runs     map[string]string `json:"runs"`
	Removed  []string          `json:"removed"`
}

type Report struct {
	Entries     []Entry `json:"entries"`
	Bake        Bake    `json:"bake"`
	TarSHA256   string  `json:"tarSha256"`
	DataBytes   int64   `json:"dataBytes"`
	SSHHostKeys int     `json:"sshHostKeys"`
}

type Preinstall struct {
	Packages []string `json:"packages"`
	Files    []struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Content string `json:"content"`
		Source  string `json:"source"`
	} `json:"files"`
	Runs []string `json:"runs"`
}

type Build struct {
	Dir        string
	Preinstall Preinstall
	Overlay    string            `json:"overlay"`
	Remove     []string          `json:"remove"`
	Assets     map[string]string `json:"assets"`
	Runs       map[string]string `json:"runs"`
	From       string            `json:"preinstall"`
}

type Options struct {
	SourceDateEpoch int64
	Assets          map[string]string
}

func LoadBuild(file string) (Build, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Build{}, fmt.Errorf("guestrootfs: reading %s: %w", file, err)
	}
	var b Build
	if err := json.Unmarshal(raw, &b); err != nil {
		return Build{}, fmt.Errorf("guestrootfs: %s: %w", file, err)
	}
	b.Dir = filepath.Dir(file)
	if b.From == "" {
		return Build{}, fmt.Errorf("guestrootfs: %s names no preinstall manifest, and the manifest is "+
			"what says which packages and files every guest finds already installed", file)
	}
	pre, err := os.ReadFile(filepath.Join(b.Dir, b.From))
	if err != nil {
		return Build{}, fmt.Errorf("guestrootfs: reading the preinstall manifest: %w", err)
	}
	if err := json.Unmarshal(pre, &b.Preinstall); err != nil {
		return Build{}, fmt.Errorf("guestrootfs: %s: %w", b.From, err)
	}
	return b, nil
}

func RunKey(run string) string {
	sum := sha256.Sum256([]byte(run))
	return hex.EncodeToString(sum[:])
}

func (b Build) Spec(opts Options) (Spec, error) {
	if opts.SourceDateEpoch <= 0 {
		return Spec{}, fmt.Errorf("guestrootfs: no SOURCE_DATE_EPOCH: every timestamp the build " +
			"writes is clamped to it, and without one two builds of the same inputs differ")
	}
	spec := Spec{
		SourceDateEpoch: opts.SourceDateEpoch,
		Remove:          append([]string(nil), b.Remove...),
		RequirePackages: append([]string(nil), b.Preinstall.Packages...),
		Runs:            map[string]string{},
		Assets:          map[string]string{},
	}
	for _, run := range b.Preinstall.Runs {
		why, ok := b.Runs[RunKey(run)]
		if !ok {
			return Spec{}, fmt.Errorf("guestrootfs: the preinstall manifest runs %q, and the image build "+
				"runs nothing inside the rootfs: list its sha256 %s under runs in the build file with "+
				"what replaces it, or the image would silently lack what the run installs",
				run, RunKey(run))
		}
		spec.Runs[RunKey(run)] = why
	}
	if b.Overlay != "" {
		root := filepath.Join(b.Dir, b.Overlay)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("guestrootfs: the overlay holds %s, which is not a regular file", p)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			spec.Files = append(spec.Files, File{Path: "/" + filepath.ToSlash(rel), Mode: mode, Body: body})
			return nil
		})
		if err != nil {
			return Spec{}, fmt.Errorf("guestrootfs: reading the overlay: %w", err)
		}
	}
	for _, f := range b.Preinstall.Files {
		if !strings.HasPrefix(f.Path, "/") {
			return Spec{}, fmt.Errorf("guestrootfs: the preinstall file %q is not an absolute path", f.Path)
		}
		body := []byte(f.Content)
		if f.Source != "" {
			raw, err := os.ReadFile(filepath.Join(b.Dir, f.Source))
			if err != nil {
				return Spec{}, fmt.Errorf("guestrootfs: the preinstall file %s: %w", f.Path, err)
			}
			body = raw
		}
		mode := int64(0o644)
		if f.Mode != "" {
			m, err := strconv.ParseInt(f.Mode, 8, 64)
			if err != nil {
				return Spec{}, fmt.Errorf("guestrootfs: the preinstall file %s has mode %q: %w", f.Path, f.Mode, err)
			}
			mode = m
		}
		spec.Files = append(spec.Files, File{Path: f.Path, Mode: mode, Body: body})
	}
	for guestPath, name := range b.Assets {
		local, ok := opts.Assets[name]
		if !ok {
			return Spec{}, fmt.Errorf("guestrootfs: the build bakes the asset %s at %s and none was given "+
				"for it", name, guestPath)
		}
		body, err := os.ReadFile(local)
		if err != nil {
			return Spec{}, fmt.Errorf("guestrootfs: the asset %s: %w", name, err)
		}
		spec.Files = append(spec.Files, File{Path: guestPath, Mode: 0o755, Body: body})
		spec.Assets[guestPath] = name
	}
	seen := map[string]bool{}
	for _, f := range spec.Files {
		if seen[f.Path] {
			return Spec{}, fmt.Errorf("guestrootfs: %s is written twice by the build", f.Path)
		}
		seen[f.Path] = true
	}
	sort.Slice(spec.Files, func(i, j int) bool { return spec.Files[i].Path < spec.Files[j].Path })
	return spec, nil
}

func guestPath(name string) string {
	p := path.Clean("/" + strings.TrimPrefix(name, "./"))
	return p
}

func (s Spec) removed(p string) bool {
	for _, pattern := range s.Remove {
		if strings.HasSuffix(pattern, "/**") {
			if strings.HasPrefix(p, strings.TrimSuffix(pattern, "**")) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pattern, p); ok {
			return true
		}
	}
	return false
}

func installedPackages(status []byte) map[string]bool {
	out := map[string]bool{}
	var name string
	scanner := bufio.NewScanner(bytes.NewReader(status))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Status: ") && name != "":
			if strings.HasSuffix(strings.TrimSpace(line), " installed") && strings.HasPrefix(line, "Status: install ") {
				out[name] = true
			}
		}
	}
	return out
}

type hashingWriter struct {
	w io.Writer
	h io.Writer
}

func (hw hashingWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	return n, err
}

func blocks(n int64) int64 { return (n + 4095) / 4096 * 4096 }

func Compose(base io.Reader, spec Spec, out io.Writer) (Report, error) {
	if spec.SourceDateEpoch <= 0 {
		return Report{}, fmt.Errorf("guestrootfs: no SOURCE_DATE_EPOCH")
	}
	epoch := time.Unix(spec.SourceDateEpoch, 0).UTC()
	sum := sha256.New()
	w := tar.NewWriter(hashingWriter{w: out, h: sum})
	report := Report{Bake: Bake{Files: map[string]string{}, Assets: map[string]string{}, Runs: spec.Runs, Removed: []string{}}}
	replace := map[string]File{}
	for _, f := range spec.Files {
		replace[f.Path] = f
	}
	written := map[string]bool{}
	dirs := map[string]bool{}
	removed := map[string]bool{}
	prefix := ""
	var status []byte

	emit := func(h *tar.Header, body []byte) error {
		if h.ModTime.IsZero() || h.ModTime.After(epoch) {
			h.ModTime = epoch
		}
		h.ModTime = h.ModTime.UTC().Truncate(time.Second)
		h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
		var pax map[string]string
		for k, v := range h.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.") {
				if pax == nil {
					pax = map[string]string{}
				}
				pax[k] = v
			}
		}
		h.PAXRecords = pax
		h.Format = tar.FormatUnknown
		if pax != nil {
			h.Format = tar.FormatPAX
		}
		if err := w.WriteHeader(h); err != nil {
			return fmt.Errorf("guestrootfs: writing %s: %w", h.Name, err)
		}
		e := Entry{Path: guestPath(h.Name), Mode: fmt.Sprintf("%04o", h.Mode&0o7777), UID: h.Uid, GID: h.Gid, Link: h.Linkname}
		switch h.Typeflag {
		case tar.TypeReg:
			if _, err := w.Write(body); err != nil {
				return fmt.Errorf("guestrootfs: writing %s: %w", h.Name, err)
			}
			d := sha256.Sum256(body)
			e.Type, e.Size, e.SHA256 = "file", int64(len(body)), hex.EncodeToString(d[:])
			report.DataBytes += blocks(int64(len(body)))
		case tar.TypeDir:
			e.Type = "dir"
			report.DataBytes += 4096
		case tar.TypeSymlink:
			e.Type = "symlink"
			if len(h.Linkname) > 59 {
				report.DataBytes += 4096
			}
		case tar.TypeLink:
			e.Type = "hardlink"
		default:
			e.Type = "special"
		}
		if strings.HasPrefix(e.Path, "/etc/ssh/ssh_host_") {
			report.SSHHostKeys++
		}
		report.DataBytes += 256
		report.Entries = append(report.Entries, e)
		written[e.Path] = true
		return nil
	}

	r := tar.NewReader(base)
	first := true
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Report{}, fmt.Errorf("guestrootfs: reading the base: %w", err)
		}
		if first {
			if strings.HasPrefix(h.Name, "./") || h.Name == "." {
				prefix = "./"
			}
			first = false
		}
		p := guestPath(h.Name)
		if spec.removed(p) {
			removed[p] = true
			report.Bake.Removed = append(report.Bake.Removed, p)
			continue
		}
		if h.Typeflag == tar.TypeLink {
			target := guestPath(h.Linkname)
			if removed[target] {
				return Report{}, fmt.Errorf("guestrootfs: %s is a hard link to %s, which the build removes", p, target)
			}
			if _, ok := replace[target]; ok {
				return Report{}, fmt.Errorf("guestrootfs: %s is a hard link to %s, which the build replaces", p, target)
			}
		}
		if h.Typeflag == tar.TypeDir {
			dirs[p] = true
		}
		var body []byte
		if h.Typeflag == tar.TypeReg {
			body, err = io.ReadAll(r)
			if err != nil {
				return Report{}, fmt.Errorf("guestrootfs: reading %s from the base: %w", p, err)
			}
			if p == dpkgStatusPath {
				status = body
			}
		}
		if f, ok := replace[p]; ok {
			if h.Typeflag != tar.TypeReg {
				return Report{}, fmt.Errorf("guestrootfs: the build writes %s and the base holds a "+
					"non-file there", p)
			}
			body = f.Body
			h.Size = int64(len(body))
			h.Mode = f.Mode
			h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "root", "root"
			h.ModTime = epoch
			h.PAXRecords = nil
			delete(replace, p)
			report.Bake.Files[p] = sha256sum.Hex(body)
		}
		if err := emit(h, body); err != nil {
			return Report{}, err
		}
	}
	if status == nil {
		return Report{}, fmt.Errorf("guestrootfs: the base carries no %s, so nothing says which "+
			"packages it has installed", dpkgStatusPath)
	}
	installed := installedPackages(status)
	var missing []string
	for _, p := range spec.RequirePackages {
		if !installed[p] {
			missing = append(missing, p)
			continue
		}
		report.Bake.Packages = append(report.Bake.Packages, p)
	}
	if len(missing) > 0 {
		return Report{}, fmt.Errorf("guestrootfs: the preinstall manifest needs %s and the base's "+
			"dpkg status does not have them installed. The build installs no packages (no apt, no "+
			"chroot): pin a base that carries them, or take them out of the manifest",
			strings.Join(missing, ", "))
	}
	var added []string
	for p := range replace {
		added = append(added, p)
	}
	sort.Strings(added)
	for _, p := range added {
		var parents []string
		for d := path.Dir(p); d != "/" && !dirs[d]; d = path.Dir(d) {
			parents = append([]string{d}, parents...)
		}
		for _, d := range parents {
			h := &tar.Header{Name: prefix + strings.TrimPrefix(d, "/") + "/", Typeflag: tar.TypeDir, Mode: 0o755,
				Uname: "root", Gname: "root", ModTime: epoch}
			if err := emit(h, nil); err != nil {
				return Report{}, err
			}
			dirs[d] = true
		}
		f := replace[p]
		h := &tar.Header{Name: prefix + strings.TrimPrefix(p, "/"), Typeflag: tar.TypeReg, Mode: f.Mode,
			Size: int64(len(f.Body)), Uname: "root", Gname: "root", ModTime: epoch}
		if err := emit(h, f.Body); err != nil {
			return Report{}, err
		}
		if name, ok := spec.Assets[p]; ok {
			report.Bake.Assets[name] = sha256sum.Hex(f.Body)
		} else {
			report.Bake.Files[p] = sha256sum.Hex(f.Body)
		}
	}
	if err := w.Close(); err != nil {
		return Report{}, fmt.Errorf("guestrootfs: %w", err)
	}
	for p, name := range spec.Assets {
		if _, ok := report.Bake.Assets[name]; !ok {
			report.Bake.Assets[name] = report.Bake.Files[p]
			delete(report.Bake.Files, p)
		}
	}
	sort.Slice(report.Entries, func(i, j int) bool { return report.Entries[i].Path < report.Entries[j].Path })
	report.TarSHA256 = hex.EncodeToString(sum.Sum(nil))
	return report, nil
}
