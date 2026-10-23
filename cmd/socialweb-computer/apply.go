package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

var (
	applyOptions  = []string{"set", "values", "phase", "dry-run", "wait-timeout", "instance", "state-dir", "kcp-kubeconfig"}
	renderOptions = []string{"set", "values", "phase"}
)

type listedSet struct {
	Name       string   `json:"name"`
	Summary    string   `json:"summary"`
	Values     string   `json:"values"`
	Phases     []string `json:"phases"`
	Workspaces []string `json:"workspaces"`
	Files      int      `json:"files"`
}

type listing struct {
	Sets      []listedSet           `json:"sets"`
	Unapplied []manifests.Unapplied `json:"unapplied"`
}

func runManifestsList(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	ix, err := manifests.Load()
	if err != nil {
		return a.fail(exitFailed, "manifests list: %v", err)
	}
	out := listing{Unapplied: ix.Unapplied}
	for _, s := range ix.Sets {
		ls := listedSet{Name: s.Name, Summary: s.Summary, Values: s.Values}
		seen := map[string]bool{}
		for _, p := range s.Phases {
			ls.Phases = append(ls.Phases, p.Name)
			ls.Files += len(p.Files)
			if !seen[p.Workspace] {
				seen[p.Workspace] = true
				ls.Workspaces = append(ls.Workspaces, p.Workspace)
			}
		}
		out.Sets = append(out.Sets, ls)
	}
	if vals.Bool("json") {
		a.printJSON(out)
		return exitOK
	}
	for _, s := range out.Sets {
		fmt.Fprintf(a.stdout, "%-22s values=%-20s phases=%-2d files=%-2d %s\n", s.Name, s.Values, len(s.Phases), s.Files, s.Summary)
		fmt.Fprintf(a.stdout, "%-22s %s\n", "", strings.Join(s.Phases, " -> "))
	}
	fmt.Fprintln(a.stdout)
	for _, u := range out.Unapplied {
		fmt.Fprintf(a.stdout, "unapplied %-30s %s\n", u.Path, u.Why)
	}
	return exitOK
}

func selectSets(vals clicfg.Values) ([]manifests.Set, map[string]any, error) {
	ix, err := manifests.Load()
	if err != nil {
		return nil, nil, err
	}
	names := strings.Split(vals.String("set"), ",")
	if vals.String("set") == "" {
		return nil, nil, fmt.Errorf("--set names no set; one of %s", strings.Join(ix.Names(), ", "))
	}
	raw := map[string]json.RawMessage{}
	if p := vals.String("values"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	var sets []manifests.Set
	values := map[string]any{}
	for _, n := range names {
		s, ok := ix.Set(strings.TrimSpace(n))
		if !ok {
			return nil, nil, fmt.Errorf("no set %q; one of %s", n, strings.Join(ix.Names(), ", "))
		}
		v, err := manifests.DecodeValues(s.Values, raw[s.Name])
		if err != nil {
			return nil, nil, err
		}
		delete(raw, s.Name)
		if sel := vals.String("phase"); sel != "" {
			only, err := selectPhases(s, sel)
			if err != nil {
				return nil, nil, err
			}
			s.Phases = only
		}
		sets = append(sets, s)
		values[s.Name] = v
	}
	if len(raw) > 0 {
		var extra []string
		for k := range raw {
			extra = append(extra, k)
		}
		sort.Strings(extra)
		return nil, nil, fmt.Errorf("the values file names %v, which --set does not apply", extra)
	}
	return sets, values, nil
}

func selectPhases(s manifests.Set, sel string) ([]manifests.Phase, error) {
	index := map[string]int{}
	for i, p := range s.Phases {
		index[p.Name] = i
	}
	pick := make([]bool, len(s.Phases))
	for item := range strings.SplitSeq(sel, ",") {
		from, to, isRange := strings.Cut(strings.TrimSpace(item), "..")
		if !isRange {
			to = from
		}
		i, ok := index[from]
		j, ok2 := index[to]
		if !ok || !ok2 || j < i {
			return nil, fmt.Errorf("set %s has no phase or ordered phase range %q (phases %v)", s.Name, item, phaseNames(s))
		}
		for k := i; k <= j; k++ {
			pick[k] = true
		}
	}
	var out []manifests.Phase
	for i, p := range s.Phases {
		if pick[i] {
			out = append(out, p)
		}
	}
	return out, nil
}

func phaseNames(s manifests.Set) []string {
	out := make([]string, 0, len(s.Phases))
	for _, p := range s.Phases {
		out = append(out, p.Name)
	}
	return out
}

func runManifestsRender(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	sets, values, err := selectSets(vals)
	if err != nil {
		return a.fail(exitRefused, "manifests render: %v", err)
	}
	for _, s := range sets {
		for _, p := range s.Phases {
			ws, texts, err := s.RenderText(p, values[s.Name])
			if err == nil {
				_, err = s.RenderPhase(p, values[s.Name])
			}
			if err != nil {
				return a.fail(exitFailed, "manifests render: set %s phase %s: %v", s.Name, p.Name, err)
			}
			for _, tx := range texts {
				fmt.Fprintf(a.stdout, "--- # set=%s phase=%s workspace=%s file=%s\n", s.Name, p.Name, ws, path.Clean(tx.Path))
				a.stdout.Write(tx.Body)
			}
		}
	}
	return exitOK
}

func (a *app) applyConfig(vals clicfg.Values) (*rest.Config, error) {
	kubeconfig := vals.String("kcp-kubeconfig")
	if kubeconfig == "" {
		_, rec, code := a.record("apply", vals)
		if code != exitOK {
			return nil, fmt.Errorf("no --kcp-kubeconfig and no dev instance record")
		}
		kubeconfig = rec.AdminKubeconfig
	}
	return kcpserver.FrontRestConfig(kubeconfig)
}

func runApply(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) > 0 {
		return a.fail(exitRefused, "apply: unexpected arguments %v; name sets with --set", rest)
	}
	sets, values, err := selectSets(vals)
	if err != nil {
		return a.fail(exitRefused, "apply: %v", err)
	}
	timeout, err := time.ParseDuration(vals.String("wait-timeout"))
	if err != nil {
		return a.fail(exitRefused, "apply: --wait-timeout: %v", err)
	}
	cfg, err := a.applyConfig(vals)
	if err != nil {
		return a.fail(exitRefused, "apply: %v", err)
	}
	ap, err := kcpapply.New(cfg, kcpapply.Options{DryRun: vals.Bool("dry-run"), WaitTimeout: timeout, Log: a.log})
	if err != nil {
		return a.fail(exitFailed, "apply: %v", err)
	}
	all := kcpapply.Report{DryRun: vals.Bool("dry-run"), Phases: []kcpapply.PhaseReport{}}
	for _, s := range sets {
		r, err := ap.Apply(ctx, s, values[s.Name])
		all.Phases = append(all.Phases, r.Phases...)
		if err != nil {
			a.printJSON(all)
			return a.fail(exitFailed, "apply: %v", err)
		}
	}
	a.printJSON(all)
	return exitOK
}
