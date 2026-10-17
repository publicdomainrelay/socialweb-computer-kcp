package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	denov1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/denoruntime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
)

type podTarget struct {
	App        string `json:"app,omitempty"`
	Instance   string `json:"instance"`
	Workspace  string `json:"workspace"`
	DenoPod    string `json:"denopod"`
	kubeconfig string
	stateDir   string
}

type podRun struct {
	RunID string `json:"runID"`
	PID   int    `json:"pid,omitempty"`
	Dir   string `json:"dir,omitempty"`
}

type podRestartReport struct {
	podTarget
	Request    string                         `json:"request"`
	Before     podRun                         `json:"before"`
	After      podRun                         `json:"after"`
	Phase      string                         `json:"phase"`
	Ready      bool                           `json:"ready"`
	Listeners  []denov1alpha1.DenoPodListener `json:"listeners,omitempty"`
	TypeScript tsbundle.Info                  `json:"typescript"`
	Seconds    float64                        `json:"seconds"`
}

func workspacePath(ws string) string {
	if ws == appctl.DefaultWorkspace || strings.HasPrefix(ws, appctl.DefaultWorkspace+":") {
		return ws
	}
	return appctl.DefaultWorkspace + ":" + ws
}

func splitPodName(s string) (string, string) {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return workspacePath(s[:i]), s[i+1:]
	}
	return "", s
}

func podComponents(cs []apiComponent, ws, name string) []string {
	var found []string
	for _, c := range cs {
		if c.Kind == "DenoPod" && c.Name == name && (ws == "" || c.Workspace == ws) && !slices.Contains(found, c.Workspace) {
			found = append(found, c.Workspace)
		}
	}
	return found
}

type apiComponent struct {
	Kind      string
	Name      string
	Workspace string
}

func (a *app) resolvePodTarget(ctx context.Context, l devinstance.Layout, rec devinstance.Record, target string) (podTarget, error) {
	t := podTarget{Instance: l.ID, kubeconfig: rec.AdminKubeconfig, stateDir: l.StateDir}
	app, pod, hasApp := strings.Cut(target, "/")
	if !hasApp {
		app, pod = "", target
	}
	ws, name := splitPodName(pod)
	t.App, t.DenoPod = app, name
	if name == "" || strings.Contains(name, "/") {
		return t, fmt.Errorf("%q is not [<app>/][<workspace>:]<denopod>", target)
	}
	var comps []apiComponent
	base := ""
	if rec.Profile == bootstrap.ProfileMarketNetwork && rec.AdminKubeconfig != "" {
		n, err := appctl.ReadNetwork(ctx, rec.AdminKubeconfig)
		if err != nil {
			return t, fmt.Errorf("reading the MarketNetwork of %s: %w", l.ID, err)
		}
		if app == "" {
			for _, c := range n.Status.Components {
				comps = append(comps, apiComponent{c.Kind, c.Name, c.Workspace})
			}
		} else {
			var names []string
			for _, ap := range n.Status.Apps {
				names = append(names, ap.Name)
				if ap.Name != app {
					continue
				}
				if ap.Instance == "" {
					return t, fmt.Errorf("network child %s has no instance yet (phase %s)", app, ap.Phase)
				}
				cl, err := devinstance.NewLayout(ap.Instance, filepath.Join(l.StateDir, appctl.AppsDir), filepath.Join(l.RuntimeDir, appctl.AppsDir))
				if err != nil {
					return t, err
				}
				var crec devinstance.Record
				if err := devinstance.ReadJSON(cl.Record, &crec); err != nil {
					return t, fmt.Errorf("network child %s (instance %s) has no record: %w", app, ap.Instance, err)
				}
				t.Instance, t.kubeconfig, t.stateDir, base = ap.Instance, crec.AdminKubeconfig, crec.Layout.StateDir, crec.BaseWorkspace
				if crec.SharedKcp != "" {
					t.stateDir = crec.SharedKcp
				}
				for _, c := range ap.Components {
					comps = append(comps, apiComponent{c.Kind, c.Name, c.Workspace})
				}
			}
			if t.Instance == l.ID {
				return t, fmt.Errorf("market network %s has no child application %s (it has: %s)", l.ID, app, strings.Join(names, ", "))
			}
		}
	} else if app != "" {
		return t, fmt.Errorf("instance %s is not a market network; name the DenoPod as [<workspace>:]<denopod>", l.ID)
	}
	if ws != "" {
		t.Workspace = manifests.Rebase(base, ws)
		return t, nil
	}
	found := podComponents(comps, "", name)
	switch len(found) {
	case 1:
		t.Workspace = found[0]
	case 0:
		var all []string
		for _, c := range comps {
			if c.Kind == "DenoPod" {
				all = append(all, c.Workspace+":"+c.Name)
			}
		}
		if len(all) == 0 {
			t.Workspace = appctl.DefaultWorkspace
			return t, nil
		}
		return t, fmt.Errorf("no DenoPod %s among the components (%s); name it as <workspace>:<denopod>", name, strings.Join(all, ", "))
	default:
		return t, fmt.Errorf("DenoPod %s is in %s; name it as <workspace>:%s", name, strings.Join(found, " and "), name)
	}
	return t, nil
}

func podClient(kubeconfig, ws string) (dynamic.ResourceInterface, error) {
	base, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + ws
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return dc.Resource(appctl.DenoPodGVR).Namespace("default"), nil
}

func readPod(u *unstructured.Unstructured) (denov1alpha1.DenoPod, error) {
	var p denov1alpha1.DenoPod
	b, err := u.MarshalJSON()
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

func runOf(stateDir, runID string) podRun {
	r := podRun{RunID: runID}
	if runID == "" {
		return r
	}
	r.Dir = filepath.Join(denoruntime.RunsDir(stateDir), runID)
	var st struct {
		PID int `json:"pid"`
	}
	if b, err := os.ReadFile(filepath.Join(r.Dir, "state.json")); err == nil && json.Unmarshal(b, &st) == nil {
		r.PID = st.PID
	}
	return r
}

func restarted(p denov1alpha1.DenoPod, token, oldRun string, wantListeners bool) bool {
	c := findCondition(p.Status.Conditions, denov1alpha1.ConditionRestarted)
	return c != nil && c.Message == token && p.Status.RunID != "" && p.Status.RunID != oldRun &&
		p.Status.Phase == denov1alpha1.DenoPodRunning && p.Status.Ready && (!wantListeners || len(p.Status.Listeners) > 0)
}

func findCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

func runDevRestartPod(ctx context.Context, a *app, vals clicfg.Values, target string) int {
	started := time.Now()
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "dev restart: ready-timeout: %v", err)
	}
	l, rec, code := a.record("dev restart", vals)
	if code != exitOK {
		return code
	}
	t, err := a.resolvePodTarget(ctx, l, rec, target)
	if err != nil {
		return a.fail(exitRefused, "dev restart: %v", err)
	}
	ri, err := podClient(t.kubeconfig, t.Workspace)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	u, err := ri.Get(ctx, t.DenoPod, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return a.fail(exitRefused, "dev restart: no DenoPod %s in %s of instance %s", t.DenoPod, t.Workspace, t.Instance)
	}
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	before, err := readPod(u)
	if err != nil {
		return a.fail(exitFailed, "dev restart: %v", err)
	}
	if before.Status.Phase != denov1alpha1.DenoPodRunning {
		return a.fail(exitRefused, "dev restart: DenoPod %s is %s; only a Running DenoPod is restarted", t.DenoPod, before.Status.Phase)
	}
	r := podRestartReport{podTarget: t, Request: started.UTC().Format(time.RFC3339Nano), Before: runOf(t.stateDir, before.Status.RunID),
		TypeScript: tsbundle.Describe()}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{denov1alpha1.AnnotationRestart: r.Request}}})
	if _, err := ri.Patch(ctx, t.DenoPod, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return a.fail(exitFailed, "dev restart: requesting the restart: %v", err)
	}
	wantListeners := len(before.Status.Listeners) > 0
	deadline := time.Now().Add(readyTimeout)
	var now denov1alpha1.DenoPod
	for {
		u, err := ri.Get(ctx, t.DenoPod, metav1.GetOptions{})
		if err == nil {
			if now, err = readPod(u); err == nil && restarted(now, r.Request, before.Status.RunID, wantListeners) {
				break
			}
		}
		if time.Now().After(deadline) {
			return a.fail(exitFailed, "dev restart: DenoPod %s did not come back Running and ready with a new run within %s (run %q, phase %s, ready %v, %d listeners)",
				t.DenoPod, readyTimeout, now.Status.RunID, now.Status.Phase, now.Status.Ready, len(now.Status.Listeners))
		}
		select {
		case <-ctx.Done():
			return a.fail(exitFailed, "dev restart: %v", errors.Join(ctx.Err(), err))
		case <-time.After(200 * time.Millisecond):
		}
	}
	r.After = runOf(t.stateDir, now.Status.RunID)
	r.Phase, r.Ready, r.Listeners = string(now.Status.Phase), now.Status.Ready, now.Status.Listeners
	r.Seconds = time.Since(started).Seconds()
	a.printJSON(r)
	return exitOK
}
