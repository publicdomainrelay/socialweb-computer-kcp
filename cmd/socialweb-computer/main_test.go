package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/capabilities"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
)

func runT(t *testing.T, env clicfg.MapEnv, goos string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, env, &out, &errb, goos, "amd64")
	return code, out.String(), errb.String()
}

func TestAVerbWithNoBackendOnThisOSExits2NamingItsPlan(t *testing.T) {
	cases := []struct {
		goos string
		args []string
		plan string
	}{
		{"windows", []string{"run", "bidder"}, "plan 0011"},
		{"darwin", []string{"run", "bidder"}, "plan 0012"},
		{"windows", []string{"dev", "up"}, "plan 0011"},
		{"darwin", []string{"internal", "devsup"}, "plan 0012"},
	}
	for _, c := range cases {
		code, _, stderr := runT(t, clicfg.MapEnv{"SOCIALWEB_HOST_BACKEND": "proc"}, c.goos, c.args...)
		if code != 2 || !strings.Contains(stderr, c.plan) || !strings.Contains(stderr, c.goos) {
			t.Errorf("%s %v: exit %d, %q; want 2 naming %s", c.goos, c.args, code, stderr, c.plan)
		}
	}
	for _, v := range capabilities.Tree() {
		for _, goos := range []string{"windows", "darwin"} {
			if why, missing := capabilities.Missing(v, goos, "arm64"); missing && !strings.Contains(why, "plan 001") {
				t.Errorf("%s on %s: %q names no plan", v.Path, goos, why)
			}
		}
	}
}

func TestServeWithoutAHostBackendExits2NamingTheOption(t *testing.T) {
	code, _, stderr := runT(t, clicfg.MapEnv{}, "linux", "serve", "gateway", "kcp")
	if code != 2 || !strings.Contains(stderr, "host-backend") || !strings.Contains(stderr, "SOCIALWEB_HOST_BACKEND") {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	code, _, stderr = runT(t, clicfg.MapEnv{"SOCIALWEB_HOST_BACKEND": "docker"}, "linux", "serve", "gateway", "kcp")
	if code != 2 || !strings.Contains(stderr, "docker") {
		t.Fatalf("an unknown backend: exit %d, %q", code, stderr)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := runT(t, clicfg.MapEnv{}, "linux", "version", "--json")
	var v versionInfo
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Name != "socialweb-computer" || v.GOOS != "linux" || v.Go == "" {
		t.Fatalf("exit %d %q", code, out)
	}
}
