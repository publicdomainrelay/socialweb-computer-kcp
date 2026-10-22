//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	secretPath     = "/root/secrets/it/value"
	readyMarker    = "IT-SECRET-READ"
	requestTimeout = 5 * time.Minute
	releaseTimeout = 3 * time.Minute
)

type requestSummary struct {
	Name             string `json:"name"`
	Phase            string `json:"phase"`
	Message          string `json:"message"`
	SecretsUrl       string `json:"secretsUrl"`
	SecretsFetchedAt string `json:"secretsFetchedAt"`
	KcpRunning       bool   `json:"kcpRunning"`
	Contract         *struct {
		Phase         string   `json:"phase"`
		WinnerDid     string   `json:"winnerDid"`
		Bids          int      `json:"bids"`
		ReceiptUri    string   `json:"receiptUri"`
		VMAddress     string   `json:"vmAddress"`
		HostKeys      []string `json:"hostKeys"`
		ReleaseOK     *bool    `json:"releaseOk"`
		ReleaseStatus int      `json:"releaseStatus"`
	} `json:"contract"`
}

func lastJSON(stdout string, into any) error {
	i := strings.LastIndex(stdout, "\n{\n")
	if i < 0 {
		if !strings.HasPrefix(stdout, "{\n") {
			return fmt.Errorf("no summary in %q", stdout)
		}
		i = -1
	}
	return json.Unmarshal([]byte(stdout[i+1:]), into)
}

func encodings(value string) []string {
	out := []string{value}
	for i := 0; i < 3; i++ {
		s := value[i:]
		s = s[:len(s)/3*3]
		out = append(out, base64.StdEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s)))
	}
	return out
}

func TestRequestSecretsReachTheGuestThroughTheRFPFlow(t *testing.T) {
	t.Parallel()
	n := StartNetwork(t, NetworkOptions{Bidders: 1})
	started := time.Now()
	secret := "it-secret-" + randHex(16)
	name := "rq" + randHex(3)
	bundle, _ := json.Marshal([]map[string]string{{"path": secretPath, "value": secret}})
	secretsFile := filepath.Join(n.Dir, "secrets.json")
	if err := h.WriteFile(secretsFile, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	exec := fmt.Sprintf("systemctl start setup-secrets.service; cat %s; echo; echo %s; read -r _", secretPath, readyMarker)
	p := n.Start("run-request", requestTimeout, "run", "request", "--network", n.Instance, "--name", name,
		"--secrets", secretsFile, "--hold", "--exec", exec, "--contract-timeout", "2m", "--network-timeout", "2m",
		"--state-dir", n.StateRoot)
	if err := p.WaitOutput(readyMarker); err != nil {
		t.Fatalf("the guest did not run the command: %v", err)
	}
	t.Logf("the guest ran the command %.1fs after run request started", time.Since(started).Seconds())
	got := strings.TrimSpace(strings.SplitN(p.Stdout(), readyMarker, 2)[0])
	if got != secret {
		p.Kill(fmt.Errorf("wrong secret in the guest"))
		p.Wait()
		t.Fatalf("the guest holds %q at %s, want the secret from --secrets", got, secretPath)
	}

	patterns := encodings(secret)
	scanned := []string{n.StateDir, filepath.Join(n.StateRoot, "requests", name)}
	var leaks []string
	files, err := n.FilesContaining(scanned, patterns, "vms")
	if err != nil {
		t.Errorf("scanning the state: %v", err)
	}
	for _, f := range files {
		leaks = append(leaks, "file "+f)
	}
	procs, err := n.processesMentioning(patterns...)
	if err != nil {
		t.Errorf("scanning the processes: %v", err)
	}
	for _, pr := range procs {
		leaks = append(leaks, "process argument or environment "+pr)
	}
	if c, err := n.JournalContains(patterns, started); err != nil {
		t.Errorf("reading the journal: %v", err)
	} else if c > 0 {
		leaks = append(leaks, fmt.Sprintf("journal of %s: %d line(s)", h.Account, c))
	}
	for _, f := range n.LogsContaining(patterns) {
		if f != p.base+".stdout" {
			leaks = append(leaks, "log "+f)
		}
	}
	stores, _ := n.FilesNamed(n.StateDir, "repos.db", "kine.db")
	t.Logf("scanned %s, the processes and the journal while the guest held the secret: %d kine stores (the network's one kcp), %d record stores (the PDSes and the requester's own repo)",
		strings.Join(scanned, " and "), countSuffix(stores, "kine.db"), countSuffix(stores, "repos.db"))
	if countSuffix(stores, "kine.db") < 1 || countSuffix(stores, "repos.db") < 2 {
		t.Errorf("the scan did not cover the network's kcp and its record stores (the PDSes and the requester's own repo): %v", stores)
	}
	if len(leaks) > 0 {
		p.Kill(fmt.Errorf("the secret leaked"))
		p.Wait()
		t.Fatalf("the secret value is outside the guest while the contract is held:\n  %s", strings.Join(leaks, "\n  "))
	}
	if err := p.Input("done\n"); err != nil {
		t.Fatalf("ending the guest command: %v", err)
	}
	r := p.Wait()
	if r.Err != nil {
		t.Fatalf("run request: %v", r.Err)
	}
	var s requestSummary
	if err := lastJSON(r.Stdout, &s); err != nil {
		t.Fatalf("run request summary: %v", err)
	}
	if s.Phase != "Held" || s.Contract == nil || s.Contract.ReceiptUri == "" || s.Contract.WinnerDid == "" {
		t.Fatalf("the contract is not held with a receipt: phase %s: %s", s.Phase, s.Message)
	}
	if s.SecretsFetchedAt == "" {
		t.Errorf("the contract status does not report the guest's secrets fetch")
	}
	if len(s.Contract.HostKeys) == 0 {
		t.Errorf("the guest reported no ssh host keys")
	}
	if !strings.Contains(r.Stderr, "StrictHostKeyChecking=yes") {
		t.Errorf("run request did not reach the guest over ssh with StrictHostKeyChecking=yes")
	}
	t.Logf("contract held: winner %s, %d bid(s), receipt %s, secrets fetched %s, host keys %d", s.Contract.WinnerDid,
		s.Contract.Bids, s.Contract.ReceiptUri, s.SecretsFetchedAt, len(s.Contract.HostKeys))

	rel := n.Run("run-request-release", releaseTimeout, "run", "request", "release", name, "--contract-timeout", "2m",
		"--network-timeout", "2m", "--state-dir", n.StateRoot)
	if rel.Err != nil {
		t.Fatalf("run request release: %v", rel.Err)
	}
	var rs requestSummary
	if err := lastJSON(rel.Stdout, &rs); err != nil {
		t.Fatalf("run request release summary: %v", err)
	}
	if rs.Phase != "Released" || rs.Contract == nil || rs.Contract.ReleaseStatus != 200 {
		t.Fatalf("the release did not get vm.delete 200: phase %s: %s", rs.Phase, rs.Message)
	}
	if err := n.WaitNoGuests(time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("released: vm.delete 200; no ComputeVM, VM unit or VM work dir left")

	if c, err := n.JournalContains(patterns, started); err != nil {
		t.Errorf("reading the journal: %v", err)
	} else if c > 0 {
		leaks = append(leaks, fmt.Sprintf("journal of %s: %d line(s)", h.Account, c))
	}
	for _, f := range n.LogsContaining(patterns) {
		if f != p.base+".stdout" {
			leaks = append(leaks, "log "+f)
		}
	}
	files, err = n.FilesContaining(scanned, patterns, "vms")
	if err != nil {
		t.Errorf("scanning the state after the release: %v", err)
	}
	for _, f := range files {
		leaks = append(leaks, "file after the release "+f)
	}
	for _, l := range leaks {
		t.Errorf("the secret value is outside the guest: %s", l)
	}
}

func countSuffix(list []string, suffix string) int {
	c := 0
	for _, s := range list {
		if strings.HasSuffix(s, suffix) {
			c++
		}
	}
	return c
}
