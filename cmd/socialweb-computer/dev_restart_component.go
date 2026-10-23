package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/componentcore"
)

type componentTarget struct {
	App        string
	Instance   string
	Kubeconfig string
	Base       string
	Workspaces []string
	Kind       string
	Name       string
}

type componentRestartReport struct {
	Instance  string                      `json:"instance"`
	App       string                      `json:"app,omitempty"`
	Workspace string                      `json:"workspace"`
	Kind      string                      `json:"kind"`
	Name      string                      `json:"name"`
	Request   string                      `json:"request"`
	Before    componentcore.RestartStatus `json:"before"`
	After     componentcore.RestartStatus `json:"after"`
	Seconds   float64                     `json:"seconds"`
}

func canonicalComponentKind(name string) (string, bool) {
	for _, kind := range appctl.ComponentTypes {
		if strings.EqualFold(kind.Kind, name) {
			return kind.Kind, true
		}
	}
	return "", false
}

func workspaceName(base, workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return base
	}
	if workspace != "root" && !strings.HasPrefix(workspace, "root:") {
		workspace = "root:" + workspace
	}
	return manifests.Rebase(base, workspace)
}

func (a *app) resolveComponentTarget(ctx context.Context, l devinstance.Layout, rec devinstance.Record, target string) (componentTarget, error) {
	t := componentTarget{Instance: l.ID, Kubeconfig: rec.AdminKubeconfig, Base: baseWorkspace(rec)}
	rest := target
	if head, tail, ok := strings.Cut(rest, "/"); ok {
		if kind, ok := canonicalComponentKind(head); ok && !strings.Contains(tail, "/") {
			t.Kind, rest = kind, tail
		} else if !strings.Contains(tail, "/") {
			t.App, rest = head, tail
		} else {
			return t, fmt.Errorf("%q is not [<app>/][<workspace>:][<kind>/]<component>", target)
		}
	}
	if strings.Contains(rest, "/") {
		return t, fmt.Errorf("%q is not [<app>/][<workspace>:][<kind>/]<component>", target)
	}
	workspace := ""
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		workspace, rest = rest[:i], rest[i+1:]
	}
	t.Name = rest
	if t.Name == "" {
		return t, fmt.Errorf("%q names no component", target)
	}
	switch {
	case t.App != "":
		if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
			return t, fmt.Errorf("instance %s is not a market network; name the component as [<workspace>:][<kind>/]<component>", l.ID)
		}
		n, err := appctl.ReadNetwork(ctx, rec.AdminKubeconfig)
		if err != nil {
			return t, fmt.Errorf("reading the MarketNetwork of %s: %w", l.ID, err)
		}
		var found bool
		for _, ap := range n.Status.Apps {
			if ap.Name != t.App {
				continue
			}
			found = true
			if ap.Instance == "" {
				return t, fmt.Errorf("network child %s has no instance yet (phase %s)", t.App, ap.Phase)
			}
			cl, err := devinstance.NewLayout(ap.Instance, filepath.Join(l.StateDir, appctl.AppsDir), filepath.Join(l.RuntimeDir, appctl.AppsDir))
			if err != nil {
				return t, err
			}
			var crec devinstance.Record
			if err := fsx.ReadJSON(cl.Record, &crec); err != nil {
				return t, fmt.Errorf("network child %s (instance %s) has no record: %w", t.App, ap.Instance, err)
			}
			t.Instance, t.Kubeconfig, t.Base = ap.Instance, crec.AdminKubeconfig, baseWorkspace(crec)
		}
		if !found {
			var names []string
			for _, ap := range n.Status.Apps {
				names = append(names, ap.Name)
			}
			return t, fmt.Errorf("market network %s has no child application %s (it has: %s)", l.ID, t.App, strings.Join(names, ", "))
		}
	case rec.Profile == bootstrap.ProfileMarketNetwork && rec.AdminKubeconfig != "":
		n, err := appctl.ReadNetwork(ctx, rec.AdminKubeconfig)
		if err == nil {
			seen := map[string]bool{}
			add := func(workspace string) {
				if workspace == "" || seen[workspace] {
					return
				}
				seen[workspace] = true
				t.Workspaces = append(t.Workspaces, workspace)
			}
			for _, workspace := range componentcore.AppWorkspaces(baseWorkspace(rec)) {
				add(workspace)
			}
			for _, c := range n.Status.Components {
				add(c.Workspace)
			}
		}
	}
	if workspace != "" {
		t.Workspaces = []string{workspaceName(t.Base, workspace)}
	}
	if len(t.Workspaces) == 0 {
		t.Workspaces = componentcore.AppWorkspaces(t.Base)
	}
	if t.Kind == appctl.ComponentKindPolicyEngine {
		return t, fmt.Errorf("the PolicyEngine is the component controller; dev restart core restarts it")
	}
	if t.Kubeconfig == "" {
		return t, fmt.Errorf("instance %s records no admin kubeconfig; dev up first", t.Instance)
	}
	return t, nil
}

func runDevRestartComponent(ctx context.Context, a *app, vals clicfg.Values, target string) int {
	started := time.Now()
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "dev restart: ready-timeout: %v", err)
	}
	l, rec, code := a.record("dev restart", vals)
	if code != exitOK {
		return code
	}
	t, err := a.resolveComponentTarget(ctx, l, rec, target)
	if err != nil {
		return a.fail(exitRefused, "dev restart: %v", err)
	}
	factory, err := componentcore.NewDynamicFactory(t.Kubeconfig)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	refs, err := componentcore.FindComponents(ctx, factory, t.Workspaces, t.Kind, t.Name)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	switch len(refs) {
	case 0:
		available := componentcore.DescribeComponents(ctx, factory, t.Workspaces)
		if len(available) == 0 {
			return a.fail(exitRefused, "dev restart: no component %s in %s of instance %s", t.Name, strings.Join(t.Workspaces, ", "), t.Instance)
		}
		return a.fail(exitRefused, "dev restart: no component %s in %s of instance %s (it has: %s)", t.Name,
			strings.Join(t.Workspaces, ", "), t.Instance, strings.Join(available, ", "))
	case 1:
	default:
		var names []string
		for _, ref := range refs {
			names = append(names, ref.Workspace+":"+ref.Kind+"/"+ref.Name)
		}
		return a.fail(exitRefused, "dev restart: %s names %d components (%s); name it as <workspace>:<kind>/<component>",
			t.Name, len(refs), strings.Join(names, ", "))
	}
	ref := refs[0]
	before, err := componentcore.ReadStatus(ctx, factory, ref)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	stamp, err := componentcore.Restart(ctx, factory, ref)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	after, err := componentcore.WaitRestarted(ctx, factory, ref, stamp, before, readyTimeout)
	r := componentRestartReport{Instance: t.Instance, App: t.App, Workspace: ref.Workspace, Kind: ref.Kind, Name: ref.Name,
		Request: stamp, Before: before, After: after, Seconds: time.Since(started).Seconds()}
	a.printJSON(r)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	return exitOK
}
