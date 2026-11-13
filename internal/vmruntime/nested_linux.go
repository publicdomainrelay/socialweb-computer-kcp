package vmruntime

import (
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// kvmCreateVM is KVM_CREATE_VM, _IO(KVMIO=0xAE, 0x01).
const kvmCreateVM = 0xAE01

var virtFlag = regexp.MustCompile(`(?m)^flags\s*:.*\b(vmx|svm)\b`)

// HostNestedVirt reports whether a QEMU guest of this host can run KVM guests of its own: the CPU has vmx
// (Intel) or svm (AMD), its KVM module has nested on, and /dev/kvm creates a VM. QEMU passes the flag through
// with -cpu host.
func HostNestedVirt() bool {
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	m := virtFlag.FindSubmatch(cpuinfo)
	if m == nil {
		return false
	}
	module := map[string]string{"vmx": "kvm_intel", "svm": "kvm_amd"}[string(m[1])]
	b, err := os.ReadFile("/sys/module/" + module + "/parameters/nested")
	if v := strings.TrimSpace(string(b)); err != nil || (v != "Y" && v != "1") {
		return false
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer kvm.Close()
	vm, _, errno := unix.Syscall(unix.SYS_IOCTL, kvm.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return false
	}
	unix.Close(int(vm))
	return true
}
