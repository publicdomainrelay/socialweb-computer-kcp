package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/progress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

var vmOptions = append([]string{backendOpt, "instance", "state-dir", "asset-cache", "handle", "vm-runtime-dir", "guest-image-source", "vm-stop-grace"},
	devOnly("userns-stage-path", "pasta-path", "vmexec-path", "firecracker-path")...)

func (a *app) vmExe(vals clicfg.Values, executable, option string) (string, error) {
	if !assets.Release() {
		p := vals.String(option)
		if p == "" || !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path in this dev build (never PATH), got %q", option, p)
		}
		return p, nil
	}
	cache, err := a.assetCache(vals)
	if err != nil {
		return "", err
	}
	e, err := assets.Resolve(assets.Options{Cache: cache, Log: a.log}, executable)
	if err != nil {
		return "", err
	}
	return e.Path, nil
}

func (a *app) vmRuntime(ctx context.Context, vals clicfg.Values) (vmruntime.Runtime, hostsvc.Manager, error) {
	state := vals.String("state-dir")
	if state == "" || !filepath.IsAbs(state) {
		return nil, nil, fmt.Errorf("state-dir must be an absolute path: the VM work dirs, images and the slot lock live under it")
	}
	if vals.String("handle") == "" {
		return nil, nil, fmt.Errorf("handle is required: VM units are named by the handle of the bidder instance that owns them")
	}
	var exe vmruntime.Executables
	for _, x := range []struct {
		dst          *string
		name, option string
	}{
		{&exe.Stage, "swc-userns-stage", "userns-stage-path"},
		{&exe.Pasta, "pasta", "pasta-path"},
		{&exe.VMExec, "socialweb-vmexec", "vmexec-path"},
		{&exe.Firecracker, "firecracker", "firecracker-path"},
	} {
		p, err := a.vmExe(vals, x.name, x.option)
		if err != nil {
			return nil, nil, err
		}
		*x.dst = p
	}
	p, err := a.placement(ctx, vals)
	if err != nil {
		return nil, nil, err
	}
	grace, err := vmStopGrace(vals)
	if err != nil {
		p.manager.Close()
		return nil, nil, err
	}
	rt, err := vmruntime.ForHost(vmruntime.Config{
		Manager: p.manager, StateDir: state, RuntimeDir: vals.String("vm-runtime-dir"), Exec: exe,
		ImageSource: vals.String("guest-image-source"), StopGrace: grace,
		ImageOut: a.stderr, ImageTTY: progress.IsTTYWriter(a.stderr),
	})
	if err != nil {
		p.manager.Close()
		return nil, nil, err
	}
	return rt, p.manager, nil
}

var vmExecutables = []struct{ name, option string }{
	{"swc-userns-stage", "userns-stage-path"},
	{"pasta", "pasta-path"},
	{"socialweb-vmexec", "vmexec-path"},
	{"firecracker", "firecracker-path"},
}

var devVMOptions = append([]string{"handle", "vm-host-backend", "vm-runtime-dir", "guest-image-source", "vm-stop-grace"},
	devOnly("userns-stage-path", "pasta-path", "vmexec-path", "firecracker-path")...)

func (a *app) devVMExecutables(vals clicfg.Values) (map[string]devinstance.Executable, error) {
	out := map[string]devinstance.Executable{}
	for _, x := range vmExecutables {
		if !assets.Release() && vals.String(x.option) == "" {
			continue
		}
		p, err := a.vmExe(vals, x.name, x.option)
		if err != nil {
			if assets.Release() {
				if a.log != nil {
					a.log.Warn("the dev VM executable does not resolve; the network will record it missing", "executable", x.name, "err", err.Error())
				}
				continue
			}
			return nil, err
		}
		out[x.name] = devinstance.Executable{Path: p}
	}
	return out, nil
}

func (a *app) devVM(vals clicfg.Values, l devinstance.Layout) (*devinstance.VMRuntime, error) {
	handle := vals.String("handle")
	if handle == "" {
		return nil, nil
	}
	backend := hostsvc.BackendProc
	if b := vals.String("vm-host-backend"); b != "" {
		var err error
		if backend, err = hostsvc.ParseBackend(b); err != nil {
			return nil, err
		}
	}
	rt := vals.String("vm-runtime-dir")
	rt = cmp.Or(rt, l.RuntimeDir)
	if !filepath.IsAbs(rt) {
		return nil, fmt.Errorf("vm-runtime-dir %q must be absolute: each VM's API socket lives under it", rt)
	}
	grace, err := vmStopGrace(vals)
	if err != nil {
		return nil, err
	}
	vm := &devinstance.VMRuntime{Handle: handle, Backend: backend, ImageSource: vals.String("guest-image-source"),
		StopGrace: grace.String(), RuntimeDir: rt, Executables: map[string]string{}}
	for _, x := range vmExecutables {
		p, err := a.vmExe(vals, x.name, x.option)
		if err != nil {
			return nil, err
		}
		vm.Executables[x.name] = p
	}
	return vm, nil
}

func vmStopGrace(vals clicfg.Values) (time.Duration, error) {
	if s := vals.String("vm-stop-grace"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("vm-stop-grace: %w", err)
		}
		return d, nil
	}
	return versions.VMStopGrace()
}

func (a *app) instanceVMRuntime(ctx context.Context, rec devinstance.Record) (vmruntime.Runtime, hostsvc.Manager, error) {
	v := rec.VM
	cfg := hostsvc.Config{Backend: v.Backend}
	if v.Backend == hostsvc.BackendProc {
		cfg.ProcSocket = rec.Layout.Socket
		if rec.Parent != nil {
			cfg.ProcSocket = rec.Parent.Socket
		}
	}
	m, err := hostsvc.Open(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("the VM placement backend %s: %w", v.Backend, err)
	}
	grace, err := time.ParseDuration(v.StopGrace)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	rt, err := vmruntime.ForHost(vmruntime.Config{Manager: m, StateDir: rec.Layout.StateDir, RuntimeDir: v.RuntimeDir,
		Exec: vmruntime.Executables{Stage: v.Executables["swc-userns-stage"], Pasta: v.Executables["pasta"],
			VMExec: v.Executables["socialweb-vmexec"], Firecracker: v.Executables["firecracker"]},
		ImageSource: v.ImageSource, StopGrace: grace, ImageOut: a.stderr, ImageTTY: progress.IsTTYWriter(a.stderr)})
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return rt, m, nil
}

func (a *app) downVMs(ctx context.Context, rec devinstance.Record) ([]string, []string) {
	if rec.VM == nil {
		return nil, nil
	}
	rt, m, err := a.instanceVMRuntime(ctx, rec)
	if err != nil {
		a.log.Warn("the VM runtime of the instance; its VMs are not stopped here", "err", err.Error())
		return nil, []string{err.Error()}
	}
	defer m.Close()
	listed, err := rt.List(ctx, rec.VM.Handle)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var stopped, left []string
	for _, l := range listed {
		ref, err := rt.Load(rec.VM.Handle, l.Name)
		if err != nil {
			ref = vmruntime.RuntimeRef{Handle: rec.VM.Handle, Name: l.Name, Unit: l.Unit}
		}
		res, err := rt.Stop(ctx, ref)
		if err != nil {
			left = append(left, l.Unit+": "+err.Error())
			continue
		}
		stopped = append(stopped, l.Unit)
		a.log.Info("VM stopped with its instance", "unit", l.Unit, "graceful", res.Graceful, "result", res.Result)
	}
	if err := m.StopSlice(ctx, vmruntime.SliceName(rec.VM.Handle)); err != nil {
		left = append(left, err.Error())
	}
	return stopped, left
}

func runVM(verb string) verbFunc {
	return func(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
		rt, m, err := a.vmRuntime(ctx, vals)
		if err != nil {
			return a.fail(exitRefused, "vm %s: %v", verb, err)
		}
		defer m.Close()
		handle := vals.String("handle")
		name := ""
		if verb != "start" && verb != "list" {
			if len(rest) != 1 {
				return a.fail(exitRefused, "vm %s: name exactly one VM", verb)
			}
			name = rest[0]
		}
		var out any
		switch verb {
		case "start":
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return a.fail(exitFailed, "vm start: reading the spec from stdin: %v", err)
			}
			var spec vmruntime.VMSpec
			if err := json.Unmarshal(raw, &spec); err != nil {
				return a.fail(exitRefused, "vm start: the spec on stdin: %v", err)
			}
			spec.Handle = handle
			p, err := rt.Prepare(ctx, spec)
			if err != nil {
				return a.fail(exitFailed, "vm start: %v", err)
			}
			ref, err := rt.Start(ctx, p)
			if err != nil {
				return a.fail(exitFailed, "vm start: %v", err)
			}
			out = ref
		case "inspect":
			ref, err := rt.Load(handle, name)
			if os.IsNotExist(err) {
				out = vmruntime.Observation{Verdict: hostsvc.VerdictLost.String(), Reason: hostsvc.LostUnitGone, Detail: "no runtime record for " + name}
				break
			}
			if err != nil {
				return a.fail(exitFailed, "vm inspect: %v", err)
			}
			obs, err := rt.Inspect(ctx, ref)
			if err != nil {
				return a.fail(exitFailed, "vm inspect: %v", err)
			}
			out = obs
		case "stop":
			ref, err := rt.Load(handle, name)
			if os.IsNotExist(err) {
				ref = vmruntime.RuntimeRef{Handle: handle, Name: name, Unit: vmruntime.UnitName(handle, name)}
			} else if err != nil {
				return a.fail(exitFailed, "vm stop: %v", err)
			}
			res, err := rt.Stop(ctx, ref)
			if err != nil {
				return a.fail(exitFailed, "vm stop: %v", err)
			}
			out = res
		case "list":
			l, err := rt.List(ctx, handle)
			if err != nil {
				return a.fail(exitFailed, "vm list: %v", err)
			}
			out = l
		}
		a.printJSON(out)
		return exitOK
	}
}

func devOnly(names ...string) []string {
	if clicfg.ReleaseBuild() {
		return nil
	}
	return names
}
