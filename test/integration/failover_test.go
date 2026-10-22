//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	failoverVMReadySec     = 90
	failoverRequestTimeout = 5 * time.Minute
	failoverAcceptWait     = 4 * time.Minute
	failoverStopWait       = 2 * time.Minute
	failoverEndWait        = (failoverVMReadySec + 180) * time.Second
	failoverReplaceWait    = 6 * time.Minute
	failoverBackWait       = 3 * time.Minute
	failoverOrphanWait     = 3 * time.Minute
	failoverVMWait         = 30 * time.Second
	failoverPod            = "it-failover"
)

func claimWinner(c v1alpha1.NodeClaim) string {
	if c.Status.Contract.Winner == nil {
		return ""
	}
	return c.Status.Contract.Winner.DID
}

func claimEnded(c v1alpha1.NodeClaim) bool {
	switch c.Status.Phase {
	case v1alpha1.NodeClaimFailed, v1alpha1.NodeClaimLost, v1alpha1.NodeClaimReleased:
		return true
	}
	return false
}

func (n *Network) bothBiddersWin(t *testing.T, dids [2]string) {
	t.Helper()
	var procs [2]*Proc
	for i := range procs {
		name := "fo" + randHex(3)
		procs[i] = n.Start(fmt.Sprintf("run-request-%d", i+1), failoverRequestTimeout, "run", "request", "--network", n.Instance, "--name", name,
			"--exec", "true", "--contract-timeout", "4m", "--network-timeout", "2m", "--state-dir", n.StateRoot,
			"--", "--deny-bidder-dids", dids[1-i])
	}
	for i, p := range procs {
		r := p.Wait()
		if r.Err != nil {
			t.Fatalf("run request denying %s: %v\n%s", dids[1-i], r.Err, r.Tail(15))
		}
		var s requestSummary
		if err := lastJSON(r.Stdout, &s); err != nil {
			t.Fatalf("run request summary: %v", err)
		}
		if s.Phase != "Released" || s.Contract == nil || s.Contract.ReceiptUri == "" {
			t.Fatalf("the contract of request %d did not complete: phase %s: %s", i+1, s.Phase, s.Message)
		}
		if s.Contract.WinnerDid != dids[i] {
			t.Fatalf("request %d was won by %s, want %s", i+1, s.Contract.WinnerDid, dids[i])
		}
		t.Logf("%s won a contract through the RFP flow: receipt %s", n.bidderName(i), s.Contract.ReceiptUri)
	}
	if err := n.WaitNoGuests(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestAStoppedBidderIsReplacedAndReleasesItsOrphan(t *testing.T) {
	t.Parallel()
	n := StartNetwork(t, NetworkOptions{Bidders: 2, WithGateway: true, UpTimeout: 5 * time.Minute,
		GatewayOptions: map[string]string{"vm-ready-timeout-sec": fmt.Sprint(failoverVMReadySec)}})
	ctx := context.Background()
	var dids [2]string
	for i := range dids {
		var err error
		if dids[i], err = n.BidderDid(i); err != nil {
			t.Fatal(err)
		}
	}
	n.bothBiddersWin(t, dids)

	tn := n.Tenant(t, "tenant", tenantReady)
	pods := tn.Kube.CoreV1().Pods("default")
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := tn.Kube.CoreV1().ServiceAccounts("default").Get(ctx, "default", metav1.GetOptions{}); err == nil {
			break
		} else if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			t.Fatalf("the default ServiceAccount of default: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: failoverPod, Namespace: "default"},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{
				Name: "probe", Image: probeImage, Command: []string{"sh", "-c", "sleep 3600"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
			}},
		},
	}
	started := time.Now()
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	var first v1alpha1.NodeClaim
	winner := -1
	for deadline := time.Now().Add(failoverAcceptWait); winner < 0; {
		if err := n.health(); err != nil {
			t.Fatalf("waiting for the first accept: %v", err)
		}
		cs, err := tn.Claims(ctx)
		if err == nil {
			for _, c := range cs {
				if c.Status.Contract.AcceptUri == "" {
					continue
				}
				for i := range dids {
					if claimWinner(c) == dids[i] {
						first, winner = c, i
					}
				}
			}
		}
		if winner >= 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no NodeClaim reached an accept inside %s: %s", failoverAcceptWait, tn.Summary(ctx))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if first.Status.Phase == v1alpha1.NodeClaimJoining || first.Status.Phase == v1alpha1.NodeClaimRunning {
		t.Fatalf("the node of claim %s already joined (%s) when its accept was seen; the stop cannot fall before the join", first.Name, first.Status.Phase)
	}
	other := 1 - winner
	oldRFP := first.Status.Contract.RFPUri
	oldReceipt := first.Status.Contract.ReceiptUri
	for deadline := time.Now().Add(2 * time.Minute); oldReceipt == ""; {
		if time.Now().After(deadline) {
			t.Fatalf("claim %s has an accept but no receipt", first.Name)
		}
		time.Sleep(200 * time.Millisecond)
		cs, err := tn.Claims(ctx)
		if err != nil {
			continue
		}
		for _, c := range cs {
			if c.Name != first.Name {
				continue
			}
			first = c
			if c.Status.Phase == v1alpha1.NodeClaimJoining || c.Status.Phase == v1alpha1.NodeClaimRunning {
				t.Fatalf("the node of claim %s joined (%s) before its receipt was recorded", first.Name, first.Status.Phase)
			}
			oldReceipt = c.Status.Contract.ReceiptUri
			oldRFP = c.Status.Contract.RFPUri
		}
	}
	for deadline := time.Now().Add(failoverVMWait); ; time.Sleep(200 * time.Millisecond) {
		vms, err := n.BidderVMsOf(ctx, winner)
		onVM := false
		for _, vm := range vms {
			if err == nil && vm.Spec.Contract != nil && vm.Spec.Contract.ReceiptUri == oldReceipt {
				onVM = true
			}
		}
		if onVM {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not record receipt %s on a ComputeVM inside %s (%v)", n.bidderName(winner), oldReceipt, failoverVMWait, err)
		}
	}
	t.Logf("+%.1fs: claim %s (%s) accepted by %s, whose contract names its ComputeVM; stopping it before the node joins", time.Since(started).Seconds(), first.Name, first.Status.Phase, n.bidderName(winner))
	if err := n.SetBidderStopped(ctx, winner, true); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitBidderStopped(winner, failoverStopWait); err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Now()
	t.Logf("+%.1fs: %s stopped (its core and components are down); its VM units: %q", time.Since(started).Seconds(), n.bidderName(winner), n.bidderUnits(winner))
	if cs, _ := tn.Claims(ctx); len(cs) > 0 {
		for _, c := range cs {
			if c.Status.Phase == v1alpha1.NodeClaimJoining || c.Status.Phase == v1alpha1.NodeClaimRunning {
				if c.Status.Contract.RFPUri == oldRFP {
					t.Fatalf("claim %s joined (%s) before the stop finished", c.Name, c.Status.Phase)
				}
			}
		}
	}

	var ended string
	for deadline := time.Now().Add(failoverEndWait); ended == ""; {
		if err := n.health(); err != nil {
			t.Fatalf("waiting for the claim of the stopped bidder to end: %v", err)
		}
		cs, err := tn.Claims(ctx)
		if err == nil {
			seen := false
			for _, c := range cs {
				if c.Status.Contract.RFPUri != oldRFP {
					continue
				}
				seen = true
				if c.Status.Contract.ReceiptUri != "" {
					oldReceipt = c.Status.Contract.ReceiptUri
				}
				if claimEnded(c) {
					ended = fmt.Sprintf("%s %s: %s", c.Name, c.Status.Phase, c.Status.Message)
				}
			}
			if !seen {
				ended = "claim " + first.Name + " is gone"
			}
		}
		if ended != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claim %s of the stopped bidder did not end inside %s: %s", first.Name, failoverEndWait, tn.Summary(ctx))
		}
		time.Sleep(time.Second)
	}
	t.Logf("+%.1fs (%.1fs after the stop): the claim of the stopped bidder ended: %s", time.Since(started).Seconds(), time.Since(stoppedAt).Seconds(), ended)

	var replacement v1alpha1.NodeClaim
	var node string
	for deadline := time.Now().Add(failoverReplaceWait); node == ""; {
		if err := n.health(); err != nil {
			t.Fatalf("waiting for the replacement: %v", err)
		}
		cs, err := tn.Claims(ctx)
		if err == nil {
			for _, c := range cs {
				if c.Status.Contract.RFPUri == oldRFP || c.Status.Contract.RFPUri == "" {
					continue
				}
				if claimWinner(c) == dids[winner] {
					t.Fatalf("the stopped %s won the replacement RFP of claim %s", n.bidderName(winner), c.Name)
				}
				if c.Status.Phase == v1alpha1.NodeClaimFailed || c.Status.Phase == v1alpha1.NodeClaimLost {
					t.Fatalf("the replacement claim %s is %s: %s", c.Name, c.Status.Phase, c.Status.Message)
				}
				if c.Status.Phase == v1alpha1.NodeClaimRunning && claimWinner(c) == dids[other] {
					if p, err := pods.Get(ctx, failoverPod, metav1.GetOptions{}); err == nil && podReady(p) {
						replacement, node = c, p.Spec.NodeName
					}
				}
			}
		}
		if node != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no replacement node from %s ran the pod inside %s: %s", n.bidderName(other), failoverReplaceWait, tn.Summary(ctx))
		}
		time.Sleep(time.Second)
	}
	t.Logf("+%.1fs (%.1fs after the stop): %s won the new RFP; claim %s Running; pod %s runs on node %s",
		time.Since(started).Seconds(), time.Since(stoppedAt).Seconds(), n.bidderName(other), replacement.Name, failoverPod, node)

	if err := n.SetBidderStopped(ctx, winner, false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitBidderReady(winner, failoverBackWait); err != nil {
		t.Fatal(err)
	}
	backAt := time.Now()
	t.Logf("%s is Ready again with a running bidder", n.bidderName(winner))

	var orphans string
	orphanSeen := false
	for deadline := time.Now().Add(failoverOrphanWait); ; {
		if err := n.health(); err != nil {
			t.Fatalf("waiting for the orphan to be released: %v", err)
		}
		vms, err := n.BidderVMsOf(ctx, winner)
		var desc []string
		for _, vm := range vms {
			desc = append(desc, describeVM(vm))
		}
		for _, vm := range vms {
			if vm.Spec.Contract != nil && vm.Spec.Contract.ReceiptUri == replacement.Status.Contract.ReceiptUri {
				t.Fatalf("the returned %s holds the receipt of the replacement contract: %s", n.bidderName(winner), describeVM(vm))
			}
			if vm.Spec.Contract != nil && oldReceipt != "" && vm.Spec.Contract.ReceiptUri != oldReceipt {
				t.Fatalf("the returned %s holds a ComputeVM that is not the orphan of receipt %s: %s", n.bidderName(winner), oldReceipt, describeVM(vm))
			}
			orphanSeen = true
		}
		now := strings.Join(desc, "; ")
		if now != orphans {
			t.Logf("%s ComputeVMs +%.1fs: %q", n.bidderName(winner), time.Since(backAt).Seconds(), now)
			orphans = now
		}
		if err == nil && len(vms) == 0 && n.bidderUnits(winner) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s kept its orphaned guest %s after coming back (%v), units %q", n.bidderName(winner), failoverOrphanWait, err, n.bidderUnits(winner))
		}
		time.Sleep(2 * time.Second)
	}
	if !orphanSeen {
		t.Logf("the returned %s was already done with its orphan at the first read", n.bidderName(winner))
	}
	t.Logf("the returned %s released its orphaned ComputeVM (old receipt %q) %.1fs after it came back", n.bidderName(winner), oldReceipt, time.Since(backAt).Seconds())

	lines, err := tn.ReleaseLines()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		t.Logf("release line: %+v", l)
	}
	if p, err := pods.Get(ctx, failoverPod, metav1.GetOptions{}); err != nil || !podReady(p) || p.Spec.NodeName != node {
		t.Fatalf("pod %s is not Ready on %s after the stopped bidder returned: %v", failoverPod, node, err)
	}
	if err := pods.Delete(ctx, failoverPod, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(scaleInTimeout); ; {
		if err := n.health(); err != nil {
			t.Fatalf("scaling in: %v", err)
		}
		if len(n.guestsLeft()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster did not scale in to zero inside %s: %s; left %v", scaleInTimeout, tn.Summary(ctx), n.guestsLeft())
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("scaled in: no guest left")
}
