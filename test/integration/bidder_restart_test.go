//go:build integration

package integration

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

const (
	restartHeldMarker  = "IT-RESTART-HELD"
	restartRequestWait = 6 * time.Minute
	restartBackWait    = 2 * time.Minute
)

var issuerURLPattern = regexp.MustCompile(`"issuerUrl":"([^"]+)"`)

type vmIdentity struct {
	Units        map[string]string
	ComputeVM    string
	Phase        v1alpha1.ComputeVMPhase
	Pid          int64
	StartTime    string
	Address      string
	OnNetworkURI string
	ReceiptURI   string
}

func (v vmIdentity) String() string {
	units := make([]string, 0, len(v.Units))
	for u, id := range v.Units {
		units = append(units, u+"="+id)
	}
	sort.Strings(units)
	return fmt.Sprintf("ComputeVM %s %s pid %d start %s address %s onNetwork %s receipt %s units [%s]",
		v.ComputeVM, v.Phase, v.Pid, v.StartTime, v.Address, v.OnNetworkURI, v.ReceiptURI, strings.Join(units, " "))
}

func (n *Network) unitIdentities(i int) (map[string]string, error) {
	r := n.h.As(context.Background(), "sh", "-c", `for u in $(systemctl --user list-units --all --plain --no-legend --no-pager "$1*" | cut -d' ' -f1); do
  inv=$(systemctl --user show -p InvocationID --value "$u")
  pid=$(systemctl --user show -p MainPID --value "$u")
  state=$(systemctl --user show -p ActiveState --value "$u")
  ticks=""
  [ "$pid" != 0 ] && ticks=$(cut -d' ' -f22 "/proc/$pid/stat" 2>/dev/null)
  echo "$u $state $inv $pid $ticks"
done; true`, "sh", vmruntime.UnitPrefix(n.Handles[i]))
	if r.Err != nil {
		return nil, r.Err
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		out[f[0]] = strings.Join(f[1:], "/")
	}
	return out, nil
}

func (n *Network) vmIdentityOf(ctx context.Context, i int) (vmIdentity, error) {
	vms, err := n.BidderVMsOf(ctx, i)
	if err != nil {
		return vmIdentity{}, err
	}
	if len(vms) != 1 {
		return vmIdentity{}, fmt.Errorf("want one ComputeVM on %s, found %d", n.bidderName(i), len(vms))
	}
	vm := vms[0]
	id := vmIdentity{ComputeVM: vm.Name, Phase: vm.Status.Phase}
	if rt := vm.Status.Runtime; rt != nil {
		id.Pid, id.StartTime = rt.Pid, rt.StartTime
	}
	if m := vm.Status.Market; m != nil {
		id.Address, id.OnNetworkURI = m.Address, m.OnNetworkUri
	}
	if c := vm.Spec.Contract; c != nil {
		id.ReceiptURI = c.ReceiptUri
	}
	if id.Units, err = n.unitIdentities(i); err != nil {
		return vmIdentity{}, err
	}
	return id, nil
}

func (n *Network) waitIdentity(ctx context.Context, i int, want vmIdentity, timeout time.Duration) (vmIdentity, error) {
	deadline := time.Now().Add(timeout)
	for {
		got, err := n.vmIdentityOf(ctx, i)
		if err == nil && got.String() == want.String() {
			return got, nil
		}
		if herr := n.health(); herr != nil {
			return got, herr
		}
		if time.Now().After(deadline) {
			return got, fmt.Errorf("after %s the VM is not the one rented (%v):\n  want %s\n  got  %s", timeout, err, want, got)
		}
		time.Sleep(time.Second)
	}
}

func (n *Network) issuerURLs(i int) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range issuerURLPattern.FindAllStringSubmatch(n.appRunLogs(n.bidderName(i)), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func TestARentedVMSurvivesABidderKillAndRestart(t *testing.T) {
	t.Parallel()
	n := StartNetwork(t, NetworkOptions{Bidders: 1})
	ctx := context.Background()
	name := "br" + randHex(3)
	p := n.Start("run-request", restartRequestWait, "run", "request", "--network", n.Instance, "--name", name, "--hold",
		"--exec", "echo "+restartHeldMarker+"; read -r _", "--contract-timeout", "4m", "--network-timeout", "2m", "--state-dir", n.StateRoot)
	if err := p.WaitOutput(restartHeldMarker); err != nil {
		t.Fatalf("the guest did not run the command: %v", err)
	}
	before, err := n.vmIdentityOf(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if before.Phase != v1alpha1.ComputeVMPhaseRunning || before.Pid == 0 || before.Address == "" || before.ReceiptURI == "" || len(before.Units) == 0 {
		t.Fatalf("the rented VM is not Running with an identity: %s", before)
	}
	issuers := n.issuerURLs(0)
	if len(issuers) != 1 {
		t.Fatalf("want one issuer URL in the bidder's logs before the restarts, found %v", issuers)
	}
	t.Logf("rented: %s; issuer %s", before, issuers[0])

	cases := []struct {
		name string
		do   func() error
	}{
		{"kill -9 of the bidder", func() error {
			pid, _, err := n.bidderProcess(0)
			if err != nil {
				return err
			}
			if err := n.signal("KILL", pid); err != nil {
				return err
			}
			_, _, err = n.waitBidderBack(0, pid, restartBackWait)
			return err
		}},
		{"dev restart of the bidder component", func() error {
			r := n.Run("restart-bidder", restartBackWait, "dev", "restart", "--instance", n.Instance, "--state-dir", n.StateRoot, "bidder-1/bidder")
			return r.Err
		}},
	}
	for _, c := range cases {
		started := time.Now()
		if err := c.do(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		after, err := n.waitIdentity(ctx, 0, before, restartBackWait)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := n.issuerURLs(0); len(got) != 1 || got[0] != issuers[0] {
			t.Fatalf("%s: the bidder's issuer URL moved: before %s, logs now name %v", c.name, issuers[0], got)
		}
		t.Logf("%s: bidder back and the VM is the same %.1fs later: %s", c.name, time.Since(started).Seconds(), after)
	}

	if err := p.Input("done\n"); err != nil {
		t.Fatalf("ending the guest command: %v", err)
	}
	r := p.Wait()
	if r.Err != nil {
		t.Fatalf("run request: %v\n%s", r.Err, r.Tail(15))
	}
	var s requestSummary
	if err := lastJSON(r.Stdout, &s); err != nil {
		t.Fatalf("run request summary: %v", err)
	}
	if s.Phase != "Held" || s.Contract == nil || s.Contract.ReceiptUri != before.ReceiptURI {
		t.Fatalf("the contract is not held with the receipt rented before the restarts: phase %s: %s", s.Phase, s.Message)
	}
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
	t.Logf("released after three bidder restarts: vm.delete 200; no ComputeVM, VM unit or VM work dir left")
}
