package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fswatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/nsexec"
)

func runNsExec(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	binds, err := nsexec.ParseBinds(vals.String("bind"))
	if err != nil {
		return a.fail(exitRefused, "internal ns-exec: %v", err)
	}
	c := nsexec.Config{Hostname: vals.String("private-hostname"), Binds: binds, Argv: rest}
	if vals.Bool("inside") {
		err = nsexec.Inside(c)
	} else {
		var exe string
		if exe, err = os.Executable(); err == nil {
			err = nsexec.Enter(c, []string{exe, "internal", "ns-exec"})
		}
	}
	return a.fail(exitFailed, "internal ns-exec: %v", err)
}

func rollID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

func withdrawRollRequest(path, id string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cur clusterprovider.RollRequest
	if json.Unmarshal(b, &cur) == nil && cur.ID == id {
		os.Remove(path)
	}
}

func rollStalled(state, key string, req clusterprovider.RollRequest) string {
	bound := clusterprovider.DefaultRollObserveBound
	now := time.Now().UTC()
	if o, ok, err := clusterprovider.ReadRollObserved(state, key, req.ID); err == nil && ok {
		if o.State == clusterprovider.RollStateWaiting && o.Error != "" && now.Sub(o.FirstAt) > bound {
			return fmt.Sprintf("%s took roll %s at %s but cannot read the cluster's readiness for %s: %s",
				o.Provider, req.ID, o.FirstAt.Format(time.RFC3339), now.Sub(o.FirstAt).Round(time.Second), o.Error)
		}
		return ""
	}
	if now.Sub(req.RequestedAt) <= bound {
		return ""
	}
	s, ok, err := clusterprovider.ReadRollerStatus(state)
	switch {
	case err != nil:
		return fmt.Sprintf("no cluster provider took roll %s within %s, and its status %s does not read: %v", req.ID, bound, clusterprovider.RollerStatusPath(state), err)
	case !ok:
		return fmt.Sprintf("no cluster provider took roll %s within %s; none has written %s, so none runs a roller on %s", req.ID, bound, clusterprovider.RollerStatusPath(state), state)
	case now.Sub(s.PassAt) > bound:
		return fmt.Sprintf("%s did not take roll %s within %s; its last pass was at %s (last error %q at %s)", s.Provider, req.ID, bound,
			s.PassAt.Format(time.RFC3339), s.LastError, s.LastErrorAt.Format(time.RFC3339))
	case s.Error != "":
		return fmt.Sprintf("%s did not take roll %s within %s; its last pass at %s failed: %s", s.Provider, req.ID, bound, s.PassAt.Format(time.RFC3339), s.Error)
	case !slices.Contains(s.Clusters, key):
		return fmt.Sprintf("%s did not take roll %s within %s; its last pass at %s lists %d clusters and not %s", s.Provider, req.ID, bound, s.PassAt.Format(time.RFC3339), len(s.Clusters), key)
	}
	return fmt.Sprintf("%s did not take roll %s within %s; its last pass at %s reported no error (last error %q at %s)", s.Provider, req.ID, bound,
		s.PassAt.Format(time.RFC3339), s.LastError, s.LastErrorAt.Format(time.RFC3339))
}

func runUnitsRoll(kind string) verbFunc {
	verb := "units roll " + kind
	return func(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
		if len(rest) != 1 {
			return a.fail(exitRefused, "%s: name exactly one cluster key (the <account>-<cluster> the provider knows it by), got %v", verb, rest)
		}
		key := rest[0]
		state := vals.String("cluster-state-dir")
		if state == "" || !filepath.IsAbs(state) {
			return a.fail(exitRefused, "%s: --cluster-state-dir must name the cluster provider's absolute state dir", verb)
		}
		wait, err := time.ParseDuration(vals.String("ready-timeout"))
		if err != nil {
			return a.fail(exitRefused, "%s: ready-timeout: %v", verb, err)
		}
		req := clusterprovider.RollRequest{ID: rollID(), Kind: kind, RequestedAt: time.Now().UTC()}
		switch kind {
		case clusterprovider.RollKindCluster:
			req.K3s, req.Kine, req.IdleBound = vals.String("k3s-path"), vals.String("kine-path"), vals.String("idle-bound")
			if _, err := time.ParseDuration(req.IdleBound); err != nil {
				return a.fail(exitRefused, "%s: idle-bound: %v", verb, err)
			}
		case clusterprovider.RollKindLink:
			req.Service, req.Dumbpipe = vals.String("service"), vals.String("dumbpipe-path")
			if req.Service == "" {
				return a.fail(exitRefused, "%s: --service names the link to replace (apiserver or openbao)", verb)
			}
		}
		for _, p := range []string{req.K3s, req.Kine, req.Dumbpipe} {
			if p != "" && !filepath.IsAbs(p) {
				return a.fail(exitRefused, "%s: %s must be an absolute path", verb, p)
			}
		}
		path, err := clusterprovider.WriteRollRequest(state, key, req)
		if err != nil {
			return a.fail(exitFailed, "%s: %v", verb, err)
		}
		a.log.Info("roll requested; the cluster provider carries it out", "cluster", key, "roll", req.ID, "request", path)
		rolls, err := rollWatch(path, state, key, req.ID)
		if err != nil {
			return a.fail(exitFailed, "%s: %v", verb, err)
		}
		defer rolls.Close()
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		stall := time.NewTimer(rollStallCheck(state, key, req, wait))
		defer stall.Stop()
		for {
			rep, ok, err := clusterprovider.ReadRollReport(state, key, req.ID)
			if err != nil {
				return a.fail(exitFailed, "%s: %v", verb, err)
			}
			if ok {
				a.printJSON(rep)
				if rep.Error != "" {
					return exitFailed
				}
				return exitOK
			}
			if why := rollStalled(state, key, req); why != "" {
				withdrawRollRequest(path, req.ID)
				return a.fail(exitFailed, "%s: %s; the request is withdrawn", verb, why)
			}
			select {
			case <-ctx.Done():
				return a.fail(exitFailed, "%s: interrupted; the request at %s stays for the provider", verb, path)
			case <-deadline.C:
				return a.fail(exitFailed, "%s: no report for %s after %s; the request at %s stays for the provider", verb, req.ID, wait, path)
			case <-stall.C:
				stall.Reset(rollStallCheck(state, key, req, wait))
			case ev, open := <-rolls.Events():
				if !open {
					return a.fail(exitFailed, "%s: the watch of %s closed", verb, path)
				}
				if ev.Has(fsnotify.Create) && filepath.Base(ev.Name) == clusterprovider.RollsDirName {
					_ = rolls.Add(ev.Name)
				}
			case <-rolls.Errors():
			}
		}
	}
}

func rollWatch(request, state, key, id string) (*fswatch.Sub, error) {
	w, err := fswatch.Shared().Subscribe(filepath.Dir(request))
	if err != nil {
		return nil, err
	}
	if report, err := clusterprovider.RollReportPath(state, key, id); err == nil {
		_ = w.Add(filepath.Dir(report))
	}
	return w, nil
}

func rollStallCheck(state, key string, req clusterprovider.RollRequest, wait time.Duration) time.Duration {
	bound := clusterprovider.DefaultRollObserveBound
	since := req.RequestedAt
	if o, ok, err := clusterprovider.ReadRollObserved(state, key, req.ID); err == nil && ok {
		since = o.FirstAt
	}
	if next := time.Until(since.Add(bound)); next > 0 {
		return next + time.Second
	}
	return wait
}
