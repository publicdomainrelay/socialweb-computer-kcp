package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devcheck"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const helloTimeout = 2 * time.Second

func (a *app) record(verb string, vals clicfg.Values) (devinstance.Layout, devinstance.Record, int) {
	l, err := a.layout(vals)
	if err != nil {
		return l, devinstance.Record{}, a.fail(exitRefused, "%s: %v", verb, err)
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return l, rec, a.fail(exitFailed, "%s: instance %s has no record at %s; dev up first", verb, l.ID, l.Record)
	}
	return l, rec, exitOK
}

func runDevEnv(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	_, rec, code := a.record("dev env", vals)
	if code != exitOK {
		return code
	}
	ex := devinstance.Exports(rec)
	if vals.Bool("json") {
		a.printJSON(exportMap(ex))
		return exitOK
	}
	fmt.Fprint(a.stdout, devinstance.ShellExports(ex))
	return exitOK
}

type lsEntry struct {
	Instance  string    `json:"instance"`
	Worktree  string    `json:"worktree,omitempty"`
	Profile   string    `json:"profile"`
	Ready     bool      `json:"ready"`
	StateDir  string    `json:"stateDir"`
	KcpURL    string    `json:"kcpURL"`
	Units     int       `json:"units"`
	Created   time.Time `json:"created"`
	Reachable bool      `json:"devsupReachable"`
	DevsupPID int       `json:"devsupPID,omitempty"`
}

func runDevLs(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return a.fail(exitRefused, "dev ls: %v", err)
	}
	recs, err := devinstance.List(root)
	if err != nil {
		return a.fail(exitFailed, "dev ls: %v", err)
	}
	out := []lsEntry{}
	for _, rec := range recs {
		e := lsEntry{Instance: rec.Layout.ID, Worktree: rec.Worktree, Profile: rec.Profile, Ready: rec.Ready,
			StateDir: rec.Layout.StateDir, KcpURL: rec.KcpURL, Units: len(rec.Units), Created: rec.Created}
		hctx, cancel := context.WithTimeout(ctx, helloTimeout)
		if c, h, err := helloDevsup(hctx, rec.Layout); err == nil {
			e.Reachable, e.DevsupPID = true, h.PID
			c.Close()
		}
		cancel()
		out = append(out, e)
	}
	a.printJSON(out)
	return exitOK
}

func lastLines(b []byte, n int) []byte {
	if n <= 0 {
		return b
	}
	s := strings.TrimSuffix(string(b), "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func runDevLogs(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	l, err := a.layout(vals)
	if err != nil {
		return a.fail(exitRefused, "dev logs: %v", err)
	}
	if len(rest) != 1 {
		return a.fail(exitRefused, "dev logs: name one unit: %s (or its unit name)", strings.Join(devinstance.LogNames(l), ", "))
	}
	path, err := devinstance.LogFile(l, rest[0])
	if err != nil {
		return a.fail(exitRefused, "dev logs: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return a.fail(exitFailed, "dev logs: %v", err)
	}
	a.stdout.Write(lastLines(b, vals.Int("tail")))
	return exitOK
}

type checkReport struct {
	OK      bool              `json:"ok"`
	At      time.Time         `json:"at"`
	Results []devcheck.Result `json:"results"`
}

func runDevCheck(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	name := strconv.Itoa(os.Geteuid())
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	rs := devcheck.Run(devcheck.Host{Root: "/", Euid: os.Geteuid(), User: name, Env: a.env, LookPath: exec.LookPath})
	a.printJSON(checkReport{OK: !devcheck.Failed(rs), At: time.Now().UTC(), Results: rs})
	if devcheck.Failed(rs) {
		return exitFailed
	}
	return exitOK
}

type coreRestartReport struct {
	Instance       string                 `json:"instance"`
	Parent         string                 `json:"parent,omitempty"`
	Binary         devinstance.Executable `json:"binary"`
	Before         devinstance.Core       `json:"before"`
	After          devinstance.Core       `json:"after"`
	UnitsUnchanged bool                   `json:"unitsUnchanged"`
	Seconds        float64                `json:"seconds"`
}

func coreTarget(target string) (string, bool) {
	if target == "core" {
		return "", true
	}
	app, ok := strings.CutSuffix(target, "/core")
	if !ok || app == "" || strings.ContainsAny(app, "/:") {
		return "", false
	}
	if _, isKind := canonicalComponentKind(app); isKind {
		return "", false
	}
	return app, true
}

func (a *app) coreRecord(ctx context.Context, vals clicfg.Values, childApp string) (devinstance.Layout, devinstance.Record, int) {
	l, err := a.layout(vals)
	if err != nil {
		return l, devinstance.Record{}, a.fail(exitRefused, "dev restart: %v", err)
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		root, rerr := devinstance.StateRoot(vals.String("state-dir"), a.env)
		if rerr != nil {
			return l, rec, a.fail(exitRefused, "dev restart: %v", rerr)
		}
		cl, crec, ok := findNetworkChild(root, l.ID)
		if !ok {
			return l, rec, a.fail(exitFailed, "dev restart: instance %s has no record at %s and is no child application of a market network under %s; dev up first", l.ID, l.Record, root)
		}
		l, rec = cl, crec
	}
	if childApp != "" {
		cl, crec, err := networkChild(ctx, l, rec, childApp)
		if err != nil {
			return l, rec, a.fail(exitRefused, "dev restart: %v", err)
		}
		l, rec = cl, crec
	}
	if rec.Parent != nil {
		l = rec.Layout
	}
	return l, rec, exitOK
}

func runDevRestart(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	started := time.Now()
	if len(rest) == 2 {
		vals = vals.With("instance", rest[0])
		rest = rest[1:]
	}
	if len(rest) != 1 {
		return a.fail(exitRefused, "dev restart: dev restart [<instance>] [<app>/]core [--binary path] | dev restart [<instance>] [<app>/][<workspace>:][<kind>/]<component>; the PolicyEngine is the component controller and restarts with core; kcp and cluster rolls are dev roll (M3-KCP-ROLL)")
	}
	childApp, isCore := coreTarget(rest[0])
	if !isCore {
		return runDevRestartComponent(ctx, a, vals, rest[0])
	}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "dev restart: ready-timeout: %v", err)
	}
	l, rec, code := a.coreRecord(ctx, vals, childApp)
	if code != exitOK {
		return code
	}
	path := vals.String("binary")
	if path == "" {
		if path, err = selfExe(); err != nil {
			return a.fail(exitFailed, "dev restart: %v", err)
		}
	}
	bin, err := hashExe("binary", path)
	if err != nil {
		return a.fail(exitRefused, "dev restart: %v", err)
	}
	r := coreRestartReport{Instance: l.ID, Binary: bin}
	var m hostsvc.Manager
	if rec.Parent != nil {
		r.Parent = rec.Parent.ID
		if m, err = a.instanceManager(ctx, rec); err != nil {
			return a.fail(exitFailed, "dev restart: the supervisor of %s, which places the core of %s, is unreachable (%v)", rec.Parent.ID, l.ID, err)
		}
	} else if m, _, err = helloDevsup(ctx, l); err != nil {
		return a.fail(exitFailed, "dev restart: devsup of %s is unreachable (%v); dev up places a new core", l.ID, err)
	}
	defer m.Close()
	fsx.ReadJSON(l.CoreRecord, &r.Before)
	lock, err := lockFile(ctx, l.Lock, 30*time.Second)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	if err := m.Stop(ctx, l.CoreUnit()); err != nil {
		lock.Close()
		return a.fail(exitFailed, "dev restart: stopping the core: %v", err)
	}
	m.ResetFailed(ctx, l.CoreUnit())
	if rec.Executables == nil {
		rec.Executables = map[string]devinstance.Executable{}
	}
	rec.Executables["socialweb-computer"] = bin
	if err := fsx.WriteJSON(l.Record, rec, 0o600); err != nil {
		lock.Close()
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	lock.Close()
	cspec, err := coreSpec(&rec, bin.Path, readyTimeout)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	st, err := m.Start(ctx, cspec)
	if err != nil {
		return a.fail(exitFailed, "dev restart: placing the core: %v", err)
	}
	if st.Adopted && rec.Parent != nil {
		return a.fail(exitFailed, "dev restart: the supervisor of %s placed the core of %s again (invocation %s) before this restart did; run dev restart again",
			rec.Parent.ID, l.ID, st.InvocationID)
	}
	if r.After, err = waitCore(ctx, l, m, cspec.Name, st.InvocationID, readyTimeout); err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	r.UnitsUnchanged = len(r.After.Adoptions) == len(rec.Units)
	for _, ad := range r.After.Adoptions {
		u := rec.Unit(ad.Role)
		if u == nil || !ad.Adopted || ad.InvocationID != u.Identity.InvocationID {
			r.UnitsUnchanged = false
		}
	}
	r.Seconds = time.Since(started).Seconds()
	a.printJSON(r)
	if !r.UnitsUnchanged {
		return exitFailed
	}
	return exitOK
}
