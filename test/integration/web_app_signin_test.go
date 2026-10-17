//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	webSessionCookie = "swc_session"
	webAppPod        = "gateway/socialweb-computer-k8s"
	signInTimeout    = 3 * time.Minute
)

type webSession struct {
	t      testing.TB
	origin string
	client *http.Client
	token  string
}

func (w *webSession) try(method, path string, body any) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.origin+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Origin", w.origin)
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.AddCookie(&http.Cookie{Name: webSessionCookie, Value: w.token})
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, raw, nil
}

func (w *webSession) do(method, path string, body any) (int, http.Header, []byte) {
	w.t.Helper()
	code, header, raw, err := w.try(method, path, body)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, header, raw
}

func (w *webSession) did() (int, string) {
	w.t.Helper()
	code, _, raw, err := w.try(http.MethodGet, "/session", nil)
	if err != nil {
		w.t.Fatalf("GET /session: %v", err)
	}
	var out struct {
		DID string `json:"did"`
	}
	json.Unmarshal(raw, &out)
	return code, out.DID
}

func startTLSForwarder(t testing.TB, ln net.Listener, cert tls.Certificate, backend func() string) {
	t.Helper()
	tl := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	t.Cleanup(func() { tl.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, err := net.DialTimeout("tcp", backend(), 5*time.Second)
				if err != nil {
					return
				}
				defer b.Close()
				go io.Copy(b, c)
				io.Copy(c, b)
			}()
		}
	}()
}

func (n *Network) podServing(t testing.TB, port string) string {
	t.Helper()
	r := n.h.As(context.Background(), "sh", "-c", `grep -l -F ":$1\"" "$2"/denokcp/runs/*/listeners.json | head -1`, "sh", port, n.StateDir)
	path := strings.TrimSpace(r.Stdout)
	if r.Err != nil || path == "" {
		t.Fatalf("no DenoPod run directory under %s reports a listener on port %s: %v", n.StateDir, port, r.Err)
	}
	return filepath.Dir(path)
}

func (n *Network) kubectlNodes(kc, bin string) (int, string) {
	r := n.h.As(context.Background(), "env", "PATH="+bin, kubectlBinary, "--kubeconfig", kc, "--request-timeout=15s", "get", "nodes", "-o", "name")
	return r.Code, strings.TrimSpace(r.Stdout)
}

func TestWebAppSignInThroughThePluginKeepsTheCookieAcrossARestartAndIssuesACertificate(t *testing.T) {
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

	core := tn.Kube.CoreV1()
	if _, err := core.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsServer}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if _, err := core.ServiceAccounts(nsServer).Get(ctx, "default", metav1.GetOptions{}); err == nil {
			break
		} else if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			t.Fatalf("the default ServiceAccount of %s: %v", nsServer, err)
		}
	}
	waitNodes := func(want int, podName string) {
		t.Helper()
		started := time.Now()
		if _, err := core.Pods(nsServer).Create(ctx, serverPod(podName, "", true), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		for {
			if err := n.health(); err != nil {
				t.Fatalf("waiting for %d node(s): %v", want, err)
			}
			if err := tn.ClaimFailure(ctx); err != nil {
				t.Fatalf("waiting for %d node(s): %v", want, err)
			}
			p, perr := core.Pods(nsServer).Get(ctx, podName, metav1.GetOptions{})
			if perr == nil && podReady(p) {
				claims, cerr := tn.Claims(ctx)
				running := 0
				for _, c := range claims {
					if c.Status.Phase == "Running" && c.Status.Contract.ReceiptUri != "" {
						running++
					}
				}
				if cerr != nil || running != want {
					t.Fatalf("pod %s is Ready on %s, want %d Running NodeClaims with a receipt, got %d (%v)", podName, p.Spec.NodeName, want, running, cerr)
				}
				t.Logf("pod %s Ready on market node %s +%.1fs (%d NodeClaim(s) Running with a receipt)", podName, p.Spec.NodeName, time.Since(started).Seconds(), running)
				return
			}
			if time.Since(started) > scaleOutTimeout {
				t.Fatalf("pod %s was not Ready within %s: %s", podName, scaleOutTimeout, tn.Summary(ctx))
			}
			time.Sleep(2 * time.Second)
		}
	}
	waitNodes(1, "it-web-a")

	restart := n.Run("dev-restart", 3*time.Minute, "dev", "restart", n.Instance, webAppPod, "--state-dir", n.StateRoot)
	if restart.Err != nil {
		t.Fatalf("dev restart %s: %v\n%s", webAppPod, restart.Err, restart.Tail(10))
	}
	var restarted struct {
		Before struct{ PID int } `json:"before"`
		After  struct{ PID int } `json:"after"`
		Ready  bool              `json:"ready"`
	}
	if err := json.Unmarshal([]byte(restart.Stdout), &restarted); err != nil || restarted.Before.PID == 0 || restarted.Before.PID == restarted.After.PID || !restarted.Ready {
		t.Fatalf("dev restart did not replace the web app process: %+v %v", restarted, err)
	}
	t.Logf("restarted the web app DenoPod: pid %d -> %d in %.1fs", restarted.Before.PID, restarted.After.PID, restart.Elapsed.Seconds())
	for deadline, back := time.Now().Add(30*time.Second), time.Now(); ; time.Sleep(500 * time.Millisecond) {
		code, _, raw, err := web.try(http.MethodGet, "/session", nil)
		if err == nil && code == http.StatusOK && strings.Contains(string(raw), acct.DID) {
			t.Logf("the same cookie answered GET /session %.1fs after the restart returned", time.Since(back).Seconds())
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /session with the same cookie after the restart: %d %.100s %v", code, raw, err)
		}
	}
	if code, _, body := web.do(http.MethodPost, "/api/clusters/list", map[string]any{}); code != http.StatusOK || !strings.Contains(string(body), `"tenant"`) {
		t.Fatalf("the cluster list through the same cookie after the restart: %d %.200s", code, body)
	}
	waitNodes(2, "it-web-b")
	if vms, err := n.BidderVMs(ctx); err != nil || len(vms) < 2 {
		t.Errorf("the bidder holds %d ComputeVM(s), want at least 2: %v", len(vms), err)
	}

	code, header, body := web.do(http.MethodPost, "/api/clusters/kubeconfig", map[string]any{"name": "tenant"})
	if code != http.StatusOK || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the kubeconfig download: %d, Cache-Control %q, %.100s", code, header.Get("Cache-Control"), body)
	}
	var download struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := json.Unmarshal(body, &download); err != nil || download.Kubeconfig == "" {
		t.Fatalf("the kubeconfig download has no kubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load([]byte(download.Kubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, ai := range cfg.AuthInfos {
		if len(ai.ClientCertificateData) == 0 || len(ai.ClientKeyData) == 0 || ai.Exec != nil || ai.Token != "" {
			t.Fatalf("the downloaded kubeconfig is not a plain client certificate")
		}
	}
	bin := filepath.Join(n.Dir, "bin")
	if r := n.h.As(ctx, "mkdir", "-p", "-m", "0755", bin); r.Err != nil {
		t.Fatal(r.Err)
	}
	kc := filepath.Join(n.Dir, "kc.yaml")
	if err := n.h.WriteFile(kc, []byte(download.Kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(download.Kubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	restCfg.Timeout = 15 * time.Second
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	if code, nodes := n.kubectlNodes(kc, bin); code != 0 || len(strings.Fields(nodes)) != 2 {
		t.Fatalf("kubectl get nodes with the downloaded kubeconfig: exit %d, nodes %q, want 2", code, nodes)
	}
	if l, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil || len(l.Items) != 2 {
		t.Fatalf("client-go Nodes().List with the downloaded kubeconfig: %v", err)
	}
	liveCerts := func() (live, revoked int) {
		certs, err := tn.Certificates(ctx)
		if err != nil {
			t.Fatalf("listing the ClusterClientCertificates: %v", err)
		}
		for _, c := range certs {
			if c.Spec.Did != acct.DID {
				t.Errorf("ClusterClientCertificate %s is for %s, want %s", c.Name, c.Spec.Did, acct.DID)
			}
			if c.Spec.Revoked {
				revoked++
			} else {
				live++
			}
		}
		return
	}
	if live, revoked := liveCerts(); live != 1 || revoked != 0 {
		t.Fatalf("after one download: %d live and %d revoked ClusterClientCertificates, want 1 and 0", live, revoked)
	}

	code, _, body = web.do(http.MethodDelete, "/session", nil)
	var signedOut struct {
		Revoked bool `json:"revoked"`
	}
	json.Unmarshal(body, &signedOut)
	if code != http.StatusOK || !signedOut.Revoked {
		t.Fatalf("DELETE /session: %d %.200s, want 200 and revoked true", code, body)
	}
	if code, _ := web.did(); code != http.StatusUnauthorized {
		t.Fatalf("GET /session with the cookie after the sign-out: %d, want 401", code)
	}

	revokedAt := time.Now()
	req, _ := http.NewRequest(http.MethodPost, "http://"+n.xrpcAddress(t)+"/xrpc/"+clusterNSID+"revokeIdentity", strings.NewReader(`{"name":"tenant"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+acct.Token(t, "revokeIdentity"))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revokeIdentity: %v %v", resp, err)
	}
	resp.Body.Close()
	for deadline := revokedAt.Add(revokeRefusalTime); ; time.Sleep(200 * time.Millisecond) {
		if herr := n.health(); herr != nil {
			t.Fatalf("waiting for the revoked certificate to be refused: %v", herr)
		}
		_, lerr := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		code, _ := n.kubectlNodes(kc, bin)
		if lerr != nil && (apierrors.IsUnauthorized(lerr) || apierrors.IsForbidden(lerr)) && code != 0 {
			t.Logf("the downloaded certificate was refused %.2fs after revokeIdentity", time.Since(revokedAt).Seconds())
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the certificate was not refused within %s of revokeIdentity: %v", revokeRefusalTime, lerr)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		live, revoked := liveCerts()
		if live == 0 && revoked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after revokeIdentity: %d live and %d revoked ClusterClientCertificates, want 0 and 1", live, revoked)
		}
	}

	gwDir := n.GatewayDir()
	dirs := []string{filepath.Join(gwDir, "logs"), filepath.Join(gwDir, "denokcp", "runs")}
	r := n.h.AsInput(ctx, []byte(web.token+"\n"), append([]string{"grep", "-rlF", "-f", "-"}, dirs...)...)
	if r.Code > 1 {
		t.Fatalf("the leak scan of the cookie token failed: exit %d: %s", r.Code, r.Tail(3))
	}
	if strings.TrimSpace(r.Stdout) != "" {
		t.Errorf("the session cookie token is in: %s", r.Stdout)
	}
	r = n.h.As(ctx, append([]string{"grep", "-rl", "-E", `"refresh_token"|dpopPrivate|PRIVATE KEY|client-key-data`, "--include=stdout.txt", "--include=stderr.txt", "--include=core.log"}, dirs...)...)
	if r.Code > 1 {
		t.Fatalf("the leak scan of tokens and keys failed: exit %d: %s", r.Code, r.Tail(3))
	}
	if strings.TrimSpace(r.Stdout) != "" {
		t.Errorf("a refresh token, a DPoP private key or a client key is in: %s", r.Stdout)
	}
	kcpKubeconfig := filepath.Join(gwDir, "kcp", "admin.kubeconfig")
	kcpRaw, err := n.read(kcpKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	kcpRest, err := clientcmd.RESTConfigFromKubeConfig(kcpRaw)
	if err != nil {
		t.Fatal(err)
	}
	kcpBase := strings.TrimSuffix(strings.SplitN(kcpRest.Host, "/clusters/", 2)[0], "/")
	scanned := map[string]int{}
	for _, scan := range []struct{ ws, kind string }{{"root:app", "denopods"}, {"root:app", "marketidentities"}, {tn.Home, "nodeclaims"}, {tn.Home, "computecontracts"}} {
		r := n.h.As(ctx, "env", "PATH="+bin, kubectlBinary, "--kubeconfig", kcpKubeconfig, "--server", kcpBase+"/clusters/"+scan.ws,
			"get", scan.kind, "-A", "-o", "yaml")
		scanned[scan.kind] = len(r.Stdout)
		for _, secret := range []string{web.token, `"refresh_token"`, "dpopPrivate", "PRIVATE KEY", "client-key-data"} {
			if strings.Contains(r.Stdout, secret) {
				t.Errorf("the %s of workspace %s hold %q", scan.kind, scan.ws, map[bool]string{true: "the session cookie token", false: secret}[secret == web.token])
			}
		}
	}
	if scanned["denopods"] < 1000 || scanned["nodeclaims"] < 1000 {
		t.Fatalf("the object scan read too little: %v", scanned)
	}
	t.Logf("scanned the kcp objects for secrets (bytes by kind): %v", scanned)
}

func (n *Network) webFront() string {
	gw, _ := n.App("gateway")
	for _, l := range gw.Listeners {
		if l.Name == "web-app-front" {
			return l.Address
		}
	}
	return ""
}

func (n *Network) webSignIn(t testing.TB, front net.Listener, origin string, acct *pdsAccount) *webSession {
	t.Helper()
	webFront, pdsName := n.webFront(), ""
	pdsURL, err := url.Parse(acct.pds)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range n.Up.Network.Status.Network.Hosts {
		if h.Address == pdsURL.Host {
			pdsName = h.Name
		}
	}
	if webFront == "" || pdsName == "" {
		t.Fatalf("the gateway app reports no web-app-front listener (%q) or the network names no host for the PDS %s (%q)", webFront, pdsURL.Host, pdsName)
	}

	podDir := n.podServing(t, pdsURL.Port())
	leafCrt, err := n.read(filepath.Join(podDir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := n.read(filepath.Join(podDir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.X509KeyPair(leafCrt, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	startTLSForwarder(t, front, leaf, n.webFront)

	bundle, err := n.read(n.Up.Network.Status.Network.CaBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("the network CA bundle does not parse")
	}
	web := &webSession{t: t, origin: origin, client: &http.Client{Timeout: 30 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}

	work := t.TempDir()
	creds := filepath.Join(work, "owner.creds")
	if err := os.WriteFile(creds, []byte(fmt.Sprintf("export OWNER_ATPROTO_HANDLE=%s\nexport OWNER_ATPROTO_PASSWORD=%s\n", acct.identifier, acct.password)), 0o600); err != nil {
		t.Fatal(err)
	}
	cookies := filepath.Join(work, "cookies.json")
	denoBin, err := exec.LookPath("deno")
	if err != nil {
		home, _ := os.UserHomeDir()
		denoBin = filepath.Join(home, ".deno", "bin", "deno")
	}
	signCtx, cancel := context.WithTimeout(context.Background(), signInTimeout)
	defer cancel()
	signStarted := time.Now()
	cmd := exec.CommandContext(signCtx, denoBin, "run", "-A", "--no-lock", filepath.Join(n.h.Repo, "scripts", "oauth-signin.ts"),
		"--creds", creds, "--account", "OWNER", "--url", origin+"/", "--out", filepath.Join(n.Logs, "oauth-signin"),
		"--cookie-file", cookies, "--sign-in-as", acct.DID, "--chrome-arg=--ignore-certificate-errors",
		"--chrome-arg=--host-resolver-rules=MAP "+pdsName+":443 "+pdsURL.Host)
	signOut, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(signOut)), "\n")
		if len(lines) > 6 {
			lines = lines[len(lines)-6:]
		}
		t.Fatalf("scripts/oauth-signin.ts: %v\n%s", err, strings.Join(lines, "\n"))
	}
	t.Logf("signed the owner %s in through the web app and the PDS in %.1fs (screenshots %s)", acct.DID, time.Since(signStarted).Seconds(), filepath.Join(n.Logs, "oauth-signin"))
	raw, err := os.ReadFile(cookies)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(cookies)
	var jar []struct {
		Name, Value      string
		HTTPOnly, Secure bool
		SameSite         string
	}
	if err := json.Unmarshal(raw, &jar); err != nil {
		t.Fatal(err)
	}
	for _, c := range jar {
		if c.Name == webSessionCookie {
			web.token = c.Value
			if !c.HTTPOnly || !c.Secure || c.SameSite != "Strict" {
				t.Errorf("the session cookie attributes: httpOnly %t secure %t sameSite %q, want true true Strict", c.HTTPOnly, c.Secure, c.SameSite)
			}
		}
	}
	if web.token == "" {
		t.Fatalf("the sign-in left no %s cookie", webSessionCookie)
	}
	if code, did := web.did(); code != http.StatusOK || did != acct.DID {
		t.Fatalf("GET /session after the sign-in: %d, did %q, want 200 and %s", code, did, acct.DID)
	}
	if code, _, body := web.do(http.MethodPost, "/api/clusters/list", map[string]any{}); code != http.StatusOK || !strings.Contains(string(body), `"tenant"`) {
		t.Fatalf("the cluster list through the cookie: %d %.200s", code, body)
	}
	if code, _, _ := (&webSession{t: t, origin: origin, client: web.client}).do(http.MethodGet, "/session", nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /session with no cookie: %d, want 401", code)
	}
	return web
}
