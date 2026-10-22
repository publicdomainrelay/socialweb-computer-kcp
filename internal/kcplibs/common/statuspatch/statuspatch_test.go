package statuspatch

import (
	"encoding/json"
	"testing"
)

func TestMergeWrapsStatus(t *testing.T) {
	body, err := Merge(map[string]any{"phase": "Running"})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["status"]["phase"] != "Running" {
		t.Fatalf("body = %s", body)
	}
}

func TestWithResourceVersion(t *testing.T) {
	body, err := Merge(map[string]any{"phase": "Running"})
	if err != nil {
		t.Fatal(err)
	}
	stamped, err := WithResourceVersion(body, "17")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(stamped, &obj); err != nil {
		t.Fatal(err)
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok || metadata["resourceVersion"] != "17" {
		t.Fatalf("stamped = %s", stamped)
	}
	if _, err := WithResourceVersion(body, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizerRemoveTestsAndReplaces(t *testing.T) {
	body, err := FinalizerRemove([]string{"a", "b"}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("a present finalizer must produce a patch")
	}
	var ops []map[string]any
	if err := json.Unmarshal(body, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0]["op"] != "test" || ops[1]["op"] != "add" {
		t.Fatalf("ops = %v", ops)
	}
	none, err := FinalizerRemove([]string{"a"}, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Fatalf("an absent finalizer must produce no patch, got %s", none)
	}
}

func TestFinalizerAdd(t *testing.T) {
	body, err := FinalizerAdd([]string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(body, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0]["op"] != "add" || ops[0]["path"] != "/metadata/finalizers" {
		t.Fatalf("ops = %v", ops)
	}
}

func TestFinalizersOf(t *testing.T) {
	body := []byte(`{"kind":"PolicyWorkflowRun","metadata":{"name":"r","finalizers":["a","b"]}}`)
	finalizers, err := FinalizersOf(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalizers) != 2 || finalizers[0] != "a" || finalizers[1] != "b" {
		t.Fatalf("finalizers = %v", finalizers)
	}
}
