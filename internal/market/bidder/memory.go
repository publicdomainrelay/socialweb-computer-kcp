package bidder

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	MemoryPoll = 15 * time.Second

	MemoryRearmQuiet = 5 * time.Minute

	MemoryTypeGap = 10 * time.Minute

	MemoryEventCap = 10

	memoryLowMark = 0.9
)

type memorySample struct {
	high, max, oomKill uint64

	current, highLimit, maxLimit uint64
}

type memoryWatch struct {
	dir string

	last memorySample

	armed map[string]bool

	increasedAt map[string]time.Time

	sentAt map[string]time.Time

	sent map[string]int
}

func (b *Bidder) watchMemory(ctx context.Context) {
	root := cgroupRoot()
	if root == "" {
		return
	}
	watches := map[string]*memoryWatch{}
	tick := time.NewTicker(MemoryPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		live := map[string]bool{}
		for _, vm := range b.vms.Filter(b.liveGuest) {
			if terms := vm.Spec.Contract; terms == nil || terms.Receipt.URI == "" || vm.UID == "" || vm.Spec.BackendRef.Kind != v1alpha1.BackendKindFirecrackerVM {
				continue
			}
			live[vm.Name] = true
			w := watches[vm.Name]
			if w == nil {
				w = &memoryWatch{armed: map[string]bool{"high": true, "max": true}, increasedAt: map[string]time.Time{},
					sentAt: map[string]time.Time{}, sent: map[string]int{}}
				watches[vm.Name] = w
			}
			b.observeMemory(ctx, root, &vm, w)
		}
		for name := range watches {
			if !live[name] {
				delete(watches, name)
			}
		}
	}
}

func (b *Bidder) observeMemory(ctx context.Context, root string, vm *v1alpha1.ComputeVM, w *memoryWatch) {
	if w.dir == "" {
		w.dir = vmCgroupDir(root, vm.Name)
		if w.dir == "" {
			return
		}
		sample, err := readMemory(w.dir)
		if err != nil {
			w.dir = ""
			return
		}
		w.last = sample
		return
	}
	sample, err := readMemory(w.dir)
	if err != nil {
		return
	}
	prev := w.last
	w.last = sample
	now := b.now().UTC()
	for _, crossing := range []struct {
		kind           string
		was, is, limit uint64
	}{{"high", prev.high, sample.high, sample.highLimit}, {"max", prev.max, sample.max, sample.maxLimit}} {
		if crossing.is > crossing.was {
			w.increasedAt[crossing.kind] = now
			if w.armed[crossing.kind] && w.total() < MemoryEventCap && now.Sub(w.sentAt[crossing.kind]) >= MemoryTypeGap {
				w.armed[crossing.kind] = false
				w.sentAt[crossing.kind] = now
				w.sent[crossing.kind]++
				b.publishMemory(ctx, vm, crossing.kind, market.ComputeEventsVMMemoryPressure{Kind: crossing.kind, Threshold: int64(crossing.limit),
					Observed: int64(sample.current), ObservedAt: now.Format(time.RFC3339), CreatedAt: now.Format(time.RFC3339)})
			}
			continue
		}
		low := uint64(float64(crossing.limit) * memoryLowMark)
		if !w.armed[crossing.kind] && now.Sub(w.increasedAt[crossing.kind]) >= MemoryRearmQuiet && (crossing.limit == 0 || sample.current < low) {
			w.armed[crossing.kind] = true
		}
	}
	if kills := sample.oomKill - min(prev.oomKill, sample.oomKill); kills > 0 && w.total() < MemoryEventCap {
		w.sent["oom"]++
		b.publishMemory(ctx, vm, "oom", market.ComputeEventsVMOomKilled{Kills: int64(kills), Total: int64(sample.oomKill), Limit: int64(sample.maxLimit),
			ObservedAt: now.Format(time.RFC3339), CreatedAt: now.Format(time.RFC3339)})
	}
}

func (b *Bidder) publishMemory(ctx context.Context, vm *v1alpha1.ComputeVM, kind string, typed market.Record) {
	collection := typed.NSID()
	record, err := market.EncodeRecord(typed)
	if err != nil {
		b.log.Warn("encoding a VM memory event failed", "computevm", vm.Name, "collection", collection, "err", err)
		return
	}
	tag := ""
	for n := 1; ; n++ {
		tag = fmt.Sprintf("%s-%d", kind, n)
		if _, ok := b.syncs.Get(reportSyncName(string(vm.UID), collection, tag)); !ok {
			break
		}
	}
	if _, err := b.ensureReportSync(ctx, vm, collection, tag, record); err != nil {
		b.log.Warn("ensuring a VM memory event StateSync failed", "computevm", vm.Name, "collection", collection, "err", err)
		return
	}
	b.log.Warn("bidder_vm_memory_event", "computevm", vm.Name, "collection", collection, "tag", tag, "record", record)
}

func (w *memoryWatch) total() int {
	n := 0
	for _, sent := range w.sent {
		n += sent
	}
	return n
}

func cgroupRoot() string {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		parts := strings.Split(path, "/")
		for i, part := range parts {
			if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
				return filepath.Join(append([]string{"/sys/fs/cgroup"}, parts[:i+1]...)...)
			}
		}
	}
	return ""
}

func vmCgroupDir(root, vmName string) string {
	suffix := "-" + vmName + ".service"
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || found != "" {
			return fs.SkipDir
		}
		if strings.HasPrefix(d.Name(), "socialweb-vm-") && strings.HasSuffix(d.Name(), suffix) {
			found = path
			return fs.SkipAll
		}
		if strings.HasSuffix(d.Name(), ".service") && path != root {
			return fs.SkipDir
		}
		return nil
	})
	return found
}

func readMemory(dir string) (memorySample, error) {
	var s memorySample
	f, err := os.Open(filepath.Join(dir, "memory.events"))
	if err != nil {
		return s, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 {
			continue
		}
		n, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "high":
			s.high = n
		case "max":
			s.max = n
		case "oom_kill":
			s.oomKill = n
		}
	}
	s.current = readBytes(filepath.Join(dir, "memory.current"))
	s.highLimit = readBytes(filepath.Join(dir, "memory.high"))
	s.maxLimit = readBytes(filepath.Join(dir, "memory.max"))
	return s, nil
}

func readBytes(path string) uint64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n
}
