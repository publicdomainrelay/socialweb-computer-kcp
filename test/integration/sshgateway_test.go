//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const sshGatewayTestName = "sshgw"

type sshGatewayReport struct {
	SSH        string                          `json:"ssh"`
	Listeners  []v1alpha1.ApplicationListener  `json:"listeners"`
	Components []v1alpha1.ApplicationComponent `json:"components"`
}

func TestSshGatewayExposesItsServerAndReusesTheGuestAcrossRestart(t *testing.T) {
	dp := dumbpipePath(t)
	n := StartNetwork(t, NetworkOptions{Bidders: 1, Dumbpipe: dp, UpTimeout: 10 * time.Minute})
	ctx := context.Background()

	did, handle, password, service := sshGatewayOwnerAccount(t, n)
	ownerFile := writeOwnerSessions(t, n, did, handle, password, service)
	sshAddr, httpAddr := startSshGateway(t, n, ownerFile)

	rightSigner, rightKey := sshTestKey(t)
	wrongSigner, _ := sshTestKey(t)
	registerSSHKey(t, httpAddr, did, handle, password, rightKey)

	if client, err := dialSSH(sshAddr, did, wrongSigner); err == nil {
		client.Close()
		t.Fatalf("the ssh door accepted a key that is not associated with %s", did)
	}

	sessionName := "it-" + randHex(4)
	first := runSSHCommand(t, sshAddr, did, rightSigner, sessionName, "hostname")
	if first == "" {
		t.Fatalf("the guest answered with an empty hostname")
	}

	vms, err := n.BidderVMs(ctx)
	if err != nil {
		t.Fatalf("listing the bidder's ComputeVMs: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("want exactly one rented guest after the first command, got %d: %s", len(vms), describeVMs(vms))
	}

	if r := n.Run("restart-ssh-server", 4*time.Minute, "dev", "restart", sshGatewayTestName+"/ssh-server",
		"--instance", n.Instance, "--state-dir", n.StateRoot); r.Err != nil {
		t.Fatalf("restarting the ssh server: %v\n%s", r.Err, r.Tail(10))
	}

	second := runSSHCommand(t, sshAddr, did, rightSigner, sessionName, "hostname")
	if second != first {
		t.Fatalf("the second session reached another guest: hostname %q, want %q", second, first)
	}
	vms, err = n.BidderVMs(ctx)
	if err != nil {
		t.Fatalf("listing the bidder's ComputeVMs after the restart: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("the ssh server restart re-rented a guest: want 1 ComputeVM, got %d: %s", len(vms), describeVMs(vms))
	}
}

func describeVMs(vms []v1alpha1.ComputeVM) string {
	out := make([]string, 0, len(vms))
	for _, vm := range vms {
		out = append(out, describeVM(vm))
	}
	return strings.Join(out, "; ")
}

func sshGatewayOwnerAccount(t testing.TB, n *Network) (string, string, string, string) {
	t.Helper()
	if n.Up.Network == nil || n.Up.Network.Status.Network == nil {
		t.Fatalf("dev up reported no network value")
	}
	alice := ""
	for _, host := range n.Up.Network.Status.Network.Hosts {
		if host.Name == "pds.default.alice.svc.kcp.local" {
			alice = host.Address
		}
	}
	if alice == "" {
		t.Fatalf("the network value names no alice PDS host: %+v", n.Up.Network.Status.Network.Hosts)
	}
	service := "http://" + alice
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL:    service,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Timeout:    30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the alice PDS client: %v", err)
	}
	handle := "ssh-" + randHex(4) + ".alice.test"
	password := randHex(16)
	session, err := pds.CreateAccount(context.Background(), pdsclient.CreateAccountRequest{
		Identifier: handle,
		Password:   password,
		Email:      handle + "@alice.test",
	})
	if err != nil {
		t.Fatalf("creating the owner account %s on %s: %v", handle, service, err)
	}
	if _, err := syntax.ParseDID(session.DID.String()); err != nil {
		t.Fatalf("the owner account reported an invalid DID: %v", err)
	}
	t.Logf("owner account %s on %s is %s", handle, service, session.DID)
	return session.DID.String(), handle, password, service
}

func writeOwnerSessions(t testing.TB, n *Network, did, handle, password, service string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]sshtransport.OwnerSession{
		did: {Service: service, Identifier: handle, Password: password},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(n.Dir, "owner-sessions.json")
	if err := n.h.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("writing the owner sessions file: %v", err)
	}
	return path
}

func startSshGateway(t testing.TB, n *Network, ownerFile string) (string, string) {
	t.Helper()
	r := n.Run("gateway-ssh", 6*time.Minute, "run", "gateway", "ssh", "--network", n.Instance,
		"--host-backend", "proc", "--name", sshGatewayTestName, "--state-dir", n.StateRoot,
		"--network-timeout", "5m", "--", "--test-owner-sessions-file", ownerFile)
	if r.Err != nil {
		t.Fatalf("run gateway ssh: %v\n%s", r.Err, r.Tail(15))
	}
	var report sshGatewayReport
	if err := json.Unmarshal([]byte(r.Stdout), &report); err != nil {
		t.Fatalf("run gateway ssh printed no report: %v\n%s", err, r.Stdout)
	}
	sshURL := report.SSH
	httpAddr := ""
	for _, listener := range report.Listeners {
		if strings.HasSuffix(listener.Component, "-ssh-local") && listener.Address != "" {
			if sshURL == "" {
				sshURL = listener.Address
			}
			continue
		}
		if strings.Contains(listener.Component, "svc-sshserver") && strings.HasSuffix(listener.Component, "-local") && listener.Address != "" {
			httpAddr = listener.Address
		}
	}
	sshAddr := hostPortOf(sshURL)
	if sshAddr == "" {
		t.Fatalf("run gateway ssh reported no local ssh exposure: %s", r.Stdout)
	}
	if httpAddr == "" {
		t.Fatalf("run gateway ssh reported no local sshserver http exposure: %s", r.Stdout)
	}
	t.Logf("ssh door at %s, keys API at http://%s", sshAddr, httpAddr)
	return sshAddr, "http://" + httpAddr
}

func hostPortOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw
	}
	return ""
}

func sshTestKey(t testing.TB) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func registerSSHKey(t testing.TB, httpAddr, did, identifier, password, publicKey string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"publicKey": publicKey, "name": "integration"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(httpAddr, "/")+"/keys", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(identifier, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("registering the ssh key through %s/keys: %v", httpAddr, err)
	}
	defer resp.Body.Close()
	var out struct {
		Rkey  string `json:"rkey"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Rkey == "" {
		t.Fatalf("registering the ssh key for %s: HTTP %d %s", did, resp.StatusCode, out.Error)
	}
	t.Logf("registered ssh key %s for %s", out.Rkey, did)
}

func dialSSH(addr, user string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	})
}

func runSSHCommand(t testing.TB, addr, user string, signer ssh.Signer, sessionName, command string) string {
	t.Helper()
	client, err := dialSSH(addr, user, signer)
	if err != nil {
		t.Fatalf("ssh as %s: %v", user, err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("opening the ssh session: %v", err)
	}
	defer session.Close()
	if err := session.Setenv("LC_SESSION", sessionName); err != nil {
		t.Fatalf("sending LC_SESSION: %v", err)
	}
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.Run(command); err != nil {
		t.Fatalf("running %q in the guest: %v\nstderr: %s", command, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
