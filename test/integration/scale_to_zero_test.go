//go:build integration

package integration

import (
	"context"
	"fmt"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	probeImage      = "docker.io/library/busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	nsServer        = "it-srv"
	nsAllowed       = "it-cli-allowed"
	nsOther         = "it-cli-other"
	allowLabel      = "it-allow"
	spreadLabel     = "it-spread"
	tenantReady     = 3 * time.Minute
	scaleOutTimeout = 4 * time.Minute
	scaleHold       = 10 * time.Second
	scaleInTimeout  = scaleHold + 90*time.Second
	probeTimeout    = 90 * time.Second
	policyTimeout   = 30 * time.Second
)

func serverPod(name, node string, spread bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsServer, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{
				Name: "http", Image: probeImage,
				Command: []string{"sh", "-c", "mkdir -p /www && echo it-ok > /www/index.html && exec httpd -f -p 8080 -h /www"},
				Ports:   []corev1.ContainerPort{{ContainerPort: 8080}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}},
					PeriodSeconds: 1},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
			}},
		},
	}
	if spread {
		p.Labels[spreadLabel] = "yes"
		p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{spreadLabel: "yes"}},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		}}
	}
	return p
}

type matrix struct {
	t     *testing.T
	tn    *Tenant
	node  string
	count int
}

func (m *matrix) probe(ns, script string) (bool, string, error) {
	ctx := context.Background()
	m.count++
	name := fmt.Sprintf("probe-%d", m.count)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"it-probe": "yes"}},
		Spec: corev1.PodSpec{
			NodeName: m.node, RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{Name: "probe", Image: probeImage, Command: []string{"sh", "-c", "sleep 5; " + script}}},
		},
	}
	pods := m.tn.Kube.CoreV1().Pods(ns)
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return false, "", err
	}
	defer pods.Delete(context.Background(), name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	deadline := time.Now().Add(probeTimeout)
	for {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				return true, "", nil
			case corev1.PodFailed:
				return false, "", nil
			}
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" || w.Reason == "CreateContainerError") {
					return false, "", fmt.Errorf("probe %s/%s: %s: %s", ns, name, w.Reason, w.Message)
				}
			}
		}
		if err := m.tn.ClaimFailure(ctx); err != nil {
			return false, "", err
		}
		if time.Now().After(deadline) {
			return false, "", fmt.Errorf("probe %s/%s did not finish within %s", ns, name, probeTimeout)
		}
		time.Sleep(time.Second)
	}
}

func (m *matrix) expect(what, ns, script string, want bool) {
	m.t.Helper()
	deadline := time.Now().Add(policyTimeout)
	for {
		got, _, err := m.probe(ns, script)
		if err != nil {
			m.t.Fatalf("%s: %v", what, err)
		}
		if got == want {
			m.t.Logf("netpol: %s: %s", what, map[bool]string{true: "reached", false: "blocked"}[got])
			return
		}
		if time.Now().After(deadline) {
			m.t.Fatalf("%s: want %s, got %s for %s", what, map[bool]string{true: "reached", false: "blocked"}[want],
				map[bool]string{true: "reached", false: "blocked"}[got], policyTimeout)
		}
	}
}

func wget(ip string) string {
	return fmt.Sprintf("wget -q -T 3 -O /dev/null http://%s:8080/", ip)
}

func (m *matrix) apply(p *networkingv1.NetworkPolicy) func() {
	m.t.Helper()
	np := m.tn.Kube.NetworkingV1().NetworkPolicies(p.Namespace)
	if _, err := np.Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		m.t.Fatalf("NetworkPolicy %s/%s: %v", p.Namespace, p.Name, err)
	}
	return func() {
		if err := np.Delete(context.Background(), p.Name, metav1.DeleteOptions{}); err != nil {
			m.t.Fatalf("deleting NetworkPolicy %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
}

func TestTenantClusterScalesFromZeroOnMarketVMsAndBack(t *testing.T) {
	t.Parallel()
	n := SharedNetwork(t, NetworkOptions{Bidders: 2, WithGateway: true, UpTimeout: 5 * time.Minute})
	started := time.Now()
	ctx := context.Background()
	tn := n.Tenant(t, "tenant", tenantReady)
	t.Logf("tenant cluster %s answers /readyz at %s (%.1fs after the network was Ready)", tn.Home, tn.Address, time.Since(started).Seconds())
	if nodes, err := tn.Nodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("the tenant cluster does not idle at zero nodes: %d nodes, %v", len(nodes), err)
	}
	if claims, err := tn.Claims(ctx); err != nil || len(claims) != 0 {
		t.Fatalf("the idle tenant cluster has NodeClaims: %d, %v", len(claims), err)
	}

	core := tn.Kube.CoreV1()
	for _, ns := range []struct {
		name   string
		labels map[string]string
	}{{nsServer, nil}, {nsAllowed, map[string]string{allowLabel: "yes"}}, {nsOther, nil}} {
		if _, err := core.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.name, Labels: ns.labels}},
			metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range []string{nsServer, nsAllowed, nsOther} {
		for deadline := time.Now().Add(30 * time.Second); ; {
			if _, err := core.ServiceAccounts(ns).Get(ctx, "default", metav1.GetOptions{}); err == nil {
				break
			} else if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
				t.Fatalf("the default ServiceAccount of %s: %v", ns, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	scaleOut := time.Now()
	for _, name := range []string{"it-a", "it-b"} {
		if _, err := core.Pods(nsServer).Create(ctx, serverPod(name, "", true), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := core.Services(nsServer).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "it-b", Namespace: nsServer},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "it-b"}, Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var firstClaim, nodesReady time.Duration
	var podA, podB *corev1.Pod
	last := ""
	for {
		if err := n.health(); err != nil {
			t.Fatalf("scale out: %v", err)
		}
		if err := tn.ClaimFailure(ctx); err != nil {
			t.Fatalf("scale out: %v", err)
		}
		if s := tn.Summary(ctx); s != last {
			t.Logf("scale out +%.1fs: %s", time.Since(scaleOut).Seconds(), s)
			last = s
		}
		if cs, _ := tn.Claims(ctx); len(cs) > 0 && firstClaim == 0 {
			firstClaim = time.Since(scaleOut)
		}
		a, errA := core.Pods(nsServer).Get(ctx, "it-a", metav1.GetOptions{})
		b, errB := core.Pods(nsServer).Get(ctx, "it-b", metav1.GetOptions{})
		if errA == nil && errB == nil && podReady(a) && podReady(b) {
			podA, podB = a, b
			nodesReady = time.Since(scaleOut)
			break
		}
		for _, p := range []*corev1.Pod{a, b} {
			if p != nil && p.Status.Phase == corev1.PodFailed {
				t.Fatalf("pod %s Failed: %s", p.Name, p.Status.Message)
			}
		}
		if time.Since(scaleOut) > scaleOutTimeout {
			t.Fatalf("two nodes did not join and run the pods within %s: %s", scaleOutTimeout, tn.Summary(ctx))
		}
		time.Sleep(2 * time.Second)
	}
	if podA.Spec.NodeName == podB.Spec.NodeName {
		t.Fatalf("the anti-affinity pods share node %s", podA.Spec.NodeName)
	}
	var claims []v1alpha1.NodeClaim
	for deadline := time.Now().Add(30 * time.Second); ; {
		var err error
		claims, err = tn.Claims(ctx)
		if err != nil || len(claims) != 2 {
			t.Fatalf("want two NodeClaims for two nodes, got %d (%v)", len(claims), err)
		}
		running := 0
		for _, c := range claims {
			if c.Status.Phase == "Running" && c.Status.Contract.ReceiptUri != "" {
				running++
			}
		}
		if running == len(claims) {
			break
		}
		if time.Now().After(deadline) {
			for _, c := range claims {
				t.Errorf("NodeClaim %s: phase %s receipt %q (want Running once its node joined)", c.Name, c.Status.Phase, c.Status.Contract.ReceiptUri)
			}
			break
		}
		time.Sleep(time.Second)
	}
	receipts := map[string]bool{}
	receiptDeadline := time.Now().Add(2 * time.Minute)
	for {
		fresh, err := tn.Claims(ctx)
		if err != nil {
			t.Fatalf("re-reading the NodeClaims for their receipts: %v", err)
		}
		claims = fresh
		missing := 0
		for _, c := range claims {
			if c.Status.Contract.ReceiptUri == "" {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(receiptDeadline) {
			t.Fatalf("%d NodeClaim(s) reached Running without a receipt in status.contract.receiptUri", missing)
		}
		time.Sleep(time.Second)
	}
	for _, c := range claims {
		receipts[c.Status.Contract.ReceiptUri] = true
	}
	t.Logf("scale out: first NodeClaim +%.1fs, two market RFP VMs joined and both pods Ready +%.1fs (nodes %s, %s)",
		firstClaim.Seconds(), nodesReady.Seconds(), podA.Spec.NodeName, podB.Spec.NodeName)
	if vms, err := n.BidderVMs(ctx); err != nil {
		t.Errorf("listing the bidders' ComputeVMs: %v", err)
	} else {
		var described []string
		for _, vm := range vms {
			described = append(described, describeVM(vm))
		}
		t.Logf("the bidders hold %d ComputeVM(s) (all tests on this network): %s", len(vms), strings.Join(described, "; "))
		ours := 0
		for _, vm := range vms {
			if vm.Spec.Contract != nil && receipts[vm.Spec.Contract.ReceiptUri] {
				ours++
			}
		}
		if ours != len(claims) {
			t.Errorf("the bidders run %d ComputeVMs for this test's %d NodeClaims", ours, len(claims))
		}
	}

	podC, err := core.Pods(nsServer).Create(ctx, serverPod("it-c", podB.Spec.NodeName, false), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(probeTimeout); !podReady(podC); {
		if time.Now().After(deadline) {
			t.Fatalf("pod it-c on %s not Ready within %s", podB.Spec.NodeName, probeTimeout)
		}
		time.Sleep(time.Second)
		if podC, err = core.Pods(nsServer).Get(ctx, "it-c", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	runJobThroughXRPC(t, n, tn, "tenant", nsServer, podA.Spec.NodeName)

	m := &matrix{t: t, tn: tn, node: podA.Spec.NodeName}
	ipB, ipC := podB.Status.PodIP, podC.Status.PodIP
	dns := "nslookup it-b.it-srv.svc.cluster.local"
	m.expect("pod on node A reaches pod B on node B", nsServer, wget(ipB), true)
	m.expect("DNS through CoreDNS resolves the service of pod B", nsOther, dns, true)
	m.expect("the service name of pod B answers", nsOther, "wget -q -T 3 -O /dev/null http://it-b.it-srv.svc.cluster.local:8080/", true)

	undo := m.apply(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "deny-ingress", Namespace: nsServer},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}})
	m.expect("deny-all ingress on it-srv: node A to pod B", nsOther, wget(ipB), false)
	undo()
	m.expect("without deny-all ingress: node A to pod B", nsOther, wget(ipB), true)

	undo = m.apply(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "deny-egress", Namespace: nsOther},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}})
	m.expect("egress deny on it-cli-other: node A to pod B", nsOther, wget(ipB), false)
	undo()
	m.expect("without egress deny: node A to pod B", nsOther, wget(ipB), true)

	m.expect("without the ipBlock policy: node A to pod C", nsOther, wget(ipC), true)
	undo = m.apply(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "egress-one-ipblock", Namespace: nsOther},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: ipB + "/32"}}}}}}})
	m.expect("egress allowed to the ipBlock of pod B: node A to pod B", nsOther, wget(ipB), true)
	m.expect("egress allowed to the ipBlock of pod B: node A to pod C", nsOther, wget(ipC), false)
	undo()
	m.expect("without the ipBlock policy again: node A to pod C", nsOther, wget(ipC), true)

	undo = m.apply(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-labelled-namespaces", Namespace: nsServer},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{allowLabel: "yes"}}}}}}}})
	m.expect("namespaceSelector allow: the labelled namespace to pod B", nsAllowed, wget(ipB), true)
	m.expect("namespaceSelector allow: another namespace to pod B", nsOther, wget(ipB), false)
	undo()
	m.expect("without the namespaceSelector policy: another namespace to pod B", nsOther, wget(ipB), true)

	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port53 := intstr.FromInt32(53)
	undo = m.apply(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default-deny-dns-allow", Namespace: nsOther},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &port53}, {Protocol: &tcp, Port: &port53}}}}}})
	m.expect("default deny with DNS allowed: DNS resolves", nsOther, dns, true)
	m.expect("default deny with DNS allowed: node A to pod B", nsOther, wget(ipB), false)
	undo()
	m.expect("without default deny: node A to pod B", nsOther, wget(ipB), true)

	seenNodes := 2
	scaleInTo := func(want int) {
		t.Helper()
		started, last := time.Now(), ""
		for {
			if err := n.health(); err != nil {
				t.Fatalf("scale in to %d: %v", want, err)
			}
			if err := tn.ClaimFailure(ctx); err != nil {
				t.Fatalf("scale in to %d: %v", want, err)
			}
			if _, err := tn.Kube.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx); err != nil {
				t.Fatalf("scale in to %d: the control plane stopped answering /readyz: %v", want, err)
			}
			nodes, nerr := tn.Nodes(ctx)
			cs, cerr := tn.Claims(ctx)
			if s := tn.Summary(ctx); s != last {
				t.Logf("scale in to %d +%.1fs: %s", want, time.Since(started).Seconds(), s)
				last = s
			}
			if nerr == nil && seenNodes-len(nodes) > 1 {
				t.Fatalf("scale in released %d nodes at once, want one by one", seenNodes-len(nodes))
			}
			if nerr == nil {
				seenNodes = len(nodes)
			}
			if nerr == nil && cerr == nil && len(nodes) == want && len(cs) == want {
				t.Logf("scale in: %d node(s) and %d NodeClaim(s) +%.1fs", want, want, time.Since(started).Seconds())
				return
			}
			if time.Since(started) > scaleInTimeout {
				t.Fatalf("the cluster did not scale in to %d within %s (the hold is %s): %s", want, scaleInTimeout, scaleHold, tn.Summary(ctx))
			}
			time.Sleep(2 * time.Second)
		}
	}
	for _, name := range []string{"it-b", "it-c"} {
		if err := core.Pods(nsServer).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
			t.Fatal(err)
		}
	}
	scaleInTo(1)
	if left, err := tn.Nodes(ctx); err == nil && len(left) == 1 && left[0].Name != podA.Spec.NodeName {
		t.Errorf("scale in released %s, the node that still runs it-a, and kept %s", podA.Spec.NodeName, left[0].Name)
	}
	for _, ns := range []string{nsServer, nsAllowed, nsOther} {
		if err := core.Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	scaleInTo(0)
	if err := n.WaitNoGuestsOf(receipts, time.Minute); err != nil {
		t.Fatal(err)
	}
	lines, err := tn.ReleaseLines()
	if err != nil {
		t.Fatal(err)
	}
	released := 0
	for _, l := range lines {
		if !l.Confirmed {
			t.Errorf("NodeClaim %s released without a confirmed release: %+v", l.Claim, l)
		} else {
			released++
		}
	}
	if released < 2 {
		t.Errorf("want a confirmed release for each of the two NodeClaims, got %d: %+v", released, lines)
	}
	if _, err := tn.Kube.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx); err != nil {
		t.Errorf("the control plane is not Ready at zero nodes: %v", err)
	}
	t.Logf("released: %d confirmed releases, no ComputeVM, VM unit or VM work dir left, control plane Ready", released)
}

func podReady(p *corev1.Pod) bool {
	if p == nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
