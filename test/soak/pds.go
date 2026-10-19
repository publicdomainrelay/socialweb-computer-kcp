//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type networkView struct {
	Network struct {
		Status struct {
			Listeners []struct {
				Component string `json:"component"`
				Address   string `json:"address"`
			} `json:"listeners"`
			Network struct {
				CaBundleRef string `json:"caBundleRef"`
			} `json:"network"`
			Apps []struct {
				Name      string `json:"name"`
				Did       string `json:"did"`
				Listeners []struct {
					Component string `json:"component"`
					Name      string `json:"name"`
					Address   string `json:"address"`
				} `json:"listeners"`
			} `json:"apps"`
		} `json:"status"`
	} `json:"network"`
}

func (n networkView) dids() []string {
	var out []string
	for _, a := range n.Network.Status.Apps {
		if a.Did != "" {
			out = append(out, a.Did)
		}
	}
	return out
}

func kvSample(stateDir string) map[string]any {
	out := map[string]any{}
	var paths []string
	for _, pat := range []string{"network/state/*/kv.sqlite3", "apps/dev/*/components/*/pds", "apps/dev/*/components/*/*/kv.sqlite3", "apps/dev/*/components/pds/repo.sqlite3"} {
		m, _ := filepath.Glob(filepath.Join(stateDir, pat))
		paths = append(paths, m...)
	}
	for _, p := range paths {
		rel, _ := filepath.Rel(stateDir, p)
		entry := map[string]any{}
		if info, err := os.Stat(p); err == nil {
			entry["bytes"] = info.Size()
		}
		if info, err := os.Stat(p + "-wal"); err == nil {
			entry["walBytes"] = info.Size()
		}
		if db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(5000)"); err == nil {
			var n int64
			if err := db.QueryRow("select count(*) from kv").Scan(&n); err == nil {
				entry["rows"] = n
			} else {
				entry["rowsError"] = err.Error()
			}
			db.Close()
		}
		out[rel] = entry
	}
	return out
}

func kineKey(name string) (resource, cluster string) {
	seg := strings.Split(strings.TrimPrefix(name, "/registry/"), "/")
	if len(seg) >= 3 && strings.Contains(seg[0], ".") {
		return seg[0] + "/" + seg[1], seg[2]
	}
	if len(seg) >= 2 {
		return seg[0], seg[1]
	}
	return name, ""
}

func kineSample(stateDir string) map[string]any {
	out := map[string]any{}
	for app, dir := range childStateDirs(stateDir) {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == "apps" && app == "network" {
				return fs.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".db") {
				return nil
			}
			db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(5000)")
			if err != nil {
				return nil
			}
			defer db.Close()
			rows, err := db.Query("select name from kine where id in (select max(id) from kine group by name) and deleted = 0")
			if err != nil {
				return nil
			}
			defer rows.Close()
			resources := map[string]int{}
			clusters := map[string]bool{}
			total := 0
			for rows.Next() {
				var name string
				if rows.Scan(&name) != nil {
					continue
				}
				r, c := kineKey(name)
				resources[r]++
				clusters[c] = true
				total++
			}
			rel, _ := filepath.Rel(stateDir, p)
			out[rel] = map[string]any{"live": total, "clusters": len(clusters), "resources": resources}
			return nil
		})
	}
	return out
}

func pdsSample(ctx context.Context, binary, instance, stateDir string) map[string]any {
	out := map[string]any{}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(cctx, binary, "dev", "status", "--instance", instance, "--state-dir", stateDir).Output()
	var nv networkView
	if err != nil || json.Unmarshal(raw, &nv) != nil {
		return map[string]any{"error": fmt.Sprint(err)}
	}
	ca, err := os.ReadFile(nv.Network.Status.Network.CaBundleRef)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	defer client.CloseIdleConnections()
	get := func(base, path string, into any) error {
		resp, err := client.Get(base + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(into)
	}
	type target struct{ name, base, did string }
	var targets []target
	for _, l := range nv.Network.Status.Listeners {
		if strings.HasSuffix(l.Component, "/pds") {
			targets = append(targets, target{name: l.Component, base: "https://" + l.Address})
		}
	}
	for _, a := range nv.Network.Status.Apps {
		repo := ""
		for _, l := range a.Listeners {
			if l.Name == "tcp" && (l.Component == "pds" || (l.Component == "requester" && repo == "")) {
				repo = l.Address
			}
		}
		if repo != "" {
			targets = append(targets, target{name: a.Name + "/repo", base: "http://" + repo, did: a.Did})
		}
	}
	for _, tg := range targets {
		l := struct{ Component string }{tg.name}
		base := tg.base
		perRepo := map[string]any{}
		dids := nv.dids()
		if tg.did != "" {
			dids = []string{tg.did}
		}
		for _, did := range dids {
			r := struct{ Did string }{did}
			var desc struct {
				Collections []string `json:"collections"`
			}
			if err := get(base, "/xrpc/com.atproto.repo.describeRepo?repo="+r.Did, &desc); err != nil || len(desc.Collections) == 0 {
				continue
			}
			counts := map[string]int{}
			for _, c := range desc.Collections {
				cursor := ""
				for {
					var page struct {
						Records []json.RawMessage `json:"records"`
						Cursor  string            `json:"cursor"`
					}
					path := "/xrpc/com.atproto.repo.listRecords?limit=100&repo=" + r.Did + "&collection=" + c
					if cursor != "" {
						path += "&cursor=" + cursor
					}
					if err := get(base, path, &page); err != nil {
						break
					}
					counts[strings.TrimPrefix(c, "com.publicdomainrelay.temp.")] += len(page.Records)
					if page.Cursor == "" || len(page.Records) == 0 {
						break
					}
					cursor = page.Cursor
				}
			}
			perRepo[r.Did] = counts
		}
		out[l.Component] = perRepo
	}
	return out
}
