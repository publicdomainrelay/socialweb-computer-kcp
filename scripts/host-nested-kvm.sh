#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 [--check|--apply] [--qemu PATH]" >&2
  echo "  --check  (default) report the KVM module, its nested parameter now and in modprobe.d; exit 1 when nested is off" >&2
  echo "  --apply  (root) write /etc/modprobe.d/socialweb-nested-kvm.conf; when nested is off now, reload the module," >&2
  echo "           refused while any VM process runs or the module is in use (a reload ends every VM on the host)" >&2
  echo "  --qemu   also start PATH (the pinned qemu-system-x86_64, default \$SOCIALWEB_QEMU_PATH) with -accel kvm -cpu host" >&2
  exit 2
}

mode=check
qemu="${SOCIALWEB_QEMU_PATH:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --check) mode=check; shift ;;
    --apply) mode=apply; shift ;;
    --qemu) qemu="$2"; shift 2 ;;
    *) usage ;;
  esac
done

die() { echo "host-nested-kvm: $*" >&2; exit 1; }
say() { echo "host-nested-kvm: $*" >&2; }

case "$(grep -m1 -o -w 'vmx\|svm' /proc/cpuinfo || true)" in
  vmx) mod=kvm_intel ;;
  svm) mod=kvm_amd ;;
  *) die "the CPU flags carry neither vmx nor svm: no hardware virtualization (or it is off in firmware)" ;;
esac
conf=/etc/modprobe.d/socialweb-nested-kvm.conf
param="/sys/module/$mod/parameters/nested"

nested_now() { [ -r "$param" ] && case "$(cat "$param")" in 1|Y) return 0 ;; esac; return 1; }

if [ "$mode" = apply ]; then
  [ "$(id -u)" = 0 ] || die "--apply writes $conf and may reload $mod: run it as root"
  echo "options $mod nested=1" >"$conf"
  say "wrote $conf: options $mod nested=1"
  if ! nested_now; then
    vms="$(pgrep -x 'firecracker|qemu-system-x86' || true)"
    [ -z "$vms" ] || die "nested is off and VM processes run (pids: $(echo $vms)); stop them or reboot to apply $conf"
    modprobe -r "$mod" || die "$mod is in use; reboot to apply $conf"
    modprobe "$mod"
  fi
fi

[ -c /dev/kvm ] || die "/dev/kvm is missing ($mod loaded: $(lsmod | grep -c "^$mod " || true))"
persisted="$(grep -hs "^options $mod .*nested=" /etc/modprobe.d/*.conf | tail -1 || true)"
say "$mod nested=$(cat "$param" 2>/dev/null || echo missing); modprobe.d: ${persisted:-none (the module default)}"
nested_now || die "nested virtualization is off: sudo $0 --apply"

if [ -n "$qemu" ]; then
  root="$(dirname "$(dirname "$(dirname "$qemu")")")"
  set +e
  LD_LIBRARY_PATH="$root/usr/lib/sys" timeout 5 "$qemu" -L "$root/usr/share/qemu" -M microvm,acpi=off -accel kvm -cpu host \
    -m 64 -nodefaults -no-user-config -display none -S
  rc=$?
  set -e
  [ "$rc" = 124 ] || die "$qemu did not start a KVM microvm (exit $rc)"
  say "$qemu starts a KVM microvm with -cpu host"
fi
say "ok: guests booted with -cpu host get /dev/kvm"
