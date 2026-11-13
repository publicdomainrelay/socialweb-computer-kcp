package bidder

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contracthost "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/memsize"
)

type vmLimits struct {
	cpus int64

	memMiB int

	nestedVirt bool

	images bool
}

func (l vmLimits) mem() string {
	if l.memMiB <= 0 {
		return ""
	}
	return strconv.Itoa(l.memMiB) + "M"
}

func (b *Bidder) vmLimits() vmLimits {
	if !b.servesKind(string(v1alpha1.ComputeKindVM)) {
		return vmLimits{}
	}
	return vmLimits{cpus: int64(runtime.NumCPU()), memMiB: hostMemMiB(), nestedVirt: b.cfg.NestedVirt, images: b.cfg.ImageTarget != ""}
}

func hostMemMiB() int { return meminfoMiB("MemTotal:") }

func availableMemMiB() int { return meminfoMiB("MemAvailable:") }

func meminfoMiB(key string) int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) >= 2 && fields[0] == key {
			kib, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return kib / 1024
		}
	}
	return 0
}

type reservation struct {
	cpus int64

	mib int

	until time.Time

	vm string
}

func (b *Bidder) staticRefused(record vmRecord) (vmLimits, int, string) {
	limits := b.vmLimits()
	if record.NestedVirt && !limits.nestedVirt {
		return limits, 0, "the RFP asks for nestedVirt; this bidder's offering does not set it (its VMM is not QEMU or its host has KVM nested virtualization off)"
	}
	if record.Image != nil && !limits.images {
		return limits, 0, "the RFP asks for a requester image; this bidder's offering does not set images"
	}
	if record.Image != nil {
		if disk, err := memsize.ParseMiB(record.Disk); err != nil || disk <= 0 {
			return limits, 0, fmt.Sprintf("the RFP asks for a requester image and its disk %q is not a size such as 50G", record.Disk)
		}
	}
	if limits.cpus > 0 && int64(record.CPUs) > limits.cpus {
		return limits, 0, fmt.Sprintf("the RFP asks for %d vCPUs; this bidder offers at most %d (its offering's maxCpus)", record.CPUs, limits.cpus)
	}
	mib, err := memsize.ParseMiB(record.Mem)
	if err != nil || mib <= 0 {
		return limits, 0, fmt.Sprintf("the RFP's mem %q is not a memory size such as 8G", record.Mem)
	}
	if limits.memMiB > 0 && mib > limits.memMiB {
		return limits, 0, fmt.Sprintf("the RFP asks for %s of memory; this bidder offers at most %s (its offering's maxMem)", record.Mem, limits.mem())
	}
	return limits, mib, ""
}

func (b *Bidder) reserve(key string, record vmRecord, hold time.Duration) string {
	if record.Kind == string(v1alpha1.ComputeKindDeno) {
		return ""
	}
	limits, mib, refused := b.staticRefused(record)
	if refused != "" {
		return refused
	}
	b.reserveMu.Lock()
	defer b.reserveMu.Unlock()
	mib = contracthost.VMCommitMiB(mib)
	if refused := b.capacityRefused(limits, key, int64(record.CPUs), mib); refused != "" || hold <= 0 {
		return refused
	}
	if b.reserved == nil {
		b.reserved = map[string]reservation{}
	}
	until := b.now().Add(hold)
	if held, ok := b.reserved[key]; ok && held.until.After(until) {
		until = held.until
	}
	b.reserved[key] = reservation{cpus: int64(record.CPUs), mib: mib, until: until}
	return ""
}

func (b *Bidder) reservedFor(key, vm string) {
	b.reserveMu.Lock()
	defer b.reserveMu.Unlock()
	if held, ok := b.reserved[key]; ok {
		held.vm = vm
		if seen := b.now().Add(ReservedVMSeenGrace); held.until.After(seen) {
			held.until = seen
		}
		b.reserved[key] = held
	}
}

func (b *Bidder) unreserve(key string) {
	b.reserveMu.Lock()
	defer b.reserveMu.Unlock()
	delete(b.reserved, key)
}

func (b *Bidder) liveGuest(vm v1alpha1.ComputeVM) bool {
	return vm.DeletionTimestamp == nil && b.owns(vm.Spec.Owner.Handle) && !b.guests.Ended(&vm) && !b.guests.Releasing(&vm)
}

func (b *Bidder) committed(except string) (int64, int, int) {
	var cpus int64
	mib := 0
	for _, vm := range b.vms.Filter(b.liveGuest) {
		cpus += int64(vm.Spec.VM.Cpus)
		if m, err := memsize.ParseMiB(vm.Spec.VM.Mem); err == nil {
			mib += contracthost.VMCommitMiB(m)
		}
	}
	pending := 0
	now := b.now()
	for key, held := range b.reserved {
		if !now.Before(held.until) {
			delete(b.reserved, key)
			continue
		}
		if held.vm != "" {
			if _, ok := b.vms.Get(held.vm); ok {
				delete(b.reserved, key)
				continue
			}
		}
		if key == except {
			continue
		}
		cpus += held.cpus
		mib += held.mib
		pending += held.mib
	}
	return cpus, mib, pending
}

func (b *Bidder) capacityRefused(limits vmLimits, except string, cpus int64, mib int) string {
	heldCPUs, heldMiB, pendingMiB := b.committed(except)
	if limits.cpus > 0 && heldCPUs+cpus > limits.cpus {
		return fmt.Sprintf("the RFP asks for %d vCPUs; this bidder's running and reserved guests hold %d of its %d", cpus, heldCPUs, limits.cpus)
	}
	if limits.memMiB > 0 && heldMiB+mib > limits.memMiB {
		return fmt.Sprintf("the RFP asks for %dM of memory; this bidder's running and reserved guests hold %dM of its %s", mib, heldMiB, limits.mem())
	}
	if avail := availableMemMiB(); limits.memMiB > 0 && avail > 0 && mib > avail-pendingMiB {
		return fmt.Sprintf("the RFP asks for %dM of memory; this bidder's host has %dM available, %dM of it reserved", mib, avail, pendingMiB)
	}
	return ""
}

func (b *Bidder) acceptRefused(rfpURI, acceptURI, acceptCID string, payload market.ResolvedRecord) string {
	if b.vmForAccept(acceptURI, acceptCID) != nil {
		return ""
	}
	record, err := parseVMRecord(payload.Value)
	if err != nil {
		return ""
	}
	return b.reserve(rfpURI, record, AcceptReserveHold)
}

func (b *Bidder) submittedAcceptRefused(ctx context.Context, acceptURI, acceptCID string) string {
	resolved, err := b.records.Resolve(ctx, market.RecordRef{URI: acceptURI, CID: acceptCID})
	if err != nil {
		return ""
	}
	accept, err := market.ParseAccept(resolved.Value)
	if err != nil {
		return ""
	}
	rfpResolved, err := b.records.Resolve(ctx, market.RecordRef{URI: accept.RFP.URI, CID: accept.RFP.CID})
	if err != nil {
		return ""
	}
	rfp, err := market.ParseRFP(rfpResolved.Value)
	if err != nil {
		return ""
	}
	payload, err := b.records.Resolve(ctx, market.RecordRef{URI: rfp.Payload.URI, CID: rfp.Payload.CID})
	if err != nil {
		return ""
	}
	return b.acceptRefused(accept.RFP.URI, acceptURI, acceptCID, payload)
}
