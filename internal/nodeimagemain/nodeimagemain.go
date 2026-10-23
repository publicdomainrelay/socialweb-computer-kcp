package nodeimagemain

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestrootfs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	ModeGuestRootfs   = "guest-rootfs"
	ModeGuestManifest = "guest-manifest"

	guestVcpu   = 2
	guestMemMiB = 2048

	guestInterface = "eth0"
)

const usage = `nodeimage - the build pipeline steps of scripts/build-guest-image.sh

  nodeimage guest-rootfs -build rootfs.json -base base.tar -asset dumbpipe=<path> \
      -source-date-epoch N -out rootfs.tar -report report.json
  nodeimage guest-manifest -target firecracker-x86_64 -dir <out> -report report.json

guest-rootfs composes the guest rootfs tar from the pinned base and the rootfs build file.
guest-manifest writes image.json for the guest image in -dir from versions.json and the report.
`

type assetFlags map[string]string

func (a assetFlags) String() string {
	names := make([]string, 0, len(a))
	for k, v := range a {
		names = append(names, k+"="+v)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func (a assetFlags) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("-asset takes name=path, got %q", value)
	}
	a[name] = path
	return nil
}

func Main(_ context.Context, inv foldmain.Invocation) int {
	if len(inv.Args) == 0 {
		fmt.Fprint(inv.Stderr, usage)
		return 2
	}
	mode, args := inv.Args[0], inv.Args[1:]
	fs := flag.NewFlagSet("nodeimage "+mode, flag.ContinueOnError)
	fs.SetOutput(inv.Stderr)
	fs.Usage = func() { fmt.Fprint(inv.Stderr, usage) }
	var run func() error
	switch mode {
	case ModeGuestRootfs:
		build := fs.String("build", "", "the rootfs build file (deploy/guest-image/rootfs.json)")
		base := fs.String("base", "", "the pinned base root tarball, decompressed")
		assets := assetFlags{}
		fs.Var(assets, "asset", "name=path of an asset the build bakes; repeatable")
		sourceDateEpoch := fs.Int64("source-date-epoch", 0, "the timestamp every entry is clamped to")
		out := fs.String("out", "", "the composed tar")
		report := fs.String("report", "", "the rootfs report")
		run = func() error {
			return guestRootfs(inv.Stdout, *build, *base, assets, *sourceDateEpoch, *out, *report)
		}
	case ModeGuestManifest:
		target := fs.String("target", "", "the guest image target in versions.json")
		dir := fs.String("dir", "", "the directory holding vmlinux and rootfs.ext4")
		report := fs.String("report", "", "the rootfs report")
		run = func() error { return guestManifest(inv.Stdout, *target, *dir, *report) }
	default:
		fmt.Fprintf(inv.Stderr, "nodeimage: unknown step %q\n", mode)
		fmt.Fprint(inv.Stderr, usage)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if err := run(); err != nil {
		fmt.Fprintf(inv.Stderr, "nodeimage %s: %v\n", mode, err)
		return 1
	}
	return 0
}

func guestRootfs(stdout io.Writer, buildPath, basePath string, assets map[string]string, sourceDateEpoch int64, outPath, reportPath string) error {
	b, err := guestrootfs.LoadBuild(buildPath)
	if err != nil {
		return err
	}
	spec, err := b.Spec(guestrootfs.Options{SourceDateEpoch: sourceDateEpoch, Assets: assets})
	if err != nil {
		return err
	}
	base, err := os.Open(basePath)
	if err != nil {
		return fmt.Errorf("the base root tarball: %w", err)
	}
	defer base.Close()
	tmp := outPath + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	report, err := guestrootfs.Compose(bufio.NewReaderSize(base, 1<<20), spec, w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(reportPath, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "rootfs tar %s sha256 %s entries %d data %d bytes ssh host keys %d\n",
		outPath, report.TarSHA256, len(report.Entries), report.DataBytes, report.SSHHostKeys)
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "internal", "versions", "versions.json")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no checkout of this module above the working directory; " +
				"the guest manifest records the sha256 of the kernel config it names")
		}
		dir = parent
	}
}

func fileRecord(dir, name string) (guestimage.FileRecord, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return guestimage.FileRecord{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return guestimage.FileRecord{}, err
	}
	return guestimage.FileRecord{File: name, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n}, nil
}

func kernelRelease(version string, config []byte) string {
	s := bufio.NewScanner(strings.NewReader(string(config)))
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "CONFIG_LOCALVERSION="); ok {
			if local, err := strconv.Unquote(v); err == nil {
				return version + local
			}
		}
	}
	return version
}

func guestManifest(stdout io.Writer, target, dir, reportPath string) error {
	g, err := versions.GuestImages()
	if err != nil {
		return err
	}
	tg, ok := g.Targets[target]
	if !ok {
		return fmt.Errorf("versions.json pins no guest image target %s", target)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("the rootfs report: %w", err)
	}
	var report guestrootfs.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("%s: %w", reportPath, err)
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(root, tg.KernelConfig))
	if err != nil {
		return fmt.Errorf("the kernel config: %w", err)
	}
	inputs := map[string]guestimage.Input{
		"kernelSource": {URL: g.Kernel.Source.URL, SHA256: g.Kernel.Source.SHA256},
		"kernelConfig": {Path: tg.KernelConfig, SHA256: sha256sum.Hex(config)},
		"base":         {URL: tg.Base.URL, SHA256: tg.Base.SHA256},
	}
	for name, rel := range map[string]string{"builder": g.Builder["dockerfile"], "rootfsBuild": "deploy/guest-image/rootfs.json"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		inputs[name] = guestimage.Input{Path: rel, SHA256: sha256sum.Hex(body)}
	}
	kernel, err := fileRecord(dir, guestimage.KernelName)
	if err != nil {
		return err
	}
	rootfs, err := fileRecord(dir, guestimage.RootfsName)
	if err != nil {
		return err
	}
	m := guestimage.Manifest{
		VMM:             tg.Hypervisor,
		BootArgs:        tg.BootArgs,
		Vcpu:            guestVcpu,
		MemMiB:          guestMemMiB,
		GuestInterface:  guestInterface,
		Target:          target,
		Hypervisor:      tg.Hypervisor,
		Arch:            tg.Arch,
		KernelVersion:   kernelRelease(g.Kernel.Version, config),
		Kernel:          kernel,
		Rootfs:          rootfs,
		RootfsLabel:     guestrootfs.RootfsLabel,
		RootfsTarSHA256: report.TarSHA256,
		SourceDateEpoch: tg.SourceDateEpoch,
		Bake:            report.Bake,
		Inputs:          inputs,
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, guestimage.ManifestName)
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest %s\n", path)
	return nil
}
