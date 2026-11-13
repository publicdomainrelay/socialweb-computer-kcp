package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	memorySampleEvery = 10 * time.Second

	memoryRearmAfter = 5 * time.Minute

	memoryEventEvery = 10 * time.Minute

	memoryEventsPerContract = 10

	memoryKindPSI = "psi"

	memoryKindSwap = "swap"

	memoryOOMKey = "oomKill"

	psiThreshold = 1000

	swapThreshold = 1000

	memoryStateFile = "memory-state.json"

	psiMemoryPath = "/proc/pressure/memory"

	meminfoPath = "/proc/meminfo"

	vmstatPath = "/proc/vmstat"
)

type memoryPressureRecord struct {
	Type string `json:"$type"`

	Kind string `json:"kind"`

	Threshold int64 `json:"threshold"`

	PSISomeAvg10 *int64 `json:"psiSomeAvg10,omitempty"`

	PSIFullAvg10 *int64 `json:"psiFullAvg10,omitempty"`

	SwapUsed int64 `json:"swapUsed"`

	SwapTotal int64 `json:"swapTotal"`

	ObservedAt string `json:"observedAt"`

	CreatedAt string `json:"createdAt"`
}

type oomKillRecord struct {
	Type string `json:"$type"`

	Kills int64 `json:"kills"`

	Total int64 `json:"total"`

	ObservedAt string `json:"observedAt"`

	CreatedAt string `json:"createdAt"`
}

type memoryState struct {
	Fired map[string]bool `json:"fired"`

	Below map[string]time.Time `json:"below"`

	Last map[string]time.Time `json:"last"`

	Posted int `json:"posted"`

	OOM int64 `json:"oom"`
}

func (a *Agent) runMemory(ctx context.Context) {
	path := filepath.Join(a.cfg.StateDir, memoryStateFile)
	st := memoryState{Fired: map[string]bool{}, Below: map[string]time.Time{}, Last: map[string]time.Time{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Fired == nil || st.Below == nil || st.Last == nil {
		st.Fired, st.Below, st.Last = map[string]bool{}, map[string]time.Time{}, map[string]time.Time{}
	}
	_, psiErr := os.Stat(psiMemoryPath)
	a.log.Info("the memory watch starts", "psi", psiErr == nil, "psiThreshold", psiThreshold, "swapThreshold", swapThreshold, "posted", st.Posted, "oomReported", st.OOM)
	ticker := time.NewTicker(memorySampleEvery)
	defer ticker.Stop()
	for {
		a.sampleMemory(ctx, &st, path)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) sampleMemory(ctx context.Context, st *memoryState, path string) {
	now := time.Now()
	allowed := func(key string) bool {
		return st.Posted < memoryEventsPerContract && now.Sub(st.Last[key]) >= memoryEventEvery
	}
	posted := func(key string) {
		st.Last[key] = now
		st.Posted++
	}
	gauges := map[string][2]int64{}
	some, full, psi := psiAvg10()
	if psi {
		gauges[memoryKindPSI] = [2]int64{some, psiThreshold}
	}
	used, total := swapBytes()
	if total > 0 {
		gauges[memoryKindSwap] = [2]int64{used * 10000 / total, swapThreshold}
	}
	for kind, g := range gauges {
		value, threshold := g[0], g[1]
		switch {
		case !st.Fired[kind] && value >= threshold && allowed(kind):
			record := memoryPressureRecord{Type: MemoryPressureNSID, Kind: kind, Threshold: threshold, SwapUsed: used, SwapTotal: total, ObservedAt: stamp(now), CreatedAt: createdAt()}
			if psi {
				record.PSISomeAvg10, record.PSIFullAvg10 = &some, &full
			}
			a.log.Warn("the guest crossed a memory threshold", "kind", kind, "observed", value, "threshold", threshold)
			if err := a.poster.Post(ctx, record.Type, kind, record); err != nil && ctx.Err() != nil {
				return
			}
			st.Fired[kind] = true
			delete(st.Below, kind)
			posted(kind)
		case st.Fired[kind] && value < threshold/2:
			if st.Below[kind].IsZero() {
				st.Below[kind] = now
			} else if now.Sub(st.Below[kind]) >= memoryRearmAfter {
				a.log.Info("a memory threshold re-arms", "kind", kind, "observed", value, "threshold", threshold)
				st.Fired[kind] = false
				delete(st.Below, kind)
			}
		case st.Fired[kind]:
			delete(st.Below, kind)
		}
	}
	if total, ok := vmstat("oom_kill"); ok {
		if total < st.OOM {
			st.OOM = total
		}
		if total > st.OOM && allowed(memoryOOMKey) {
			record := oomKillRecord{Type: OOMKillNSID, Kills: total - st.OOM, Total: total, ObservedAt: stamp(now), CreatedAt: createdAt()}
			a.log.Warn("the guest kernel killed a process for memory", "kills", record.Kills, "total", total)
			if err := a.poster.Post(ctx, record.Type, memoryOOMKey, record); err != nil && ctx.Err() != nil {
				return
			}
			st.OOM = total
			posted(memoryOOMKey)
		}
	}
	if raw, err := json.Marshal(st); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func psiAvg10() (some, full int64, ok bool) {
	raw, err := os.ReadFile(psiMemoryPath)
	if err != nil {
		return 0, 0, false
	}
	seen := 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, found := strings.CutPrefix(fields[1], "avg10=")
		f, err := strconv.ParseFloat(v, 64)
		if !found || err != nil {
			continue
		}
		switch fields[0] {
		case "some":
			some, seen = min(int64(f*100), 10000), seen+1
		case "full":
			full, seen = min(int64(f*100), 10000), seen+1
		}
	}
	return some, full, seen == 2
}

func swapBytes() (used, total int64) {
	t, okT := meminfo("SwapTotal")
	f, okF := meminfo("SwapFree")
	if !okT || !okF || t == 0 {
		return 0, 0
	}
	return (t - f) * 1024, t * 1024
}

func meminfo(key string) (int64, bool) { return procValue(meminfoPath, key+":") }

func vmstat(key string) (int64, bool) { return procValue(vmstatPath, key) }

func procValue(path, key string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == key {
			v, err := strconv.ParseInt(fields[1], 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}
