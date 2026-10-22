//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	clusterNSID   = "com.publicdomainrelay.temp.cluster."
	streamCluster = "demo"
	streamPod     = "it-stream"
)

var (
	exposureGVR   = schema.GroupVersionResource{Group: "exposure.socialweb.computer", Version: "v1alpha1", Resource: "exposures"}
	gatewayAPIGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "gatewayapis"}
)

type streamEvent struct {
	Type    string
	Payload map[string]any
}

func (e streamEvent) str(k string) string { s, _ := e.Payload[k].(string); return s }

func (e streamEvent) String() string {
	b, _ := json.Marshal(e.Payload)
	return e.Type + " " + string(b)
}

type pdsAccount struct {
	pds        string
	client     *http.Client
	access     string
	DID        string
	identifier string
	password   string
}

func (n *Network) workspaceClient(t testing.TB, app string) dynamic.Interface {
	t.Helper()
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	ws := "root:" + app
	if a, ok := n.App(app); ok && a.Workspace != "" {
		ws = a.Workspace
	}
	cfg.Host = strings.TrimSuffix(strings.SplitN(cfg.Host, "/clusters/", 2)[0], "/") + "/clusters/" + ws
	cfg.Timeout = 15 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dyn
}

func (n *Network) xrpcAddress(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dyn := n.workspaceClient(t, "gateway")
	var last error
	for {
		u, err := dyn.Resource(exposureGVR).Get(ctx, "svc-gatewayapi-gateway-api-local", metav1.GetOptions{})
		if err == nil {
			if endpoint, _, _ := unstructured.NestedString(u.Object, "status", "endpoint"); endpoint != "" {
				return endpoint
			}
			if raw, _, _ := unstructured.NestedString(u.Object, "status", "url"); raw != "" {
				if parsed, perr := url.Parse(raw); perr == nil && parsed.Host != "" {
					return parsed.Host
				}
			}
		}
		last = err
		if ctx.Err() != nil {
			t.Fatalf("the gateway API's local Exposure reported no endpoint within 30s: %v", last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (n *Network) gatewayAudience(t testing.TB) string {
	t.Helper()
	dyn := n.workspaceClient(t, "gateway")
	u, err := dyn.Resource(gatewayAPIGVR).Get(context.Background(), "gateway-api", metav1.GetOptions{})
	if err == nil {
		if aud, _, _ := unstructured.NestedString(u.Object, "spec", "audience"); aud != "" {
			return aud
		}
		if list, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "serviceAuth", "audience"); len(list) > 0 && list[0] != "" {
			return list[0]
		}
		if did, _, _ := unstructured.NestedString(u.Object, "spec", "identity", "did"); did != "" {
			return did
		}
	}
	if a, ok := n.App("gateway"); ok && a.Did != "" {
		return a.Did
	}
	t.Fatalf("the gateway API names no service-auth audience: %v", err)
	return ""
}

func postJSON(c *http.Client, u, bearer string, in any) (map[string]any, int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(b)
	}
	method := http.MethodPost
	if in == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"raw": string(raw)}
	}
	return out, resp.StatusCode, nil
}

func (n *Network) OwnerAccount(t testing.TB) *pdsAccount {
	t.Helper()
	st := n.Up.Network.Status
	if st.Network == nil || st.Network.CaBundleRef == "" {
		t.Fatal("the market network reports no CA bundle")
	}
	ca, err := n.read(st.Network.CaBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatalf("the CA bundle %s does not parse", st.Network.CaBundleRef)
	}
	raw, err := n.read(filepath.Join(n.GatewayDir(), "owner-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sessions map[string]struct {
		Service    string `json:"service"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("the gateway's owner-sessions.json does not hold one account: %d %v", len(sessions), err)
	}
	a := &pdsAccount{client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}}
	for did, s := range sessions {
		a.DID, a.pds, a.identifier, a.password = did, strings.TrimSuffix(s.Service, "/"), s.Identifier, s.Password
		out, code, err := postJSON(a.client, a.pds+"/xrpc/com.atproto.server.createSession", "",
			map[string]string{"identifier": s.Identifier, "password": s.Password})
		if err != nil || code != http.StatusOK {
			t.Fatalf("createSession for the owner %s: %d %v", did, code, err)
		}
		a.access, _ = out["accessJwt"].(string)
	}
	return a
}

func (a *pdsAccount) Fresh(t testing.TB) *pdsAccount {
	t.Helper()
	suffix := "test"
	handle := fmt.Sprintf("fresh%d.%s", time.Now().UnixNano()%1000000, suffix)
	pw := fmt.Sprintf("pw-%d", time.Now().UnixNano())
	out, code, err := postJSON(a.client, a.pds+"/xrpc/com.atproto.server.createAccount", "",
		map[string]string{"handle": handle, "email": handle + "@example.test", "password": pw})
	if err != nil || code != http.StatusOK {
		t.Fatalf("createAccount %s on %s: %d %v %v", handle, a.pds, code, out, err)
	}
	f := &pdsAccount{client: a.client, pds: a.pds}
	f.DID, _ = out["did"].(string)
	f.access, _ = out["accessJwt"].(string)
	if f.DID == "" || f.access == "" {
		t.Fatalf("createAccount %s returned no did or session: %v", handle, out)
	}
	return f
}

func (a *pdsAccount) Token(t testing.TB, aud, method string) string {
	t.Helper()
	return a.TokenFor(t, aud, clusterNSID+method)
}

func (a *pdsAccount) TokenFor(t testing.TB, aud, lxm string) string {
	t.Helper()
	q := url.Values{"aud": {aud}, "lxm": {lxm}}
	out, code, err := postJSON(a.client, a.pds+"/xrpc/com.atproto.server.getServiceAuth?"+q.Encode(), a.access, nil)
	tok, _ := out["token"].(string)
	if err != nil || code != http.StatusOK || tok == "" {
		t.Fatalf("getServiceAuth %s: %d %v %v", lxm, code, out, err)
	}
	return tok
}

type subscription struct {
	t      testing.TB
	conn   *websocket.Conn
	events chan streamEvent
	ended  chan error
}

func subscribe(t testing.TB, addr, token, name string) *subscription {
	t.Helper()
	u := "ws://" + addr + "/xrpc/" + clusterNSID + "subscribeClusters"
	if name != "" {
		u += "?name=" + url.QueryEscape(name)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %d %v", u, code, err)
	}
	s := &subscription{t: t, conn: conn, events: make(chan streamEvent, 1024), ended: make(chan error, 1)}
	t.Cleanup(func() { conn.Close() })
	go s.read()
	return s
}

func (s *subscription) read() {
	for {
		kind, b, err := s.conn.ReadMessage()
		if err != nil {
			s.ended <- err
			close(s.events)
			return
		}
		if kind != websocket.BinaryMessage {
			s.ended <- fmt.Errorf("a %d frame; atproto event streams send binary frames only", kind)
			close(s.events)
			return
		}
		dec := cbor.NewDecoder(bytes.NewReader(b))
		var head struct {
			Op int64  `cbor:"op"`
			T  string `cbor:"t"`
		}
		var body map[string]any
		if err := dec.Decode(&head); err != nil {
			s.ended <- fmt.Errorf("the frame header: %w", err)
			close(s.events)
			return
		}
		if err := dec.Decode(&body); err != nil {
			s.ended <- fmt.Errorf("the frame payload: %w", err)
			close(s.events)
			return
		}
		switch head.Op {
		case -1:
			s.events <- streamEvent{Type: "error", Payload: body}
		case 1:
			s.events <- streamEvent{Type: strings.TrimPrefix(head.T, "#"), Payload: body}
		}
	}
}

func (s *subscription) until(what string, timeout time.Duration, done func(streamEvent) bool, fail func(streamEvent) error) []streamEvent {
	s.t.Helper()
	var seen []streamEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				s.t.Fatalf("%s: the stream ended: %v (seen %v)", what, <-s.ended, seen)
			}
			s.t.Logf("stream: %s", ev)
			seen = append(seen, ev)
			if done(ev) {
				return seen
			}
			if ev.Type == "error" {
				s.t.Fatalf("%s: error frame %s", what, ev)
			}
			if fail != nil {
				if err := fail(ev); err != nil {
					s.t.Fatalf("%s: %v", what, err)
				}
			}
		case <-deadline:
			s.t.Fatalf("%s: not seen within %s (seen %d events)", what, timeout, len(seen))
		}
	}
}

func phases(evs []streamEvent, typ string) []string {
	var out []string
	for _, e := range evs {
		if e.Type == typ && (len(out) == 0 || out[len(out)-1] != e.str("phase")) {
			out = append(out, e.str("phase"))
		}
	}
	return out
}

func failedClaim(ev streamEvent) error {
	if ev.Type == "nodeClaim" && (ev.str("phase") == string(v1alpha1.NodeClaimFailed) || ev.str("phase") == string(v1alpha1.NodeClaimLost)) {
		return fmt.Errorf("NodeClaim %s is %s: %s", ev.str("name"), ev.str("phase"), ev.str("message"))
	}
	return nil
}

func TestClusterEventsStreamFromSubscribeClusters(t *testing.T) {
	t.Parallel()
	n := SharedNetwork(t, NetworkOptions{Bidders: 2, WithGateway: true, UpTimeout: 5 * time.Minute})
	addr := n.xrpcAddress(t)
	acct := n.OwnerAccount(t)
	aud := n.gatewayAudience(t)
	gw := &http.Client{Timeout: 6 * time.Minute}

	s := subscribe(t, addr, acct.Token(t, aud, "subscribeClusters"), "")
	snap := s.until("the snapshot", 30*time.Second, func(ev streamEvent) bool {
		return ev.Type == "info" && ev.str("name") == "SnapshotComplete"
	}, nil)
	if len(snap) < 2 || snap[0].Type != "cluster" || snap[0].str("name") != "tenant" || snap[0].Payload["snapshot"] != true {
		t.Fatalf("the snapshot does not start with the bootstrap cluster tenant: %v", snap)
	}
	tenantUp := func(ev streamEvent) bool {
		return ev.Type == "cluster" && ev.str("name") == "tenant" && ev.str("phase") == string(v1alpha1.ClusterReady)
	}
	tenantWasReady := tenantUp(snap[0])

	fresh := acct.Fresh(t)
	empty := subscribe(t, addr, fresh.Token(t, aud, "subscribeClusters"), "")
	emptySnap := empty.until("an empty snapshot for an account with no clusters", 30*time.Second, func(ev streamEvent) bool {
		return ev.Type == "info" && ev.str("name") == "SnapshotComplete"
	}, nil)
	for _, ev := range emptySnap {
		if ev.Type == "error" || ev.Type == "cluster" {
			t.Fatalf("an account with no clusters got %s before SnapshotComplete: %v", ev.Type, ev.Payload)
		}
	}

	if out, code, err := postJSON(gw, "http://"+addr+"/xrpc/"+clusterNSID+"createCluster", acct.Token(t, aud, "createCluster"),
		map[string]string{"name": streamCluster}); err != nil || code != http.StatusOK {
		t.Fatalf("createCluster: %d %v %v", code, out, err)
	}
	created := time.Now()

	unknown := subscribe(t, addr, acct.Token(t, aud, "subscribeClusters"), "nope")
	unknown.until("ClusterNotFound for an unknown name", 15*time.Second, func(ev streamEvent) bool {
		return ev.Type == "error" && ev.str("error") == "ClusterNotFound"
	}, nil)

	up := s.until("cluster "+streamCluster+" Ready", tenantReady, func(ev streamEvent) bool {
		return ev.Type == "cluster" && ev.str("name") == streamCluster && ev.str("phase") == string(v1alpha1.ClusterReady)
	}, nil)
	var demo []streamEvent
	members := map[string]bool{}
	for _, e := range up {
		if e.str("name") == streamCluster || e.str("cluster") == streamCluster {
			demo = append(demo, e)
		}
		if e.Type == "member" && e.str("cluster") == streamCluster {
			members[e.str("member")] = true
		}
	}
	t.Logf("cluster %s Ready %.1fs after createCluster; phases %v; members %v", streamCluster,
		time.Since(created).Seconds(), phases(demo, "cluster"), members)
	if got := strings.Join(phases(demo, "cluster"), ","); !strings.Contains(got, string(v1alpha1.ClusterInitializing)) ||
		strings.Index(got, string(v1alpha1.ClusterInitializing)) > strings.LastIndex(got, string(v1alpha1.ClusterReady)) {
		t.Errorf("cluster %s did not report Initializing before Ready: %s", streamCluster, got)
	}
	if !tenantWasReady {
		firstMember, tenantReadyAt := -1, -1
		for i, e := range up {
			if firstMember < 0 && e.Type == "member" && e.str("cluster") == streamCluster {
				firstMember = i
			}
			if tenantReadyAt < 0 && tenantUp(e) {
				tenantReadyAt = i
			}
		}
		if firstMember < 0 || (tenantReadyAt >= 0 && tenantReadyAt < firstMember) {
			t.Errorf("cluster %s started only after the bootstrap cluster was Ready (first member event %d, tenant Ready %d): clusters are brought up one at a time",
				streamCluster, firstMember, tenantReadyAt)
		}
		t.Logf("parallel bring-up: %s first member event at index %d, tenant Ready at index %d", streamCluster, firstMember, tenantReadyAt)
	}
	if len(members) == 0 {
		t.Errorf("no #member events while the control plane of %s came up", streamCluster)
	}

	tn := n.Tenant(t, streamCluster, 30*time.Second)
	ctx := context.Background()
	core := tn.Kube.CoreV1()
	if _, err := core.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsServer}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := core.ServiceAccounts(nsServer).Get(ctx, "default", metav1.GetOptions{}); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the default ServiceAccount of %s: %v", nsServer, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	scaleOut := time.Now()
	if _, err := core.Pods(nsServer).Create(ctx, serverPod(streamPod, "", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	out := s.until("a NodeClaim Running", scaleOutTimeout, func(ev streamEvent) bool {
		return ev.Type == "nodeClaim" && ev.str("phase") == string(v1alpha1.NodeClaimRunning)
	}, failedClaim)
	claimPhases := phases(out, "nodeClaim")
	t.Logf("NodeClaim %s %.1fs after the pod; phases %v", claimPhases[len(claimPhases)-1], time.Since(scaleOut).Seconds(), claimPhases)
	for _, want := range []v1alpha1.NodeClaimPhase{v1alpha1.NodeClaimBidding, v1alpha1.NodeClaimProvisioning} {
		if !strings.Contains(strings.Join(claimPhases, ","), string(want)) {
			t.Errorf("the stream showed no NodeClaim %s before it joined: %v", want, claimPhases)
		}
	}
	for deadline := time.Now().Add(probeTimeout); ; {
		p, err := core.Pods(nsServer).Get(ctx, streamPod, metav1.GetOptions{})
		if err == nil && podReady(p) {
			t.Logf("pod %s Ready on %s %.1fs after it was created", streamPod, p.Spec.NodeName, time.Since(scaleOut).Seconds())
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s not Ready within %s of its NodeClaim joining: %s", streamPod, probeTimeout, tn.Summary(ctx))
		}
		time.Sleep(time.Second)
	}

	if err := core.Pods(nsServer).Delete(ctx, streamPod, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
		t.Fatal(err)
	}
	scaleIn := time.Now()
	s.until("the NodeClaim gone after scale to zero", scaleInTimeout, func(ev streamEvent) bool {
		return ev.Type == "nodeClaim" && ev.Payload["deleted"] == true
	}, nil)
	t.Logf("NodeClaim deleted %.1fs after the pod", time.Since(scaleIn).Seconds())
}
