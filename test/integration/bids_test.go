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

var (
	bidGVR          = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "bids"}
	marketBidderGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "marketbidders"}
)

const (
	bidScaleOut    = 4 * time.Minute
	bidReplay      = 90 * time.Second
	bidderRestart  = 2 * time.Minute
	bidderRootName = "app"
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

func bidderMarket(ctx context.Context, dyn dynamic.Interface) (*v1alpha1.MarketLoopStatus, error) {
	u, err := dyn.Resource(marketBidderGVR).Get(ctx, bidderRootName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var o v1alpha1.MarketBidder
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return nil, err
	}
	return o.Status.Market, nil
}

func describeBids(bids []v1alpha1.Bid) string {
	var b strings.Builder
	for _, x := range bids {
		fmt.Fprintf(&b, "\n  %s rfp=%s phase=%s vm=%s", x.Name, x.Spec.Rfp.Uri, x.Status.Phase, x.Status.ComputeVM)
	}
	return b.String()
}

func maxCursor(m *v1alpha1.MarketLoopStatus) int64 {
	var c int64
	if m == nil {
		return 0
	}
	for _, s := range m.Streams {
		if s.Cursor > c {
			c = s.Cursor
		}
	}
	return c
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
	before, err := bidderMarket(ctx, dyn)
	if err != nil || before == nil {
		t.Fatalf("the bidder's root object has no status.market: %v", err)
	}
	if before.Bids != int64(len(bids)) {
		t.Fatalf("status.market.bids is %d for %d Bid(s):%s", before.Bids, len(bids), describeBids(bids))
	}
	t.Logf("before the restart: %d Bid(s), status.market %+v", len(bids), *before)

	r := n.Run("restart-bidder", bidderRestart, "dev", "restart", "--instance", n.Instance, "--state-dir", n.StateRoot, "bidder-1/bidder")
	if r.Err != nil {
		t.Fatalf("dev restart bidder-1/bidder: %v\n%s", r.Err, r.Tail(10))
	}
	cursor := maxCursor(before)
	deadline = time.Now().Add(bidReplay)
	for {
		m, err := bidderMarket(ctx, dyn)
		if err == nil && m != nil && maxCursor(m) >= cursor && m.UpdatedAt > before.UpdatedAt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted bidder's stream did not reach cursor %d inside %s", cursor, bidReplay)
		}
		time.Sleep(time.Second)
	}
	after, err := bidderBids(ctx, dyn)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(bids) {
		t.Fatalf("the replay after the restart changed the Bids from %d to %d:%s", len(bids), len(after), describeBids(after))
	}
	m, _ := bidderMarket(ctx, dyn)
	if m == nil || m.Bids != before.Bids {
		t.Fatalf("status.market.bids moved across the restart: before %d, after %+v", before.Bids, m)
	}
}
