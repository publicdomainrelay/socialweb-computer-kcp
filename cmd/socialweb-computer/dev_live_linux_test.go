//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/livegate"
)

type liveDev struct {
	bin   string
	state string
	kcp   string
	kine  string
}

func openLiveDev(t *testing.T) *liveDev {
	t.Helper()
	livegate.Short(t, "this test runs real kcp and kine processes under devsup (two dev instances, ~1 GB, ~20 s)")
	d := &liveDev{kcp: testExe("SOCIALWEB_KCP_PATH", "kcp"), kine: testExe("SOCIALWEB_KINE_PATH", "kine")}
	for name, p := range map[string]string{"kcp": d.kcp, "kine": d.kine} {
		if p == "" {
			livegate.RequireLocal(t, "no %s executable here (its SOCIALWEB_*_PATH env or PATH)", name)
			return nil
		}
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		livegate.RequireLocal(t, "XDG_RUNTIME_DIR is unset")
		return nil
	}
	dir := t.TempDir()
	d.bin = filepath.Join(dir, "socialweb-computer")
	if out, err := exec.Command("go", "build", "-o", d.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	d.state = filepath.Join(dir, "state")
	return d
}

func testExe(env, name string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	abs, _ := filepath.Abs(p)
	return abs
}

func (d *liveDev) runE(out any, args ...string) error {
	cmd := exec.Command(d.bin, append(args, "--state-dir", d.state)...)
	cmd.Env = append(os.Environ(), "SOCIALWEB_KCP_PATH="+d.kcp, "SOCIALWEB_KINE_PATH="+d.kine)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %v\n%s", args, err, stderr.String())
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("%v: %v\n%s", args, err, stdout.String())
		}
	}
	return nil
}

func (d *liveDev) run(t *testing.T, out any, args ...string) {
	t.Helper()
	if err := d.runE(out, args...); err != nil {
		t.Fatal(err)
	}
}

func (d *liveDev) up(t *testing.T, id string) upReport {
	var r upReport
	d.run(t, &r, "dev", "up", "--instance", id, "--profile", "quick")
	return r
}

func (d *liveDev) down(t *testing.T, id string) downReport {
	var r downReport
	d.run(t, &r, "dev", "down", "--instance", id)
	return r
}
