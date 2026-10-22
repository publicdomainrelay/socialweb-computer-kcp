//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const k8sNSID = "computer.socialweb.k8s."

type k8sXRPC struct {
	t       *testing.T
	addr    string
	acct    *pdsAccount
	aud     string
	cluster string
	gw      *http.Client
}

func (k *k8sXRPC) apply(in map[string]any) (map[string]any, int) {
	k.t.Helper()
	in["cluster"] = k.cluster
	out, code, err := postJSON(k.gw, "http://"+k.addr+"/xrpc/"+k8sNSID+"kubectlApplyServerSide",
		k.acct.TokenFor(k.t, k.aud, k8sNSID+"kubectlApplyServerSide"), in)
	if err != nil {
		k.t.Fatalf("kubectlApplyServerSide: %v", err)
	}
	return out, code
}

func (k *k8sXRPC) job(ns, name string) (map[string]any, int) {
	k.t.Helper()
	q := url.Values{"cluster": {k.cluster}, "namespace": {ns}, "name": {name}, "tailLines": {"20"}}
	out, code, err := postJSON(k.gw, "http://"+k.addr+"/xrpc/"+k8sNSID+"getJob?"+q.Encode(),
		k.acct.TokenFor(k.t, k.aud, k8sNSID+"getJob"), nil)
	if err != nil {
		k.t.Fatalf("getJob: %v", err)
	}
	return out, code
}

func runJobThroughXRPC(t *testing.T, n *Network, tn *Tenant, cluster, ns, node string) {
	ctx := context.Background()
	k := &k8sXRPC{t: t, addr: n.xrpcAddress(t), acct: n.OwnerAccount(t), aud: n.gatewayAudience(t), cluster: cluster,
		gw: &http.Client{Timeout: 60 * time.Second}}

	cm := func(level string) map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "it-app-config", "namespace": ns},
			"data":     map[string]any{"environment": "production", "log_level": level},
		}
	}
	out, code := k.apply(map[string]any{"namespace": ns, "name": "it-app-config", "fieldManager": "kubectl", "force": false, "manifest": cm("info")})
	if code != http.StatusOK || out["uid"] == "" || out["kind"] != "ConfigMap" {
		t.Fatalf("apply ConfigMap: %d %v", code, out)
	}
	got, err := tn.Kube.CoreV1().ConfigMaps(ns).Get(ctx, "it-app-config", metav1.GetOptions{})
	if err != nil || got.Data["log_level"] != "info" || got.Data["environment"] != "production" {
		t.Fatalf("the applied ConfigMap is not in the cluster: %v %v", got, err)
	}
	if out, code = k.apply(map[string]any{"fieldManager": "other", "manifest": cm("debug")}); code != http.StatusConflict || out["error"] != "ApplyConflict" {
		t.Fatalf("a second field manager changing log_level without force: want 409 ApplyConflict, got %d %v", code, out)
	}
	if out, code = k.apply(map[string]any{"fieldManager": "other", "force": true, "manifest": cm("debug")}); code != http.StatusOK {
		t.Fatalf("apply with force: %d %v", code, out)
	}
	if got, err = tn.Kube.CoreV1().ConfigMaps(ns).Get(ctx, "it-app-config", metav1.GetOptions{}); err != nil || got.Data["log_level"] != "debug" {
		t.Fatalf("force did not take log_level: %v %v", got, err)
	}
	if out, code = k.apply(map[string]any{"manifest": map[string]any{"apiVersion": "example.invalid/v1", "kind": "Nope", "metadata": map[string]any{"name": "x"}}}); code != http.StatusBadRequest || out["error"] != "KindNotFound" {
		t.Fatalf("an unknown kind: want 400 KindNotFound, got %d %v", code, out)
	}

	marker := fmt.Sprintf("it-xrpc-job-ok-%d", time.Now().UnixNano())
	job := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "it-xrpc-job", "namespace": ns},
		"spec": map[string]any{
			"backoffLimit": 0,
			"template": map[string]any{
				"spec": map[string]any{
					"nodeName":                      node,
					"restartPolicy":                 "Never",
					"terminationGracePeriodSeconds": 0,
					"containers": []any{map[string]any{
						"name": "run", "image": probeImage,
						"command":   []any{"sh", "-c", "echo " + marker + "; uname -m"},
						"resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "16Mi"}},
					}},
				},
			},
		},
	}
	started := time.Now()
	if out, code = k.apply(map[string]any{"manifest": job}); code != http.StatusOK || out["kind"] != "Job" {
		t.Fatalf("apply Job: %d %v", code, out)
	}
	defer func() {
		bg := metav1.DeletePropagationBackground
		if err := tn.Kube.BatchV1().Jobs(ns).Delete(ctx, "it-xrpc-job", metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
			t.Errorf("deleting the Job: %v", err)
		}
	}()

	var st map[string]any
	for deadline := time.Now().Add(probeTimeout); ; {
		st, code = k.job(ns, "it-xrpc-job")
		if code != http.StatusOK {
			t.Fatalf("getJob: %d %v", code, st)
		}
		if st["phase"] == "Succeeded" || st["phase"] == "Failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Job did not finish within %s: %v", probeTimeout, st)
		}
		time.Sleep(time.Second)
	}
	if st["phase"] != "Succeeded" {
		t.Fatalf("the Job failed: %v", st)
	}
	pods, _ := st["pods"].([]any)
	if len(pods) != 1 {
		t.Fatalf("getJob returned %d pods, want 1: %v", len(pods), st)
	}
	pod, _ := pods[0].(map[string]any)
	containers, _ := pod["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("getJob returned %d containers, want 1: %v", len(containers), pod)
	}
	c, _ := containers[0].(map[string]any)
	if c["state"] != "Terminated" || c["exitCode"] != float64(0) || !strings.Contains(fmt.Sprint(c["log"]), marker) {
		t.Fatalf("the Job's container: want Terminated, exit 0 and the marker in its log, got %v", c)
	}
	if out, code = k.job(ns, "it-no-such-job"); code != http.StatusNotFound || out["error"] != "ObjectNotFound" {
		t.Fatalf("getJob for a missing Job: want 404 ObjectNotFound, got %d %v", code, out)
	}
	t.Logf("xrpc: ConfigMap applied (conflict without force, taken with force); Job applied and Succeeded +%.1fs on %s; log %q",
		time.Since(started).Seconds(), pod["node"], strings.TrimSpace(fmt.Sprint(c["log"])))
}
