package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devsup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

func (a *app) assetCache(vals clicfg.Values) (string, error) {
	if c := vals.String("asset-cache"); c != "" {
		return filepath.Abs(c)
	}
	return assets.DevCache(a.env)
}

func (a *app) devExe(ctx context.Context, vals clicfg.Values, name, option string) (devinstance.Executable, error) {
	if !assets.Release() {
		return checkExe(ctx, option, vals.String(option))
	}
	if vals.String(option) != "" {
		return devinstance.Executable{}, fmt.Errorf("%s is a dev-build option; this release build runs only its embedded %s", option, name)
	}
	cache, err := a.assetCache(vals)
	if err != nil {
		return devinstance.Executable{}, err
	}
	e, err := assets.Resolve(assets.Options{Cache: cache, Log: a.log}, name)
	if err != nil {
		return devinstance.Executable{}, err
	}
	return checkExe(ctx, option, e.Path)
}

func currentSet(platform string) []string {
	pins, err := assets.PlatformPins(platform)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		if p.TreeSHA256 != "" {
			out = append(out, p.TreeSHA256)
		}
	}
	return out
}

func devsupRecordLister(stateRoot string) assets.UnitLister {
	return assets.UnitListerFunc(func(ctx context.Context) ([]assets.UnitRef, error) {
		paths, err := filepath.Glob(filepath.Join(stateRoot, "instances", "*", "sup", "units", "*.json"))
		if err != nil {
			return nil, err
		}
		var out []assets.UnitRef
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			var rec devsup.Record
			if err := json.Unmarshal(b, &rec); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			switch rec.Status.State {
			case hostsvc.Inactive, hostsvc.Failed, hostsvc.NotFound:
				continue
			}
			out = append(out, assets.UnitRef{Backend: string(hostsvc.BackendProc), Name: rec.Spec.Name, Exec: rec.Spec.Exec, Env: rec.Spec.Env})
		}
		return out, nil
	})
}

func runDevGC(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	window, err := time.ParseDuration(vals.String("asset-gc-window"))
	if err != nil {
		return a.fail(exitRefused, "dev gc: asset-gc-window: %v", err)
	}
	root, err := a.assetCache(vals)
	if err != nil {
		return a.fail(exitRefused, "dev gc: %v", err)
	}
	stateRoot, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return a.fail(exitRefused, "dev gc: %v", err)
	}
	running := assets.ProcessesOfUID(os.Getuid(), assets.IsDevsup)
	rep, err := assets.DevCollect(ctx, root, running, []assets.UnitLister{devsupRecordLister(stateRoot)},
		assets.GCPolicy{Keep: currentSet(assets.Platform()), Window: window})
	if err != nil {
		return a.fail(exitRefused, "dev gc: %v", err)
	}
	a.printJSON(rep)
	return exitOK
}
