use std::env;
use std::fs;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream, UdpSocket};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread::sleep;
use std::time::{Duration, Instant};

pub fn need(var: &str) -> String {
    env::var(var).unwrap_or_else(|_| {
        panic!("{var} is unset: these tests drive a socialweb-computer binary on a KVM host; see tests/vmm/README.md")
    })
}

pub struct Out {
    pub ok: bool,
    pub stdout: String,
    pub stderr: String,
}

pub fn run(cmd: &mut Command, stdin: &[u8], limit: Duration) -> Out {
    let mut child = cmd
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap_or_else(|e| panic!("spawning {cmd:?}: {e}"));
    let mut input = child.stdin.take().unwrap();
    let data = stdin.to_vec();
    let writer = std::thread::spawn(move || {
        let _ = input.write_all(&data);
    });
    let mut so = child.stdout.take().unwrap();
    let mut se = child.stderr.take().unwrap();
    let ro = std::thread::spawn(move || {
        let mut s = Vec::new();
        let _ = so.read_to_end(&mut s);
        s
    });
    let re = std::thread::spawn(move || {
        let mut s = Vec::new();
        let _ = se.read_to_end(&mut s);
        s
    });
    let start = Instant::now();
    let status = loop {
        if let Some(st) = child.try_wait().unwrap() {
            break Some(st);
        }
        if start.elapsed() > limit {
            let _ = child.kill();
            let _ = child.wait();
            break None;
        }
        sleep(Duration::from_millis(100));
    };
    let _ = writer.join();
    let stdout = String::from_utf8_lossy(&ro.join().unwrap()).into_owned();
    let stderr = String::from_utf8_lossy(&re.join().unwrap()).into_owned();
    Out { ok: status.is_some_and(|s| s.success()), stdout, stderr }
}

pub fn must(cmd: &mut Command, stdin: &[u8], limit: Duration) -> String {
    let desc = format!("{cmd:?}");
    let o = run(cmd, stdin, limit);
    assert!(o.ok, "{desc} failed\nstdout:\n{}\nstderr:\n{}", o.stdout, o.stderr);
    o.stdout
}

pub fn field(json: &str, key: &str) -> String {
    let pat = format!("\"{key}\":");
    let i = json.find(&pat).unwrap_or_else(|| panic!("no {key} in {json}")) + pat.len();
    let rest = json[i..].trim_start();
    if let Some(s) = rest.strip_prefix('"') {
        return s[..s.find('"').unwrap()].to_string();
    }
    rest[..rest.find([',', '\n', '}']).unwrap()].trim().to_string()
}

pub fn wait_for<T>(what: &str, limit: Duration, mut f: impl FnMut() -> Option<T>) -> T {
    let start = Instant::now();
    loop {
        if let Some(v) = f() {
            return v;
        }
        assert!(start.elapsed() < limit, "{what}: not within {limit:?}");
        sleep(Duration::from_secs(2));
    }
}

pub fn free_tcp_port() -> u16 {
    TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port()
}

pub fn free_udp_port() -> u16 {
    UdpSocket::bind("0.0.0.0:0").unwrap().local_addr().unwrap().port()
}

pub fn outbound_addr() -> String {
    let s = UdpSocket::bind("0.0.0.0:0").unwrap();
    s.connect("192.0.2.255:9").unwrap();
    s.local_addr().unwrap().ip().to_string()
}

pub fn scratch(name: &str) -> PathBuf {
    let d = env::temp_dir().join(format!("swc-vmm-{name}-{}", std::process::id()));
    let _ = fs::remove_dir_all(&d);
    fs::create_dir_all(&d).unwrap();
    d
}

pub fn keygen(path: &Path) -> String {
    must(
        Command::new("ssh-keygen").args(["-q", "-t", "ed25519", "-N", "", "-C", "swc-vmm-test", "-f"]).arg(path),
        b"",
        Duration::from_secs(10),
    );
    fs::read_to_string(path.with_extension("pub")).unwrap().trim().to_string()
}

fn which(name: &str) -> PathBuf {
    let path = env::var("PATH").unwrap_or_default() + ":/usr/sbin:/usr/bin";
    path.split(':')
        .map(|d| Path::new(d).join(name))
        .find(|p| p.is_file())
        .unwrap_or_else(|| panic!("{name} is not on PATH"))
}

pub struct Sshd {
    child: Child,
    pub port: u16,
}

impl Sshd {
    pub fn start(dir: &Path, tunnel_pub: &str, listen: u16) -> Sshd {
        let port = free_tcp_port();
        keygen(&dir.join("host_ed25519"));
        fs::write(
            dir.join("authorized_keys"),
            format!("restrict,port-forwarding,permitlisten=\"127.0.0.1:{listen}\",command=\"/bin/false\" {tunnel_pub}\n"),
        )
        .unwrap();
        let cfg = dir.join("sshd_config");
        fs::write(
            &cfg,
            format!(
                "Port {port}\nListenAddress 127.0.0.1\nHostKey {}\nAuthorizedKeysFile {}\nPidFile none\nUsePAM no\nStrictModes no\n\
                 PasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowTcpForwarding remote\nGatewayPorts no\nLogLevel ERROR\n",
                dir.join("host_ed25519").display(),
                dir.join("authorized_keys").display()
            ),
        )
        .unwrap();
        let child = Command::new(which("sshd"))
            .args(["-D", "-e", "-f"])
            .arg(&cfg)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(fs::File::create(dir.join("sshd.log")).unwrap())
            .spawn()
            .expect("starting sshd");
        wait_for("the test sshd listens", Duration::from_secs(10), || TcpStream::connect(("127.0.0.1", port)).ok());
        Sshd { child, port }
    }
}

impl Drop for Sshd {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

pub struct Vm {
    pub name: String,
    pub vmm: Option<String>,
    pub reference: String,
    stopped: bool,
}

pub fn swc(vmm: &Option<String>, args: &[&str]) -> Command {
    let mut c = Command::new(need("SWC"));
    c.args(["internal", "vm"]).args(args);
    if let Some(v) = vmm {
        c.args(["--vmm", v]);
    }
    c
}

impl Vm {
    pub fn start(name: &str, vmm: Option<&str>, spec: &str) -> Vm {
        let vmm = vmm.map(str::to_string);
        let reference = must(&mut swc(&vmm, &["start"]), spec.as_bytes(), Duration::from_secs(600));
        Vm { name: name.to_string(), vmm, reference, stopped: false }
    }

    pub fn get(&self, key: &str) -> String {
        field(&self.reference, key)
    }

    pub fn inspect(&self) -> String {
        must(&mut swc(&self.vmm, &["inspect", &self.name]), b"", Duration::from_secs(30))
    }

    pub fn stop(&mut self) -> String {
        self.stopped = true;
        must(&mut swc(&self.vmm, &["stop", &self.name]), b"", Duration::from_secs(300))
    }

    pub fn console(&self) -> String {
        fs::read_to_string(Path::new(&self.get("workDir")).join("console.log")).unwrap_or_default()
    }
}

impl Drop for Vm {
    fn drop(&mut self) {
        if !self.stopped {
            let o = run(&mut swc(&self.vmm, &["stop", &self.name]), b"", Duration::from_secs(300));
            eprintln!("cleanup stop {}: {} {}", self.name, o.stdout, o.stderr);
        }
    }
}

pub struct Guest {
    pub key: PathBuf,
    pub port: u16,
}

impl Guest {
    fn cmd(&self) -> Command {
        let mut c = Command::new("ssh");
        c.arg("-i").arg(&self.key).args([
            "-p",
            &self.port.to_string(),
            "-o",
            "StrictHostKeyChecking=no",
            "-o",
            "UserKnownHostsFile=/dev/null",
            "-o",
            "BatchMode=yes",
            "-o",
            "ConnectTimeout=5",
            "-o",
            "LogLevel=ERROR",
            "t@127.0.0.1",
        ]);
        c
    }

    pub fn try_sh(&self, script: &str, stdin: &[u8], limit: Duration) -> Out {
        run(self.cmd().arg(script), stdin, limit)
    }

    pub fn sh(&self, script: &str) -> String {
        self.sh_in(script, b"", Duration::from_secs(120))
    }

    pub fn sh_in(&self, script: &str, stdin: &[u8], limit: Duration) -> String {
        must(self.cmd().arg(script), stdin, limit).trim().to_string()
    }
}

pub fn processes_naming(needle: &str) -> Vec<String> {
    let mut out = Vec::new();
    for e in fs::read_dir("/proc").unwrap().flatten() {
        if !e.file_name().to_string_lossy().bytes().all(|b| b.is_ascii_digit()) {
            continue;
        }
        if let Ok(raw) = fs::read(e.path().join("cmdline")) {
            let line = String::from_utf8_lossy(&raw).replace('\0', " ");
            if line.contains(needle) {
                out.push(format!("{}: {line}", e.file_name().to_string_lossy()));
            }
        }
    }
    out
}
