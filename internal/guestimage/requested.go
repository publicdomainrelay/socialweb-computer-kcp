package guestimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	RequestedManifestMax = 64 << 10

	RequestedBootArgsMax = 4096

	RequestedMarkerMax = 256

	RequestedDialTimeout = 10 * time.Second

	RequestedHeaderTimeout = 30 * time.Second

	RequestedManifestTimeout = 30 * time.Second

	RequestedFetchTimeout = 30 * time.Minute
)

var ErrRequestedRefused = errors.New("guestimage: the requested image is refused")

type Requested struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func (r Requested) Valid() error {
	u, err := url.Parse(r.URL)
	switch {
	case err != nil:
		return fmt.Errorf("%w: the url %q: %v", ErrRequestedRefused, r.URL, err)
	case u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("%w: the url %q is not a plain https base", ErrRequestedRefused, r.URL)
	case len(r.SHA256) != SHA256HexLen || strings.Trim(r.SHA256, "0123456789abcdef") != "":
		return fmt.Errorf("%w: the sha256 %q is not 64 lowercase hex digits", ErrRequestedRefused, r.SHA256)
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

func PublicClient() *http.Client {
	dialer := &net.Dialer{Timeout: RequestedDialTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !PublicAddr(ap.Addr()) {
			return fmt.Errorf("%w: %s is not a public address", ErrRequestedRefused, address)
		}
		return nil
	}}
	return &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: RequestedDialTimeout,
			ResponseHeaderTimeout: RequestedHeaderTimeout, ForceAttemptHTTP2: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 5 {
				return fmt.Errorf("%w: a redirect to %s", ErrRequestedRefused, req.URL)
			}
			return nil
		},
	}
}

type fetchedManifest struct {
	manifest Manifest
	bytes    int64
}

var (
	manifestMu sync.Mutex

	manifests = map[Requested]fetchedManifest{}
)

const manifestsMax = 256

func FetchManifest(ctx context.Context, client *http.Client, req Requested) (Manifest, int64, error) {
	if err := req.Valid(); err != nil {
		return Manifest{}, 0, err
	}
	manifestMu.Lock()
	held, ok := manifests[req]
	manifestMu.Unlock()
	if ok {
		return held.manifest, held.bytes, nil
	}
	ctx, cancel := context.WithTimeout(ctx, RequestedManifestTimeout)
	defer cancel()
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(req.URL, "/")+"/"+ManifestName, nil)
	if err != nil {
		return Manifest{}, 0, err
	}
	resp, err := client.Do(get)
	if err != nil {
		return Manifest{}, 0, fmt.Errorf("%w: fetching %s: %w", ErrRequestedRefused, get.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, 0, fmt.Errorf("%w: GET %s answered %s", ErrRequestedRefused, get.URL, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, RequestedManifestMax+1))
	if err != nil {
		return Manifest{}, 0, fmt.Errorf("%w: reading %s: %w", ErrRequestedRefused, get.URL, err)
	}
	if len(raw) > RequestedManifestMax {
		return Manifest{}, 0, fmt.Errorf("%w: %s is over %d bytes", ErrRequestedRefused, get.URL, RequestedManifestMax)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != req.SHA256 {
		return Manifest{}, 0, fmt.Errorf("%w: %s hashes to %s, not the requested %s", ErrRequestedRefused, get.URL, got, req.SHA256)
	}
	var m struct {
		Manifest
		Bake   json.RawMessage `json:"bake"`
		Inputs json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, 0, fmt.Errorf("%w: %s: %w", ErrRequestedRefused, get.URL, err)
	}
	manifestMu.Lock()
	if len(manifests) >= manifestsMax {
		clear(manifests)
	}
	manifests[req] = fetchedManifest{manifest: m.Manifest, bytes: int64(len(raw))}
	manifestMu.Unlock()
	return m.Manifest, int64(len(raw)), nil
}

func (m Manifest) CheckFor(pin Pin, capBytes int64) error {
	file := func(f FileRecord, name string) error {
		if f.File != name || len(f.SHA256) != SHA256HexLen || strings.Trim(f.SHA256, "0123456789abcdef") != "" || f.Bytes <= 0 {
			return fmt.Errorf("%w: its %s is not {file %s, a lowercase hex sha256, bytes > 0}", ErrRequestedRefused, name, name)
		}
		return nil
	}
	switch {
	case m.Target != pin.Target || m.Hypervisor != pin.Hypervisor || m.Arch != pin.Arch:
		return fmt.Errorf("%w: it is for %s (%s %s); this bidder boots %s (%s %s)", ErrRequestedRefused,
			m.Target, m.Hypervisor, m.Arch, pin.Target, pin.Hypervisor, pin.Arch)
	case m.Vcpu <= 0 || m.MemMiB <= 0:
		return fmt.Errorf("%w: it names no vcpu or memMiB", ErrRequestedRefused)
	case len(m.BootArgs) == 0 || len(strings.Join(m.BootArgs, " ")) > RequestedBootArgsMax:
		return fmt.Errorf("%w: its bootArgs are empty or over %d bytes", ErrRequestedRefused, RequestedBootArgsMax)
	case len(m.Marker) > RequestedMarkerMax:
		return fmt.Errorf("%w: its marker is over %d bytes", ErrRequestedRefused, RequestedMarkerMax)
	}
	if err := file(m.Kernel, KernelName); err != nil {
		return err
	}
	if err := file(m.Rootfs, RootfsName); err != nil {
		return err
	}
	if capBytes <= 0 || m.Kernel.Bytes > capBytes-m.Rootfs.Bytes {
		return fmt.Errorf("%w: its kernel and rootfs are %d bytes, over the %d bytes of the requested disk", ErrRequestedRefused,
			m.Kernel.Bytes+m.Rootfs.Bytes, capBytes)
	}
	return nil
}

func (m Manifest) PinFor(pin Pin, req Requested, manifestBytes int64) Pin {
	return Pin{Target: pin.Target, Hypervisor: pin.Hypervisor, Arch: pin.Arch, Files: map[string]FilePin{
		ManifestName: {SHA256: req.SHA256, Bytes: manifestBytes},
		KernelName:   {SHA256: m.Kernel.SHA256, Bytes: m.Kernel.Bytes},
		RootfsName:   {SHA256: m.Rootfs.SHA256, Bytes: m.Rootfs.Bytes},
	}}
}

func (s Store) EnsureRequested(ctx context.Context, req Requested, pin Pin, capBytes int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestedFetchTimeout)
	defer cancel()
	s.Client = PublicClient()
	m, n, err := FetchManifest(ctx, s.Client, req)
	if err != nil {
		return Result{}, err
	}
	if err := m.CheckFor(pin, capBytes); err != nil {
		return Result{}, err
	}
	res, err := s.Ensure(ctx, m.PinFor(pin, req, n), req.URL)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrRequestedRefused, err)
	}
	return res, nil
}
