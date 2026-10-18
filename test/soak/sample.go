//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var appRE = regexp.MustCompile(`/apps/[^/]+/([a-z0-9-]+)(?:/|\s|$)`)

var podRE = regexp.MustCompile(`/(?:components|network/state)/([a-z0-9-]+(?:/[a-z0-9-]+)?)`)

type procSum struct {
	N       int   `json:"n"`
	RssKB   int64 `json:"rssKB"`
	Sockets int   `json:"sockets"`
}

func socketCount(pid int) int {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, fd := range fds {
		if l, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && strings.HasPrefix(l, "socket:") {
			n++
		}
	}
	return n
}

func kv(path string) map[string]int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.ReplaceAll(sc.Text(), ":", " "))
		if len(fields) >= 2 {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				out[fields[0]] = n
			}
		}
	}
	return out
}

func hostSample() map[string]any {
	mem := kv("/proc/meminfo")
	vm := kv("/proc/vmstat")
	load, _ := os.ReadFile("/proc/loadavg")
	var st syscall.Statfs_t
	syscall.Statfs("/var/tmp", &st)
	out := map[string]any{
		"memTotalKB": mem["MemTotal"], "memAvailableKB": mem["MemAvailable"], "swapUsedKB": mem["SwapTotal"] - mem["SwapFree"],
		"oomKill": vm["oom_kill"], "load": strings.Fields(string(load))[0:3],
		"diskUsedBytes": int64(st.Blocks-st.Bfree) * int64(st.Bsize), "diskTotalBytes": int64(st.Blocks) * int64(st.Bsize),
	}
	for _, j := range []string{"/var/log/journal", "/run/log/journal"} {
		out["journal:"+j] = treeBytes(j)
	}
	return out
}

func treeBytes(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				total += st.Blocks * 512
			} else {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func classify(args []string) (string, string, bool) {
	if len(args) == 0 {
		return "", "", false
	}
	full := strings.Join(args, " ")
	app := "network"
	if m := appRE.FindStringSubmatch(full); m != nil {
		app = m[1]
	}
	base := filepath.Base(args[0])
	switch {
	case base == "kcp", base == "kine", base == "bao", base == "firecracker", base == "dumbpipe", base == "caddy":
		return app, base, true
	case strings.HasPrefix(base, "postgres"):
		return app, "postgres", true
	case base == "deno":
		entry := "inline"
		if m := podRE.FindStringSubmatch(full); m != nil {
			entry = m[1]
		}
		return app, "deno:" + entry, true
	case base == "socialweb-computer" || base == "swc":
		words := []string{}
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				break
			}
			words = append(words, a)
			if len(words) == 3 {
				break
			}
		}
		return app, "core:" + strings.Join(words, "_"), true
	}
	return "", "", false
}

func procSample() map[string]procSum {
	uid := os.Getuid()
	entries, _ := os.ReadDir("/proc")
	out := map[string]procSum{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		info, err := os.Stat("/proc/" + e.Name())
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(raw) == 0 {
			continue
		}
		app, role, ok := classify(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"))
		if !ok {
			continue
		}
		statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err != nil {
			continue
		}
		f := strings.Fields(string(statm))
		if len(f) < 2 {
			continue
		}
		pages, _ := strconv.ParseInt(f[1], 10, 64)
		key := app + "/" + role
		s := out[key]
		s.N++
		s.RssKB += pages * int64(os.Getpagesize()) / 1024
		s.Sockets += socketCount(pid)
		out[key] = s
	}
	return out
}

func childStateDirs(stateDir string) map[string]string {
	out := map[string]string{"network": stateDir}
	matches, _ := filepath.Glob(filepath.Join(stateDir, "apps", "*", "*", "instance.json"))
	for _, m := range matches {
		dir := filepath.Dir(m)
		out[filepath.Base(dir)] = dir
	}
	return out
}

func diskSample(stateDir string) map[string]any {
	out := map[string]any{}
	dirs := childStateDirs(stateDir)
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	var sqlite []map[string]any
	for _, n := range names {
		dir := dirs[n]
		children, _ := os.ReadDir(dir)
		for _, c := range children {
			if n == "network" && c.Name() == "apps" {
				continue
			}
			out[n+"/"+c.Name()] = treeBytes(filepath.Join(dir, c.Name()))
		}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == "apps" && n == "network" {
				return fs.SkipDir
			}
			if !d.IsDir() && (strings.HasSuffix(p, ".db") || strings.HasSuffix(p, ".db-wal")) {
				if info, err := d.Info(); err == nil {
					sqlite = append(sqlite, map[string]any{"app": n, "file": filepath.Base(p), "bytes": info.Size()})
				}
			}
			return nil
		})
	}
	out["total"] = treeBytes(stateDir)
	out["sqlite"] = sqlite
	return out
}

func statusSample(ctx context.Context, binary, instance, stateDir string) any {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(cctx, binary, "status", "--instance", instance, "--state-dir", stateDir).Output()
	if len(raw) == 0 {
		return map[string]any{"error": fmt.Sprint(err)}
	}
	var apps []map[string]any
	if err := json.Unmarshal(raw, &apps); err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, a := range apps {
		list, _ := a["contracts"].([]any)
		if len(list) == 0 {
			continue
		}
		phases := map[string]int{}
		for _, c := range list {
			m, _ := c.(map[string]any)
			p, _ := m["phase"].(string)
			phases[p]++
		}
		a["contracts"] = phases
	}
	return apps
}

func runSample(args []string) int {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	every := fs.Duration("every", 5*time.Minute, "sample interval")
	outPath := fs.String("out", "/var/tmp/soak/samples.jsonl", "output file")
	binary := fs.String("binary", "/var/tmp/soak/bin/socialweb-computer", "socialweb-computer binary")
	once := fs.Bool("once", false, "write one sample and exit")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.MkdirAll(filepath.Dir(*outPath), 0o755)
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	started := time.Now()
	for {
		at := time.Now()
		s := map[string]any{"ts": at.UTC().Format(time.RFC3339), "elapsedSec": int(at.Sub(started).Seconds()), "host": hostSample(),
			"procs": procSample(), "disk": diskSample(t.layout.StateDir), "status": statusSample(ctx, *binary, *instance, *stateDir),
			"kv": kvSample(t.layout.StateDir), "pds": pdsSample(ctx, *binary, *instance, *stateDir),
			"kine": kineSample(t.layout.StateDir)}
		s["sampleSec"] = time.Since(at).Seconds()
		line, _ := json.Marshal(s)
		f, err := os.OpenFile(*outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
		if *once {
			return 0
		}
		if !sleepCtx(ctx, *every-time.Since(at)) {
			return 0
		}
	}
}
