package requester

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

type fixedServiceAuth struct{}

func (fixedServiceAuth) ServiceAuth(context.Context, string, string) (string, error) {
	return "service-bearer", nil
}

func TestSessionAgentRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		sub     string
		ok      bool
	}{
		{"atirc agent run hat-ssh-test", "hat-ssh-test", true},
		{"  atirc   agent run  bot.hat.dplc.rip ", "bot.hat.dplc.rip", true},
		{"atirc agent run", "", false},
		{"atirc agent run a b", "", false},
		{"atirc pub hello", "", false},
		{"sh -c 'atirc agent run x'", "", false},
	} {
		sub, ok := sessionAgentRun(tc.command)
		if sub != tc.sub || ok != tc.ok {
			t.Errorf("sessionAgentRun(%q) = %q %v, want %q %v", tc.command, sub, ok, tc.sub, tc.ok)
		}
	}
}

func TestAgentSubhandle(t *testing.T) {
	t.Parallel()
	const handle = "hat.dplc.rip"
	for _, tc := range []struct {
		sub  string
		want string
	}{
		{"hat-ssh-test", "hat-ssh-test.hat.dplc.rip"},
		{"Hat-SSH-Test", "hat-ssh-test.hat.dplc.rip"},
		{"bot.hat.dplc.rip", "bot.hat.dplc.rip"},
		{"hat.dplc.rip", "hat.dplc.rip"},
		{"a.b", "a.b.hat.dplc.rip"},
		{"xhat.dplc.rip", "xhat.dplc.rip.hat.dplc.rip"},
		{"bad_name", ""},
		{"-lead", ""},
		{"trail-", ""},
		{"a..b", ""},
		{".hat.dplc.rip", ""},
		{"did:plc:abc", ""},
		{"*.x", ""},
		{"x;rm", ""},
		{"café", ""},
		{strings.Repeat("a", 64), ""},
		{strings.Repeat("abcdefghi.", 25) + "x", ""},
	} {
		got, err := agentSubhandle(tc.sub, handle)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("agentSubhandle(%q) = %q %v, want %q", tc.sub, got, err, tc.want)
		}
	}
	if _, err := agentSubhandle("x", ""); err == nil {
		t.Error("agentSubhandle with no handle answered no error")
	}
}

func fakeLeasedToken(lease json.RawMessage) string {
	claims, _ := json.Marshal(map[string]any{"leases": []json.RawMessage{lease}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func TestMintSessionAtircSubhandle(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var minted []atircMintInput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/"+AtircMintNSID || r.Header.Get("Authorization") != "Bearer service-bearer" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var raw struct {
			Lease json.RawMessage `json:"lease"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		var in atircMintInput
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		minted = append(minted, in)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fakeLeasedToken(raw.Lease)})
	}))
	defer srv.Close()
	q := &Requester{
		cfg:         Config{AtircURL: srv.URL},
		deps:        Deps{HTTP: srv.Client(), Now: time.Now},
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		handle:      "hat.dplc.rip",
		serviceAuth: fixedServiceAuth{},
	}
	ok := true
	state := &v1alpha1.NodeClaimContract{
		RFPURI:    "at://did:plc:j4yw65srpdumubaadp7ijmy5/dev.socialweb.compute.rfp/3mz7ezacixc2r",
		Receipt:   &commonv1.StrongRef{URI: "at://did:plc:bidder/dev.socialweb.compute.receipt/3receipt", CID: "bafyreceipt"},
		ReceiptOk: &ok,
	}
	agent, err := q.mintSessionAtirc(context.Background(), state, "hat-ssh-test.hat.dplc.rip")
	if err != nil {
		t.Fatalf("agent mint: %v", err)
	}
	child, err := q.mintSessionAtirc(context.Background(), state, "")
	if err != nil {
		t.Fatalf("child mint: %v", err)
	}
	if agent.subhandle != "hat-ssh-test.hat.dplc.rip" || child.subhandle != "ssh-3mz7ezacixc2r.hat.dplc.rip" {
		t.Fatalf("subhandles %q %q", agent.subhandle, child.subhandle)
	}
	if env := agent.env(); len(env) != 1 || env[sessionAtircTokenEnv] != agent.token {
		t.Fatalf("the agent token is not passed in %s alone: %v", sessionAtircTokenEnv, env)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(minted) != 2 || minted[0].Subhandle != "hat-ssh-test.hat.dplc.rip" {
		t.Fatalf("mint calls %+v", minted)
	}
	for _, in := range minted {
		if in.Lease == nil || in.Lease.Receipt.URI != state.Receipt.URI || !strings.HasPrefix(in.Lease.Termination, "at://did:plc:bidder/"+market.ComputeEventsVMTerminatedNSID+"/") {
			t.Fatalf("mint %s carries lease %+v", in.Subhandle, in.Lease)
		}
	}
	if cmd := atircCommand(sessionAgentCommand); !strings.HasSuffix(cmd, "; atirc agent run") || strings.Contains(cmd, agent.token) {
		t.Fatalf("agent command %q", cmd)
	}
}

func TestOrphanSweepSkipsWithoutVault(t *testing.T) {
	t.Parallel()
	called := false
	q := &Requester{
		deps: Deps{VaultWrite: func(context.Context, commonv1.SecretRef) (vault.KV, error) {
			called = true
			return nil, nil
		}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	q.startOrphanRevokes()
	q.startOrphanRevokes()
	q.wg.Wait()
	if called {
		t.Fatal("the orphan sweep read a vault the requester does not name")
	}
}
