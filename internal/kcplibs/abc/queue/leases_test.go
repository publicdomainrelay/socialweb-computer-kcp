package queue

import (
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

func TestLeasesCountHeldForParent(t *testing.T) {
	leases := NewLeases(time.Minute)
	parent := ref.New("root:alice", "default", "pod")
	first := ref.New("root:alice", "default", "run-a")
	second := ref.New("root:alice", "default", "run-b")
	other := ref.New("root:bob", "default", "pod")

	now := time.Unix(1000, 0)
	leases.Grant(first, parent, now)
	leases.Grant(second, parent, now)
	leases.Grant(ref.New("root:alice", "default", "run-c"), other, now)

	if held := leases.Count(parent, nil, now, testLifecycle); held != 2 {
		t.Fatalf("held = %d, want 2", held)
	}
	if held := leases.Count(other, nil, now, testLifecycle); held != 1 {
		t.Fatalf("held for another parent = %d, want 1", held)
	}
}

func TestLeasesReleaseOnRunningOrTerminal(t *testing.T) {
	leases := NewLeases(time.Minute)
	parent := ref.New("root:alice", "default", "pod")
	first := ref.New("root:alice", "default", "run-a")
	second := ref.New("root:alice", "default", "run-b")
	now := time.Unix(1000, 0)
	leases.Grant(first, parent, now)
	leases.Grant(second, parent, now)

	observed := map[ref.Ref]string{
		first:  "Running",
		second: "Succeeded",
	}
	if held := leases.Count(parent, observed, now, testLifecycle); held != 0 {
		t.Fatalf("held = %d, want 0 once both are observed", held)
	}
	if leases.Len() != 0 {
		t.Fatalf("entries = %d, want 0", leases.Len())
	}
}

func TestLeasesExpire(t *testing.T) {
	leases := NewLeases(time.Minute)
	parent := ref.New("root:alice", "default", "pod")
	run := ref.New("root:alice", "default", "run-a")
	leases.Grant(run, parent, time.Unix(1000, 0))
	if held := leases.Count(parent, nil, time.Unix(1000+61, 0), testLifecycle); held != 0 {
		t.Fatalf("held = %d, want 0 after the ttl", held)
	}
}

func TestLeasesForget(t *testing.T) {
	leases := NewLeases(time.Minute)
	parent := ref.New("root:alice", "default", "pod")
	run := ref.New("root:alice", "default", "run-a")
	leases.Grant(run, parent, time.Unix(1000, 0))
	leases.Forget(run)
	if held := leases.Count(parent, nil, time.Unix(1000, 0), testLifecycle); held != 0 {
		t.Fatalf("held = %d, want 0 after forget", held)
	}
}
