//go:build linux

package devcheck

func hostChecks(h Host) []Result {
	return []Result{
		notRoot(h), runtimeDir(h), device(h, CheckKVM, "/dev/kvm", "VM guests (firecracker)",
			"add the user to the kvm group (root, once) and log in again"),
		device(h, CheckTun, "/dev/net/tun", "guest networking (pasta)",
			"load the tun module (root): modprobe tun"),
		userns(h), apparmor(h), newuidmap(h), subuid(h), logind(h),
	}
}

func runtimeDir(h Host) Result {
	r := Result{Name: CheckRuntimeDir, Needed: "devsup sockets", Status: StatusOK}
	v, ok := h.Env.LookupEnv("XDG_RUNTIME_DIR")
	r.Detail = v
	if !ok || v == "" {
		r.Status, r.Detail = StatusFail, "XDG_RUNTIME_DIR is unset"
		r.Fix = "run from a login session (or enable linger) so /run/user/<uid> exists and XDG_RUNTIME_DIR is set"
	}
	return r
}
