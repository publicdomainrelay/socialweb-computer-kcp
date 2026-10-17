package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denorun"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devcore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devsup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/denoruntime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/gatewaykcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	devsupReadyTimeout = 15 * time.Second
	devsupExitTimeout  = 90 * time.Second
	networkDownTimeout = 2 * time.Minute
	coreStopTimeout    = 10 * time.Second
)

func (a *app) fail(code int, format string, args ...any) int {
	fmt.Fprintf(a.stderr, format+"\n", args...)
	return code
}

func (a *app) printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(a.stdout, string(b))
}

func (a *app) layout(vals clicfg.Values) (devinstance.Layout, error) {
	return a.layoutOf(vals.String("instance"), vals)
}

func (a *app) layoutOf(name string, vals clicfg.Values) (devinstance.Layout, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return devinstance.Layout{}, err
	}
	id, err := devinstance.ID(name, cwd, a.env)
	if err != nil {
		return devinstance.Layout{}, err
	}
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return devinstance.Layout{}, err
	}
	rt, err := devinstance.RuntimeRoot(a.env)
	if err != nil {
		return devinstance.Layout{}, err
	}
	return devinstance.NewLayout(id, root, rt)
}

func (a *app) worktree(vals clicfg.Values) string {
	if vals.String("instance") != "" {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ""
	}
	return devinstance.WorktreeRoot(real)
}

func writeEnvFiles(l devinstance.Layout, ex []devinstance.Export) error {
	if err := os.WriteFile(filepath.Join(l.StateDir, devinstance.EnvFile), []byte(devinstance.ShellExports(ex)), 0o600); err != nil {
		return err
	}
	return devinstance.WriteJSON(filepath.Join(l.StateDir, devinstance.EnvJSONFile), exportMap(ex))
}

func exportMap(ex []devinstance.Export) map[string]string {
	out := map[string]string{}
	for _, e := range ex {
		out[e.Name] = e.Value
	}
	return out
}

func coreSpec(rec *devinstance.Record, exe string, readyTimeout time.Duration) (hostsvc.UnitSpec, error) {
	return bootstrap.Config{Layout: rec.Layout, Exe: exe, ReadyTimeout: readyTimeout}.CoreSpec(rec)
}

func (a *app) postgresRoot(vals clicfg.Values, profile string) (devinstance.Executable, error) {
	root := vals.String("postgres-root")
	if assets.Release() {
		if root != "" {
			return devinstance.Executable{}, errors.New("postgres-root is a dev-build option; this release build runs its embedded postgres")
		}
		cache, err := a.assetCache(vals)
		if err != nil {
			return devinstance.Executable{}, err
		}
		e, err := assets.Resolve(assets.Options{Cache: cache, Log: a.log}, pgunit.Asset)
		if err != nil {
			return devinstance.Executable{}, err
		}
		if root, err = pgunit.RootOf(e.Path); err != nil {
			return devinstance.Executable{}, err
		}
	}
	if root == "" {
		if bootstrap.NeedsPostgres(profile) {
			return devinstance.Executable{}, fmt.Errorf("--profile %s runs kine on postgres: postgres-root must name the absolute root of the extracted postgres asset tree (versions.json postgres; never PATH)", profile)
		}
		return devinstance.Executable{}, nil
	}
	in, err := pgunit.FromRoot(root)
	if err != nil {
		return devinstance.Executable{}, err
	}
	e, err := hashExe("postgres-root", in.Path("postgres"))
	if err != nil {
		return devinstance.Executable{}, err
	}
	return devinstance.Executable{Path: root, SHA256: e.SHA256, Version: in.Version}, nil
}

func selfExe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func checkExe(ctx context.Context, option, path string) (devinstance.Executable, error) {
	e, err := hashExe(option, path)
	if err != nil {
		return e, err
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(vctx, path, "--version").CombinedOutput()
	e.Version = strings.TrimSpace(string(out))
	return e, nil
}

func hashExe(option, path string) (devinstance.Executable, error) {
	if path == "" {
		return devinstance.Executable{}, fmt.Errorf("%s is required in this dev build (an absolute path; never PATH)", option)
	}
	if !filepath.IsAbs(path) {
		return devinstance.Executable{}, fmt.Errorf("%s %q must be an absolute path; this build never looks in PATH", option, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return devinstance.Executable{}, fmt.Errorf("%s: %w", option, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return devinstance.Executable{}, fmt.Errorf("%s %s is not an executable file", option, path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return devinstance.Executable{}, err
	}
	return devinstance.Executable{Path: path, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func lockFile(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking)
		if err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%s is held by another dev up or dev down of this instance", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func procConfig(l devinstance.Layout) hostsvc.Config {
	return hostsvc.Config{Backend: hostsvc.BackendProc, ProcSocket: l.Socket}
}

func helloDevsup(ctx context.Context, l devinstance.Layout) (hostsvc.ProcClient, hostsvc.ProcHello, error) {
	c, err := hostsvc.OpenProcClient(ctx, procConfig(l))
	if err != nil {
		return nil, hostsvc.ProcHello{}, err
	}
	h, err := c.Hello(ctx)
	if err != nil {
		return nil, hostsvc.ProcHello{}, err
	}
	return c, h, nil
}

var errDevsupDetached = errors.New("the instance is already up with a detached devsup")

func startOrAdoptDevsup(ctx context.Context, a *app, l devinstance.Layout, exe string, dieWith int, fg bool) (hostsvc.ProcClient, devinstance.Devsup, error) {
	if c, h, err := helloDevsup(ctx, l); err == nil {
		if fg {
			c.Close()
			return nil, devinstance.Devsup{}, fmt.Errorf("%w (devsup pid %d); dev down it first, or run dev up without --fg", errDevsupDetached, h.PID)
		}
		return c, devinstance.Devsup{Instance: h.Instance, PID: h.PID, StartTicks: h.StartTicks, BootID: h.BootID, Adopted: true}, nil
	}
	if err := os.MkdirAll(l.SupState, 0o700); err != nil {
		return nil, devinstance.Devsup{}, err
	}
	argv := []string{exe, "internal", "devsup", "--state", l.SupState, "--socket", l.Socket, "--instance", l.ID}
	if dieWith != 0 {
		argv = append(argv, "--die-with", strconv.Itoa(dieWith))
	}
	pid, err := devinstance.SpawnDetached(argv, filepath.Join(l.SupState, "devsup.log"))
	if err != nil {
		return nil, devinstance.Devsup{}, fmt.Errorf("starting devsup: %w", err)
	}
	deadline := time.Now().Add(devsupReadyTimeout)
	for {
		c, h, err := helloDevsup(ctx, l)
		if err == nil && h.PID == pid {
			a.log.Info("devsup started", "pid", pid, "instance", h.Instance, "socket", l.Socket)
			return c, devinstance.Devsup{Instance: h.Instance, PID: h.PID, StartTicks: h.StartTicks, BootID: h.BootID}, nil
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(l.SupState, "devsup.log"))
			return nil, devinstance.Devsup{}, fmt.Errorf("devsup pid %d did not answer on %s: %v\n%s", pid, l.Socket, err, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type upReport struct {
	Instance        string                            `json:"instance"`
	Profile         string                            `json:"profile"`
	StateDir        string                            `json:"stateDir"`
	Socket          string                            `json:"socket"`
	AdminKubeconfig string                            `json:"adminKubeconfig"`
	KcpURL          string                            `json:"kcpURL"`
	KineURL         string                            `json:"kineURL"`
	Devsup          devinstance.Devsup                `json:"devsup"`
	Units           []devinstance.Unit                `json:"units"`
	Core            devinstance.Core                  `json:"core"`
	StartedAt       time.Time                         `json:"startedAt"`
	ReadyAt         time.Time                         `json:"readyAt"`
	Seconds         float64                           `json:"seconds"`
	Env             map[string]string                 `json:"env"`
	EnvFile         string                            `json:"envFile"`
	Marker          string                            `json:"marker,omitempty"`
	Foreground      bool                              `json:"foreground,omitempty"`
	Executables     map[string]devinstance.Executable `json:"executables"`
	Network         *v1alpha1.MarketNetwork           `json:"network,omitempty"`
	layout          devinstance.Layout
}

func runDevUp(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	return devUp(ctx, a, vals, false)
}

func runDevHost(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	return devUp(ctx, a, vals, true)
}

func (a *app) parentOf(ctx context.Context, vals clicfg.Values, child devinstance.Layout) (devinstance.Parent, hostsvc.ProcHello, error) {
	pl, err := a.layoutOf(vals.String("parent"), vals)
	if err != nil {
		return devinstance.Parent{}, hostsvc.ProcHello{}, err
	}
	if pl.ID == child.ID {
		return devinstance.Parent{}, hostsvc.ProcHello{}, fmt.Errorf("instance %s cannot be its own parent", child.ID)
	}
	var prec devinstance.Record
	if err := devinstance.ReadJSON(pl.Record, &prec); err != nil {
		return devinstance.Parent{}, hostsvc.ProcHello{}, fmt.Errorf("parent %s has no instance record (dev host first): %w", pl.ID, err)
	}
	if !prec.Host || !prec.Ready || prec.AdminKubeconfig == "" {
		return devinstance.Parent{}, hostsvc.ProcHello{}, fmt.Errorf("instance %s is not a ready dev host (dev host --instance %s)", pl.ID, pl.ID)
	}
	if prec.Parent != nil {
		return devinstance.Parent{}, hostsvc.ProcHello{}, fmt.Errorf("parent %s is itself a child; instance trees do not nest", pl.ID)
	}
	c, h, err := helloDevsup(ctx, pl)
	if err != nil {
		return devinstance.Parent{}, hostsvc.ProcHello{}, fmt.Errorf("the devsup of parent %s: %w", pl.ID, err)
	}
	c.Close()
	return devinstance.Parent{ID: pl.ID, Socket: pl.Socket, Kubeconfig: prec.AdminKubeconfig, Workspace: devinstance.ChildWorkspace(child.ID)}, h, nil
}

func bringUp(ctx context.Context, a *app, vals clicfg.Values, host bool) (upReport, int) {
	started := time.Now().UTC()
	verb := "dev up"
	if host {
		verb = "dev host"
	}
	if err := devinstance.RefuseRoot(os.Geteuid()); err != nil {
		return upReport{}, a.fail(exitRefused, "%s: %v", verb, err)
	}
	if b := vals.String(backendOpt); b != "" && b != string(hostsvc.BackendProc) {
		return upReport{}, a.fail(exitRefused, "%s: host-backend=%s: dev places every unit under devsup (proc); systemd-user is for install", verb, b)
	}
	profile, standInsFile := vals.String("profile"), vals.String("stand-ins")
	parent := vals.String("parent")
	if host && profile == "" {
		profile = bootstrap.ProfileQuick
	}
	if parent != "" && standInsFile != "" {
		return upReport{}, a.fail(exitRefused, "dev up: --parent places the bootstrap layer through the parent's kcp; stand-ins have no kcp")
	}
	var standIns []devinstance.StandIn
	switch {
	case profile != "" && standInsFile != "":
		return upReport{}, a.fail(exitRefused, "dev up: --profile %s and --stand-ins name two bootstrap layers; pass one", profile)
	case profile == "" && standInsFile == "":
		return upReport{}, a.fail(exitRefused, "dev up: name a profile (--profile; built: quick, prodlike) or the units to place (--stand-ins)")
	case standInsFile != "":
		s, err := bootstrap.ReadStandIns(standInsFile)
		if err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
		standIns, profile = s, bootstrap.ProfileStandIns
	default:
		if err := bootstrap.CheckProfile(profile); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	composes := profile == bootstrap.ProfileMarketNetwork
	if composes && (host || parent != "") {
		return upReport{}, a.fail(exitRefused, "%s: --profile %s is an application of its own whose children are workspaces of its kcp; it neither nests under a parent nor is a dev host", verb, profile)
	}
	var networkOverlay []byte
	if f := vals.String("app-values"); f != "" {
		if !composes {
			return upReport{}, a.fail(exitRefused, "dev up: app-values names values for the children of --profile %s", bootstrap.ProfileMarketNetwork)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: app-values: %v", err)
		}
		networkOverlay = b
	}
	networkTimeout, err := time.ParseDuration(vals.String("network-timeout"))
	if err != nil {
		return upReport{}, a.fail(exitRefused, "dev up: network-timeout: %v", err)
	}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return upReport{}, a.fail(exitRefused, "dev up: ready-timeout: %v", err)
	}
	if readyTimeout < 0 {
		return upReport{}, a.fail(exitRefused, "dev up: ready-timeout: %s is negative", readyTimeout)
	}
	if composes && vals.Int("bidders") > 0 {
		if err := a.vmHostPreflight(ctx, vals); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	var kcpExe, kineExe devinstance.Executable
	pgExe, err := a.postgresRoot(vals, profile)
	if err != nil {
		return upReport{}, a.fail(exitRefused, "dev up: %v", err)
	}
	if standIns == nil {
		if kcpExe, err = a.devExe(ctx, vals, "kcp", "kcp-path"); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
		if want, err := versions.Get("kcp"); err == nil && want.Version != "" && !strings.Contains(kcpExe.Version, want.Version) {
			return upReport{}, a.fail(exitRefused, "dev up: kcp-path %s reports %q; versions.json pins %s", kcpExe.Path, kcpExe.Version, want.Version)
		}
		if kineExe, err = a.devExe(ctx, vals, "kine", "kine-path"); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	denoExe, err := a.denoExe(ctx, vals)
	if err != nil {
		return upReport{}, a.fail(exitRefused, "dev up: %v", err)
	}
	sourceRoot, err := closureSourceRoot(vals)
	if err != nil {
		return upReport{}, a.fail(exitRefused, "dev up: %v", err)
	}
	var baoExe devinstance.Executable
	if standIns == nil && (assets.Release() || vals.String("bao-path") != "" || composes) {
		if baoExe, err = a.devExe(ctx, vals, "bao", "bao-path"); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	var pluginExes map[string]devinstance.Executable
	if standIns == nil {
		if pluginExes, err = a.openBaoPluginExes(vals); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	var dumbpipeExe devinstance.Executable
	if standIns == nil && (assets.Release() || vals.String("dumbpipe-path") != "") {
		if dumbpipeExe, err = a.devExe(ctx, vals, appctl.DumbpipeExecutable, "dumbpipe-path"); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	var gatewayExes map[string]devinstance.Executable
	if composes && vals.Bool("with-gateway") {
		if gatewayExes, err = a.gatewayChildExecutables(vals); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
	}
	exe, err := selfExe()
	if err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	name := vals.String("instance")
	if host && name == "" {
		name = devinstance.HostID
	}
	l, err := a.layoutOf(name, vals)
	if err != nil {
		return upReport{}, a.fail(exitRefused, "%s: %v", verb, err)
	}
	var networkSpec v1alpha1.MarketNetworkSpec
	if composes {
		imageSource := vals.String("guest-image-source")
		if imageSource == "" {
			if imageSource, err = publishedGuestImageSource(ctx, vals.String("guest-image-release-base")); err != nil {
				return upReport{}, a.fail(exitRefused, "dev up: %v", err)
			}
		}
		vm := appctl.BidderVMDefaults{GuestImageSource: imageSource, VmHostBackend: vals.String("vm-host-backend")}
		if networkSpec, err = appctl.NetworkSpec(l.ID, vals.Int("bidders"), vals.Bool("with-gateway"), networkOverlay, vm); err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: %v", err)
		}
		for i := range networkSpec.Apps {
			if networkSpec.Apps[i].Kind != v1alpha1.ApplicationKindGateway {
				continue
			}
			if networkSpec.Apps[i].ClusterProvider, err = gatewaykcp.ClusterProviderFromEnv(a.env.LookupEnv, networkSpec.Apps[i].ClusterProvider); err != nil {
				return upReport{}, a.fail(exitRefused, "dev up: the gateway's clusterprovider settings: %v", err)
			}
		}
	}
	var par *devinstance.Parent
	var parentHello hostsvc.ProcHello
	if parent != "" {
		p, h, err := a.parentOf(ctx, vals, l)
		if err != nil {
			return upReport{}, a.fail(exitRefused, "dev up: --parent %s: %v", parent, err)
		}
		par, parentHello = &p, h
	}
	for _, d := range []string{l.StateDir, l.RuntimeDir, l.Logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return upReport{}, a.fail(exitFailed, "dev up: %v", err)
		}
		os.Chmod(d, 0o700)
	}
	lock, err := lockFile(l.Lock, 30*time.Second)
	if err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	fg := vals.Bool("fg")
	dieWith := vals.Int("die-with")
	if fg {
		dieWith = os.Getpid()
	}
	var m hostsvc.Manager
	var sup devinstance.Devsup
	if par != nil {
		if err := devinstance.EnsureChildWorkspace(ctx, par.Kubeconfig, l.ID, devinstance.ChildWorkspaceReady); err != nil {
			lock.Close()
			return upReport{}, a.fail(exitFailed, "dev up: %v", err)
		}
		r, err := devinstance.OpenRemote(ctx, *par, a.log)
		if err != nil {
			lock.Close()
			return upReport{}, a.fail(exitFailed, "dev up: %v", err)
		}
		m = r
		sup = devinstance.Devsup{Instance: parentHello.Instance, PID: parentHello.PID, StartTicks: parentHello.StartTicks,
			BootID: parentHello.BootID, Adopted: true}
		a.log.Info("the bootstrap layer is placed through the parent's kcp", "parent", par.ID, "workspace", par.Workspace)
	} else {
		c, s, err := startOrAdoptDevsup(ctx, a, l, exe, dieWith, fg)
		if errors.Is(err, errDevsupDetached) {
			lock.Close()
			return upReport{}, a.fail(exitRefused, "%s: %v", verb, err)
		}
		if err != nil {
			lock.Close()
			return upReport{}, a.fail(exitFailed, "%s: %v", verb, err)
		}
		m, sup = c, s
	}
	defer m.Close()
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err != nil {
		rec = devinstance.Record{Created: started}
	}
	rec.Layout, rec.Profile, rec.Backend, rec.Devsup, rec.StandIns = l, profile, hostsvc.BackendProc, sup, standIns
	rec.Host, rec.Parent = host || composes, par
	rec.Worktree = a.worktree(vals)
	if host {
		rec.Worktree = ""
	}
	rec.BaseWorkspace = vals.String("kcp-base-workspace")
	rec.Executables = map[string]devinstance.Executable{"kcp": kcpExe, "kine": kineExe, "postgres": pgExe, denorun.Executable: denoExe, "socialweb-computer": {Path: exe}}
	if baoExe.Path != "" {
		rec.Executables["bao"] = baoExe
	}
	if dumbpipeExe.Path != "" {
		rec.Executables[appctl.DumbpipeExecutable] = dumbpipeExe
	}
	for name, e := range gatewayExes {
		rec.Executables[name] = e
	}
	for name, e := range pluginExes {
		rec.Executables[name] = e
	}
	rec.SourceRoot = sourceRoot
	rec.OpenBao = nil
	if addr := vals.String("openbao-addr"); addr != "" {
		tokenFile := vals.String("openbao-token-file")
		if !filepath.IsAbs(tokenFile) {
			lock.Close()
			return upReport{}, a.fail(exitRefused, "dev up: openbao-addr %s needs openbao-token-file as an absolute path, got %q", addr, tokenFile)
		}
		rec.OpenBao = &devinstance.OpenBao{Address: addr, TokenFile: tokenFile}
	}
	rec.VM = nil
	if !host {
		vmExes, err := a.devVMExecutables(vals)
		if err != nil {
			lock.Close()
			return upReport{}, a.fail(exitRefused, "%s: %v", verb, err)
		}
		for name, e := range vmExes {
			rec.Executables[name] = e
		}
		if rec.VM, err = a.devVM(vals, l); err != nil {
			lock.Close()
			return upReport{}, a.fail(exitRefused, "%s: %v", verb, err)
		}
	}
	bcfg := bootstrap.Config{Layout: l, KcpPath: kcpExe.Path, KinePath: kineExe.Path, PostgresRoot: pgExe.Path, Exe: exe,
		ReadyTimeout: readyTimeout, Log: a.log}
	if err := bootstrap.Ensure(ctx, m, bcfg, &rec); err != nil {
		devinstance.WriteJSON(l.Record, rec)
		lock.Close()
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	if err := devinstance.WriteJSON(l.Record, rec); err != nil {
		lock.Close()
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	lock.Close()
	cspec, err := coreSpec(&rec, exe, readyTimeout)
	if err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	cst, err := m.Start(ctx, cspec)
	if err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: placing the core: %v", err)
	}
	core, err := waitCore(ctx, l, m, cspec.Name, cst.InvocationID, readyTimeout)
	if err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	devinstance.ReadJSON(l.Record, &rec)
	if host || composes {
		if err := devinstance.WaitHost(ctx, rec.AdminKubeconfig, readyTimeout); err != nil {
			return upReport{}, a.fail(exitFailed, "%s: %v", verb, err)
		}
	}
	var network *v1alpha1.MarketNetwork
	if composes {
		if err := appctl.ApplyNetwork(ctx, rec.AdminKubeconfig, networkSpec, readyTimeout); err != nil {
			return upReport{}, a.fail(exitFailed, "dev up: %v", err)
		}
		a.log.Info("the MarketNetwork root object is applied; its core composes the children", "bidders", vals.Int("bidders"),
			"gateway", vals.Bool("with-gateway"), "apps", len(networkSpec.Apps))
		o, err := appctl.WaitNetwork(ctx, rec.AdminKubeconfig, networkTimeout, func(st v1alpha1.MarketNetworkStatus) {
			var apps []string
			for _, ap := range st.Apps {
				apps = append(apps, ap.Name+"="+string(ap.Phase))
			}
			a.log.Info("market network", "phase", string(st.Phase), "message", st.Message, "apps", strings.Join(apps, " "))
		})
		network = &o
		if err != nil {
			a.printJSON(network)
			return upReport{}, a.fail(exitFailed, "dev up: %v", err)
		}
	}
	ex := devinstance.Exports(rec)
	if err := writeEnvFiles(l, ex); err != nil {
		return upReport{}, a.fail(exitFailed, "dev up: %v", err)
	}
	marker := ""
	if rec.Worktree != "" {
		if err := devinstance.LinkMarker(rec.Worktree, l.StateDir); err != nil {
			a.log.Warn("worktree marker", "err", err.Error())
		} else {
			marker = filepath.Join(rec.Worktree, devinstance.Marker)
		}
	}
	ready := time.Now().UTC()
	return upReport{Instance: l.ID, Profile: rec.Profile, StateDir: l.StateDir, Socket: l.Socket,
		AdminKubeconfig: rec.AdminKubeconfig, KcpURL: rec.KcpURL, KineURL: rec.KineURL, Devsup: sup, Units: rec.Units,
		Core: core, StartedAt: started, ReadyAt: ready, Seconds: ready.Sub(started).Seconds(),
		Env: exportMap(ex), EnvFile: filepath.Join(l.StateDir, devinstance.EnvFile), Marker: marker, Foreground: fg,
		Executables: rec.Executables, Network: network, layout: l}, exitOK
}

func devUp(ctx context.Context, a *app, vals clicfg.Values, host bool) int {
	r, code := bringUp(ctx, a, vals, host)
	if code != exitOK {
		return code
	}
	a.printJSON(r)
	if !r.Foreground {
		return exitOK
	}
	a.log.Info("dev up --fg: the instance lives as long as this process; Ctrl-C is dev down", "instance", r.layout.ID)
	<-ctx.Done()
	d := downInstance(context.Background(), a, r.layout, vals.Bool("keep-state"))
	a.printJSON(d)
	if len(d.Remaining) > 0 {
		return exitFailed
	}
	return exitOK
}

func waitCore(ctx context.Context, l devinstance.Layout, m hostsvc.Manager, unit, invocation string, timeout time.Duration) (devinstance.Core, error) {
	deadline := time.Now().Add(timeout)
	for {
		var c devinstance.Core
		if err := devinstance.ReadJSON(l.CoreRecord, &c); err == nil && c.InvocationID == invocation && !c.AdoptedAt.IsZero() {
			return c, nil
		}
		if st, err := m.Inspect(ctx, unit); err == nil && ((st.InvocationID != "" && st.InvocationID != invocation) ||
			st.State == hostsvc.Failed || st.State == hostsvc.Inactive || st.State == hostsvc.NotFound) {
			b, _ := os.ReadFile(filepath.Join(l.Logs, "core.log"))
			return devinstance.Core{}, fmt.Errorf("the core (invocation %s) ended before it recorded its adoption: unit %s is %s %s (invocation %s, exit status %d)\n%s",
				invocation, unit, st.State, st.Result, st.InvocationID, st.ExecMainStatus, lastLines(b, 40))
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(l.Logs, "core.log"))
			return devinstance.Core{}, fmt.Errorf("the core (invocation %s) did not record its adoption within %s\n%s", invocation, timeout, b)
		}
		select {
		case <-ctx.Done():
			return devinstance.Core{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type statusReport struct {
	Instance string              `json:"instance"`
	Found    bool                `json:"found"`
	Record   *devinstance.Record `json:"record,omitempty"`
	Core     *devinstance.Core   `json:"core,omitempty"`
	Devsup   struct {
		Reachable bool                 `json:"reachable"`
		Hello     *hostsvc.ProcHello   `json:"hello,omitempty"`
		Units     []hostsvc.UnitStatus `json:"units,omitempty"`
		Error     string               `json:"error,omitempty"`
	} `json:"devsup"`
	Ports        map[string]int          `json:"ports,omitempty"`
	Network      *v1alpha1.MarketNetwork `json:"network,omitempty"`
	NetworkError string                  `json:"networkError,omitempty"`
	Apps         []appStatusView         `json:"apps,omitempty"`
}

func runDevStatus(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	l, err := a.layout(vals)
	if err != nil {
		return a.fail(exitRefused, "dev status: %v", err)
	}
	r := statusReport{Instance: l.ID}
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err == nil {
		r.Found, r.Record = true, &rec
		r.Ports = map[string]int{}
		for _, u := range rec.Units {
			r.Ports[u.Role] = u.Port
		}
	}
	var core devinstance.Core
	if err := devinstance.ReadJSON(l.CoreRecord, &core); err == nil {
		r.Core = &core
	}
	sl := l
	if rec.Parent != nil {
		sl.Socket = rec.Parent.Socket
	}
	if c, h, err := helloDevsup(ctx, sl); err == nil {
		r.Devsup.Reachable, r.Devsup.Hello = true, &h
		for _, prefix := range []string{l.Names().UnitPrefix(), l.Names().CorePrefix()} {
			units, err := c.List(ctx, prefix)
			if err != nil {
				r.Devsup.Error = err.Error()
			}
			r.Devsup.Units = append(r.Devsup.Units, units...)
		}
	} else {
		r.Devsup.Error = err.Error()
	}
	if rec.Profile == bootstrap.ProfileMarketNetwork && rec.AdminKubeconfig != "" {
		if o, err := appctl.ReadNetwork(ctx, rec.AdminKubeconfig); err == nil {
			r.Network = &o
		} else {
			r.NetworkError = err.Error()
		}
		r.Apps = appStatuses(ctx, l, rec)
	}
	a.printJSON(r)
	if !r.Found {
		return exitFailed
	}
	return exitOK
}

type downReport struct {
	Instance       string               `json:"instance"`
	MarkerRemoved  bool                 `json:"markerRemoved"`
	DevsupStopped  bool                 `json:"devsupStopped"`
	DevsupPID      int                  `json:"devsupPID,omitempty"`
	Swept          []devsup.SweepReport `json:"swept,omitempty"`
	LeftoverKilled []int                `json:"leftoverKilled"`
	Remaining      []int                `json:"remaining"`
	VMsStopped     []string             `json:"vmsStopped,omitempty"`
	VMsLeft        []string             `json:"vmsLeft,omitempty"`
	RemovedState   bool                 `json:"removedState"`
	RemovedRuntime bool                 `json:"removedRuntime"`
	Seconds        float64              `json:"seconds"`
}

func runDevDown(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	l, err := a.layout(vals)
	if err != nil {
		return a.fail(exitRefused, "dev down: %v", err)
	}
	if _, err := os.Stat(l.StateDir); err == nil {
		lock, err := lockFile(l.Lock, 2*time.Minute)
		if err != nil {
			return a.fail(exitFailed, "dev down: %v", err)
		}
		defer lock.Close()
	}
	r := downInstance(ctx, a, l, vals.Bool("keep-state"))
	a.printJSON(r)
	if len(r.Remaining) > 0 {
		return exitFailed
	}
	return exitOK
}

func downInstance(ctx context.Context, a *app, l devinstance.Layout, keepState bool) downReport {
	started := time.Now()
	r := downReport{Instance: l.ID, LeftoverKilled: []int{}, Remaining: []int{}}
	var rec devinstance.Record
	devinstance.ReadJSON(l.Record, &rec)
	vmOwners := []devinstance.Record{rec}
	if rec.Profile == bootstrap.ProfileMarketNetwork {
		vmOwners = append(vmOwners, networkChildren(l)...)
	}
	if rec.Profile == bootstrap.ProfileMarketNetwork && rec.AdminKubeconfig != "" {
		if err := appctl.DeleteNetwork(ctx, rec.AdminKubeconfig, networkDownTimeout); err != nil {
			a.log.Warn("deleting the MarketNetwork before devsup stops; devsup stops what is left", "err", err.Error())
		} else {
			a.log.Info("the MarketNetwork is deleted: its children, their kcps and its services are gone")
		}
	}
	for _, o := range vmOwners {
		stopped, left := a.downVMs(ctx, o)
		r.VMsStopped, r.VMsLeft = append(r.VMsStopped, stopped...), append(r.VMsLeft, left...)
	}
	if rec.Parent != nil {
		r.Remaining = downChild(ctx, a, l, *rec.Parent)
		r.DevsupPID = rec.Devsup.PID
	} else if c, h, err := helloDevsup(ctx, l); err == nil {
		r.DevsupPID = h.PID
		if err := c.Shutdown(ctx); err != nil {
			a.log.Warn("devsup shutdown", "err", err.Error())
		}
		deadline := time.Now().Add(devsupExitTimeout)
		for devsup.Alive(h.PID) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		r.DevsupStopped = !devsup.Alive(h.PID)
	} else {
		a.log.Info("devsup unreachable; sweeping its records", "err", err.Error())
		if rep, err := devsup.SweepOffline(l.SupState); err == nil {
			r.Swept = rep
		}
	}
	if pids := devsup.KillInstance(l.ID); len(pids) > 0 {
		r.LeftoverKilled = pids
	}
	if pids := appctl.KillUnder(l.StateDir, 5*time.Second); len(pids) > 0 {
		r.LeftoverKilled = append(r.LeftoverKilled, pids...)
	}
	time.Sleep(100 * time.Millisecond)
	if left := devsup.InstanceProcesses(l.ID); len(left) > 0 {
		r.Remaining = left
	}
	if err := os.RemoveAll(l.RuntimeDir); err == nil {
		r.RemovedRuntime = true
		os.Remove(filepath.Dir(l.RuntimeDir))
	}
	if !keepState {
		if rec.Worktree != "" {
			r.MarkerRemoved = devinstance.UnlinkMarker(rec.Worktree, l.StateDir)
		}
		if err := assets.RemoveTree(l.StateDir); err == nil {
			r.RemovedState = true
		}
	}
	r.Seconds = time.Since(started).Seconds()
	return r
}

func networkChildren(l devinstance.Layout) []devinstance.Record {
	state, rt := filepath.Join(l.StateDir, appctl.AppsDir), filepath.Join(l.RuntimeDir, appctl.AppsDir)
	entries, err := os.ReadDir(devinstance.DevRoot(state))
	if err != nil {
		return nil
	}
	var out []devinstance.Record
	for _, e := range entries {
		cl, err := devinstance.NewLayout(e.Name(), state, rt)
		if err != nil {
			continue
		}
		var rec devinstance.Record
		if devinstance.ReadJSON(cl.Record, &rec) == nil && rec.VM != nil {
			out = append(out, rec)
		}
	}
	return out
}

func downChild(ctx context.Context, a *app, l devinstance.Layout, p devinstance.Parent) []int {
	if err := devinstance.DeleteChildWorkspace(ctx, p.Kubeconfig, l.ID); err != nil {
		a.log.Warn("deleting the child workspace in the parent", "parent", p.ID, "err", err.Error())
	} else {
		a.log.Info("deleted the child workspace in the parent; its supervisor stops the units", "parent", p.ID, "workspace", p.Workspace)
	}
	m, err := hostsvc.Open(ctx, hostsvc.Config{Backend: hostsvc.BackendProc, ProcSocket: p.Socket})
	if err != nil {
		a.log.Info("the parent devsup is unreachable; nothing of this child runs there", "parent", p.ID, "err", err.Error())
		return []int{}
	}
	defer m.Close()
	deadline := time.Now().Add(devsupExitTimeout)
	for {
		units, err := devinstance.ChildUnits(ctx, m, l.ID)
		var live []int
		for _, u := range units {
			if u.State == hostsvc.Active || u.State == hostsvc.Activating || u.State == hostsvc.Deactivating {
				live = append(live, u.MainPID)
			}
		}
		if err == nil && len(live) == 0 {
			return []int{}
		}
		if time.Now().After(deadline) {
			return live
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (a *app) instanceManager(ctx context.Context, rec devinstance.Record) (hostsvc.Manager, error) {
	if rec.Parent != nil {
		return devinstance.OpenRemote(ctx, *rec.Parent, a.log)
	}
	return hostsvc.Open(ctx, procConfig(rec.Layout))
}

func runDevsup(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if err := devinstance.RefuseRoot(os.Geteuid()); err != nil {
		return a.fail(exitRefused, "internal devsup: %v", err)
	}
	for _, n := range []string{"state", "socket"} {
		if err := a.spec.Require(vals, n); err != nil {
			return a.fail(exitRefused, "internal devsup: %v", err)
		}
	}
	err := devsup.Run(ctx, devsup.Options{State: vals.String("state"), Socket: vals.String("socket"),
		DieWith: vals.Int("die-with"), Instance: vals.String("instance"), Log: a.log})
	if err != nil {
		return a.fail(exitFailed, "internal devsup: %v", err)
	}
	return exitOK
}

func runDevCore(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if err := a.spec.Require(vals, "instance-dir"); err != nil {
		return a.fail(exitRefused, "internal dev-core: %v", err)
	}
	var rec devinstance.Record
	if err := devinstance.ReadJSON(filepath.Join(vals.String("instance-dir"), "instance.json"), &rec); err != nil {
		return a.fail(exitFailed, "internal dev-core: %v", err)
	}
	timeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "internal dev-core: ready-timeout: %v", err)
	}
	os.Setenv(denoruntime.WatchListEnv, "false")
	var deno denoruntime.Config
	if rec.SharedKcp == "" {
		if deno, err = a.coreDenoRuntime(vals, rec, timeout); err != nil {
			return a.fail(exitFailed, "internal dev-core: %v", err)
		}
	}
	if rec.Profile == bootstrap.ProfileMarketNetwork && rec.OpenBao == nil {
		deno.OpenBaoFrom = appctl.NetworkOpenBao(rec.AdminKubeconfig, rec.BaseWorkspace)
	}
	m, err := a.instanceManager(ctx, rec)
	if err != nil {
		return a.fail(exitFailed, "internal dev-core: %v", err)
	}
	defer m.Close()
	if rec.Gateway != nil {
		if err := a.runGatewayChild(ctx, rec, deno, m, timeout); err != nil && !errors.Is(err, context.Canceled) {
			return a.fail(exitFailed, "internal dev-core: the gateway child %s: %v", rec.Layout.ID, err)
		}
		return exitOK
	}
	cfg := devcore.Config{Layout: rec.Layout, ReadyTimeout: timeout, Log: a.log, DenoRuntime: &deno, SignInGuard: bidder.CheckSignIn}
	if rec.VM != nil {
		rt, vm, err := a.instanceVMRuntime(ctx, rec)
		if err != nil {
			return a.fail(exitFailed, "internal dev-core: the instance is a bidder application instance (handle %s) and its VM runtime does not open: %v", rec.VM.Handle, err)
		}
		defer vm.Close()
		cfg.ComputeVM = &computevm.Config{Handle: rec.VM.Handle, Runtime: rt, Manager: vm, Log: a.log}
	}
	if err := devcore.Run(ctx, cfg, m); err != nil {
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		return a.fail(exitFailed, "internal dev-core: %v", err)
	}
	return exitOK
}
