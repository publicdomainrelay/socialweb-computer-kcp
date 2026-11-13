use std::fs;
use std::net::UdpSocket;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;

use swc_vmm_tests::*;

fn json_str(s: &str) -> String {
    format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\"").replace('\n', "\\n"))
}

fn user_data(client_pub: &str, tunnel_key: &str, guest_ssh: u16, sshd: u16) -> String {
    let user = need("USER");
    let key: String = tunnel_key.lines().map(|l| format!("      {l}\n")).collect();
    format!(
        "#cloud-config\n\
         users:\n  - name: t\n    shell: /bin/bash\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    ssh_authorized_keys: [\"{client_pub}\"]\n\
         write_files:\n  - path: /etc/swc-test/tunnel_key\n    permissions: \"0600\"\n    content: |\n{key}\
         runcmd:\n  - echo userdata-ok > /run/swc-test-userdata\n  \
         - systemd-run --unit=swc-test-tunnel -p Restart=always -p RestartSec=3 /usr/bin/ssh -i /etc/swc-test/tunnel_key \
         -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=5 -o ExitOnForwardFailure=yes \
         -N -R 127.0.0.1:{guest_ssh}:127.0.0.1:22 -p {sshd} {user}@192.0.2.1\n"
    )
}

fn image_dir() -> PathBuf {
    let src = need("SOCIALWEB_GUEST_IMAGE_SOURCE");
    PathBuf::from(src.strip_prefix("file://").expect("SOCIALWEB_GUEST_IMAGE_SOURCE must be a file:// dir for the nested boot"))
}

fn vmm_exe(vmm: &str) -> String {
    need(if vmm == "qemu" { "SOCIALWEB_QEMU_PATH" } else { "SOCIALWEB_FIRECRACKER_PATH" })
}

fn exercise(vmm: Option<&str>, want: &str) {
    let name = format!("vmmtest-{want}-{}", std::process::id());
    let dir = scratch(&name);
    let tunnel_pub = keygen(&dir.join("tunnel"));
    let client_pub = keygen(&dir.join("client"));
    let guest_ssh = free_tcp_port();
    let sshd = Sshd::start(&dir, &tunnel_pub, guest_ssh);
    let webrtc = free_udp_port();
    let ud = user_data(&client_pub, &fs::read_to_string(dir.join("tunnel")).unwrap(), guest_ssh, sshd.port);
    let spec = format!(
        "{{\"name\":\"{name}\",\"userData\":{},\"vcpu\":2,\"memMiB\":2048,\"diskMiB\":4096,\"hostPorts\":[{}],\"webrtcUDPPort\":{webrtc}}}",
        json_str(&ud),
        sshd.port
    );

    let mut vm = Vm::start(&name, vmm, &spec);
    assert_eq!(vm.get("hypervisor"), want, "the VM booted under another VMM: {}", vm.reference);
    assert_eq!(field(&vm.inspect(), "verdict"), "Running");

    let guest = Guest { key: dir.join("client"), port: guest_ssh };
    let mut tries = 0;
    wait_for("ssh into the guest through its reverse tunnel", Duration::from_secs(300), || {
        tries += 1;
        let o = guest.try_sh("true", b"", Duration::from_secs(20));
        if !o.ok && tries % 15 == 0 {
            let c = vm.console();
            eprintln!("still no ssh ({}); console tail:\n{}", o.stderr.trim(), &c[c.len().saturating_sub(2000)..]);
        }
        o.ok.then_some(())
    });

    assert_eq!(guest.sh("cat /run/swc-test-userdata"), "userdata-ok", "user-data did not run");
    assert_eq!(guest.sh("cat /etc/hostname"), name, "meta-data did not name the guest");

    assert_eq!(guest.sh("nproc"), "2");
    let mem_kib: u64 = guest.sh("awk '/^MemTotal/{print $2}' /proc/meminfo").parse().unwrap();
    assert!((1_700_000..=2_097_152).contains(&mem_kib), "MemTotal {mem_kib} KiB for 2048 MiB");
    let root_mib: u64 = guest.sh("df -BM --output=size / | tail -1 | tr -dc 0-9").parse().unwrap();
    assert!(root_mib >= 3500, "the root fs is {root_mib} MiB for diskMiB 4096");

    let written = guest.sh(
        "dd if=/dev/urandom of=/var/tmp/blob bs=1M count=256 conv=fsync status=none && sha256sum < /var/tmp/blob",
    );
    let read = guest.sh("sync && echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null && sha256sum < /var/tmp/blob");
    assert_eq!(written, read, "the disk does not read back what was written");

    assert_eq!(
        guest.sh("curl -fsS -o /dev/null -w '%{http_code}' --max-time 30 https://cloud-images.ubuntu.com/"),
        "200",
        "no network out"
    );

    let touched = guest.sh(
        "python3 -c \"m=int([l for l in open('/proc/meminfo') if l.startswith('MemAvailable')][0].split()[1])*1024*9//10; \
         b=bytearray(m); b[::4096]=b'\\x01'*len(range(0,m,4096)); print(m>>20)\"",
    );
    eprintln!("{want}: the guest touched {touched} MiB");
    assert_eq!(field(&vm.inspect(), "verdict"), "Running", "the VM did not survive its guest filling its memory");

    iroh_tunnel(&guest, &dir, &name);
    webrtc_forward(&guest, webrtc);

    if want == "qemu" {
        nested(&guest);
    }

    let exe = vmm_exe(want);
    let (unit, vmm_id, work, socket, vmm_uid) =
        (vm.get("unit"), vm.get("vmmID"), vm.get("workDir"), vm.get("socket"), vm.get("vmmUID"));
    let stopped = vm.stop();
    assert_eq!(field(&stopped, "graceful"), "true", "ctrl-alt-del did not end the guest: {stopped}");
    eprintln!("{want}: stopped gracefully in {} ns; checking for orphans", field(&stopped, "elapsed"));

    let listed = must(&mut swc(&vm.vmm, &["list"]), b"", Duration::from_secs(30));
    assert!(!listed.contains(&format!("\"{name}\"")), "vm list still has {name}: {listed}");
    assert!(!Path::new(&work).exists(), "the work dir {work} is left");
    assert!(!Path::new(&socket).parent().unwrap().exists(), "the socket dir of {socket} is left");
    let left = [processes_naming(&vmm_id), processes_naming(&work)].concat();
    assert!(left.is_empty(), "processes outlive the VM: {left:?}");
    let state = run(Command::new("systemctl").args(["--user", "is-active", &unit]), b"", Duration::from_secs(10));
    assert_eq!(state.stdout.trim(), "inactive", "unit {unit} is {}", state.stdout.trim());
    let acl = must(Command::new("getfacl").args(["-n", "--omit-header", &exe]), b"", Duration::from_secs(10));
    assert!(!acl.contains(&format!("user:{vmm_uid}:")), "the grant to uid {vmm_uid} on {exe} is left: {acl}");
    drop(sshd);
    let _ = fs::remove_dir_all(&dir);
}

fn iroh_tunnel(guest: &Guest, dir: &Path, name: &str) {
    guest.sh("sudo systemd-run --unit=swc-test-dumbpipe /usr/local/bin/dumbpipe listen-tcp --host 127.0.0.1:22");
    let ticket = wait_for("the guest's dumbpipe ticket", Duration::from_secs(60), || {
        let t = guest.sh("sudo journalctl -u swc-test-dumbpipe -o cat | grep -o 'endpoint[a-z0-9]*' | head -1 || true");
        (!t.is_empty()).then_some(t)
    });
    let encoded = guest.sh_in("base64 -w0 /usr/local/bin/dumbpipe", b"", Duration::from_secs(60));
    let bin = dir.join("dumbpipe");
    must(Command::new("sh").arg("-c").arg("base64 -d > \"$0\" && chmod +x \"$0\"").arg(&bin), encoded.as_bytes(), Duration::from_secs(30));
    let port = free_tcp_port();
    let mut connect = Command::new(&bin)
        .args(["connect-tcp", "--addr", &format!("127.0.0.1:{port}"), &ticket])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(fs::File::create(dir.join("dumbpipe.log")).unwrap())
        .spawn()
        .unwrap();
    let through = Guest { key: guest.key.clone(), port };
    let host = wait_for("ssh over the iroh tunnel", Duration::from_secs(90), || {
        let o = through.try_sh("cat /etc/hostname", b"", Duration::from_secs(30));
        o.ok.then(|| o.stdout.trim().to_string())
    });
    let _ = connect.kill();
    let _ = connect.wait();
    assert_eq!(host, name);
    eprintln!("{name}: ssh over iroh answered {host}");
    guest.sh("sudo systemctl stop swc-test-dumbpipe");
}

fn webrtc_forward(guest: &Guest, port: u16) {
    guest.sh(&format!("sudo systemd-run --unit=swc-test-udp sh -c 'nc -ul {port} > /run/swc-test-udp'"));
    let lan = outbound_addr();
    let payload = format!("swc-webrtc-{}", std::process::id());
    let s = UdpSocket::bind("0.0.0.0:0").unwrap();
    wait_for(&format!("a datagram to {lan}:{port} reaches the guest"), Duration::from_secs(60), || {
        s.send_to(payload.as_bytes(), (lan.as_str(), port)).unwrap();
        guest.sh("cat /run/swc-test-udp 2>/dev/null || true").contains(&payload).then_some(())
    });
    guest.sh("sudo systemctl stop swc-test-udp");
}

fn nested(guest: &Guest) {
    assert_eq!(guest.sh("test -c /dev/kvm && echo yes"), "yes", "no /dev/kvm in a QEMU guest");
    let flag = guest.sh("grep -m1 -o -w 'vmx\\|svm' /proc/cpuinfo || true");
    assert!(flag == "vmx" || flag == "svm", "the QEMU guest's CPU has neither vmx nor svm (-cpu host)");
    eprintln!("qemu nested: guest cpu flag {flag}");
    let exe = PathBuf::from(need("SOCIALWEB_QEMU_PATH"));
    let root = exe.parent().unwrap().parent().unwrap().parent().unwrap();
    let tar = Command::new("tar").arg("-C").arg(root).args(["-cf", "-", "usr"]).output().unwrap();
    assert!(tar.status.success());
    guest.sh_in("mkdir -p q && tar -C q -xf -", &tar.stdout, Duration::from_secs(300));
    guest.sh_in("cat > vmlinux", &fs::read(image_dir().join("vmlinux")).unwrap(), Duration::from_secs(300));
    let out = guest.sh_in(
        "sudo timeout 120 env LD_LIBRARY_PATH=$HOME/q/usr/lib/sys $HOME/q/usr/bin/qemu-system-x86_64 -L $HOME/q/usr/share/qemu \
         -M microvm,acpi=off -accel kvm -cpu host -m 256 -nodefaults -no-user-config -display none -no-reboot -serial stdio \
         -kernel vmlinux -append 'console=ttyS0 panic=-1 pci=off acpi=off' </dev/null 2>&1; true",
        b"",
        Duration::from_secs(180),
    );
    assert!(
        out.contains("Hypervisor detected: KVM") && out.contains("Unable to mount root fs"),
        "the nested guest kernel did not boot under KVM:\n{}",
        &out[out.len().saturating_sub(3000)..]
    );
    for l in out.lines().filter(|l| l.contains("Hypervisor detected") || l.contains("Unable to mount root")) {
        eprintln!("qemu nested: {l}");
    }
}

#[test]
fn qemu_is_the_default_and_its_guest_runs_a_nested_guest() {
    exercise(None, "qemu");
}

#[test]
fn firecracker_stays_selectable() {
    exercise(Some("firecracker"), "firecracker");
}

/// A market network inside a QEMU guest: `dev up --profile market-network` with one bidder, then a `run request
/// --network` the guest's bidder must hold, so the guest kernel can fence a guest of its own (bridge-family nf_tables
/// reject). Needs SWC_GUEST_BIN, a release-like socialweb-computer for linux/amd64 (assets embedded).
#[test]
fn qemu_guest_hosts_a_market_network_bidder() {
    let bin = PathBuf::from(need("SWC_GUEST_BIN"));
    let name = format!("vmmtest-bidder-{}", std::process::id());
    let dir = scratch(&name);
    let tunnel_pub = keygen(&dir.join("tunnel"));
    let client_pub = keygen(&dir.join("client"));
    let guest_ssh = free_tcp_port();
    let sshd = Sshd::start(&dir, &tunnel_pub, guest_ssh);
    let ud = user_data(&client_pub, &fs::read_to_string(dir.join("tunnel")).unwrap(), guest_ssh, sshd.port);
    let spec = format!(
        "{{\"name\":\"{name}\",\"userData\":{},\"vcpu\":4,\"memMiB\":12288,\"diskMiB\":30720,\"hostPorts\":[{}],\"webrtcUDPPort\":{}}}",
        json_str(&ud),
        sshd.port,
        free_udp_port()
    );
    let mut vm = Vm::start(&name, None, &spec);
    let guest = Guest { key: dir.join("client"), port: guest_ssh };
    wait_for("ssh into the guest", Duration::from_secs(300), || guest.try_sh("true", b"", Duration::from_secs(20)).ok.then_some(()));
    guest.sh_in("cat > swc && chmod 0755 swc", &fs::read(&bin).unwrap(), Duration::from_secs(600));
    // the staging module's account: dev refuses root
    guest.sh_in(
        "set -e; sudo useradd -m -G kvm stg; sudo sed -i /^stg:/d /etc/subuid /etc/subgid; \
         sudo usermod --add-subuids 1000000-2048575 --add-subgids 1000000-2048575 stg; \
         command -v newuidmap >/dev/null || { sudo apt-get update -q >/dev/null; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q uidmap >/dev/null; }; \
         sudo loginctl enable-linger stg; sudo install -m 0755 swc /home/stg/swc",
        b"",
        Duration::from_secs(600),
    );
    // stg's own cwd (dev stats it); cmd has no single quotes
    let as_stg = |cmd: &str| format!("sudo runuser -u stg -- sh -c 'cd /home/stg && HOME=/home/stg XDG_RUNTIME_DIR=/run/user/$(id -u stg) {cmd}' 2>&1");
    let st = "--instance g --state-dir /home/stg/s";
    let up = guest.sh_in(&as_stg(&format!("/home/stg/swc dev up {st} --profile market-network --bidders 1 --host-backend proc --network-timeout 15m")), b"", Duration::from_secs(1200));
    eprintln!("dev up: {}", &up[up.len().saturating_sub(300)..]);
    let req = guest.sh_in(&as_stg(&format!("/home/stg/swc run request --network g --state-dir /home/stg/s --name r1 --exec \"uname -r\"")), b"", Duration::from_secs(900));
    let _ = guest.try_sh(&as_stg(&format!("/home/stg/swc dev down {st}")), b"", Duration::from_secs(300));
    let _ = vm.stop();
    let _ = fs::remove_dir_all(&dir);
    assert!(!req.contains("fencing the guest") && req.contains("Held"), "the guest's bidder did not hold a guest of its own:\n{}", &req[req.len().saturating_sub(3000)..]);
}
