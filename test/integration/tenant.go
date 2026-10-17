//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var (
	nodeClaimGVR  = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "nodeclaims"}
	computeVMGVR  = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "computevms"}
	clientCertGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "clusterclientcertificates"}
)

func (n *Network) BidderVMs(ctx context.Context) ([]v1alpha1.ComputeVM, error) {
	var out []v1alpha1.ComputeVM
	for i := range n.Handles {
		vms, err := n.BidderVMsOf(ctx, i)
		if err != nil {
			return nil, err
		}
		out = append(out, vms...)
	}
	return out, nil
}

func (n *Network) BidderVMsOf(ctx context.Context, i int) ([]v1alpha1.ComputeVM, error) {
	var out []v1alpha1.ComputeVM
	dyn, err := n.bidderClient(i)
	if err != nil {
		return nil, err
	}
	l, err := dyn.Resource(computeVMGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, u := range l.Items {
		var vm v1alpha1.ComputeVM
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &vm); err != nil {
			return nil, err
		}
		out = append(out, vm)
	}
	return out, nil
}

func describeVM(vm v1alpha1.ComputeVM) string {
	receipt := ""
	if vm.Spec.Contract != nil {
		receipt = vm.Spec.Contract.ReceiptUri
	}
	return fmt.Sprintf("%s (%s, owner %s, requester %s, receipt %s)", vm.Name, vm.Status.Phase, vm.Spec.Owner.Handle, vm.Spec.Requester.Did, receipt)
}

type Tenant struct {
	n          *Network
	GatewayDir string
	ClusterDir string
	Home       string
	Address    string
	Kube       *kubernetes.Clientset
	claims     dynamic.ResourceInterface
	certs      dynamic.ResourceInterface
}

func (n *Network) GatewayDir() string {
	return filepath.Join(n.StateDir, "apps", "dev", n.Instance+"-gateway")
}

func (n *Network) read(path string) ([]byte, error) {
	r := n.h.As(context.Background(), "cat", path)
	if r.Err != nil {
		return nil, r.Err
	}
	return []byte(r.Stdout), nil
}

func (n *Network) Tenant(t testing.TB, name string, timeout time.Duration) *Tenant {
	t.Helper()
	gw := n.GatewayDir()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		tn, err := n.openTenant(gw, name)
		if err == nil {
			_, err = tn.Kube.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(context.Background())
			if err == nil {
				return tn
			}
		}
		last = err
		if herr := n.health(); herr != nil {
			t.Fatalf("waiting for the tenant cluster %s: %v", name, herr)
		}
		if ferr := n.memberFailing(gw, name); ferr != nil {
			t.Fatalf("waiting for the tenant cluster %s: %v", name, ferr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tenant cluster %s did not answer /readyz within %s: %v", name, timeout, last)
		}
		time.Sleep(2 * time.Second)
	}
}

func (n *Network) memberFailing(gw, name string) error {
	r := n.h.As(context.Background(), "sh", "-c", `grep -h -s 'failed after placement' "$1"/logs/core.log | grep -F -- "-$2-" | tail -3`, "sh", gw, name)
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(lines) < 3 || lines[0] == "" {
		return nil
	}
	var rec struct {
		Unit string `json:"unit"`
		Err  string `json:"err"`
	}
	json.Unmarshal([]byte(lines[len(lines)-1]), &rec)
	return fmt.Errorf("a member of the cluster keeps failing after placement: %s: %s", rec.Unit, rec.Err)
}

func (n *Network) openTenant(gw, name string) (*Tenant, error) {
	r := n.h.As(context.Background(), "sh", "-c", `ls -d "$1"/clusterprovider/pdr-*-"$2" 2>/dev/null`, "sh", gw, name)
	dirs := strings.Fields(r.Stdout)
	if len(dirs) != 1 {
		return nil, fmt.Errorf("no single cluster dir for %s under %s/clusterprovider: %v", name, gw, dirs)
	}
	dir := dirs[0]
	owner := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(dir), "pdr-"), "-"+name)
	var front struct {
		Address string `json:"address"`
	}
	b, err := n.read(filepath.Join(dir, "front.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &front); err != nil || front.Address == "" {
		return nil, fmt.Errorf("front.json of %s: %v", name, err)
	}
	tls := filepath.Join(dir, "server", "tls")
	ca, err := n.read(filepath.Join(tls, "server-ca.crt"))
	if err != nil {
		return nil, err
	}
	cert, err := n.read(filepath.Join(tls, "client-admin.crt"))
	if err != nil {
		return nil, err
	}
	key, err := n.read(filepath.Join(tls, "client-admin.key"))
	if err != nil {
		return nil, err
	}
	cfg := &rest.Config{Host: "https://" + front.Address, Timeout: 15 * time.Second,
		TLSClientConfig: rest.TLSClientConfig{CAData: ca, CertData: cert, KeyData: key}}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	kcfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, err
	}
	home := "root:" + owner + ":" + name
	kcfg.Host = strings.TrimSuffix(strings.SplitN(kcfg.Host, "/clusters/", 2)[0], "/") + "/clusters/" + home
	kcfg.Timeout = 15 * time.Second
	dyn, err := dynamic.NewForConfig(kcfg)
	if err != nil {
		return nil, err
	}
	return &Tenant{n: n, GatewayDir: gw, ClusterDir: dir, Home: home, Address: front.Address, Kube: kube,
		claims: dyn.Resource(nodeClaimGVR), certs: dyn.Resource(clientCertGVR)}, nil
}

func (tn *Tenant) Claims(ctx context.Context) ([]v1alpha1.NodeClaim, error) {
	l, err := tn.claims.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.NodeClaim, 0, len(l.Items))
	for _, u := range l.Items {
		var c v1alpha1.NodeClaim
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (tn *Tenant) Certificates(ctx context.Context) ([]v1alpha1.ClusterClientCertificate, error) {
	l, err := tn.certs.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.ClusterClientCertificate, 0, len(l.Items))
	for _, u := range l.Items {
		var c v1alpha1.ClusterClientCertificate
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (tn *Tenant) Nodes(ctx context.Context) ([]corev1.Node, error) {
	l, err := tn.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func nodeReady(nd corev1.Node) bool {
	for _, c := range nd.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (tn *Tenant) ClaimFailure(ctx context.Context) error {
	cs, err := tn.Claims(ctx)
	if err != nil {
		return nil
	}
	for _, c := range cs {
		switch c.Status.Phase {
		case v1alpha1.NodeClaimFailed, v1alpha1.NodeClaimLost:
			return fmt.Errorf("NodeClaim %s is %s: %s", c.Name, c.Status.Phase, c.Status.Message)
		}
	}
	return nil
}

func (tn *Tenant) Summary(ctx context.Context) string {
	var parts []string
	if cs, err := tn.Claims(ctx); err == nil {
		for _, c := range cs {
			parts = append(parts, fmt.Sprintf("claim %s %s", c.Name, c.Status.Phase))
		}
	}
	if ns, err := tn.Nodes(ctx); err == nil {
		for _, nd := range ns {
			parts = append(parts, fmt.Sprintf("node %s ready=%t", nd.Name, nodeReady(nd)))
		}
	}
	if len(parts) == 0 {
		return "no claims, no nodes"
	}
	return strings.Join(parts, ", ")
}

type ReleaseLine struct {
	Claim       string `json:"claim"`
	Confirmed   bool   `json:"confirmed"`
	MessageName string `json:"message"`
}

func (tn *Tenant) ReleaseLines() ([]ReleaseLine, error) {
	r := tn.n.h.As(context.Background(), "sh", "-c", `grep -h -s node_claim_released "$1"/denokcp/runs/*/stdout.txt; true`, "sh", tn.GatewayDir)
	if r.Err != nil {
		return nil, r.Err
	}
	var out []ReleaseLine
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		var rl ReleaseLine
		if json.Unmarshal([]byte(l), &rl) == nil && rl.Claim != "" {
			out = append(out, rl)
		}
	}
	return out, nil
}

func (tn *Tenant) SshSessions(since time.Time) ([]string, error) {
	r := tn.n.h.As(context.Background(), "sh", "-c",
		`grep -r -h -s -E 'runSession|ssh_session|pollReady|sshReady[^"]|ssh_connect' "$1" --include=stdout.txt --include=stderr.txt --include=core.log | head -5
journalctl --user --no-pager -o cat --since "@$2" | grep -E 'sshd.*(Accepted|session opened|Failed password|Invalid user)' | head -5; true`,
		"sh", tn.n.StateDir, fmt.Sprint(since.Unix()))
	if r.Err != nil {
		return nil, r.Err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
