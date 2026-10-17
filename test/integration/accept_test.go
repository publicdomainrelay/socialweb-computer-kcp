//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	acceptRequestTimeout = 5 * time.Minute
	acceptReplay         = 90 * time.Second
	bidderBack           = 2 * time.Minute
)

func (n *Network) bidderDir(i int) string {
	return filepath.Join(n.StateDir, "apps", "dev", fmt.Sprintf("%s-bidder-%d", n.Instance, i+1))
}

func (n *Network) runsDir() string { return filepath.Join(n.StateDir, "denokcp", "runs") }

func (n *Network) appRunLogs(app string) string {
	r := n.h.As(context.Background(), "sh", "-c", `for d in "$1"/pod-*; do
  grep -q -s -F -- ".default.app.$2.svc." "$d/service.txt" && cat "$d"/std*.txt 2>/dev/null
done; true`, "sh", n.runsDir(), app)
	return r.Stdout
}

func (n *Network) bidderProcess(i int) (int, string, error) {
	r := n.h.As(context.Background(), "sh", "-c", `for d in "$1"/pod-*; do
  [ -e "$d/done.json" ] && continue
  grep -q -s -x -F -- "bidder.default.app.$2.svc.kcp.local" "$d/service.txt" || continue
  grep -q -s -F '"bidder ready"' "$d/stdout.txt" "$d/stderr.txt" || continue
  pid=$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$d/state.json")
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && echo "$pid $d"
done; true`, "sh", n.runsDir(), n.bidderName(i))
	if r.Err != nil {
		return 0, "", r.Err
	}
	lines := strings.Fields(strings.TrimSpace(r.Stdout))
	if len(lines) != 2 {
		return 0, "", fmt.Errorf("want one live run of %s's bidder under %s, found %q", n.bidderName(i), n.runsDir(), r.Stdout)
	}
	pid, err := strconv.Atoi(lines[0])
	return pid, lines[1], err
}

func (n *Network) waitBidderBack(i, oldPID int, timeout time.Duration) (int, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		pid, dir, err := n.bidderProcess(i)
		if err == nil && pid != oldPID {
			return pid, dir, nil
		}
		if err := n.health(); err != nil {
			return 0, "", fmt.Errorf("waiting for the bidder to come back: %w", err)
		}
		if time.Now().After(deadline) {
			return 0, "", fmt.Errorf("no new bidder process inside %s (last: %v)", timeout, err)
		}
		time.Sleep(time.Second)
	}
}

func (n *Network) signal(sig string, pid int) error {
	r := n.h.As(context.Background(), "kill", "-"+sig, "--", "-"+strconv.Itoa(pid))
	if r.Err != nil {
		return fmt.Errorf("kill -%s %d: %v %s", sig, pid, r.Err, r.Stderr)
	}
	return nil
}

func (n *Network) grepCount(pattern string, globs ...string) int {
	r := n.h.As(context.Background(), append([]string{"sh", "-c", `cat "$@" 2>/dev/null | grep -c -a -F -e "$0"; true`, pattern}, globs...)...)
	c, _ := strconv.Atoi(strings.TrimSpace(r.Stdout))
	return c
}

func runLogs(dir string) []string {
	return []string{filepath.Join(dir, "stdout.txt"), filepath.Join(dir, "stderr.txt")}
}

func wonBids(ctx context.Context, dyn dynamic.Interface) ([]v1alpha1.Bid, []v1alpha1.Bid, error) {
	bids, err := bidderBids(ctx, dyn)
	if err != nil {
		return nil, nil, err
	}
	var won []v1alpha1.Bid
	for _, b := range bids {
		if b.Status.Phase == v1alpha1.BidPhaseWon {
			won = append(won, b)
		}
	}
	return bids, won, nil
}

func rewindStreams(ctx context.Context, dyn dynamic.Interface) (int, error) {
	u, err := dyn.Resource(marketBidderGVR).Get(ctx, bidderRootName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	streams, _, _ := unstructured.NestedSlice(u.Object, "status", "market", "streams")
	for i, s := range streams {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		m["cursor"] = int64(1)
		streams[i] = m
	}
	if len(streams) == 0 {
		return 0, fmt.Errorf("the bidder's root object has no status.market.streams")
	}
	if err := unstructured.SetNestedSlice(u.Object, streams, "status", "market", "streams"); err != nil {
		return 0, err
	}
	_, err = dyn.Resource(marketBidderGVR).UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return len(streams), err
}

func TestAcceptOfAnEndedContractStartsNoVM(t *testing.T) {
	n := StartNetwork(t, NetworkOptions{Bidders: 1, WithGateway: true, UpTimeout: 5 * time.Minute})
	ctx := context.Background()
	tn := n.Tenant(t, "tenant", tenantReady)
	dyn, err := n.bidderClient(0)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "it-accept-probe", Namespace: "default"},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{
				Name: "probe", Image: probeImage, Command: []string{"sh", "-c", "sleep 3600"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
			}},
		},
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := tn.Kube.CoreV1().ServiceAccounts("default").Get(ctx, "default", metav1.GetOptions{}); err == nil {
			break
		} else if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			t.Fatalf("the default ServiceAccount of default: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	pods := tn.Kube.CoreV1().Pods("default")
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(scaleOutTimeout)
	for {
		if err := tn.ClaimFailure(ctx); err != nil {
			t.Fatalf("the NodeClaim failed: %v\n%s", err, tn.Summary(ctx))
		}
		nodes, _ := tn.Nodes(ctx)
		if len(nodes) > 0 && nodeReady(nodes[0]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no node joined inside %s\n%s", scaleOutTimeout, tn.Summary(ctx))
		}
		time.Sleep(time.Second)
	}
	if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(scaleInTimeout)
	for {
		if err := tn.ClaimFailure(ctx); err != nil {
			t.Fatalf("the NodeClaim failed: %v\n%s", err, tn.Summary(ctx))
		}
		vms, err := n.BidderVMs(ctx)
		if err == nil && len(vms) == 0 && len(n.guestsLeft()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster did not scale in to zero inside %s\n%s", scaleInTimeout, tn.Summary(ctx))
		}
		time.Sleep(2 * time.Second)
	}
	bids, won, err := wonBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	var accepts []string
	for _, b := range won {
		if b.Status.Accept != nil && b.Status.Accept.Uri != "" {
			accepts = append(accepts, b.Status.Accept.Uri)
		}
	}
	if len(accepts) == 0 {
		t.Fatalf("no Won Bid names its accept:%s", describeBids(bids))
	}
	bidderLogs := n.appRunLogs(n.bidderName(0))
	for _, a := range accepts {
		if !strings.Contains(bidderLogs, a) {
			t.Fatalf("the bidder never logged the accept %s; the replay cannot deliver it again", a)
		}
	}
	t.Logf("scaled to zero: every guest released; %d Won Bid(s), accepts %s", len(won), strings.Join(accepts, " "))

	pid, _, err := n.bidderProcess(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.signal("STOP", pid); err != nil {
		t.Fatal(err)
	}
	rewound, err := rewindStreams(ctx, dyn)
	if err != nil {
		n.signal("KILL", pid)
		t.Fatalf("rewinding the bidder's stream cursors: %v", err)
	}
	if rm := n.h.As(ctx, "find", n.bidderDir(0), "-path", "*/contracts/tombstones/*.json", "-print", "-delete"); rm.Err != nil {
		n.signal("KILL", pid)
		t.Fatalf("removing the bidder's tombstones: %v", rm.Err)
	} else {
		t.Logf("removed the bidder's local contract records: %d file(s)", len(strings.Fields(rm.Stdout)))
	}
	if err := n.signal("KILL", pid); err != nil {
		t.Fatal(err)
	}
	t.Logf("bidder %d killed with %d stream cursor(s) rewound to 1: the restart replays the firehose and the accepts of the ended contracts", pid, rewound)

	newPID, dir, err := n.waitBidderBack(0, pid, bidderBack)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(acceptReplay)
	for {
		if vms, err := n.BidderVMs(ctx); err == nil && len(vms) > 0 {
			t.Fatalf("the replayed accept of an ended contract started a ComputeVM: %s", describeVM(vms[0]))
		}
		if left := n.guestsLeft(); len(left) > 0 {
			t.Fatalf("the replayed accept of an ended contract started a guest: %s", strings.Join(left, "; "))
		}
		if n.grepCount("bidder received accept for VM", runLogs(dir)...) > 0 {
			t.Fatalf("the restarted bidder %d took a replayed accept of an ended contract", newPID)
		}
		refused := 0
		for _, a := range accepts {
			if n.grepCount(a, runLogs(dir)...) > 0 && n.grepCount("accept_contract_ended", runLogs(dir)...) > 0 {
				refused++
			}
		}
		if refused == len(accepts) {
			t.Logf("the restarted bidder %d refused the %d replayed accept(s): a receipt in its own repo answers each", newPID, refused)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted bidder %d did not refuse the replayed accepts inside %s (%d of %d)", newPID, acceptReplay, refused, len(accepts))
		}
		time.Sleep(time.Second)
	}
	if vms, err := n.BidderVMs(ctx); err != nil || len(vms) > 0 {
		t.Fatalf("ComputeVMs after the replay: %d (%v)", len(vms), err)
	}
	after, wonAfter, err := wonBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(bids) || len(wonAfter) != len(won) {
		t.Fatalf("the Bids changed across the replay:%s", describeBids(after))
	}
}

func (n *Network) grepGlob(pattern, glob string) int {
	r := n.h.As(context.Background(), "sh", "-c", `cat $1 2>/dev/null | grep -c -a -F -e "$0"; true`, pattern, glob)
	c, _ := strconv.Atoi(strings.TrimSpace(r.Stdout))
	return c
}

func TestAcceptSurvivesABidderRestart(t *testing.T) {
	n := StartNetwork(t, NetworkOptions{Bidders: 1})
	ctx := context.Background()
	dyn, err := n.bidderClient(0)
	if err != nil {
		t.Fatal(err)
	}
	pid, _, err := n.bidderProcess(0)
	if err != nil {
		t.Fatal(err)
	}
	name := "rb" + randHex(3)
	requesterLogs := filepath.Join(n.runsDir(), "pod-*", "std*.txt")
	p := n.Start("run-request", acceptRequestTimeout, "run", "request", "--network", n.Instance, "--name", name, "--exec", "true",
		"--contract-timeout", "4m", "--network-timeout", "2m", "--state-dir", n.StateRoot)
	deadline := time.Now().Add(3 * time.Minute)
	var stoppedAt v1alpha1.BidPhase
	for stoppedAt == "" {
		select {
		case <-p.done:
			r := p.Wait()
			t.Fatalf("run request ended before the bidder bid: %v\n%s", r.Err, r.Tail(15))
		default:
		}
		bids, err := bidderBids(ctx, dyn)
		if err == nil {
			for _, b := range bids {
				if b.Status.Phase == v1alpha1.BidPhasePublished || b.Status.Phase == v1alpha1.BidPhaseWon {
					if err := n.signal("STOP", pid); err != nil {
						t.Fatal(err)
					}
					stoppedAt = b.Status.Phase
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bidder published no Bid inside 3m")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stoppedAt != v1alpha1.BidPhasePublished {
		n.signal("CONT", pid)
		t.Fatalf("the accept reached the bidder before it was stopped (Bid %s); the restart did not fall between the bid and the accept", stoppedAt)
	}
	t.Logf("bidder %d stopped right after its Bid was Published", pid)
	deadline = time.Now().Add(45 * time.Second)
	for n.grepGlob("submitAccept_retry", requesterLogs) == 0 {
		if time.Now().After(deadline) {
			n.signal("KILL", pid)
			t.Fatalf("the requester did not re-send submitAccept inside 45s while the bidder was stopped")
		}
		time.Sleep(time.Second)
	}
	if err := n.signal("KILL", pid); err != nil {
		t.Fatal(err)
	}
	t.Logf("the requester re-sends submitAccept; bidder %d killed", pid)
	newPID, _, err := n.waitBidderBack(0, pid, bidderBack)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bidder back as %d", newPID)
	r := p.Wait()
	if r.Err != nil {
		t.Fatalf("run request after the bidder restart: %v\n%s", r.Err, r.Tail(15))
	}
	var s requestSummary
	if err := lastJSON(r.Stdout, &s); err != nil {
		t.Fatalf("run request summary: %v", err)
	}
	if s.Phase != "Released" || s.Contract == nil || s.Contract.ReceiptUri == "" || s.Contract.VMAddress == "" {
		t.Fatalf("the contract did not complete across the bidder restart: phase %s: %s", s.Phase, s.Message)
	}
	kept := filepath.Join(n.StateRoot, "requests", name, "logs", "*")
	retries := n.grepGlob("submitAccept_retry", kept)
	t.Logf("contract completed across the bidder restart: receipt %s, guest %s, %d submitAccept re-send(s)", s.Contract.ReceiptUri, s.Contract.VMAddress, retries)
	_, won, err := wonBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(won) != 1 {
		t.Fatalf("%d Won Bids; want one", len(won))
	}
	if err := n.WaitNoGuests(time.Minute); err != nil {
		t.Fatal(err)
	}
}

const acceptCopies = 10

type acceptCopiesLine struct {
	Copies   int      `json:"copies"`
	Receipts []string `json:"receipts"`
}

func TestOneAcceptMakesOneComputeVM(t *testing.T) {
	n := StartNetwork(t, NetworkOptions{Bidders: 1})
	ctx := context.Background()
	dyn, err := n.bidderClient(0)
	if err != nil {
		t.Fatal(err)
	}
	_, bidderRun, err := n.bidderProcess(0)
	if err != nil {
		t.Fatal(err)
	}
	name := "ao" + randHex(3)
	p := n.Start("run-request", acceptRequestTimeout, "run", "request", "--network", n.Instance, "--name", name, "--exec", "true",
		"--contract-timeout", "4m", "--network-timeout", "2m", "--state-dir", n.StateRoot, "--", "--accept-copies", strconv.Itoa(acceptCopies))
	seen := map[string]string{}
	receipts := map[string]bool{}
	observe := func() {
		vms, err := n.BidderVMs(ctx)
		if err != nil {
			return
		}
		for _, vm := range vms {
			accept := ""
			if c := vm.Spec.Contract; c != nil {
				accept = c.AcceptUri
				if c.ReceiptUri != "" {
					receipts[c.ReceiptUri] = true
				}
			}
			if accept != "" || seen[vm.Name] == "" {
				seen[vm.Name] = accept
			}
		}
		if len(seen) > 1 {
			var all []string
			for _, vm := range vms {
				all = append(all, describeVM(vm))
			}
			p.Kill(fmt.Errorf("one accept made %d ComputeVMs", len(seen)))
			t.Fatalf("one accept made %d ComputeVMs: %v (live: %s)", len(seen), seen, strings.Join(all, "; "))
		}
	}
	for running := true; running; {
		select {
		case <-p.done:
			running = false
		default:
			observe()
			time.Sleep(200 * time.Millisecond)
		}
	}
	r := p.Wait()
	if r.Err != nil {
		t.Fatalf("run request: %v\n%s", r.Err, r.Tail(15))
	}
	var s requestSummary
	if err := lastJSON(r.Stdout, &s); err != nil {
		t.Fatalf("run request summary: %v", err)
	}
	if s.Phase != "Released" || s.Contract == nil || s.Contract.ReceiptUri == "" || s.Contract.VMAddress == "" {
		t.Fatalf("the contract did not complete: phase %s: %s", s.Phase, s.Message)
	}
	if len(seen) != 1 {
		t.Fatalf("want one ComputeVM for the accept, saw %d: %v", len(seen), seen)
	}
	if len(receipts) > 1 || (len(receipts) == 1 && !receipts[s.Contract.ReceiptUri]) {
		t.Fatalf("ComputeVMs carry receipts %v; the requester holds %s", receipts, s.Contract.ReceiptUri)
	}
	kept := filepath.Join(n.StateRoot, "requests", name, "logs", "*")
	out := n.h.As(ctx, "sh", "-c", `cat $1 2>/dev/null | grep -a -F -e "$0"; true`, "submitAccept_copies", kept)
	var copies acceptCopiesLine
	for _, line := range strings.Split(strings.TrimSpace(out.Stdout), "\n") {
		if i := strings.Index(line, "{"); i >= 0 && json.Unmarshal([]byte(line[i:]), &copies) == nil && copies.Copies > 0 {
			break
		}
	}
	if copies.Copies != acceptCopies {
		t.Fatalf("the requester logged no submitAccept_copies line for %d copies (got %+v)", acceptCopies, copies)
	}
	if len(copies.Receipts) != 1 || copies.Receipts[0] != s.Contract.ReceiptUri {
		t.Fatalf("%d concurrent copies of one accept were answered with receipts %v; want only %s", acceptCopies, copies.Receipts, s.Contract.ReceiptUri)
	}
	_, won, err := wonBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(won) != 1 || won[0].Status.ClaimedBy == "" {
		t.Fatalf("want one Won Bid with a claim:%s", describeBids(won))
	}
	dups := n.grepCount("accept_duplicate_delivery", runLogs(bidderRun)...)
	resends := n.grepGlob("submitAccept_retry", kept)
	t.Logf("one ComputeVM %v and one receipt %s for %d concurrent copies of the accept; %d answered from the accept in flight, %d re-send(s), Bid claimed by %s",
		seen, s.Contract.ReceiptUri, acceptCopies, dups, resends, won[0].Status.ClaimedBy)
	if err := n.WaitNoGuests(time.Minute); err != nil {
		t.Fatal(err)
	}
}
