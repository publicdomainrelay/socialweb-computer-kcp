//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type suiteTB struct {
	testing.TB
	name     string
	mu       sync.Mutex
	cleanups []func()
	failed   bool
}

type suiteFatal struct{ msg string }

func (s *suiteTB) Helper()      {}
func (s *suiteTB) Name() string { return s.name }
func (s *suiteTB) Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "integration [%s]: %s\n", s.name, fmt.Sprintf(format, args...))
}
func (s *suiteTB) Log(args ...any) { s.Logf("%s", fmt.Sprint(args...)) }
func (s *suiteTB) Errorf(format string, args ...any) {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
	s.Logf("ERROR: "+format, args...)
}
func (s *suiteTB) Error(args ...any) { s.Errorf("%s", fmt.Sprint(args...)) }
func (s *suiteTB) Fatalf(format string, args ...any) {
	s.Errorf(format, args...)
	panic(suiteFatal{msg: fmt.Sprintf(format, args...)})
}
func (s *suiteTB) Fatal(args ...any) { s.Fatalf("%s", fmt.Sprint(args...)) }
func (s *suiteTB) FailNow()          { s.Fatalf("FailNow") }
func (s *suiteTB) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}
func (s *suiteTB) Cleanup(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanups = append(s.cleanups, f)
}

func (s *suiteTB) runCleanups() {
	s.mu.Lock()
	fs := slices.Clone(s.cleanups)
	s.cleanups = nil
	s.mu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

type sharedEntry struct {
	once sync.Once
	tb   *suiteTB
	net  *Network
	err  error
}

var (
	sharedMu   sync.Mutex
	sharedNets = map[string]*sharedEntry{}
)

func SharedNetwork(t testing.TB, opts NetworkOptions) *Network {
	t.Helper()
	key := fmt.Sprintf("b%d-gw%t-d%s", opts.Bidders, opts.WithGateway, opts.Dumbpipe)
	sharedMu.Lock()
	e, ok := sharedNets[key]
	if !ok {
		e = &sharedEntry{tb: &suiteTB{name: "shared-" + key}}
		sharedNets[key] = e
	}
	sharedMu.Unlock()
	e.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				if f, ok := r.(suiteFatal); ok {
					e.err = errors.New(f.msg)
					return
				}
				panic(r)
			}
		}()
		e.net = StartNetwork(e.tb, opts)
	})
	if e.err != nil {
		t.Fatalf("the shared market network %s did not come up: %v", key, e.err)
	}
	b := e.net
	view := &Network{h: b.h, t: t, Instance: b.Instance, Dir: b.Dir, StateRoot: b.StateRoot, StateDir: b.StateDir,
		Handles: b.Handles, Logs: b.Logs, Started: b.Started, Up: b.Up, seenDevsup: true}
	t.Cleanup(func() {
		if t.Failed() {
			view.diagnose()
		}
	})
	return view
}

func stopSharedNetworks() bool {
	sharedMu.Lock()
	entries := make([]*sharedEntry, 0, len(sharedNets))
	for _, e := range sharedNets {
		entries = append(entries, e)
	}
	sharedMu.Unlock()
	ok := true
	for _, e := range entries {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, fatal := r.(suiteFatal); !fatal {
						panic(r)
					}
				}
			}()
			e.tb.runCleanups()
		}()
		if e.tb.Failed() {
			ok = false
		}
	}
	return ok
}

func (n *Network) WaitNoGuestsOf(receipts map[string]bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		vms, err := n.BidderVMs(context.Background())
		if err != nil {
			return err
		}
		var left []string
		for _, vm := range vms {
			if vm.Spec.Contract != nil && receipts[vm.Spec.Contract.ReceiptUri] {
				left = append(left, describeVM(vm))
			}
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guests of this test left %s after the release: %s", timeout, strings.Join(left, "; "))
		}
		time.Sleep(2 * time.Second)
	}
}
