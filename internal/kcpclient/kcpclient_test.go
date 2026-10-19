package kcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

const (
	testHost    = "https://kcp.test:6443"
	testDID     = "did:plc:testaccount0000000000000000"
	testHash    = "3hfr4o24"
	testCluster = "web"
	accountPath = "root:" + testHash
	homePath    = "root:" + testHash + ":web"
	mountPath   = "root:" + testHash + ":web:clusters"

	mountChildURL = "https://127.0.0.1:20003"

	saName = "did-mruwiotqnrrtu5dfon2gcy3dn52w45bqgaydambqgaydambqgaydambq"

	saPath   = "/clusters/" + mountPath + "/api/v1/namespaces/users/serviceaccounts/" + saName
	dbHome   = "root:" + testHash + ":db"
	dbSAPath = "/clusters/" + dbHome + ":clusters/api/v1/namespaces/users/serviceaccounts/" + saName
)

type request struct {
	Method string
	Path   string
	Body   string
}

func (r request) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		t.Fatalf("%s %s: body is not JSON: %v (%s)", r.Method, r.Path, err, r.Body)
	}
	return out
}

func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("dig %v: %v is not an object", path, v)
		}
		v, ok = m[p]
		if !ok {
			t.Fatalf("dig %v: no %q in %v", path, p, m)
		}
	}
	return v
}

type fakeKCP struct {
	t        *testing.T
	mu       sync.Mutex
	requests []request
	respond  func(request) (int, any)

	transportErr func(request) error
}

func (f *fakeKCP) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	var err error
	if r.Body != nil {
		body, err = io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("read request body: %v", err)
		}
	}
	req := request{Method: r.Method, Path: r.URL.Path, Body: string(body)}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.transportErr != nil {
		if err := f.transportErr(req); err != nil {
			return nil, err
		}
	}

	status, obj := f.respond(req)
	if status == 0 {
		f.t.Fatalf("unhandled request %s %s", req.Method, req.Path)
	}
	var encoded []byte
	if s, ok := obj.(string); ok {
		encoded = []byte(s)
	} else if obj != nil {
		encoded, err = json.Marshal(obj)
		if err != nil {
			f.t.Fatalf("marshal response: %v", err)
		}
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
		Request:    r,
	}, nil
}

func (f *fakeKCP) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func (f *fakeKCP) hasMethod(method string) bool {
	for _, r := range f.seen() {
		if r.Method == method {
			return true
		}
	}
	return false
}

func statusBody(reason string, code int) map[string]any {
	return map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "code": code, "message": reason,
	}
}

func notFound() (int, any) { return http.StatusNotFound, statusBody("NotFound", http.StatusNotFound) }
func alreadyExists() (int, any) {
	return http.StatusConflict, statusBody("AlreadyExists", http.StatusConflict)
}

func createdBody(t *testing.T, req request) map[string]any {
	t.Helper()
	object := req.json(t)
	if strings.HasSuffix(req.Path, "/atprotoidentities") {
		return object
	}
	meta := dig(t, object, "metadata").(map[string]any)
	if strings.HasSuffix(req.Path, "/clusters") {
		return clusterBody(meta["name"].(string), "", "", "", false)
	}
	return workspaceBody(meta["name"].(string), false)
}

func workspaceBody(name string, mounted bool) map[string]any {
	spec := map[string]any{}
	if mounted {
		spec["mount"] = map[string]any{"ref": map[string]any{
			"apiVersion": "socialweb.computer/v1alpha1",
			"kind":       "Cluster",
			"name":       name,
		}}
	}
	return map[string]any{
		"kind": "Workspace", "apiVersion": "tenancy.kcp.io/v1alpha1",
		"metadata": map[string]any{"name": name},
		"spec":     spec,
	}
}

func readyWorkspaceBody(name string) map[string]any {
	b := workspaceBody(name, false)
	b["status"] = map[string]any{"phase": string(corev1alpha1.LogicalClusterPhaseReady)}
	return b
}

func clusterBody(name, phase, url, instance string, running bool) map[string]any {
	return map[string]any{
		"kind": "Cluster", "apiVersion": "socialweb.computer/v1alpha1",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"account": testDID, "running": running},
		"status":   map[string]any{"phase": phase, "URL": url, "instance": instance},
	}
}

func identityBody(did string, scope v1alpha1.IdentityScope, revoked bool) map[string]any {
	return map[string]any{
		"kind": "AtprotoIdentity", "apiVersion": "socialweb.computer/v1alpha1",
		"metadata": map[string]any{"name": AccountHash(did)},
		"spec":     map[string]any{"did": did, "scope": string(scope), "revoked": revoked},
	}
}

func workspaceList(names ...string) map[string]any {
	items := make([]any, 0, len(names))
	for _, n := range names {
		items = append(items, workspaceBody(n, false))
	}
	return map[string]any{
		"kind": "WorkspaceList", "apiVersion": "tenancy.kcp.io/v1alpha1",
		"items": items,
	}
}

func serviceAccountBody(name string) map[string]any {
	return map[string]any{
		"kind": "ServiceAccount", "apiVersion": "v1",
		"metadata": map[string]any{"name": name, "namespace": "users"},
	}
}

func newNode(name, cluster string, phase v1alpha1.NodePhase) v1alpha1.Node {
	return v1alpha1.Node{
		TypeMeta:   metav1.TypeMeta{APIVersion: "socialweb.computer/v1alpha1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.NodeSpec{Cluster: cluster},
		Status:     v1alpha1.NodeStatus{Phase: phase},
	}
}

func nodeListBody(nodes ...v1alpha1.Node) map[string]any {
	out := map[string]any{
		"kind": "NodeList", "apiVersion": "socialweb.computer/v1alpha1",
		"items": []any{},
	}
	items := out["items"].([]any)
	for _, n := range nodes {
		b, err := json.Marshal(n)
		if err != nil {
			panic(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			panic(err)
		}
		items = append(items, m)
	}
	out["items"] = items
	return out
}

func newTestClient(t *testing.T, f *fakeKCP) *Client {
	t.Helper()
	return newTestClientWith(t, f, nil)
}

func newTestClientWith(t *testing.T, f *fakeKCP, keys KubeconfigGranter) *Client {
	t.Helper()
	c, err := New(Options{Host: testHost, Transport: f, Kubeconfigs: keys})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestEnsureRefusesWithoutAPermit(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet && strings.Contains(req.Path, "/atprotoidentities/") {
			return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, true)
		}
		return http.StatusOK, workspaceBody(testHash, false)
	}}
	c := newTestClient(t, f)

	err := c.Ensure(context.Background(), gateway.Caller(testDID), testCluster)
	if err == nil {
		t.Fatal("Ensure created the tree for a REVOKED identity")
	}
	if !strings.Contains(err.Error(), revocation.ErrRevoked.Error()) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	if f.hasMethod(http.MethodPost) {
		t.Fatal("a revoked identity reached a create: revocation is a ten-second interruption again")
	}
}

func fastWait(t *testing.T) {
	t.Helper()
	poll, timeout := workspaceReadyPoll, workspaceReadyTimeout
	workspaceReadyPoll, workspaceReadyTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		workspaceReadyPoll, workspaceReadyTimeout = poll, timeout
	})
}

type statefulKCP struct {
	t      *testing.T
	mu     sync.Mutex
	stored map[string]map[string]any

	phase func(name string, read int) corev1alpha1.LogicalClusterPhaseType

	clusterURL func(name string) string

	n           int
	wsReads     map[string]int
	readyAt     map[string]int
	acctWriteAt int
	homeWriteAt int
}

func newStatefulKCP(t *testing.T) *statefulKCP {
	t.Helper()
	return &statefulKCP{
		t:       t,
		stored:  map[string]map[string]any{},
		wsReads: map[string]int{},
		readyAt: map[string]int{},
	}
}

func splitResource(path string) (resource, name string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	last := parts[len(parts)-1]
	switch {
	case strings.Contains(path, "/apis/tenancy.kcp.io/"):
		if last == "workspaces" {
			return "workspaces", ""
		}
		return "workspaces", last
	case last == "atprotoidentities", last == "clusters":
		return last, ""
	default:
		return parts[len(parts)-2], last
	}
}

func withPhase(obj map[string]any, phase corev1alpha1.LogicalClusterPhaseType) map[string]any {
	out := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		out[k] = v
	}
	out["status"] = map[string]any{"phase": string(phase)}
	return out
}

func (s *statefulKCP) respond(req request) (int, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	resource, name := splitResource(req.Path)
	if req.Method == http.MethodPost {
		switch {
		case strings.HasPrefix(req.Path, "/clusters/"+accountPath+"/") && s.acctWriteAt == 0:
			s.acctWriteAt = s.n
		case strings.HasPrefix(req.Path, "/clusters/"+homePath+"/") && s.homeWriteAt == 0:
			s.homeWriteAt = s.n
		}
	}
	switch {
	case req.Method == http.MethodGet && name != "":
		obj, ok := s.stored[resource+"\x00"+name]
		if !ok {
			return notFound()
		}
		if resource == "workspaces" {
			s.wsReads[name]++
			phase := corev1alpha1.LogicalClusterPhaseReady
			if s.phase != nil {
				phase = s.phase(name, s.wsReads[name])
			}
			if phase == corev1alpha1.LogicalClusterPhaseReady && s.readyAt[name] == 0 {
				s.readyAt[name] = s.n
			}
			obj = withPhase(obj, phase)
		}
		if resource == "clusters" && s.clusterURL != nil {
			obj = withClusterURL(obj, s.clusterURL(name))
		}
		return http.StatusOK, obj
	case req.Method == http.MethodPatch && name != "":
		obj, ok := s.stored[resource+"\x00"+name]
		if !ok {
			return notFound()
		}
		patch := req.json(s.t)
		merged := make(map[string]any, len(obj)+len(patch))
		for k, v := range obj {
			merged[k] = v
		}
		for k, v := range patch {
			incoming, isObject := v.(map[string]any)
			existing, existingIsObject := merged[k].(map[string]any)
			if !isObject || !existingIsObject {
				merged[k] = v
				continue
			}
			field := make(map[string]any, len(existing)+len(incoming))
			for kk, vv := range existing {
				field[kk] = vv
			}
			for kk, vv := range incoming {
				field[kk] = vv
			}
			merged[k] = field
		}
		s.stored[resource+"\x00"+name] = merged
		return http.StatusOK, merged
	case req.Method == http.MethodPost:
		obj := req.json(s.t)
		created := dig(s.t, obj, "metadata", "name").(string)
		if _, dup := s.stored[resource+"\x00"+created]; dup {
			return alreadyExists()
		}
		s.stored[resource+"\x00"+created] = obj
		return http.StatusCreated, obj
	}
	return 0, nil
}

func withClusterURL(obj map[string]any, url string) map[string]any {
	out := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		out[k] = v
	}
	status, _ := out["status"].(map[string]any)
	merged := make(map[string]any, len(status)+1)
	for k, v := range status {
		merged[k] = v
	}
	merged["URL"] = url
	out["status"] = merged
	return out
}

func (s *statefulKCP) storedObject(t *testing.T, resource, name string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.stored[resource+"\x00"+name]
	if !ok {
		t.Fatalf("nothing named %q was ever created in %s", name, resource)
	}
	return obj
}

func (s *statefulKCP) wrote(resource string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.stored {
		if strings.HasPrefix(k, resource+"\x00") {
			return true
		}
	}
	return false
}

func TestTheSecondLoginIsAdmitted(t *testing.T) {
	fastWait(t)
	s := newStatefulKCP(t)
	c := newTestClient(t, &fakeKCP{t: t, respond: s.respond})
	ctx := context.Background()

	if err := c.Ensure(ctx, gateway.Caller(testDID), testCluster); err != nil {
		t.Fatalf("first login: %v", err)
	}

	state, err := c.Lookup(ctx, testDID)
	if err != nil {
		t.Fatalf("Lookup after a first login: %v", err)
	}
	if state != revocation.DenyGranted {
		t.Fatalf("after a first login Lookup = %q, want %q.\n\nNothing on the creation path "+
			"wrote the account's AtprotoIdentity, so the workspace exists and its deny record "+
			"does not -- RecordMissing, which Admit refuses. Every second login is refused with "+
			"`revocation: account identity record is missing`.", state, revocation.DenyGranted)
	}

	rec := s.storedObject(t, "atprotoidentities", IdentityRecordName(testDID))
	spec := dig(t, rec, "spec").(map[string]any)
	if got := spec["scope"]; got != string(v1alpha1.IdentityScopeAccount) {
		t.Errorf("the granted record's scope = %v, want %q", got, v1alpha1.IdentityScopeAccount)
	}
	if got := spec["did"]; got != testDID {
		t.Errorf("the granted record's did = %v, want %q", got, testDID)
	}
	if got, ok := spec["revoked"]; ok && got != false {
		t.Errorf("the granted record is written revoked=%v. That is the REVOKE's write "+
			"(createDeny), and a first login that wrote it would refuse every later login "+
			"including its own.", got)
	}

	if err := c.Ensure(ctx, gateway.Caller(testDID), testCluster); err != nil {
		t.Fatalf("SECOND login refused: %v -- this is the defect: the record a first login "+
			"must write is what the second login's deny read looks for", err)
	}
}

func TestEnsureWaitsForAWorkspaceBeforeWritingIntoIt(t *testing.T) {
	t.Run("it waits at both levels, then writes", func(t *testing.T) {
		fastWait(t)
		s := newStatefulKCP(t)
		s.phase = func(_ string, read int) corev1alpha1.LogicalClusterPhaseType {
			if read < 3 {
				return corev1alpha1.LogicalClusterPhaseScheduling
			}
			return corev1alpha1.LogicalClusterPhaseReady
		}
		c := newTestClient(t, &fakeKCP{t: t, respond: s.respond})

		if err := c.Ensure(context.Background(), gateway.Caller(testDID), testCluster); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if s.wsReads[testHash] < 3 {
			t.Fatalf("the account workspace was read %d time(s): a first login reads it once "+
				"for the deny and must then keep reading it until it is addressable",
				s.wsReads[testHash])
		}
		if s.readyAt[testHash] == 0 {
			t.Fatal("no read ever observed the account workspace Ready")
		}
		if s.acctWriteAt < s.readyAt[testHash] {
			t.Fatalf("the first write inside the account workspace was request %d and the read "+
				"that saw it Ready was request %d: writing into a workspace that is not "+
				"addressable is what kcp answers with `workspaces.tenancy.kcp.io \"root:<hash>\" "+
				"not found`", s.acctWriteAt, s.readyAt[testHash])
		}
		if s.readyAt[testCluster] == 0 {
			t.Fatalf("home was read %d time(s) and never observed Ready (marks: %+v). A live "+
				"kcp answered `workspaces.tenancy.kcp.io \"root:<hash>:<name>\" not found` for "+
				"exactly this create -- the Cluster CR POSTed into a home that had just been "+
				"created", s.wsReads[testCluster], s.readyAt)
		}
		if s.homeWriteAt < s.readyAt[testCluster] {
			t.Fatalf("the first write inside home was request %d and the read that saw it Ready "+
				"was request %d", s.homeWriteAt, s.readyAt[testCluster])
		}
	})

	t.Run("a workspace that never becomes Ready is a clear refusal", func(t *testing.T) {
		fastWait(t)
		s := newStatefulKCP(t)
		s.phase = func(string, int) corev1alpha1.LogicalClusterPhaseType {
			return corev1alpha1.LogicalClusterPhaseScheduling
		}
		c := newTestClient(t, &fakeKCP{t: t, respond: s.respond})

		err := c.Ensure(context.Background(), gateway.Caller(testDID), testCluster)
		if err == nil {
			t.Fatal("Ensure succeeded against a workspace that never became Ready: the creates " +
				"that follow are refused by kcp, so this reports a tree that was never built")
		}
		if !strings.Contains(err.Error(), "Ready") {
			t.Fatalf("the refusal does not name what was waited for: %v. An operator seeing "+
				"only a deadline cannot tell a scheduling failure from a kcp outage", err)
		}
		if s.wrote("atprotoidentities") {
			t.Fatal("an identity record was written into a workspace kcp could not address")
		}
	})

	t.Run("home is the one that never becomes Ready", func(t *testing.T) {
		fastWait(t)
		s := newStatefulKCP(t)
		s.phase = func(name string, _ int) corev1alpha1.LogicalClusterPhaseType {
			if name == testHash {
				return corev1alpha1.LogicalClusterPhaseReady
			}
			return corev1alpha1.LogicalClusterPhaseScheduling
		}
		c := newTestClient(t, &fakeKCP{t: t, respond: s.respond})

		err := c.Ensure(context.Background(), gateway.Caller(testDID), testCluster)
		if err == nil {
			t.Fatal("Ensure succeeded with home stuck in Scheduling, so the Cluster CR and the " +
				"mounted child were written into a workspace kcp cannot address")
		}
		if !strings.Contains(err.Error(), homePath) {
			t.Fatalf("the refusal does not name the workspace that never scheduled: %v", err)
		}
		if s.wrote("clusters") {
			t.Fatal("the Cluster CR was POSTed into an unaddressable home")
		}
	})
}

func mountChildPosts(t *testing.T, f *fakeKCP) int {
	t.Helper()
	n := 0
	for _, r := range f.seen() {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.Path, "/workspaces") {
			continue
		}
		if name := dig(t, r.json(t), "metadata", "name"); name == ChildLabel {
			n++
		}
	}
	return n
}

func mountChildPatches(t *testing.T, f *fakeKCP) []request {
	t.Helper()
	var out []request
	for _, r := range f.seen() {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/workspaces/"+ChildLabel) {
			out = append(out, r)
		}
	}
	return out
}

func clusterReads(f *fakeKCP) int {
	n := 0
	for _, r := range f.seen() {
		if r.Method == http.MethodGet &&
			strings.HasSuffix(r.Path, "/apis/socialweb.computer/v1alpha1/clusters/"+testCluster) {
			n++
		}
	}
	return n
}

func newTestMountChild(t *testing.T, f *fakeKCP) *MountChild {
	t.Helper()
	m, err := NewMountChild(Options{Host: testHost, Transport: f})
	if err != nil {
		t.Fatalf("NewMountChild: %v", err)
	}
	return m
}

func TestEnsureStopsAtTheClusterCRAndNeverWaitsForAURL(t *testing.T) {
	fastWait(t)
	s := newStatefulKCP(t)
	s.clusterURL = func(string) string { return mountChildURL }
	f := &fakeKCP{t: t, respond: s.respond}

	if err := newTestClient(t, f).Ensure(context.Background(), gateway.Caller(testDID), testCluster); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if n := mountChildPosts(t, f); n != 0 {
		t.Fatalf("Ensure POSTed the mount child %d time(s): %+v. The child carries spec.URL, "+
			"and kcp refuses that field from any identity but a system privileged one -- "+
			"`workspaces.tenancy.kcp.io \"clusters\" is forbidden: spec.URL can only be set by "+
			"system privileged users` -- so a create-on-read path that writes it fails the whole "+
			"login", n, f.seen())
	}
	if n := clusterReads(f); n != 0 {
		t.Fatalf("Ensure read the Cluster CR back %d time(s): %+v. A market cluster's node is "+
			"minutes away, so a create path that re-reads the CR until status.URL appears spends "+
			"its caller's whole budget inside the request -- the door aborts at 30 s, and the "+
			"abort lands on whatever the gateway had not yet written", n, f.seen())
	}
}

func TestTheMountChildIsWrittenByItsOwnWriter(t *testing.T) {
	t.Run("a child that exists with an empty spec.URL is repaired from the CR", func(t *testing.T) {
		s := newStatefulKCP(t)
		s.stored["workspaces\x00"+ChildLabel] = workspaceBody(ChildLabel, true)
		f := &fakeKCP{t: t, respond: s.respond}
		m := newTestMountChild(t, f)

		if err := m.EnsureMountChild(context.Background(), homePath, testCluster, mountChildURL); err != nil {
			t.Fatalf("EnsureMountChild: %v", err)
		}

		patches := mountChildPatches(t, f)
		if len(patches) != 1 {
			t.Fatalf("the mount child kept its empty spec.URL (%d patches): %+v. This is the "+
				"production shape -- the child was built before the CR reported a URL, kcp's "+
				"mount reconciler copied nothing, and the mount serves KCP ITSELF: an admin token "+
				"is accepted, the placeholder Node our provider wrote is listed, and every object "+
				"written to the tenant apiserver is invisible", len(patches), f.seen())
		}
		if got := dig(t, patches[0].json(t), "spec", "URL"); got != mountChildURL {
			t.Fatalf("the repair wrote spec.URL = %v, want the CR's status.URL %q -- a repair "+
				"pointing anywhere else sends the tenant's kubeconfig to the wrong apiserver",
				got, mountChildURL)
		}
		if n := mountChildPosts(t, f); n != 0 {
			t.Fatalf("an existing child was re-created %d time(s) instead of repaired: a "+
				"create cannot set spec.URL on a workspace that already exists, so the child "+
				"would come back with the same empty URL", n)
		}
	})

	t.Run("a child created after the CR reports a URL carries it", func(t *testing.T) {
		s := newStatefulKCP(t)
		f := &fakeKCP{t: t, respond: s.respond}
		m := newTestMountChild(t, f)

		if err := m.EnsureMountChild(context.Background(), homePath, testCluster, mountChildURL); err != nil {
			t.Fatalf("EnsureMountChild: %v", err)
		}
		if n := mountChildPosts(t, f); n != 1 {
			t.Fatalf("the missing child was created %d time(s), want exactly 1: %+v", n, f.seen())
		}
		child := s.storedObject(t, "workspaces", ChildLabel)
		if got := dig(t, child, "spec", "URL"); got != mountChildURL {
			t.Fatalf("the created child's spec.URL = %v, want the CR's status.URL %q. A child "+
				"created without it has no mount target, and kcp will not put one there later",
				got, mountChildURL)
		}
		ref := dig(t, child, "spec", "mount", "ref")
		if got := dig(t, ref, "apiVersion"); got != "socialweb.computer/v1alpha1" {
			t.Errorf("mount ref apiVersion = %v", got)
		}
		if got := dig(t, ref, "kind"); got != "Cluster" {
			t.Errorf("mount ref kind = %v", got)
		}
		if got := dig(t, ref, "name"); got != testCluster {
			t.Errorf("mount ref name = %v, want %q", got, testCluster)
		}
	})

	t.Run("the child is created once and then left alone", func(t *testing.T) {
		s := newStatefulKCP(t)
		f := &fakeKCP{t: t, respond: s.respond}
		m := newTestMountChild(t, f)
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			if err := m.EnsureMountChild(ctx, homePath, testCluster, mountChildURL); err != nil {
				t.Fatalf("EnsureMountChild %d: %v", i+1, err)
			}
		}
		if n := mountChildPosts(t, f); n != 1 {
			t.Fatalf("the mount child was POSTed %d time(s) across three passes, want exactly 1. "+
				"A reconcile runs every pass, so a write that is not idempotent patches the tree "+
				"every 2 s on a live cluster", n)
		}
		if n := len(mountChildPatches(t, f)); n != 0 {
			t.Fatalf("the child already carrying the CR's status.URL was patched %d time(s): the "+
				"repair is for an EMPTY spec.URL, and a pass that rewrites a correct one is a "+
				"write per pass against kcp for nothing", n)
		}
	})

	t.Run("a CR with no status.URL is refused rather than addressed with nothing", func(t *testing.T) {
		s := newStatefulKCP(t)
		f := &fakeKCP{t: t, respond: s.respond}
		m := newTestMountChild(t, f)

		err := m.EnsureMountChild(context.Background(), homePath, testCluster, "")
		if err == nil {
			t.Fatal("the mount child was written from an empty URL: kcp copies spec.URL out of " +
				"the mounted object ONCE, so a child made now keeps an empty URL for ever and " +
				"the mount serves kcp itself")
		}
		if n := mountChildPosts(t, f); n != 0 {
			t.Fatalf("a child was created for an empty URL (%d POST(s)): %+v", n, f.seen())
		}
	})
}

func TestGetMapsTheClusterView(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case strings.HasSuffix(req.Path, "/clusters/web"):
			return http.StatusOK, clusterBody(testCluster, "Ready", "https://127.0.0.1:20443", "inst-7", true)
		case strings.HasSuffix(req.Path, "/nodes"):
			return http.StatusOK, nodeListBody(
				newNode("n1", testCluster, v1alpha1.NodeRunning),
				newNode("n2", testCluster, v1alpha1.NodePending),
				newNode("n3", testCluster, v1alpha1.NodeDraining),
				newNode("other", "another-cluster", v1alpha1.NodeRunning),
			)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	view, ok, err := c.Get(context.Background(), gateway.Caller(testDID), testCluster)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	want := gateway.ClusterView{
		Name: testCluster, Phase: "Ready", URL: "https://127.0.0.1:20443",
		Instance: "inst-7", Running: true,
		NodeCount: 2,
	}
	if view != want {
		t.Fatalf("Get = %+v, want %+v", view, want)
	}
}

func TestGetReportsAbsenceWithoutAnError(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) { return notFound() }}
	c := newTestClient(t, f)

	view, ok, err := c.Get(context.Background(), gateway.Caller(testDID), testCluster)
	if err != nil {
		t.Fatalf("Get on a cluster that does not exist returned an error: %v", err)
	}
	if ok || view != (gateway.ClusterView{}) {
		t.Fatalf("ok=%v view=%+v, want absence", ok, view)
	}
}

func TestListReadsTheAccountChildren(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case strings.HasSuffix(req.Path, "/apis/tenancy.kcp.io/v1alpha1/workspaces"):
			return http.StatusOK, map[string]any{
				"kind": "WorkspaceList", "apiVersion": "tenancy.kcp.io/v1alpha1",
				"items": []any{
					workspaceBody("web", false),
					workspaceBody("db", false),
					workspaceBody(ChildLabel, true),
				},
			}
		case strings.HasSuffix(req.Path, "/clusters/web"):
			return http.StatusOK, clusterBody("web", "Ready", "https://a", "i1", true)
		case strings.HasSuffix(req.Path, "/clusters/db"):
			return http.StatusOK, clusterBody("db", "Unavailable", "https://b", "i2", false)
		case strings.HasSuffix(req.Path, "/nodes"):
			return http.StatusOK, nodeListBody()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	views, err := c.List(context.Background(), gateway.Caller(testDID))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 clusters, got %d: %+v", len(views), views)
	}
	for _, v := range views {
		if v.Name == ChildLabel {
			t.Fatalf("the mounted child was listed as a cluster: %+v", views)
		}
	}
	if views[0].Name != "db" || views[1].Name != "web" {
		t.Fatalf("unexpected order or names: %+v", views)
	}
}

func TestSetRunningOffIsAFieldAndNotADeletion(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case strings.HasSuffix(req.Path, "/clusters/web"):
			return http.StatusOK, clusterBody(testCluster, "Unavailable", "https://127.0.0.1:20443", "inst-7", false)
		case strings.HasSuffix(req.Path, "/nodes"):
			return http.StatusOK, nodeListBody(newNode("n1", testCluster, v1alpha1.NodeRunning))
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	view, err := c.SetRunning(context.Background(), gateway.Caller(testDID), testCluster, false)
	if err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	seen := f.seen()
	for _, r := range seen {
		if r.Method == http.MethodDelete {
			t.Fatalf("SetRunning(false) issued a DELETE (%s): teardown is the ONLY path that "+
				"releases anything, and OFF must retain the store", r.Path)
		}
	}
	if len(seen) != 2 || seen[0].Method != http.MethodPatch {
		t.Fatalf("want one PATCH and one read, got %+v", seen)
	}
	if !strings.HasSuffix(seen[0].Path, "/clusters/web") {
		t.Errorf("patched %s, want the Cluster CR in home", seen[0].Path)
	}
	if got := dig(t, seen[0].json(t), "spec", "running"); got != false {
		t.Errorf(`patch body does not set spec.running=false: %s`, seen[0].Body)
	}
	if view.URL != "https://127.0.0.1:20443" || view.Instance != "inst-7" {
		t.Errorf("OFF cleared cluster identity: %+v", view)
	}
	if view.Running {
		t.Errorf("view still reports running: %+v", view)
	}
}

func TestDeleteIsTheRelease(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodDelete:
			return http.StatusOK, statusBody("Success", http.StatusOK)
		case strings.HasSuffix(req.Path, "/clusters/web"):
			return http.StatusOK, clusterBody(testCluster, "Ready", "https://127.0.0.1:20443", "inst-7", true)
		case strings.HasSuffix(req.Path, "/nodes"):
			return http.StatusOK, nodeListBody(newNode("n1", testCluster, v1alpha1.NodeRunning))
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	view, err := c.Delete(context.Background(), gateway.Caller(testDID), testCluster)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !f.hasMethod(http.MethodDelete) {
		t.Fatal("Delete issued no DELETE: nothing released the port")
	}
	var deleted request
	for _, r := range f.seen() {
		if r.Method == http.MethodDelete {
			deleted = r
		}
	}
	if want := "/clusters/" + homePath + "/apis/socialweb.computer/v1alpha1/clusters/web"; deleted.Path != want {
		t.Errorf("deleted %s, want the Cluster CR at %s", deleted.Path, want)
	}
	if view.Name != testCluster || view.Instance != "inst-7" {
		t.Errorf("the deleted cluster is not described: %+v", view)
	}
}

func TestDenyHasFourStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(request) (int, any)
		want    revocation.DenyState
	}{
		{
			name:    "no account workspace is a first login",
			respond: func(request) (int, any) { return notFound() },
			want:    revocation.DenyFirstLogin,
		},
		{
			name: "an account workspace with no record is TAMPERING, not a first login",
			respond: func(req request) (int, any) {
				if strings.Contains(req.Path, "/atprotoidentities/") {
					return notFound()
				}
				return http.StatusOK, workspaceBody(testHash, false)
			},
			want: revocation.DenyRecordMissing,
		},
		{
			name: "a revoked record is the deny",
			respond: func(req request) (int, any) {
				if strings.Contains(req.Path, "/atprotoidentities/") {
					return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, true)
				}
				return http.StatusOK, workspaceBody(testHash, false)
			},
			want: revocation.DenyRevoked,
		},
		{
			name: "a live account record is granted",
			respond: func(req request) (int, any) {
				if strings.Contains(req.Path, "/atprotoidentities/") {
					return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, false)
				}
				return http.StatusOK, workspaceBody(testHash, false)
			},
			want: revocation.DenyGranted,
		},
		{
			name: "a record in another DID's slot is not this DID's deny",
			respond: func(req request) (int, any) {
				if strings.Contains(req.Path, "/atprotoidentities/") {
					return http.StatusOK, identityBody("did:web:someone.else", v1alpha1.IdentityScopeAccount, false)
				}
				return http.StatusOK, workspaceBody(testHash, false)
			},
			want: revocation.DenyRecordMissing,
		},
		{
			name: "a CLUSTER-scope record in the deny's slot is not the deny",
			respond: func(req request) (int, any) {
				if strings.Contains(req.Path, "/atprotoidentities/") {
					return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeCluster, false)
				}
				return http.StatusOK, workspaceBody(testHash, false)
			},
			want: revocation.DenyRecordMissing,
		},
		{
			name: "an account workspace that is terminating is not a first login",
			respond: func(req request) (int, any) {
				body := workspaceBody(testHash, false)
				body["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-22T12:00:00Z"
				return http.StatusOK, body
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeKCP{t: t, respond: tc.respond}
			c := newTestClient(t, f)
			got, err := c.Lookup(context.Background(), testDID)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("Lookup = %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Lookup = %q, want %q", got, tc.want)
			}
			if tc.want == revocation.DenyRecordMissing && got == revocation.DenyGranted {
				t.Fatal("RecordMissing collapsed into Granted: a deleted deny record now " +
					"grants a fresh first login")
			}
			for _, r := range f.seen() {
				if !strings.HasPrefix(r.Path, "/clusters/root") {
					t.Fatalf("the deny read %s, which is outside root", r.Path)
				}
			}
		})
	}
}

func TestDenyIsConsumedByAdmit(t *testing.T) {
	for _, tc := range []struct {
		state revocation.DenyState
		err   error
	}{
		{revocation.DenyRevoked, revocation.ErrRevoked},
		{revocation.DenyRecordMissing, revocation.ErrDenyRecordMissing},
	} {
		if _, err := revocation.Admit(context.Background(), fixedDeny(tc.state), testDID); !errorIs(err, tc.err) {
			t.Errorf("Admit over %q = %v, want %v", tc.state, err, tc.err)
		}
	}
	for _, state := range []revocation.DenyState{revocation.DenyGranted, revocation.DenyFirstLogin} {
		p, err := revocation.Admit(context.Background(), fixedDeny(state), testDID)
		if err != nil {
			t.Errorf("Admit over %q: %v", state, err)
			continue
		}
		if p.Admitted() != testDID {
			t.Errorf("permit admits %q, want %q", p.Admitted(), testDID)
		}
	}
}

type fixedDeny revocation.DenyState

func (d fixedDeny) Lookup(context.Context, string) (revocation.DenyState, error) {
	return revocation.DenyState(d), nil
}

func errorIs(err, want error) bool {
	return err != nil && want != nil && strings.Contains(err.Error(), want.Error())
}

func denyFixture(t *testing.T, respond func(request) (int, any)) *fakeKCP {
	t.Helper()
	return &fakeKCP{t: t, respond: func(req request) (int, any) {
		if strings.Contains(req.Path, "/workspaces/") {
			return http.StatusOK, workspaceBody(testHash, false)
		}
		return respond(req)
	}}
}

func lastWith(f *fakeKCP, method string) *request {
	var found *request
	for _, r := range f.seen() {
		if r.Method == method {
			rr := r
			found = &rr
		}
	}
	return found
}

func TestWriteDenyPatchesTheAccountRecord(t *testing.T) {
	f := denyFixture(t, func(request) (int, any) {
		return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, false)
	})
	c := newTestClient(t, f)
	if err := c.WriteDeny(context.Background(), testDID); err != nil {
		t.Fatalf("WriteDeny: %v", err)
	}

	patch := lastWith(f, http.MethodPatch)
	if patch == nil {
		t.Fatal("WriteDeny wrote nothing for a live, unrevoked record")
	}
	if !strings.Contains(patch.Path, "/atprotoidentities/") {
		t.Fatalf("the write went to %s; the deny lives on the account's identity record", patch.Path)
	}
	if got := dig(t, patch.json(t), "spec", "revoked"); got != true {
		t.Fatalf("spec.revoked = %v, want true", got)
	}
}

func TestWriteDenyIsIdempotent(t *testing.T) {
	f := denyFixture(t, func(request) (int, any) {
		return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, true)
	})
	c := newTestClient(t, f)
	if err := c.WriteDeny(context.Background(), testDID); err != nil {
		t.Fatalf("WriteDeny: %v", err)
	}
	if f.hasMethod(http.MethodPatch) || f.hasMethod(http.MethodPost) {
		t.Fatalf("an already-revoked account was written again: %v", f.seen())
	}
}

func TestWriteDenyRepairsADeletedDenyRecord(t *testing.T) {
	f := denyFixture(t, func(req request) (int, any) {
		if req.Method == http.MethodPost {
			return http.StatusCreated, createdBody(t, req)
		}
		return notFound()
	})
	c := newTestClient(t, f)
	if err := c.WriteDeny(context.Background(), testDID); err != nil {
		t.Fatalf("WriteDeny: %v", err)
	}

	post := lastWith(f, http.MethodPost)
	if post == nil {
		t.Fatal("a deleted deny record was not repaired")
	}
	if got := dig(t, post.json(t), "spec", "revoked"); got != true {
		t.Fatalf("the repair created the record with revoked = %v, want true -- creating it "+
			"clean and patching it after leaves a window in which the deny READS CLEAN, and "+
			"a door that consults it in that window admits the identity", got)
	}
	if got := dig(t, post.json(t), "spec", "scope"); got != string(v1alpha1.IdentityScopeAccount) {
		t.Fatalf("scope = %v, want the account scope; Lookup treats a record at any other "+
			"scope as not-this-DID's-deny, so the repair would report RecordMissing forever", got)
	}
	if got := dig(t, post.json(t), "spec", "did"); got != testDID {
		t.Fatalf("did = %v, want %q", got, testDID)
	}
}

func TestWriteDenyRefusesWithoutAnAccount(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(request) (int, any) { return notFound() }}
	c := newTestClient(t, f)
	if err := c.WriteDeny(context.Background(), testDID); err == nil {
		t.Fatal("a deny was written for an account that does not exist")
	}
	for _, r := range f.seen() {
		if r.Method != http.MethodGet {
			t.Fatalf("%s %s -- nothing may be written when the account workspace is absent",
				r.Method, r.Path)
		}
	}
}

func TestWriteDenyRefusesAnEmptyDID(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(request) (int, any) { return notFound() }}
	c := newTestClient(t, f)
	if err := c.WriteDeny(context.Background(), ""); !errors.Is(err, revocation.ErrNoDID) {
		t.Fatalf("want ErrNoDID, got %v", err)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("an empty DID produced %d requests", len(f.seen()))
	}
}

func TestWriteDenyIsTheWriteHalfOfLookup(t *testing.T) {
	revoked := false
	f := denyFixture(t, func(req request) (int, any) {
		if req.Method == http.MethodPatch {
			revoked = true
			return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, true)
		}
		return http.StatusOK, identityBody(testDID, v1alpha1.IdentityScopeAccount, revoked)
	})
	c := newTestClient(t, f)
	ctx := context.Background()

	before, err := c.Lookup(ctx, testDID)
	if err != nil {
		t.Fatalf("Lookup before: %v", err)
	}
	if before != revocation.DenyGranted {
		t.Fatalf("Lookup before = %v, want Granted", before)
	}

	if err := c.WriteDeny(ctx, testDID); err != nil {
		t.Fatalf("WriteDeny: %v", err)
	}

	after, err := c.Lookup(ctx, testDID)
	if err != nil {
		t.Fatalf("Lookup after: %v", err)
	}
	if after != revocation.DenyRevoked {
		t.Fatalf("Lookup after WriteDeny = %v, want Revoked -- the two halves disagree, so "+
			"the write is not a deny", after)
	}
	if _, err := revocation.Admit(ctx, c, testDID); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("Admit after WriteDeny = %v, want ErrRevoked -- the door must refuse, and "+
			"Admit is the only constructor of a Permit", err)
	}
}

func fanOutFixture(t *testing.T, sa func(cluster string) (int, any)) *fakeKCP {
	t.Helper()
	return &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case strings.HasSuffix(req.Path, "/apis/tenancy.kcp.io/v1alpha1/workspaces"):
			return http.StatusOK, workspaceList("web", "db")
		case strings.Contains(req.Path, "/serviceaccounts/"):
			for _, cluster := range []string{testCluster, "db"} {
				if strings.Contains(req.Path, ":"+cluster+":clusters/") {
					return sa(cluster)
				}
			}
		}
		return 0, nil
	}}
}

func TestFanOutReportsEachClusterAsObserved(t *testing.T) {
	f := fanOutFixture(t, func(cluster string) (int, any) {
		if cluster == testCluster {
			return http.StatusOK, serviceAccountBody(saName)
		}
		return http.StatusServiceUnavailable, statusBody("ServiceUnavailable", http.StatusServiceUnavailable)
	})
	c := newTestClient(t, f)

	got, err := c.FanOut(context.Background(), testDID)
	if err != nil {
		t.Fatalf("FanOut: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both of the account's clusters, got %+v", got)
	}
	if want := (revocation.ClusterView{Name: "db", Cold: true}); got[0] != want {
		t.Errorf("db = %+v, want %+v -- an apiserver that did not answer is COLD, and COLD is "+
			"not absent: planarizing the two deletes from a cluster that cannot be reached", got[0], want)
	}
	if want := (revocation.ClusterView{Name: "web", ServiceAccountPresent: true}); got[1] != want {
		t.Errorf("web = %+v, want %+v", got[1], want)
	}

	var reads []request
	for _, r := range f.seen() {
		if strings.Contains(r.Path, "/serviceaccounts/") {
			reads = append(reads, r)
		}
	}
	if len(reads) != 2 {
		t.Fatalf("want one ServiceAccount read per cluster, got %+v", reads)
	}
	for i, want := range []string{dbSAPath, saPath} {
		if reads[i].Method != http.MethodGet || reads[i].Path != want {
			t.Errorf("cluster read %d = %s %s, want GET %s -- the tenant object is read LIVE "+
				"through the cluster's mount, in ns=users, by its derived name",
				i, reads[i].Method, reads[i].Path, want)
		}
	}
}

func TestFanOutReportsAGoneServiceAccountAsDone(t *testing.T) {
	f := fanOutFixture(t, func(string) (int, any) { return notFound() })
	c := newTestClient(t, f)

	got, err := c.FanOut(context.Background(), testDID)
	if err != nil {
		t.Fatalf("a deleted ServiceAccount is not an error: %v", err)
	}
	want := []revocation.ClusterView{{Name: "db"}, {Name: "web"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("FanOut = %+v, want %+v (absent and NOT cold)", got, want)
	}
	if e := revocation.Plan(got); e[0].State != v1alpha1.IdentityRevoked || e[0].DeleteSA {
		t.Fatalf("Plan over an absent ServiceAccount = %+v, want Revoked with no delete", e[0])
	}
}

func TestFanOutReportsAnUnreachableApiserverAsCold(t *testing.T) {
	refused := errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")
	f := fanOutFixture(t, func(string) (int, any) {
		return http.StatusOK, serviceAccountBody(saName)
	})
	f.transportErr = func(req request) error {
		if strings.Contains(req.Path, "/serviceaccounts/") {
			return refused
		}
		return nil
	}
	c := newTestClient(t, f)

	got, err := c.FanOut(context.Background(), testDID)
	if err != nil {
		t.Fatalf("a cluster that did not answer is COLD, not an error the whole fan-out "+
			"fails on: %v", err)
	}
	for _, v := range got {
		if !v.Cold || v.ServiceAccountPresent {
			t.Fatalf("%+v: an unanswered read is Cold with nothing observed", v)
		}
		if e := revocation.Plan([]revocation.ClusterView{v})[0]; e.State != v1alpha1.IdentityColdDeferred {
			t.Fatalf("Plan over %+v = %+v, want ColdDeferred", v, e)
		}
	}
}

func TestFanOutKeepsGoingWhenAClusterIsCold(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case strings.HasSuffix(req.Path, "/apis/tenancy.kcp.io/v1alpha1/workspaces"):
			return http.StatusOK, workspaceList("aaa-cold", "zzz-warm")
		case strings.Contains(req.Path, "/serviceaccounts/"):
			if strings.Contains(req.Path, ":zzz-warm:clusters/") {
				return http.StatusOK, serviceAccountBody(saName)
			}
			return http.StatusBadGateway, statusBody("BadGateway", http.StatusBadGateway)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	got, err := c.FanOut(context.Background(), testDID)
	if err != nil {
		t.Fatalf("FanOut: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FanOut = %+v: one unreadable cluster must not hide the others", got)
	}
	if !got[0].Cold || got[1] != (revocation.ClusterView{Name: "zzz-warm", ServiceAccountPresent: true}) {
		t.Fatalf("FanOut = %+v, want the cold cluster deferred and the warm one observed", got)
	}
}

func TestFanOutReadsNothingButTheServiceAccount(t *testing.T) {
	f := fanOutFixture(t, func(string) (int, any) { return notFound() })
	c := newTestClient(t, f)

	if _, err := c.FanOut(context.Background(), testDID); err != nil {
		t.Fatalf("FanOut: %v", err)
	}
	for _, r := range f.seen() {
		switch {
		case strings.HasSuffix(r.Path, "/apis/tenancy.kcp.io/v1alpha1/workspaces"):
		case strings.Contains(r.Path, "/serviceaccounts/"):
		default:
			t.Fatalf("FanOut read %s %s: the only observation it may make is the "+
				"ServiceAccount's own existence", r.Method, r.Path)
		}
	}
	for _, r := range f.seen() {
		if r.Method != http.MethodGet {
			t.Fatalf("FanOut issued %s %s", r.Method, r.Path)
		}
	}
}

func TestFanOutReportsAnUnreadableAccountAsAnError(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		return http.StatusInternalServerError, statusBody("InternalError", http.StatusInternalServerError)
	}}
	c := newTestClient(t, f)

	got, err := c.FanOut(context.Background(), testDID)
	if err == nil {
		t.Fatalf("FanOut = %+v with no error: an unreadable account workspace is not an "+
			"account with no clusters", got)
	}
	if f.hasMethod(http.MethodGet) {
		for _, r := range f.seen() {
			if strings.Contains(r.Path, "/serviceaccounts/") {
				t.Fatalf("a ServiceAccount was read after the enumeration failed: %s", r.Path)
			}
		}
	}
}

func TestFanOutRefusesAnEmptyDID(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(request) (int, any) { return notFound() }}
	c := newTestClient(t, f)
	if _, err := c.FanOut(context.Background(), ""); !errors.Is(err, revocation.ErrNoDID) {
		t.Fatalf("want ErrNoDID, got %v", err)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("an empty DID produced %d requests", len(f.seen()))
	}
}

func TestTheDeleteIsTheWriteHalfOfTheObservation(t *testing.T) {
	gone := false
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodDelete:
			gone = true
			return http.StatusOK, statusBody("Success", http.StatusOK)
		case strings.HasSuffix(req.Path, "/apis/tenancy.kcp.io/v1alpha1/workspaces"):
			return http.StatusOK, workspaceList(testCluster)
		case strings.Contains(req.Path, "/serviceaccounts/"):
			if gone {
				return notFound()
			}
			return http.StatusOK, serviceAccountBody(saName)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)
	ctx := context.Background()

	before, err := c.FanOut(ctx, testDID)
	if err != nil || len(before) != 1 || !before[0].ServiceAccountPresent {
		t.Fatalf("FanOut before = %+v (%v), want the ServiceAccount present", before, err)
	}
	if e := revocation.Plan(before)[0]; e.State != v1alpha1.IdentityRevocationPending || !e.DeleteSA {
		t.Fatalf("Plan before = %+v, want Pending with a delete owed", e)
	}

	if err := c.DeleteServiceAccount(ctx, testDID, testCluster); err != nil {
		t.Fatalf("DeleteServiceAccount: %v", err)
	}
	d := lastWith(f, http.MethodDelete)
	if d == nil {
		t.Fatal("no DELETE was issued: the plan's delete is still computed and discarded")
	}
	if d.Path != saPath {
		t.Fatalf("deleted %s, want %s -- the write goes to the object the READ went to: the "+
			"cluster's mount, ns=users, the derived name", d.Path, saPath)
	}

	after, err := c.FanOut(ctx, testDID)
	if err != nil {
		t.Fatalf("FanOut after: %v", err)
	}
	if e := revocation.Plan(after)[0]; e.State != v1alpha1.IdentityRevoked || e.DeleteSA {
		t.Fatalf("Plan after the delete = %+v, want Revoked with nothing owed: the completion "+
			"signal is the ABSENCE of the ServiceAccount, and this is where that absence comes from", e)
	}
}

func serviceAccountNotFound(name string) (int, any) {
	return http.StatusNotFound, map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": "NotFound", "code": http.StatusNotFound,
		"message": "serviceaccounts \"" + name + "\" not found",
		"details": map[string]any{"name": name, "kind": "serviceaccounts"},
	}
}

func TestDeleteServiceAccountTreatsAnAlreadyGoneOneAsDone(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodDelete {
			return serviceAccountNotFound(ServiceAccountName(testDID))
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.DeleteServiceAccount(context.Background(), testDID, testCluster); err != nil {
		t.Fatalf("a ServiceAccount that is already gone is not a failure: %v", err)
	}
}

func TestAnUnresolvedMountIsNotACompletedDelete(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodDelete {
			return notFound()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	err := c.DeleteServiceAccount(context.Background(), testDID, testCluster)
	if err == nil {
		t.Fatal("a 404 that does not NAME the ServiceAccount was treated as a completed " +
			"delete. On a cluster with a cleared status.URL that reports REVOKED while the " +
			"ServiceAccount is still there -- the report an operator would act on, and the " +
			"one case FanOut's three-state model cannot express")
	}
	if !strings.Contains(err.Error(), "mount did NOT resolve") {
		t.Fatalf("the error does not say WHICH 404 this was: %v. An operator seeing a bare "+
			"NotFound would go looking at the tenant apiserver instead of at the mount", err)
	}
}

func TestDeleteServiceAccountNeverCallsAnUnansweredClusterDeleted(t *testing.T) {
	for _, tc := range []struct {
		name         string
		respond      func(request) (int, any)
		transportErr func(request) error
	}{
		{
			name: "the mount is not Ready (503)",
			respond: func(request) (int, any) {
				return http.StatusServiceUnavailable, statusBody("ServiceUnavailable", http.StatusServiceUnavailable)
			},
		},
		{
			name: "the credential may not delete (403)",
			respond: func(request) (int, any) {
				return http.StatusForbidden, statusBody("Forbidden", http.StatusForbidden)
			},
		},
		{
			name: "nobody is listening",
			transportErr: func(request) error {
				return errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeKCP{t: t, transportErr: tc.transportErr}
			f.respond = func(req request) (int, any) {
				if tc.respond == nil {
					return 0, nil
				}
				return tc.respond(req)
			}
			c := newTestClient(t, f)

			if err := c.DeleteServiceAccount(context.Background(), testDID, testCluster); err == nil {
				t.Fatal("an unanswered cluster was reported as a completed delete, so the " +
					"cluster would be marked Revoked with its ServiceAccount still there")
			}
		})
	}
}

func TestDeleteServiceAccountRefusesAnEmptyDID(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(request) (int, any) { return notFound() }}
	c := newTestClient(t, f)
	if err := c.DeleteServiceAccount(context.Background(), "", testCluster); !errors.Is(err, revocation.ErrNoDID) {
		t.Fatalf("want ErrNoDID, got %v", err)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("an empty DID produced %d requests", len(f.seen()))
	}
}

func TestWriteNodeCreatesWhenAbsent(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/nodes/n1"):
			return http.StatusNotFound, statusBody("NotFound", http.StatusNotFound)
		case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/nodes"):
			return http.StatusCreated, newNode("n1", testCluster, "")
		case req.Method == http.MethodPatch && strings.HasSuffix(req.Path, "/nodes/n1/status"):
			return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "n1", v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning}); err != nil {
		t.Fatalf("WriteNode: %v", err)
	}
	var created, phased *request
	for i, r := range f.seen() {
		switch {
		case r.Method == http.MethodPost:
			created = &f.seen()[i]
		case r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/status"):
			phased = &f.seen()[i]
		}
	}
	if created == nil {
		t.Fatalf("no CREATE was issued for an absent node: %+v", f.seen())
	}
	if got := dig(t, created.json(t), "spec", "cluster"); got != testCluster {
		t.Errorf("the created node does not name its cluster: %s", created.Body)
	}
	status, _ := created.json(t)["status"].(map[string]any)
	if phase, ok := status["phase"]; ok && phase != nil && phase != "" {
		t.Errorf("the create carries status.phase, which the schema's status subresource DISCARDS: "+
			"the node would exist with no phase at all. %s", created.Body)
	}
	if phased == nil {
		t.Fatalf("no status write followed the create, so the node exists with no phase: %+v", f.seen())
	}
	if got := dig(t, phased.json(t), "status", "phase"); got != string(v1alpha1.NodeRunning) {
		t.Errorf("the status write does not set status.phase: %s", phased.Body)
	}
}

func TestWriteNodePatchesThePhaseAndNothingElse(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/nodes/n1"):
			return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodePending)
		case req.Method == http.MethodPatch && strings.HasSuffix(req.Path, "/nodes/n1/status"):
			return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "n1", v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning}); err != nil {
		t.Fatalf("WriteNode: %v", err)
	}
	var patched *request
	for i, r := range f.seen() {
		if r.Method == http.MethodPatch {
			patched = &f.seen()[i]
		}
		if r.Method == http.MethodPut {
			t.Fatalf("WriteNode issued a PUT: a full replace carries whatever was read, and a " +
				"concurrent edit to the spec would be lost")
		}
	}
	if patched == nil {
		t.Fatalf("no PATCH was issued: %+v", f.seen())
	}
	if got := dig(t, patched.json(t), "status", "phase"); got != string(v1alpha1.NodeRunning) {
		t.Errorf("the patch does not set status.phase: %s", patched.Body)
	}
	if body := patched.json(t); body["spec"] != nil {
		t.Errorf("the patch carries spec, so it can clobber a concurrent edit: %s", patched.Body)
	}
}

func TestWriteNodeIsSilentWhenThePhaseAlreadyMatches(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/nodes/n1") {
			return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "n1", v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning}); err != nil {
		t.Fatalf("WriteNode: %v", err)
	}
	for _, r := range f.seen() {
		if r.Method != http.MethodGet {
			t.Fatalf("a no-op phase write issued %s %s", r.Method, r.Path)
		}
	}
}

func TestWriteNodeCarriesTheNodesOwnExpiryReport(t *testing.T) {
	expiry := int64(1790000000)
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/nodes/n1") {
			return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
		}
		return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
	}}
	c := newTestClient(t, f)

	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "n1",
		v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning, NodeExpiry: &expiry}); err != nil {
		t.Fatalf("WriteNode: %v", err)
	}
	var patched *request
	for i, r := range f.seen() {
		if r.Method == http.MethodPatch {
			patched = &f.seen()[i]
		}
	}
	if patched == nil {
		t.Fatalf("a status carrying an expiry issued no PATCH: %+v", f.seen())
	}
	if got := dig(t, patched.json(t), "status", "nodeExpiry"); got != float64(expiry) {
		t.Errorf("the patch does not carry status.nodeExpiry: %s", patched.Body)
	}
	if got := dig(t, patched.json(t), "status", "phase"); got != string(v1alpha1.NodeRunning) {
		t.Errorf("the patch lost the phase: %s", patched.Body)
	}
}

func TestWriteNodeLeavesAnUnreportedExpiryAlone(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		return http.StatusOK, newNode("n1", testCluster, v1alpha1.NodeRunning)
	}}
	c := newTestClient(t, f)

	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "n1",
		v1alpha1.NodeStatus{Phase: v1alpha1.NodeDraining}); err != nil {
		t.Fatalf("WriteNode: %v", err)
	}
	var patched *request
	for i, r := range f.seen() {
		if r.Method == http.MethodPatch {
			patched = &f.seen()[i]
		}
	}
	if patched == nil {
		t.Fatalf("a phase-only status issued no PATCH: %+v", f.seen())
	}
	status, _ := patched.json(t)["status"].(map[string]any)
	if _, ok := status["nodeExpiry"]; ok {
		t.Errorf("a status with no report wrote nodeExpiry anyway: %s. A node's report is written "+
			"when it has one, and a sentinel invented here is indistinguishable from a report", patched.Body)
	}
}

func TestWriteNodeRefusesAnUnnamedNodeOrCluster(t *testing.T) {
	c := newTestClient(t, &fakeKCP{t: t, respond: func(request) (int, any) { return 0, nil }})
	if err := c.WriteNode(context.Background(), AccountPath(testDID), testCluster, "", v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning}); err == nil {
		t.Error("an empty node name was accepted")
	}
	if err := c.WriteNode(context.Background(), AccountPath(testDID), "", "n1", v1alpha1.NodeStatus{Phase: v1alpha1.NodeRunning}); err == nil {
		t.Error("an empty cluster was accepted: a node that names no cluster is one no cluster can count")
	}
}

func testClaimSpec() v1alpha1.NodeClaimSpec {
	return v1alpha1.NodeClaimSpec{
		Cluster:          testCluster,
		NodeName:         "web-node1",
		OwnerDid:         testDID,
		ApiserverPort:    24000,
		AdvertiseAddress: "100.127.255.254",
		TokenFile:        "/state/pdr-web/token",
		NodePasswordFile: "/state/pdr-web/nodes/web-node1/node-password",
		ServerCAFile:     "/state/pdr-web/server/tls/server-ca.crt",
		DenyBidderDids:   []string{"did:plc:bad"},
	}
}

func newClaim() v1alpha1.NodeClaim {
	return v1alpha1.NodeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "socialweb.computer/v1alpha1", Kind: "NodeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: "web-node1"},
		Spec:       testClaimSpec(),
	}
}

func claimPath(name string) string {
	return "/clusters/" + homePath + "/apis/socialweb.computer/v1alpha1/nodeclaims/" + name
}

func TestWriteNodeClaimCreatesItInTheClustersHomeWorkspace(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodGet && req.Path == claimPath("web-node1"):
			return notFound()
		case req.Method == http.MethodPost && req.Path == "/clusters/"+homePath+"/apis/socialweb.computer/v1alpha1/nodeclaims":
			return http.StatusCreated, newClaim()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNodeClaim(context.Background(), homePath, newClaim()); err != nil {
		t.Fatalf("WriteNodeClaim: %v", err)
	}
	var posted *request
	for i, r := range f.seen() {
		if r.Method == http.MethodPost {
			posted = &f.seen()[i]
		}
	}
	if posted == nil {
		t.Fatalf("no CREATE was issued for an absent claim: %+v", f.seen())
	}
	body := posted.json(t)
	spec, _ := body["spec"].(map[string]any)
	for field, want := range map[string]any{
		"cluster": testCluster, "nodeName": "web-node1", "ownerDid": testDID,
		"advertiseAddress": "100.127.255.254",
		"tokenFile":        "/state/pdr-web/token",
		"nodePasswordFile": "/state/pdr-web/nodes/web-node1/node-password",
		"serverCAFile":     "/state/pdr-web/server/tls/server-ca.crt",
	} {
		if got := spec[field]; got != want {
			t.Errorf("the created claim's spec.%s is %v, want %v. The field names are the door's "+
				"wire contract; a Go-idiomatic rename silently writes a claim the door reads as "+
				"empty", field, got, want)
		}
	}
	if got := spec["apiserverPort"]; got != float64(24000) {
		t.Errorf("the created claim's spec.apiserverPort is %v, want 24000", got)
	}
	if status, ok := body["status"].(map[string]any); ok {
		if phase, present := status["phase"]; present && phase != nil && phase != "" {
			t.Errorf("the create carries status.phase, which the schema's status subresource "+
				"DISCARDS: the claim would exist with no phase at all. %s", posted.Body)
		}
	}
}

func TestWriteNodeClaimIsSilentWhenTheSpecAlreadyMatches(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet && req.Path == claimPath("web-node1") {
			return http.StatusOK, newClaim()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNodeClaim(context.Background(), homePath, newClaim()); err != nil {
		t.Fatalf("WriteNodeClaim: %v", err)
	}
	if f.hasMethod(http.MethodPost) || f.hasMethod(http.MethodPatch) {
		t.Fatalf("a claim whose spec already matches was written again on every pass: %+v", f.seen())
	}
}

func TestWriteNodeClaimPatchesASpecThatChangedAndNeverTheStatus(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		switch {
		case req.Method == http.MethodGet && req.Path == claimPath("web-node1"):
			stale := newClaim()
			stale.Spec.AdvertiseAddress = "10.0.0.1"
			return http.StatusOK, stale
		case req.Method == http.MethodPatch && req.Path == claimPath("web-node1"):
			return http.StatusOK, newClaim()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.WriteNodeClaim(context.Background(), homePath, newClaim()); err != nil {
		t.Fatalf("WriteNodeClaim: %v", err)
	}
	var patched *request
	for i, r := range f.seen() {
		if r.Method == http.MethodPatch {
			patched = &f.seen()[i]
		}
	}
	if patched == nil {
		t.Fatalf("a claim whose advertise address changed was left alone: %+v", f.seen())
	}
	if strings.Contains(patched.Path, "/status") {
		t.Fatalf("the spec write went to the status subresource: %s", patched.Path)
	}
	if _, ok := patched.json(t)["status"]; ok {
		t.Fatalf("the spec write carries a status, and status belongs to the door alone: %s",
			patched.Body)
	}
}

func TestReadNodeClaimReportsAbsenceWithoutAnError(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet {
			return notFound()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	claim, err := c.ReadNodeClaim(context.Background(), homePath, "web-node1")
	if err != nil {
		t.Fatalf("ReadNodeClaim on an absent claim: %v -- an absent claim is an answer, and a "+
			"caller that cannot tell it from a failed read cannot tell a released node from an "+
			"unreachable apiserver", err)
	}
	if claim != nil {
		t.Fatalf("an absent claim decoded to %+v", claim)
	}
}

func TestReadNodeClaimCarriesTheDoorsStatus(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodGet {
			claimed := newClaim()
			claimed.Status = v1alpha1.NodeClaimStatus{
				Phase:   v1alpha1.NodeClaimRunning,
				Fqdn:    "node.example.invalid",
				Message: "the node's agent reached its apiserver",
				Contract: v1alpha1.NodeClaimContract{
					VMUri: "at://did:plc:x/com.example.vm/1", ReceiptCid: "bafyrei",
				},
			}
			return http.StatusOK, claimed
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	claim, err := c.ReadNodeClaim(context.Background(), homePath, "web-node1")
	if err != nil {
		t.Fatalf("ReadNodeClaim: %v", err)
	}
	if claim == nil || claim.Status.Phase != v1alpha1.NodeClaimRunning {
		t.Fatalf("the door's phase did not survive the read: %+v", claim)
	}
	if claim.Status.Fqdn != "node.example.invalid" || claim.Status.Message == "" {
		t.Fatalf("the door's fqdn and message did not survive the read: %+v", claim.Status)
	}
	if claim.Status.Contract.ReceiptCid != "bafyrei" || claim.Status.Contract.VMUri == "" {
		t.Fatalf("the contract the door wrote did not survive the read: %+v", claim.Status.Contract)
	}
}

func TestDeleteNodeClaimTreatsAnAlreadyGoneOneAsDone(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method == http.MethodDelete {
			return notFound()
		}
		return 0, nil
	}}
	c := newTestClient(t, f)

	if err := c.DeleteNodeClaim(context.Background(), homePath, "web-node1"); err != nil {
		t.Fatalf("DeleteNodeClaim on an absent claim: %v -- a teardown that already landed is not "+
			"a teardown that failed, and reporting it as one leaves a cluster that can never reach "+
			"its zero state", err)
	}
	if !f.hasMethod(http.MethodDelete) {
		t.Fatalf("no DELETE was issued: %+v", f.seen())
	}
}

func TestListNodeClaimsReadsOneHomeWorkspace(t *testing.T) {
	f := &fakeKCP{t: t, respond: func(req request) (int, any) {
		if req.Method != http.MethodGet {
			return 0, nil
		}
		claimed := newClaim()
		claimed.Status = v1alpha1.NodeClaimStatus{Phase: v1alpha1.NodeClaimJoining}
		return http.StatusOK, map[string]any{
			"kind": "NodeClaimList", "apiVersion": "socialweb.computer/v1alpha1",
			"items": []any{claimed},
		}
	}}
	c := newTestClient(t, f)

	claims, err := c.ListNodeClaims(context.Background(), homePath)
	if err != nil {
		t.Fatalf("ListNodeClaims: %v", err)
	}
	if len(claims) != 1 || claims[0].Spec.Cluster != testCluster {
		t.Fatalf("ListNodeClaims returned %+v", claims)
	}
	if got := f.seen()[0].Path; got != "/clusters/"+homePath+"/apis/socialweb.computer/v1alpha1/nodeclaims" {
		t.Fatalf("the list went to %s, want the cluster's own home workspace", got)
	}
}

func TestWriteNodeClaimRefusesAnUnnamedClaim(t *testing.T) {
	c := newTestClient(t, &fakeKCP{t: t, respond: func(request) (int, any) { return 0, nil }})
	if err := c.WriteNodeClaim(context.Background(), homePath, v1alpha1.NodeClaim{}); err == nil {
		t.Error("a claim with no name was accepted: it would be written at the empty name, which " +
			"is not a resource")
	}
	named := newClaim()
	named.Spec.Cluster = ""
	if err := c.WriteNodeClaim(context.Background(), homePath, named); err == nil {
		t.Error("a claim naming no cluster was accepted")
	}
	named = newClaim()
	named.Spec.NodeName = ""
	if err := c.WriteNodeClaim(context.Background(), homePath, named); err == nil {
		t.Error("a claim naming no node was accepted")
	}
}
