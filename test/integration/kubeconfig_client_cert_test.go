//go:build integration

package integration

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ownersGroup       = "socialweb.computer:cluster-owners"
	kubectlBinary     = "/usr/local/bin/kubectl"
	revokeRefusalTime = 2 * time.Second
)

func TestKubeconfigClientCertificateWithoutCredentialHelper(t *testing.T) {
	t.Parallel()
	n := StartNetwork(t, NetworkOptions{Bidders: 0, WithGateway: true, UpTimeout: 5 * time.Minute})
	tn := n.Tenant(t, "tenant", tenantReady)
	waitOwnerGrant(t, n, tn)
	ctx := context.Background()
	addr := n.xrpcAddress(t)
	acct := n.OwnerAccount(t)
	gw := &http.Client{Timeout: 30 * time.Second}

	post := func(lxm string, token string, body string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/xrpc/"+clusterNSID+lxm, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := gw.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", lxm, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, resp.Header, raw
	}

	code, header, raw := post("getKubeconfig", acct.Token(t, "getKubeconfig"), `{"name":"tenant","ttlSeconds":3600}`)
	if code != http.StatusOK {
		t.Fatalf("getKubeconfig ttl 3600: %d %.200s", code, raw)
	}
	if cc := header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("getKubeconfig Cache-Control: want no-store, got %q", cc)
	}
	var issued struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := json.Unmarshal(raw, &issued); err != nil || issued.Kubeconfig == "" {
		t.Fatalf("getKubeconfig returned no kubeconfig: %v", err)
	}
	kubeconfigText := issued.Kubeconfig

	kc := filepath.Join(n.Dir, "kc.yaml")
	if err := n.h.WriteFile(kc, []byte(kubeconfigText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load([]byte(kubeconfigText))
	if err != nil {
		t.Fatalf("the kubeconfig does not parse: %v", err)
	}
	if len(cfg.AuthInfos) != 1 {
		t.Fatalf("the kubeconfig holds %d users, want 1", len(cfg.AuthInfos))
	}
	for _, ai := range cfg.AuthInfos {
		if len(ai.ClientCertificateData) == 0 || len(ai.ClientKeyData) == 0 || ai.Exec != nil || ai.Token != "" {
			t.Fatalf("the kubeconfig user is not a plain client certificate: cert %d key %d exec %v token %t",
				len(ai.ClientCertificateData), len(ai.ClientKeyData), ai.Exec != nil, ai.Token != "")
		}
		block, _ := pem.Decode(ai.ClientCertificateData)
		if block == nil {
			t.Fatal("the client certificate is not PEM")
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if leaf.Subject.CommonName != acct.DID {
			t.Fatalf("certificate CN: want %s, got %s", acct.DID, leaf.Subject.CommonName)
		}
		if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != ownersGroup {
			t.Fatalf("certificate organization: want [%s], got %v", ownersGroup, leaf.Subject.Organization)
		}
		life := leaf.NotAfter.Sub(leaf.NotBefore)
		if life < time.Hour || life > time.Hour+5*time.Minute {
			t.Fatalf("certificate lifetime: want 1h to 1h05m, got %s", life)
		}
	}

	for _, ttl := range []int{59, 86401} {
		code, _, raw := post("getKubeconfig", acct.Token(t, "getKubeconfig"), fmt.Sprintf(`{"name":"tenant","ttlSeconds":%d}`, ttl))
		if code != http.StatusBadRequest || !strings.Contains(string(raw), "InvalidRequest") {
			t.Fatalf("ttlSeconds %d: want 400 InvalidRequest, got %d %.200s", ttl, code, raw)
		}
	}
	if code, _, raw := post("getKubeconfig", acct.Token(t, "listClusters"), `{"name":"tenant"}`); code != http.StatusUnauthorized {
		t.Fatalf("a token for another lxm: want 401, got %d %.200s", code, raw)
	}

	bin := filepath.Join(n.Dir, "bin")
	if r := n.h.As(ctx, "mkdir", "-p", "-m", "0755", bin); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := n.h.As(ctx, "/bin/sh", "-c", "PATH="+bin+" command -v socialweb-credential"); strings.TrimSpace(r.Stdout) != "" {
		t.Fatalf("socialweb-credential is on the test PATH: %s", r.Stdout)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfigText))
	if err != nil {
		t.Fatal(err)
	}
	restCfg.Timeout = 15 * time.Second
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	kubectlOK := true
	if r := n.h.As(ctx, "test", "-x", kubectlBinary); r.Code != 0 {
		kubectlOK = false
		t.Logf("kubectl is not at %s: the kubectl reading is skipped, the client-go reading still runs", kubectlBinary)
	}
	runKubectl := func() Result {
		return n.h.As(ctx, "env", "PATH="+bin, kubectlBinary, "--kubeconfig", kc, "--request-timeout=15s", "get", "nodes", "-o", "name")
	}
	if kubectlOK {
		if r := runKubectl(); r.Code != 0 {
			t.Fatalf("kubectl get nodes with the certificate kubeconfig: exit %d: %s", r.Code, r.Tail(5))
		}
	}
	if _, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("client-go Nodes().List with the certificate kubeconfig: %v", err)
	}

	applyBody := map[string]any{
		"cluster": "tenant", "namespace": "default", "name": "it-owner-credential", "fieldManager": "kubectl", "force": false,
		"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "it-owner-credential", "namespace": "default"},
			"data":     map[string]any{"from": "kubectlApplyServerSide"},
		},
	}
	if out, code, err := postJSON(gw, "http://"+addr+"/xrpc/"+k8sNSID+"kubectlApplyServerSide", acct.TokenFor(t, k8sNSID+"kubectlApplyServerSide"), applyBody); err != nil || code != http.StatusOK {
		t.Fatalf("kubectlApplyServerSide: %d %v %v", code, out, err)
	}
	if cm, err := tn.Kube.CoreV1().ConfigMaps("default").Get(ctx, "it-owner-credential", metav1.GetOptions{}); err != nil || cm.Data["from"] != "kubectlApplyServerSide" {
		t.Fatalf("the ConfigMap applied through the gateway is not in the tenant cluster: %v %v", cm, err)
	}

	revoked := time.Now()
	if code, _, raw := post("revokeIdentity", acct.Token(t, "revokeIdentity"), `{"name":"tenant"}`); code != http.StatusOK {
		t.Fatalf("revokeIdentity: %d %.200s", code, raw)
	}
	for deadline := revoked.Add(revokeRefusalTime); ; {
		if herr := n.health(); herr != nil {
			t.Fatalf("waiting for the revoked certificate to be refused: %v", herr)
		}
		_, lerr := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		refused := lerr != nil && (apierrors.IsUnauthorized(lerr) || apierrors.IsForbidden(lerr))
		if kubectlOK && refused {
			refused = runKubectl().Code != 0
		}
		if refused {
			t.Logf("the revoked certificate was refused %.2fs after revokeIdentity", time.Since(revoked).Seconds())
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the certificate was not refused within %s of revokeIdentity: %v", revokeRefusalTime, lerr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	gwDir := n.GatewayDir()
	r := n.h.As(ctx, "grep", "-rl", "-e", "PRIVATE KEY", "-e", "client-key-data", filepath.Join(gwDir, "logs"), n.runsDir())
	if r.Code > 1 {
		t.Fatalf("the leak scan failed: exit %d: %s", r.Code, r.Tail(3))
	}
	if strings.TrimSpace(r.Stdout) != "" {
		t.Fatalf("a private key leaked into: %s", r.Stdout)
	}
}

func waitOwnerGrant(t testing.TB, n *Network, tn *Tenant) {
	t.Helper()
	for deadline := time.Now().Add(tenantReady); ; time.Sleep(time.Second) {
		_, err := tn.Kube.RbacV1().ClusterRoleBindings().Get(context.Background(), "socialweb-owner", metav1.GetOptions{})
		if err == nil {
			return
		}
		if herr := n.health(); herr != nil {
			t.Fatalf("waiting for the tenant grant: %v", herr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tenant grant (ClusterRoleBinding socialweb-owner) was not applied within %s: %v", tenantReady, err)
		}
	}
}
