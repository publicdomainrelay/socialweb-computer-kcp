package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/workloadoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/ephemeralpds"
)

const (
	liveEnv          = "SWC_LIVE_WORKLOAD_OAUTH"
	liveIngress      = "xrpc.fedproxy.com"
	livePLC          = "https://plc.directory"
	liveAtirc        = "https://irc.fedfork.com"
	liveAtircClient  = liveAtirc + "/oauth/client-metadata.json"
	liveAtircBinEnv  = "ATIRC_BIN"
	liveRole         = "atirc-acceptance"
	liveScrobblePath = scrobbleCollection + "/"
)

var linkRE = regexp.MustCompile(`/u/([0-9a-fA-F-]{36})`)

func liveRandom() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func liveHTTP() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: http.DefaultTransport}
}

func atircScopes(t *testing.T) []string {
	t.Helper()
	res, err := liveHTTP().Get(liveAtircClient)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var meta struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range strings.Fields(meta.Scope) {
		if !strings.HasPrefix(s, "transition:") {
			out = append(out, s)
		}
	}
	return out
}

func atircBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv(liveAtircBinEnv); bin != "" {
		return bin
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	res, err := liveHTTP().Get(liveAtirc + "/dl/" + arch)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/dl/%s answered %d", liveAtirc, arch, res.StatusCode)
	}
	path := filepath.Join(t.TempDir(), "atirc")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func liveForm(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	res, err := workloadoauth.NoRedirects(client).PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func liveGet(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 socialweb-computer-acceptance")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func TestLiveWorkloadMintsAnAtircSessionAndScrobblesThroughIt(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skip("set " + liveEnv + "=1 to run the live acceptance against " + liveIngress + ", " + livePLC + " and " + liveAtirc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	is := newIssuer(t)
	pds, err := ephemeralpds.Start(ctx, ephemeralpds.Options{
		IngressProxyHost: liveIngress,
		TLS:              true,
		PLCURL:           livePLC,
		StateDir:         t.TempDir(),
		OAuthHTTPClient:  liveHTTP(),
		ReadyTimeout:     2 * time.Minute,
		Configure: func(cfg *atpds.Config) {
			cfg.OAuth.Workload = &atpds.WorkloadOAuthConfig{AllowLoopbackIssuers: true}
		},
	})
	if err != nil {
		t.Fatalf("the ephemeral PDS: %v", err)
	}
	t.Cleanup(func() {
		if err := pds.Close(context.Background()); err != nil {
			t.Logf("closing the ephemeral PDS: %v", err)
		}
	})
	t.Logf("PDS %s DID %s handle %s", pds.URL, pds.DID, pds.Handle)

	w := &world{t: t, pdsURL: pds.URL, did: pds.DID, password: pds.Password, issuer: is}
	record, err := workloadoauth.Record(workloadoauth.Role{
		Name: liveRole, Account: pds.DID, Service: pds.Server.ServiceDID(), Issuer: is.url, Subject: workloadSubject,
		Clients: []workloadoauth.Client{{ID: liveAtirc + "/oauth/client-metadata*", RedirectURI: liveAtirc + "/oauth/callback", Scopes: atircScopes(t)}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w.xrpc(http.MethodPost, "com.atproto.repo.putRecord", w.passwordSession(),
		map[string]any{"repo": pds.DID, "collection": workloadauth.RBACCollection, "rkey": liveRole, "record": record}, nil, http.StatusOK)

	browser := newJar(t)
	session, err := workloadoauth.NewSession(liveHTTP())
	if err != nil {
		t.Fatal(err)
	}
	approval, err := session.ApproveLogin(ctx, browser, liveAtirc+"/login", pds.Handle, pds.DID, liveRole,
		func(_ context.Context, aud string) (string, error) { return is.token(t, workloadSubject, aud), nil })
	if err != nil {
		t.Fatalf("the workload sign-in to atirc: %v", err)
	}
	t.Logf("atirc session for %s through %s", approval.Account, approval.PDS)

	if status, body := liveForm(t, browser, liveAtirc+"/scrobble", url.Values{"did": {pds.DID}, "on": {"1"}}); status >= 400 {
		t.Fatalf("the scrobble switch answered %d: %s", status, body)
	}
	if status, body := liveForm(t, browser, liveAtirc+"/agent/link", url.Values{"did": {pds.DID}}); status >= 400 {
		t.Fatalf("the account link answered %d: %s", status, body)
	}
	status, page := liveGet(t, browser, liveAtirc+"/")
	match := linkRE.FindStringSubmatch(page)
	if status != http.StatusOK || match == nil {
		t.Fatalf("the atirc page (HTTP %d) shows no /u/<uuid> link", status)
	}
	status, token := liveGet(t, liveHTTP(), liveAtirc+"/u/"+match[1])
	token = strings.TrimSpace(token)
	if status != http.StatusOK || token == "" {
		t.Fatalf("the link token answered %d", status)
	}

	commits, stop := pds.Server.Sequencer().Subscribe()
	defer stop()
	channel := "#swc-wid-" + liveRandom()
	text := "\U0001F680 socialweb-computer workload OAuth acceptance " + liveRandom()
	pub := exec.CommandContext(ctx, atircBinary(t), "pub", "-c", channel, text)
	pub.Env = append(os.Environ(), "ATIRC_TOKEN="+token)
	if out, err := pub.CombinedOutput(); err != nil {
		t.Logf("atirc pub failed on its token's transport (%v: %.300s); posting the same message over %s/xrpc/chat.atirc.post", err, strings.TrimSpace(string(out)), liveAtirc)
		body, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, liveAtirc+"/xrpc/chat.atirc.post", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := liveHTTP().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("chat.atirc.post answered %d: %s", res.StatusCode, raw)
		}
	} else {
		t.Logf("atirc pub posted to %s", channel)
	}
	deadline := time.After(2 * time.Minute)
	for found := false; !found; {
		select {
		case ev, ok := <-commits:
			if !ok {
				t.Fatal("the PDS commit stream closed")
			}
			if ev.Repo.String() != pds.DID {
				continue
			}
			for _, op := range ev.Ops {
				if strings.HasPrefix(op.Path, liveScrobblePath) {
					t.Logf("atirc wrote %s through its OAuth session", op.Path)
					found = true
				}
			}
		case <-deadline:
			t.Fatal("no com.vibescrobble.scrobble record reached the account's repo within 2m of atirc pub")
		}
	}
	_, _ = liveGet(t, browser, liveAtirc+"/logout")
}
