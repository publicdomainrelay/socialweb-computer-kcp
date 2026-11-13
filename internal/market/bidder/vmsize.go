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
	return vmLimits{cpus: int64(runtime.NumCPU()), memMiB: hostMemMiB()}
}

func hostMemMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
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
	return ""
}
