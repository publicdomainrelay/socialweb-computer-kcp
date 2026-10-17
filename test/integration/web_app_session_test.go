//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

const (
	coreRestartWait  = 4 * time.Minute
	cookieBackWait   = time.Minute
	kcpDumpMinBytes  = 100_000
	kcpDumpMinSpaces = 4
)

type coreRestart struct {
	Binary struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"binary"`
	Before struct {
		PID          int    `json:"pid"`
		InvocationID string `json:"invocationID"`
	} `json:"before"`
	After struct {
		PID          int    `json:"pid"`
		InvocationID string `json:"invocationID"`
	} `json:"after"`
	UnitsUnchanged bool    `json:"unitsUnchanged"`
	Seconds        float64 `json:"seconds"`
}

func TestWebAppCookieSurvivesAGatewayCoreRestartAndABinarySwapAndNoSecretLeaks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { front.Close() })
	origin := "https://" + front.Addr().String()
	n := StartNetwork(t, NetworkOptions{Bidders: 1, WithGateway: true, UpTimeout: 5 * time.Minute,
		GatewayOptions: map[string]string{"public-origin": origin}})
	tn := n.Tenant(t, "tenant", tenantReady)
	waitOwnerGrant(t, n, tn)
	acct := n.OwnerAccount(t)
	web := n.webSignIn(t, front, origin, acct)

	swap := filepath.Join(n.Dir, "upgrade", "socialweb-computer")
	if r := n.h.As(ctx, "install", "-D", "-m", "0755", n.h.Bin, swap); r.Err != nil {
		t.Fatalf("copying the binary for the swap: %v %s", r.Err, r.Tail(3))
	}
	for _, leg := range []struct {
		name   string
		binary string
	}{{"a gateway core restart", ""}, {"a gateway core binary swap", swap}} {
		args := []string{"dev", "restart", "--state-dir", filepath.Join(n.StateDir, "apps")}
		if leg.binary != "" {
			args = append(args, "--binary", leg.binary)
		}
		r := n.Run("dev-restart-gateway-core", coreRestartWait, append(args, n.Instance+"-gateway", "core")...)
		var rep coreRestart
		if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil || r.Err != nil {
			t.Fatalf("%s: %v %v\n%s", leg.name, r.Err, err, r.Tail(10))
		}
		if rep.Before.PID == 0 || rep.After.PID == rep.Before.PID || rep.After.InvocationID == rep.Before.InvocationID || !rep.UnitsUnchanged {
			t.Fatalf("%s did not replace the core with every unit adopted: %+v", leg.name, rep)
		}
		if leg.binary != "" && rep.Binary.Path != leg.binary {
			t.Fatalf("%s: the core runs %s, want %s", leg.name, rep.Binary.Path, leg.binary)
		}
		back := time.Now()
		for deadline := back.Add(cookieBackWait); ; time.Sleep(500 * time.Millisecond) {
			if herr := n.health(); herr != nil {
				t.Fatalf("%s: %v", leg.name, herr)
			}
			code, _, raw, err := web.try(http.MethodGet, "/session", nil)
			if err == nil && code == http.StatusOK && strings.Contains(string(raw), acct.DID) {
				break
			}
			if code == http.StatusUnauthorized {
				t.Fatalf("%s: the same cookie got 401 from GET /session: a new sign-in would be needed", leg.name)
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: GET /session with the same cookie: %d %.100s %v", leg.name, code, raw, err)
			}
		}
		if code, _, body := web.do(http.MethodPost, "/api/clusters/list", map[string]any{}); code != http.StatusOK || !strings.Contains(string(body), `"tenant"`) {
			t.Fatalf("%s: the cluster list through the same cookie: %d %.200s", leg.name, code, body)
		}
		t.Logf("%s: core pid %d -> %d (%s) in %.1fs, every unit adopted; the same cookie answered %.1fs later",
			leg.name, rep.Before.PID, rep.After.PID, rep.Binary.Path, rep.Seconds, time.Since(back).Seconds())
	}

	code, _, body := web.do(http.MethodPost, "/api/clusters/kubeconfig", map[string]any{"name": "tenant"})
	var download struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := json.Unmarshal(body, &download); err != nil || code != http.StatusOK || download.Kubeconfig == "" {
		t.Fatalf("the kubeconfig download after the core restarts: %d %v", code, err)
	}
	cfg, err := clientcmd.Load([]byte(download.Kubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{web.token}
	for _, ai := range cfg.AuthInfos {
		if len(ai.ClientKeyData) == 0 {
			t.Fatal("the downloaded kubeconfig has no client key")
		}
		secrets = append(secrets, base64.StdEncoding.EncodeToString(ai.ClientKeyData))
		for _, l := range strings.Split(string(ai.ClientKeyData), "\n") {
			if len(l) == 64 {
				secrets = append(secrets, l)
				break
			}
		}
	}
	if len(secrets) < 3 {
		t.Fatalf("the downloaded client key gave no PEM body line to look for")
	}

	files, err := n.FilesContaining([]string{n.StateDir}, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > 0 {
		t.Errorf("the session cookie or the downloaded client key is in files of the network: %v", files)
	}
	if c, err := n.JournalContains(secrets, n.Started); err != nil || c > 0 {
		t.Errorf("the session cookie or the downloaded client key is in %d journal line(s) of the account: %v", c, err)
	}
	if procs, err := n.processesMentioning(secrets...); err != nil || len(procs) > 0 {
		t.Errorf("the session cookie or the downloaded client key is in the arguments or environment of: %v %v", procs, err)
	}

	dump := filepath.Join(n.Dir, "kcp-dump")
	r := n.h.As(ctx, "sh", "-c", kcpDumpScript, "sh", n.StateDir, dump, kubectlBinary)
	if r.Err != nil {
		t.Fatalf("dumping every kcp workspace: %v %s", r.Err, r.Tail(5))
	}
	var spaces, dumped int
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		var ws string
		var size int
		if _, err := fmt.Sscanf(l, "%s %d", &ws, &size); err == nil {
			spaces++
			dumped += size
		}
	}
	if spaces < kcpDumpMinSpaces || dumped < kcpDumpMinBytes {
		t.Fatalf("the kcp dump read too little: %d workspace(s), %d bytes:\n%s", spaces, dumped, r.Tail(20))
	}
	hits, err := n.FilesContaining([]string{dump}, append(secrets, `"refresh_token"`, "dpopPrivate", "client-key-data"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("kcp objects hold the session cookie, the client key, a refresh token or a DPoP key: %v", hits)
	}
	t.Logf("scanned %d kcp workspace(s) of every kcp of the network (%d bytes of objects), the state dir, the journal and every process", spaces, dumped)

	records, repos := 0, map[string]bool{}
	for _, h := range n.Up.Network.Status.Network.Hosts {
		base := "https://" + h.Address
		out, code, err := postJSON(acct.client, base+"/xrpc/com.atproto.sync.listRepos?limit=1000", "", nil)
		if err != nil || code != http.StatusOK {
			continue
		}
		list, _ := out["repos"].([]any)
		for _, rp := range list {
			did, _ := rp.(map[string]any)["did"].(string)
			if did == "" {
				continue
			}
			desc, code, err := postJSON(acct.client, base+"/xrpc/com.atproto.repo.describeRepo?repo="+url.QueryEscape(did), "", nil)
			if err != nil || code != http.StatusOK {
				t.Fatalf("describeRepo %s on %s: %d %v", did, h.Name, code, err)
			}
			repos[did] = true
			cols, _ := desc["collections"].([]any)
			for _, c := range cols {
				col, _ := c.(string)
				cursor := ""
				for {
					q := url.Values{"repo": {did}, "collection": {col}, "limit": {"100"}}
					if cursor != "" {
						q.Set("cursor", cursor)
					}
					page, code, err := postJSON(acct.client, base+"/xrpc/com.atproto.repo.listRecords?"+q.Encode(), "", nil)
					if err != nil || code != http.StatusOK {
						t.Fatalf("listRecords %s %s on %s: %d %v", did, col, h.Name, code, err)
					}
					raw, _ := json.Marshal(page)
					for _, s := range append(secrets, `"refresh_token"`, "dpopPrivate", "PRIVATE KEY") {
						if strings.Contains(string(raw), s) {
							t.Errorf("a record of %s in %s on %s holds a secret (%.12s...)", did, col, h.Name, s)
						}
					}
					recs, _ := page["records"].([]any)
					records += len(recs)
					cursor, _ = page["cursor"].(string)
					if cursor == "" || len(recs) == 0 {
						break
					}
				}
			}
		}
	}
	if !repos[acct.DID] || records == 0 {
		t.Fatalf("the PDS scan did not read the owner's repository: %d repo(s), %d record(s)", len(repos), records)
	}
	t.Logf("scanned %d record(s) of %d repo(s) on the network's PDSes", records, len(repos))
}

const kcpDumpScript = `set -u
state="$1"; out="$2"; kubectl="$3"
mkdir -p -m 0700 "$out"
walk() {
  k="$1"; base="$2"; ws="$3"; tag="$4"
  f="$out/$tag.$(echo "$ws" | tr ':' '_').yaml"
  types="$("$kubectl" --kubeconfig "$k" --server "$base/clusters/$ws" api-resources --verbs=list -o name 2>/dev/null | paste -sd, -)"
  [ -n "$types" ] && "$kubectl" --kubeconfig "$k" --server "$base/clusters/$ws" get "$types" -A -o yaml >"$f" 2>/dev/null
  echo "$tag:$ws $(wc -c <"$f" 2>/dev/null || echo 0)"
  "$kubectl" --kubeconfig "$k" --server "$base/clusters/$ws" get workspaces -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null |
    while read -r c; do [ -n "$c" ] && walk "$k" "$base" "$ws:$c" "$tag"; done
}
i=0
find "$state" -name admin.kubeconfig -path '*/kcp/*' | while read -r k; do
  i=$((i + 1))
  server="$("$kubectl" config view --kubeconfig "$k" --raw -o jsonpath='{.clusters[0].cluster.server}')"
  walk "$k" "${server%%/clusters/*}" root "kcp$i"
done
`
