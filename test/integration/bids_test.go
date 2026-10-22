//go:build integration

package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var bidGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "bids"}

const (
	bidScaleOut   = 4 * time.Minute
	bidderRestart = 2 * time.Minute
	restartSettle = 15 * time.Second
)

func (n *Network) bidderClient(i int) (dynamic.Interface, error) {
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, err
	}
	cfg.Host = strings.TrimSuffix(strings.SplitN(cfg.Host, "/clusters/", 2)[0], "/") + "/clusters/root:" + n.bidderName(i)
	cfg.Timeout = 15 * time.Second
	return dynamic.NewForConfig(cfg)
}

func bidderBids(ctx context.Context, dyn dynamic.Interface) ([]v1alpha1.Bid, error) {
	l, err := dyn.Resource(bidGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.Bid, 0, len(l.Items))
	for _, u := range l.Items {
		var b v1alpha1.Bid
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func describeBids(bids []v1alpha1.Bid) string {
	var b strings.Builder
	for _, x := range bids {
		fmt.Fprintf(&b, "\n  %s rfp=%s phase=%s vm=%s", x.Name, x.Spec.Rfp.Uri, x.Status.Phase, x.Status.ComputeVM)
	}
	return b.String()
}

func TestBidderAnswersEachRFPOnceAcrossARestart(t *testing.T) {
	n := StartNetwork(t, NetworkOptions{Bidders: 1, WithGateway: true, UpTimeout: 5 * time.Minute})
	ctx := context.Background()
	tn := n.Tenant(t, "tenant", tenantReady)
	dyn, err := n.bidderClient(0)
	if err != nil {
		t.Fatal(err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "it-bid-probe", Namespace: "default"},
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
	if _, err := tn.Kube.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(bidScaleOut)
	for {
		if err := tn.ClaimFailure(ctx); err != nil {
			t.Fatalf("the NodeClaim failed: %v\n%s", err, tn.Summary(ctx))
		}
		nodes, _ := tn.Nodes(ctx)
		ready := 0
		for _, nd := range nodes {
			if nodeReady(nd) {
				ready++
			}
		}
		if ready > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no node joined inside %s\n%s", bidScaleOut, tn.Summary(ctx))
		}
		time.Sleep(time.Second)
	}

	bids, err := bidderBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	vms, err := n.BidderVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	won := map[string]string{}
	rfps := map[string]string{}
	for _, b := range bids {
		if prev, ok := rfps[b.Spec.Rfp.Uri]; ok {
			t.Fatalf("two Bids answer RFP %s: %s and %s", b.Spec.Rfp.Uri, prev, b.Name)
		}
		rfps[b.Spec.Rfp.Uri] = b.Name
		if b.Status.Phase == v1alpha1.BidPhaseWon {
			won[b.Status.ComputeVM] = b.Name
		}
	}
	if len(vms) == 0 || len(won) != len(vms) {
		t.Fatalf("%d ComputeVM(s) and %d Won Bid(s); want one Won Bid per ComputeVM:%s", len(vms), len(won), describeBids(bids))
	}
	for _, vm := range vms {
		if won[vm.Name] == "" {
			t.Fatalf("no Won Bid points at ComputeVM %s:%s", describeVM(vm), describeBids(bids))
		}
	}
	t.Logf("before the restart: %d Bid(s) for %d RFP(s), %d Won for %d ComputeVM(s)", len(bids), len(rfps), len(won), len(vms))

	pid, _, err := n.bidderProcess(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.signal("KILL", pid); err != nil {
		t.Fatal(err)
	}
	newPID, _, err := n.waitBidderBack(0, pid, bidderRestart)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitBidderReady(0, bidderRestart); err != nil {
		t.Fatal(err)
	}
	t.Logf("bidder %d killed and back as %d", pid, newPID)

	for deadline := time.Now().Add(restartSettle); time.Now().Before(deadline); time.Sleep(time.Second) {
		if err := n.health(); err != nil {
			t.Fatalf("waiting for the restarted bidder: %v", err)
		}
		settled, err := bidderBids(ctx, dyn)
		if err != nil {
			t.Fatal(err)
		}
		if len(settled) != len(bids) {
			t.Fatalf("the restart changed the Bids from %d to %d:%s", len(bids), len(settled), describeBids(settled))
		}
	}
	after, err := bidderBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(bids) {
		t.Fatalf("the restart changed the Bids from %d to %d:%s", len(bids), len(after), describeBids(after))
	}
	afterRfps := map[string]string{}
	afterWon := map[string]string{}
	for _, b := range after {
		if prev, ok := afterRfps[b.Spec.Rfp.Uri]; ok {
			t.Fatalf("two Bids answer RFP %s after the restart: %s and %s", b.Spec.Rfp.Uri, prev, b.Name)
		}
		afterRfps[b.Spec.Rfp.Uri] = b.Name
		if b.Status.Phase == v1alpha1.BidPhaseWon {
			afterWon[b.Status.ComputeVM] = b.Name
		}
	}
	if len(afterRfps) != len(rfps) || len(afterWon) != len(won) {
		t.Fatalf("after the restart: %d RFP(s) and %d Won Bid(s); before %d and %d:%s",
			len(afterRfps), len(afterWon), len(rfps), len(won), describeBids(after))
	}
	vmsAfter, err := n.BidderVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vmsAfter) != len(vms) {
		t.Fatalf("the restart changed the ComputeVMs from %d to %d", len(vms), len(vmsAfter))
	}
	for _, vm := range vmsAfter {
		if afterWon[vm.Name] == "" {
			t.Fatalf("no Won Bid points at ComputeVM %s after the restart:%s", describeVM(vm), describeBids(after))
		}
	}
}
