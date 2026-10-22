package admission

import (
	"context"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/queue"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
)

type fakeSource struct {
	parent ref.Ref

	runs []queue.Run

	capacity queue.Capacity

	blocker *queue.Blocker

	missing bool
}

func (f *fakeSource) Parent(context.Context, queue.Run) (ref.Ref, bool, error) {
	return f.parent, !f.missing, nil
}

func (f *fakeSource) Runs(context.Context, ref.Ref) ([]queue.Run, error) {
	return append([]queue.Run(nil), f.runs...), nil
}

func (f *fakeSource) Capacity(context.Context, ref.Ref) (queue.Capacity, *queue.Blocker, error) {
	return f.capacity, f.blocker, nil
}

func terminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}

func run(name, phase string, seconds int) queue.Run {
	return queue.Run{
		Ref:     ref.New("root:alice", "default", name),
		Phase:   phase,
		Created: time.Unix(int64(seconds), 0).UTC(),
	}
}

func newTestAdmission(source *fakeSource) (*Admitter, *[]ref.Ref) {
	woken := &[]ref.Ref{}
	return New(Options{
		Source:     source,
		Now:        func() time.Time { return time.Unix(1000, 0) },
		RunKind:    "policyworkflowrun",
		ParentKind: "policyworkflowpod",
		Lifecycle:  queue.Lifecycle{Running: func(phase string) bool { return phase == "Running" }, Terminal: terminal},
		Wake: func(_ string, r ref.Ref) {
			*woken = append(*woken, r)
		},
	}), woken
}

func TestAdmitGrantsALeaseAndRefusesTheSecond(t *testing.T) {
	source := &fakeSource{
		parent:   ref.New("root:alice", "default", "pod"),
		capacity: queue.Capacity{Policy: queue.PolicyForbid},
		runs:     []queue.Run{run("a", "Pending", 1), run("b", "Pending", 2)},
	}
	admission, _ := newTestAdmission(source)
	first, err := admission.Admit(context.Background(), source.runs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !first.Allowed {
		t.Fatal("the oldest run must be admitted")
	}
	second, err := admission.Admit(context.Background(), source.runs[1])
	if err != nil {
		t.Fatal(err)
	}
	if second.Allowed || second.Reason != queue.ReasonAtCapacity {
		t.Fatalf("the second run = %+v", second)
	}
}

func TestAdmitWithNoParentIsNotGated(t *testing.T) {
	source := &fakeSource{parent: ref.New("root:alice", "default", "pod"), missing: true}
	admission, _ := newTestAdmission(source)
	result, err := admission.Admit(context.Background(), run("a", "Pending", 1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Gated {
		t.Fatalf("a run with no parent is not gated: %+v", result)
	}
}

func TestAdmitReportsABlocker(t *testing.T) {
	source := &fakeSource{
		parent:   ref.New("root:alice", "default", "pod"),
		capacity: queue.Capacity{Policy: queue.PolicyForbid},
		blocker:  &queue.Blocker{Reason: "EngineNotReady", Message: "no endpoint"},
		runs:     []queue.Run{run("a", "Pending", 1)},
	}
	admission, _ := newTestAdmission(source)
	result, err := admission.Admit(context.Background(), source.runs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !result.Gated || result.Allowed || result.Reason != "EngineNotReady" {
		t.Fatalf("blocked admission = %+v", result)
	}
	if admission.Leases().Len() != 0 {
		t.Fatal("a blocked parent must not be handed a lease: the slot it would hold is not free to give")
	}
}

func TestWakeEnqueuesCapacityManyPlusTheParent(t *testing.T) {
	source := &fakeSource{
		parent:   ref.New("root:alice", "default", "pod"),
		capacity: queue.Capacity{Policy: queue.PolicyForbid},
		runs: []queue.Run{
			run("a", "Pending", 1),
			run("b", "Pending", 2),
			run("c", "Pending", 3),
		},
	}
	admission, woken := newTestAdmission(source)
	if err := admission.Wake(context.Background(), source.parent); err != nil {
		t.Fatal(err)
	}
	if len(*woken) != 2 {
		t.Fatalf("woken = %v, want the oldest run plus the parent", *woken)
	}
	if (*woken)[0].Name != "a" {
		t.Fatalf("first wake = %q, want the oldest pending run", (*woken)[0].Name)
	}
	if (*woken)[1] != source.parent {
		t.Fatalf("last wake = %v, want the parent", (*woken)[1])
	}
}
