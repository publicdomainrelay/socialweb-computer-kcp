package kcpstore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/store"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/statuspatch"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

type runObject struct {
	Metadata struct {
		Name            string   `json:"name"`
		Namespace       string   `json:"namespace"`
		Finalizers      []string `json:"finalizers"`
		ResourceVersion string   `json:"resourceVersion"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

var runs = schema.GroupVersionResource{Group: "policy.socialweb.computer", Version: "v1alpha1", Resource: "policyworkflowruns"}

var _ store.Resource[runObject] = Of[runObject](nil, runs)

type request struct {
	Method string

	Path string

	ContentType string

	Body string

	Authorization string
}

func newServer(t *testing.T, want []request, respond func(w http.ResponseWriter, r *http.Request, index int)) (*Store, *[]request) {
	t.Helper()
	seen := &[]request{}
	index := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = append(*seen, request{
			Method:        r.Method,
			Path:          r.URL.Path,
			ContentType:   r.Header.Get("Content-Type"),
			Body:          string(body),
			Authorization: r.Header.Get("Authorization"),
		})
		respond(w, r, index)
		index++
	}))
	t.Cleanup(server.Close)
	store, err := New(Options{Host: server.URL, RestConfig: nil})
	if err != nil {
		t.Fatal(err)
	}
	if want != nil {
		t.Cleanup(func() {
			if len(*seen) != len(want) {
				t.Fatalf("requests = %d, want %d: %+v", len(*seen), len(want), *seen)
			}
			for i := range want {
				if (*seen)[i].Method != want[i].Method || (*seen)[i].Path != want[i].Path {
					t.Fatalf("request %d = %s %s, want %s %s", i, (*seen)[i].Method, (*seen)[i].Path, want[i].Method, want[i].Path)
				}
			}
		})
	}
	return store, seen
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestGetDecodesThroughTheClusterPath(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "GET", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns/run-1"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"name": "run-1", "namespace": "default"},
			"status":   map[string]any{"phase": "Running"},
		})
	})
	resource := Of[runObject](store, runs)
	found, err := resource.Get(context.Background(), ref.New("root:alice", "default", "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if found.Status.Phase != "Running" || found.Metadata.Name != "run-1" {
		t.Fatalf("run = %+v", found)
	}
}

func TestListDecodesItems(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "GET", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/policyworkflowruns"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "a"}},
			map[string]any{"metadata": map[string]any{"name": "b"}},
		}})
	})
	resource := Of[runObject](store, runs)
	items, err := resource.List(context.Background(), "root:alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Metadata.Name != "a" {
		t.Fatalf("items = %+v", items)
	}
}

func TestListInDecodesItemsInOneNamespace(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "GET", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "a", "namespace": "default"}},
		}})
	})
	resource := Of[runObject](store, runs)
	items, err := resource.ListIn(context.Background(), "root:alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "a" {
		t.Fatalf("items = %+v", items)
	}
}

func TestCreatePostsTheObjectToItsNamespace(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "POST", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 201, map[string]any{"metadata": map[string]any{"name": "run-1"}})
	})
	resource := Of[runObject](store, runs)
	obj := &runObject{}
	obj.Metadata.Name = "run-1"
	obj.Metadata.Namespace = "default"
	if err := resource.Create(context.Background(), "root:alice", obj); err != nil {
		t.Fatal(err)
	}
}

func TestPatchStatusUsesTheSubresource(t *testing.T) {
	store, seen := newServer(t, []request{
		{Method: "PATCH", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns/run-1/status"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{})
	})
	resource := Of[runObject](store, runs)
	patch, err := statuspatch.Merge(map[string]any{"phase": "Succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.PatchStatus(context.Background(), ref.New("root:alice", "default", "run-1"), patch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[0].Body, `"status"`) || !strings.Contains((*seen)[0].Body, "Succeeded") {
		t.Fatalf("body = %s", (*seen)[0].Body)
	}
	if !strings.Contains((*seen)[0].ContentType, "merge-patch") {
		t.Fatalf("content type = %q", (*seen)[0].ContentType)
	}
}

func TestRemoveFinalizerTestsTheCachedList(t *testing.T) {
	store, seen := newServer(t, []request{
		{Method: "GET", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns/run-1"},
		{Method: "PATCH", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns/run-1"},
	}, func(w http.ResponseWriter, _ *http.Request, index int) {
		if index == 0 {
			writeJSON(w, 200, map[string]any{
				"metadata": map[string]any{"name": "run-1", "finalizers": []string{"a", "b"}},
			})
			return
		}
		writeJSON(w, 200, map[string]any{})
	})
	resource := Of[runObject](store, runs)
	if err := resource.RemoveFinalizer(context.Background(), ref.New("root:alice", "default", "run-1"), "a"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[1].Body, `"op":"test"`) {
		t.Fatalf("patch = %s", (*seen)[1].Body)
	}
}

func TestFinalizersAndKnownRemoval(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "GET", Path: "/clusters/root:alice/apis/policy.socialweb.computer/v1alpha1/namespaces/default/policyworkflowruns/run-1"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"name": "run-1", "finalizers": []string{"a"}},
		})
	})
	resource := Of[runObject](store, runs)
	finalizers, err := resource.Finalizers(context.Background(), ref.New("root:alice", "default", "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(finalizers) != 1 || finalizers[0] != "a" {
		t.Fatalf("finalizers = %v", finalizers)
	}
}

func TestNotFoundIsRecognised(t *testing.T) {
	store, _ := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 404, map[string]any{"kind": "Status", "reason": "NotFound", "code": 404})
	})
	resource := Of[runObject](store, runs)
	_, err := resource.Get(context.Background(), ref.New("root:alice", "default", "missing"))
	if err == nil || !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	if err := resource.Delete(context.Background(), ref.New("root:alice", "default", "missing")); err != nil {
		t.Fatalf("a delete of a missing object is not an error: %v", err)
	}
}

func TestMintServiceAccountToken(t *testing.T) {
	store, seen := newServer(t, []request{
		{Method: "POST", Path: "/clusters/root:alice/api/v1/namespaces/default/serviceaccounts/reader/token"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 201, map[string]any{"status": map[string]any{"token": "jwt-token"}})
	})
	token, err := store.MintServiceAccountToken(context.Background(), "root:alice", "default", "reader", 3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if token != "jwt-token" {
		t.Fatalf("token = %q", token)
	}
	if !strings.Contains((*seen)[0].Body, "TokenRequest") {
		t.Fatalf("body = %s", (*seen)[0].Body)
	}
}

func TestClusterPathReadsTheAnnotation(t *testing.T) {
	store, _ := newServer(t, []request{
		{Method: "GET", Path: "/clusters/2j35/apis/core.kcp.io/v1alpha1/logicalclusters/cluster"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"kcp.io/path": "root:alice"}},
		})
	})
	path, err := store.ClusterPath(context.Background(), "2j35")
	if err != nil {
		t.Fatal(err)
	}
	if path != "root:alice" {
		t.Fatalf("path = %q", path)
	}
}

func TestPathCacheResolvesOnce(t *testing.T) {
	store, seen := newServer(t, []request{
		{Method: "GET", Path: "/clusters/2j35/apis/core.kcp.io/v1alpha1/logicalclusters/cluster"},
	}, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"kcp.io/path": "root:alice"}},
		})
	})
	cache := NewPathCache(store)
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("path = %q", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("cached path = %q", path)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want the resolution to be cached", len(*seen))
	}
	cache.Forget("2j35")
	if cache.Len() != 0 {
		t.Fatal("forget must drop the entry")
	}
}

func TestPathCacheCachesAMiss(t *testing.T) {
	store, seen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 404, map[string]any{"kind": "Status", "code": 404})
	})
	cache := NewPathCache(store)
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("cached path = %q, want empty", path)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want the failed resolution cached rather than retried", len(*seen))
	}
}

func TestPathCacheRetriesATransientFailure(t *testing.T) {
	store, seen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, index int) {
		if index == 0 {
			writeJSON(w, 500, map[string]any{"kind": "Status", "code": 500})
			return
		}
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{"kcp.io/path": "root:alice"}},
		})
	})
	cache := NewPathCache(store)
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("path = %q, want empty while the store is failing", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("path = %q, want the second lookup to retry a failure that was not a not-found", path)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want the transient failure retried", len(*seen))
	}
}

func TestPathCacheRetriesEveryServerFailureAndACancelledCall(t *testing.T) {
	store, seen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, index int) {
		if index < 2 {
			code := []int{502, 429}[index]
			writeJSON(w, code, map[string]any{"kind": "Status", "code": code})
			return
		}
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{"kcp.io/path": "root:alice"}},
		})
	})
	cache := NewPathCache(store)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if path := cache.Lookup(cancelled, "2j35"); path != "" {
		t.Fatalf("path = %q, want empty for a cancelled call", path)
	}
	if len(*seen) != 0 {
		t.Fatalf("requests = %d, want a cancelled call to reach no server", len(*seen))
	}
	for _, want := range []string{"", "", "root:alice"} {
		if path := cache.Lookup(context.Background(), "2j35"); path != want {
			t.Fatalf("path = %q, want %q: a gateway error and a rate limit are outages, not answers", path, want)
		}
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("cached path = %q, want the resolved path", path)
	}
	if len(*seen) != 3 {
		t.Fatalf("requests = %d, want each outage retried and the answer then cached", len(*seen))
	}
}

func TestPathCacheRetriesABodyThatDoesNotParseAndARefusal(t *testing.T) {
	store, seen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, index int) {
		switch index {
		case 0:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"annotations":`))
		default:
			writeJSON(w, 200, map[string]any{
				"metadata": map[string]any{"annotations": map[string]string{"kcp.io/path": "root:alice"}},
			})
		}
	})
	cache := NewPathCache(store)
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("path = %q, want empty for a body that did not parse", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("path = %q, want the malformed response re-asked rather than cached", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("cached path = %q, want the resolved path", path)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want the parse failure retried and the answer cached", len(*seen))
	}

	refused, refusedSeen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, index int) {
		if index == 0 {
			writeJSON(w, 403, map[string]any{"kind": "Status", "code": 403})
			return
		}
		writeJSON(w, 200, map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{"kcp.io/path": "root:alice"}},
		})
	})
	refusedCache := NewPathCache(refused)
	if path := refusedCache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("path = %q, want empty for a workspace the caller may not read", path)
	}
	if path := refusedCache.Lookup(context.Background(), "2j35"); path != "root:alice" {
		t.Fatalf("path = %q, want a refusal re-asked: a token mid-refresh and a grant not yet visible both heal", path)
	}
	if len(*refusedSeen) != 2 {
		t.Fatalf("requests = %d, want the refusal retried", len(*refusedSeen))
	}
}

func TestPathCacheCachesAWorkspaceWithNoPath(t *testing.T) {
	store, seen := newServer(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, 200, map[string]any{"metadata": map[string]any{"annotations": map[string]any{}}})
	})
	cache := NewPathCache(store)
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("path = %q, want empty for a workspace carrying no path", path)
	}
	if path := cache.Lookup(context.Background(), "2j35"); path != "" {
		t.Fatalf("cached path = %q, want empty", path)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want the answer cached: a workspace with no path is not an outage", len(*seen))
	}
}

func TestNewRequiresAHost(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a host is required")
	}
}
