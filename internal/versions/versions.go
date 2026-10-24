package versions

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

//go:embed versions.json
var manifest []byte

type Component struct {
	Version   string `json:"version"`
	How       string `json:"how"`
	Measured  string `json:"measured"`
	Invariant string `json:"invariant,omitempty"`
	Disputed  string `json:"disputed,omitempty"`

	Image string `json:"image,omitempty"`

	BundleSHA256 string `json:"bundleSha256,omitempty"`

	Resolved string `json:"resolved,omitempty"`
}

type manifestShape struct {
	Note       string               `json:"note"`
	Components map[string]Component `json:"components"`
	Platforms  map[string]Platform  `json:"platforms"`
	Assets     map[string]Asset     `json:"assets"`
	Guest      GuestImageSet        `json:"guestImages"`
	KcpCompat  []KcpCompat          `json:"kcpCompat"`
	VMStop     VMStop               `json:"vmStop"`
	Downloads  GuestDownloadSet     `json:"guestDownloads"`
}

type GuestDownloadSet struct {
	Note  string                   `json:"note"`
	Files map[string]GuestDownload `json:"files"`
}

type GuestDownload struct {
	Version string                        `json:"version"`
	Source  string                        `json:"source"`
	Verify  string                        `json:"verify"`
	Fetch   map[string]GuestDownloadFetch `json:"fetch"`
}

type GuestDownloadFetch struct {
	Kind       string `json:"kind"`
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Member     string `json:"member"`
}

func GuestDownloadPins(name string) (GuestDownload, error) {
	m, err := load()
	if err != nil {
		return GuestDownload{}, err
	}
	d, ok := m.Downloads.Files[name]
	if !ok || len(d.Fetch) == 0 {
		return GuestDownload{}, fmt.Errorf("versions: no guest download %s", name)
	}
	return d, nil
}

type VMStop struct {
	GraceSeconds int    `json:"graceSeconds"`
	Measured     string `json:"measured"`
}

func VMStopGrace() (time.Duration, error) {
	m, err := load()
	if err != nil {
		return 0, err
	}
	if m.VMStop.GraceSeconds <= 0 {
		return 0, fmt.Errorf("versions: vmStop.graceSeconds %d is not a positive number of seconds", m.VMStop.GraceSeconds)
	}
	return time.Duration(m.VMStop.GraceSeconds) * time.Second, nil
}

type KcpCompat struct {
	From              string `json:"from"`
	To                string `json:"to"`
	StorageCompatible bool   `json:"storageCompatible"`
	DiffSHA256        string `json:"diffSHA256"`
}

func KcpCompatRecords() ([]KcpCompat, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	return m.KcpCompat, nil
}

type GuestImageFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type GuestImageInput struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type GuestImageTarget struct {
	Hypervisor      string                    `json:"hypervisor"`
	Arch            string                    `json:"arch"`
	Platform        string                    `json:"platform"`
	KernelConfig    string                    `json:"kernelConfig"`
	BootArgs        []string                  `json:"bootArgs"`
	Base            GuestImageInput           `json:"base"`
	SourceDateEpoch int64                     `json:"sourceDateEpoch"`
	Outputs         map[string]GuestImageFile `json:"outputs"`
}

type GuestImageKernel struct {
	Version string                     `json:"version"`
	Source  GuestImageInput            `json:"source"`
	Config  map[string]json.RawMessage `json:"config"`
	Reason  string                     `json:"reason"`
}

type GuestImageSet struct {
	Note     string                      `json:"note"`
	Builder  map[string]string           `json:"builder"`
	Kernel   GuestImageKernel            `json:"kernel"`
	Targets  map[string]GuestImageTarget `json:"targets"`
	Reserved map[string]string           `json:"reserved"`
}

func GuestImages() (GuestImageSet, error) {
	m, err := load()
	if err != nil {
		return GuestImageSet{}, err
	}
	return m.Guest, nil
}

const (
	PlatformRelease = "release"
	PlatformSeam    = "seam"
)

type Platform struct {
	Status string   `json:"status"`
	Owner  string   `json:"owner"`
	Note   string   `json:"note,omitempty"`
	Assets []string `json:"assets"`
}

type Asset struct {
	Version     string                   `json:"version"`
	Source      string                   `json:"source"`
	Verify      string                   `json:"verify"`
	Pending     string                   `json:"pending,omitempty"`
	Executables map[string]string        `json:"executables"`
	LibraryPath string                   `json:"libraryPath,omitempty"`
	Fetch       map[string]AssetFetch    `json:"fetch,omitempty"`
	Platforms   map[string]AssetPlatform `json:"platforms"`
}

type AssetFetch struct {
	URL     string            `json:"url,omitempty"`
	SHA256  string            `json:"sha256"`
	Archive string            `json:"archive,omitempty"`
	Members map[string]string `json:"members,omitempty"`
}

func AssetFetches(asset string) (map[string]AssetFetch, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	a, ok := m.Assets[asset]
	if !ok || len(a.Fetch) == 0 {
		return nil, fmt.Errorf("versions: no fetch pins for asset %s", asset)
	}
	return a.Fetch, nil
}

func FetchSHA256(asset, platform string) (string, error) {
	m, err := load()
	if err != nil {
		return "", err
	}
	a, ok := m.Assets[asset]
	if !ok {
		return "", fmt.Errorf("versions: no asset %s", asset)
	}
	f, ok := a.Fetch[platform]
	if !ok || f.SHA256 == "" {
		return "", fmt.Errorf("versions: asset %s pins no fetch sha256 for %s", asset, platform)
	}
	return f.SHA256, nil
}

type AssetPlatform struct {
	TreeSHA256 string `json:"treeSha256"`
	RawBytes   int64  `json:"rawBytes"`
	ZstdBytes  int64  `json:"zstdBytes"`
}

func load() (manifestShape, error) {
	var m manifestShape
	if err := json.Unmarshal(manifest, &m); err != nil {
		return m, fmt.Errorf("versions: manifest is not valid JSON: %w", err)
	}
	return m, nil
}

func All() (map[string]Component, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	return m.Components, nil
}

func Platforms() (map[string]Platform, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	return m.Platforms, nil
}

func Assets() (map[string]Asset, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	return m.Assets, nil
}

type Pin struct {
	Name        string
	Platform    string
	Version     string
	Executables map[string]string
	LibraryPath string
	AssetPlatform
}

func PlatformPins(platform string) ([]Pin, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	p, ok := m.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("versions: platform %s is not in the matrix", platform)
	}
	out := make([]Pin, 0, len(p.Assets))
	for _, name := range p.Assets {
		a, ok := m.Assets[name]
		if !ok {
			return nil, fmt.Errorf("versions: platform %s lists asset %q that has no pin", platform, name)
		}
		ap, ok := a.Platforms[platform]
		if !ok {
			return nil, fmt.Errorf("versions: asset %s has no pin for %s", name, platform)
		}
		out = append(out, Pin{Name: name, Platform: platform, Version: a.Version, Executables: a.Executables,
			LibraryPath: a.LibraryPath, AssetPlatform: ap})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type Measured struct {
	Platform  string           `json:"platform"`
	Binary    int64            `json:"binaryBytes"`
	Core      int64            `json:"coreBytes"`
	Assets    map[string]int64 `json:"assetBytes"`
	Projected int64            `json:"projectedBytes"`
	NotStaged []string         `json:"notStaged"`
}

func MeasureRelease(platform, binary, blobDir string) (Measured, error) {
	m := Measured{Platform: platform, Assets: map[string]int64{}, NotStaged: []string{}}
	st, err := os.Stat(binary)
	if err != nil {
		return m, err
	}
	m.Binary, m.Core = st.Size(), st.Size()
	pins, err := PlatformPins(platform)
	if err != nil {
		return m, err
	}
	for _, p := range pins {
		if bs, err := os.Stat(filepath.Join(blobDir, p.Name+".tar.zst")); err == nil && bs.Size() > 0 {
			m.Assets[p.Name] = bs.Size()
			m.Core -= bs.Size()
			continue
		}
		m.Assets[p.Name] = p.ZstdBytes
		m.NotStaged = append(m.NotStaged, p.Name)
	}
	m.Projected = m.Core
	for _, v := range m.Assets {
		m.Projected += v
	}
	return m, nil
}

func Get(name string) (Component, error) {
	all, err := All()
	if err != nil {
		return Component{}, err
	}
	c, ok := all[name]
	if !ok {
		return Component{}, fmt.Errorf("versions: no component %q", name)
	}
	return c, nil
}

func Names() ([]string, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for k := range all {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
