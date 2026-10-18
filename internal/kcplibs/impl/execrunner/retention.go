package execrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/logcap"
)

const (
	StdoutFile = "stdout.txt"
	StderrFile = "stderr.txt"
)

var runIDPattern = regexp.MustCompile(`^(pod|engine)-[0-9]{8}T[0-9]{6}\.[0-9]{9}-[0-9]+$`)

func IsRunID(name string) bool { return runIDPattern.MatchString(name) }

func (p *Pod) Remove(_ context.Context, runID string) error { return p.sup.remove(runID) }

func (e *Engine) Remove(_ context.Context, runID string) error { return e.sup.remove(runID) }

func (p *Pod) Sweep(named func(string) bool, before time.Time) []string {
	removed := SweepRuns(p.opts.RunsDir, named, before)
	for _, id := range removed {
		removePodCgroup(id)
	}
	removeSettledPodCgroups()
	return removed
}

func (p *Pod) Reap(named func(string) bool, startedBefore time.Time) []string {
	var out []string
	for _, r := range ReapUnnamed(p.opts.RunsDir, named, startedBefore, reapGrace) {
		out = append(out, fmt.Sprintf("%s pid %d killed %t", r.Run, r.PID, r.Killed))
	}
	return out
}

func (p *Pod) CapLogs(capBytes int64) []string { return CapRunLogs(p.opts.RunsDir, capBytes) }

func lastActive(dir string) time.Time {
	var last time.Time
	if st, err := os.Stat(dir); err == nil {
		last = st.ModTime()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return last
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last
}

func SweepRuns(runsDir string, named func(string) bool, before time.Time) []string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil
	}
	live := liveRuns(runsDir)
	var removed []string
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !IsRunID(id) || named(id) {
			continue
		}
		if _, ok := live[id]; ok {
			continue
		}
		dir := filepath.Join(runsDir, id)
		if !lastActive(dir).Before(before) {
			continue
		}
		if os.RemoveAll(dir) == nil {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return removed
}

func CapRunLogs(runsDir string, capBytes int64) []string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil
	}
	var rotated []string
	for _, e := range entries {
		if !e.IsDir() || !IsRunID(e.Name()) {
			continue
		}
		for _, name := range []string{StdoutFile, StderrFile} {
			p := filepath.Join(runsDir, e.Name(), name)
			if ok, _ := logcap.Rotate(p, capBytes); ok {
				rotated = append(rotated, filepath.Join(e.Name(), name))
			}
		}
	}
	return rotated
}
