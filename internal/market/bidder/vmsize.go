package bidder

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/memsize"
)

type vmLimits struct {
	cpus int64

	memMiB int

	nestedVirt bool
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
	return vmLimits{cpus: int64(runtime.NumCPU()), memMiB: hostMemMiB(), nestedVirt: b.cfg.NestedVirt}
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

func (b *Bidder) sizeRefused(record vmRecord) string {
	if record.Kind == string(v1alpha1.ComputeKindDeno) {
		return ""
	}
	limits := b.vmLimits()
	if record.NestedVirt && !limits.nestedVirt {
		return "the RFP asks for nestedVirt; this bidder's offering does not set it (its VMM is not QEMU or its host has KVM nested virtualization off)"
	}
	if limits.cpus > 0 && int64(record.CPUs) > limits.cpus {
		return fmt.Sprintf("the RFP asks for %d vCPUs; this bidder offers at most %d (its offering's maxCpus)", record.CPUs, limits.cpus)
	}
	mib, err := memsize.ParseMiB(record.Mem)
	if err != nil || mib <= 0 {
		return fmt.Sprintf("the RFP's mem %q is not a memory size such as 8G", record.Mem)
	}
	if limits.memMiB > 0 && mib > limits.memMiB {
		return fmt.Sprintf("the RFP asks for %s of memory; this bidder offers at most %s (its offering's maxMem)", record.Mem, limits.mem())
	}
	return b.capacityRefused(limits, int64(record.CPUs), mib)
}

func (b *Bidder) liveGuest(vm v1alpha1.ComputeVM) bool {
	return vm.DeletionTimestamp == nil && b.owns(vm.Spec.Owner.Handle) && !b.guests.Ended(&vm) && !b.guests.Releasing(&vm)
}

func (b *Bidder) committed() (int64, int) {
	var cpus int64
	mib := 0
	for _, vm := range b.vms.Filter(b.liveGuest) {
		cpus += int64(vm.Spec.VM.Cpus)
		if m, err := memsize.ParseMiB(vm.Spec.VM.Mem); err == nil {
			mib += m
		}
	}
	return cpus, mib
}

func (b *Bidder) capacityRefused(limits vmLimits, cpus int64, mib int) string {
	heldCPUs, heldMiB := b.committed()
	if limits.cpus > 0 && heldCPUs+cpus > limits.cpus {
		return fmt.Sprintf("the RFP asks for %d vCPUs; this bidder's running guests hold %d of its %d", cpus, heldCPUs, limits.cpus)
	}
	if limits.memMiB > 0 && heldMiB+mib > limits.memMiB {
		return fmt.Sprintf("the RFP asks for %dM of memory; this bidder's running guests hold %dM of its %s", mib, heldMiB, limits.mem())
	}
	if free := availableMemMiB(); limits.memMiB > 0 && free > 0 && mib > free {
		return fmt.Sprintf("the RFP asks for %dM of memory; this bidder's host has %dM available", mib, free)
	}
	return ""
}
