package servicenames

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/denospec"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

func pod(name, namespace, cluster string, env map[string]string) *unstructured.Unstructured {
	values := map[string]any{}
	for key, value := range env {
		values[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "socialweb.computer/v1alpha1",
		"kind":       "Pds",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"annotations": map[string]any{"kcp.io/cluster": cluster},
		},
		"spec": map[string]any{"env": values},
	}}
}

func TestAdvertisedAddress(t *testing.T) {
	cases := []struct {
		args string
		env  string
		want string
	}{
		{`["--port","8080"]`, "", "127.0.0.1:8080"},
		{`["--port","8080","--hostname","0.0.0.0"]`, "", "127.0.0.1:8080"},
		{`["--hostname","10.0.0.1","--port","9000"]`, "", "10.0.0.1:9000"},
		{"", `{"PORT":"3000"}`, "127.0.0.1:3000"},
		{"", `{"PORT":"3000","HOSTNAME":"example.internal"}`, "example.internal:3000"},
		{"", `{}`, ""},
	}
	for _, tc := range cases {
		if got := AdvertisedAddress(tc.args, tc.env); got != tc.want {
			t.Fatalf("AdvertisedAddress(%q, %q) = %q, want %q", tc.args, tc.env, got, tc.want)
		}
	}
}

func TestTableUsesTheWorkspacePath(t *testing.T) {
	source := SourceFunc(func() []*unstructured.Unstructured {
		return []*unstructured.Unstructured{
			pod("pds", "default", "2j35eh7jjhsc8ny9", map[string]string{ArgsKey: `["--port","8080"]`}),
			pod("quiet", "default", "2j35eh7jjhsc8ny9", map[string]string{}),
		}
	})
	paths := PathResolverFunc(func(_ context.Context, cluster string) string {
		if cluster == "2j35eh7jjhsc8ny9" {
			return "root:alice"
		}
		return ""
	})
	resolver := New(Options{Source: source, Paths: paths, Domain: "kcp.local"})

	table, workspaces := resolver.Table(context.Background())
	if len(table) != 1 {
		t.Fatalf("table = %v", table)
	}
	if table["pds.default.alice.svc.kcp.local"] != "127.0.0.1:8080" {
		t.Fatalf("table = %v", table)
	}
	if len(workspaces) != 1 || workspaces[0] != "2j35eh7jjhsc8ny9" {
		t.Fatalf("workspaces = %v", workspaces)
	}
}

func TestEnvCarriesTheSelfNameBeforeThePodIsObserved(t *testing.T) {
	resolver := New(Options{
		Source: SourceFunc(func() []*unstructured.Unstructured { return nil }),
		Domain: "kcp.local",
	})
	target := ref.New("root:alice", "default", "pds")
	env := resolver.Env(context.Background(), target, `["--port","8080"]`, "", nil)
	var table map[string]string
	if err := json.Unmarshal([]byte(env[TableKey]), &table); err != nil {
		t.Fatal(err)
	}
	if table["pds.default.alice.svc.kcp.local"] != "127.0.0.1:8080" {
		t.Fatalf("table = %v", table)
	}
	if env[DomainKey] != "kcp.local" || env[NamespaceKey] != "default" {
		t.Fatalf("env = %v", env)
	}
	if _, present := env[TokensKey]; present {
		t.Fatal("no service account means no token table")
	}
}

func TestEnvCarriesTheShimSoAWorkloadPreloadsIt(t *testing.T) {
	resolver := New(Options{
		Source: SourceFunc(func() []*unstructured.Unstructured { return nil }),
		Domain: "kcp.local",
		Shim:   "/runs/.kcpdns/shim.ts",
	})
	target := ref.New("root:alice", "default", "pds")
	env := resolver.Env(context.Background(), target, `["--port","8080"]`, "", nil)
	if env[ShimKey] != "/runs/.kcpdns/shim.ts" {
		t.Fatalf("%s = %q, a workload that does not preload the shim resolves no cluster name", ShimKey, env[ShimKey])
	}
	without := New(Options{
		Source: SourceFunc(func() []*unstructured.Unstructured { return nil }),
		Domain: "kcp.local",
	}).Env(context.Background(), target, `["--port","8080"]`, "", nil)
	if _, present := without[ShimKey]; present {
		t.Fatal("no shim configured means no shim in the environment")
	}
}

type fakeMinter struct {
	clusters []string
}

func (f *fakeMinter) MintServiceAccountToken(_ context.Context, logicalCluster, _, _ string, _ time.Duration) (string, error) {
	f.clusters = append(f.clusters, logicalCluster)
	return "token-" + logicalCluster, nil
}

func TestTokensAreMintedPerWorkspace(t *testing.T) {
	minter := &fakeMinter{}
	resolver := New(Options{
		Source: SourceFunc(func() []*unstructured.Unstructured {
			return []*unstructured.Unstructured{pod("pds", "default", "cluster-a", map[string]string{ArgsKey: `["--port","80"]`})}
		}),
		Minter: minter,
		Domain: "kcp.local",
	})
	target := ref.New("root:bob", "default", "web")
	env := resolver.Env(context.Background(), target, `["--port","80"]`, "", &denospec.ServiceAccountRef{Name: "reader"})
	if env[TokensKey] == "" {
		t.Fatal("a named service account must produce a token table")
	}
	var tokens map[string]string
	if err := json.Unmarshal([]byte(env[TokensKey]), &tokens); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("tokens = %v, want one per workspace", tokens)
	}
	if tokens["root:bob"] != "token-root:bob" {
		t.Fatalf("tokens = %v", tokens)
	}
}
