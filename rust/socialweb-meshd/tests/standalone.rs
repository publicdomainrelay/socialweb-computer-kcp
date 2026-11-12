mod common;

use std::fs::File;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Output, Stdio};
use std::time::Duration;

use common::{
    READY_WAIT, eventually, gate, identity, must, reap, scratch_dir, status,
    stop_namespace_processes, sudo, wait_for_log,
};

const GATE: &str = "SOCIALWEB_MESHD_NETNS";
const ZONE: &str = "staging.internal";
const MESH_PORT: &str = "47001";
const UPSTREAM: &str = "198.18.0.53";
const ROSTER_HOST: &str = "198.18.0.80";
const ROSTER_PORT: &str = "8080";

struct Vm {
    name: &'static str,
    n: u8,
    nat: String,
    ns: String,
    secret: PathBuf,
    log: PathBuf,
    endpoint: String,
    roster: String,
    child: Option<Child>,
}

impl Vm {
    fn overlay(&self) -> String {
        format!("10.44.0.{}", self.n)
    }
    fn public(&self) -> String {
        format!("198.18.{}.2", self.n)
    }
    fn lan(&self) -> String {
        format!("192.168.{}.2", self.n)
    }
}

struct Rig {
    dir: PathBuf,
    wan: String,
    vms: Vec<Vm>,
    children: Vec<Child>,
}

impl Rig {
    fn namespaces(&self) -> Vec<String> {
        let mut out = vec![self.wan.clone()];
        for vm in &self.vms {
            out.push(vm.nat.clone());
            out.push(vm.ns.clone());
        }
        out
    }
}

impl Drop for Rig {
    fn drop(&mut self) {
        for ns in self.namespaces() {
            stop_namespace_processes(&ns);
        }
        for vm in &mut self.vms {
            if let Some(mut child) = vm.child.take() {
                reap(&mut child, Duration::from_secs(5));
            }
        }
        for child in &mut self.children {
            reap(child, Duration::from_secs(5));
        }
        for ns in self.namespaces() {
            let _ = sudo(&["ip", "netns", "del", &ns]);
            let _ = sudo(&["rm", "-rf", &format!("/etc/netns/{ns}")]);
        }
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn ip(ns: &str, args: &[&str]) {
    let mut full = vec!["ip", "-n", ns];
    full.extend_from_slice(args);
    must(&full);
}

fn exec(ns: &str, args: &[&str]) -> Output {
    let mut full = vec!["ip", "netns", "exec", ns];
    full.extend_from_slice(args);
    sudo(&full)
}

fn exec_ok(ns: &str, args: &[&str]) -> String {
    let out = exec(ns, args);
    assert!(
        out.status.success(),
        "{ns}: {args:?}: {}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).into_owned()
}

fn spawn_in(ns: &str, args: &[&str], log: &Path) -> Child {
    Command::new("sudo")
        .args(["-n", "ip", "netns", "exec", ns])
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::from(File::create(log).unwrap()))
        .stderr(Stdio::from(
            File::create(log.with_extension("err")).unwrap(),
        ))
        .spawn()
        .expect("spawn")
}

fn plumb(wan: &str, vm: &Vm) {
    let n = vm.n;
    let public = vm.public();
    let lan = vm.lan();
    ip(&vm.nat, &["link", "set", "lo", "up"]);
    ip(&vm.ns, &["link", "set", "lo", "up"]);
    ip(
        wan,
        &[
            "link", "add", "w0", "type", "veth", "peer", "name", "w0", "netns", &vm.nat,
        ],
    );
    ip(wan, &["link", "set", "w0", "name", &format!("nat{n}")]);
    ip(
        wan,
        &[
            "addr",
            "add",
            &format!("198.18.{n}.1/24"),
            "dev",
            &format!("nat{n}"),
        ],
    );
    ip(wan, &["link", "set", &format!("nat{n}"), "up"]);
    ip(
        &vm.nat,
        &["addr", "add", &format!("{public}/24"), "dev", "w0"],
    );
    ip(&vm.nat, &["link", "set", "w0", "up"]);
    ip(
        &vm.nat,
        &["route", "add", "default", "via", &format!("198.18.{n}.1")],
    );
    ip(
        &vm.nat,
        &[
            "link", "add", "l0", "type", "veth", "peer", "name", "eth0", "netns", &vm.ns,
        ],
    );
    ip(
        &vm.nat,
        &["addr", "add", &format!("192.168.{n}.1/24"), "dev", "l0"],
    );
    ip(&vm.nat, &["link", "set", "l0", "up"]);
    ip(
        &vm.ns,
        &["addr", "add", &format!("{lan}/24"), "dev", "eth0"],
    );
    ip(&vm.ns, &["link", "set", "eth0", "up"]);
    ip(
        &vm.ns,
        &["route", "add", "default", "via", &format!("192.168.{n}.1")],
    );
    exec_ok(&vm.nat, &["sysctl", "-qw", "net.ipv4.ip_forward=1"]);
    exec_ok(
        &vm.nat,
        &[
            "iptables",
            "-t",
            "nat",
            "-A",
            "POSTROUTING",
            "-o",
            "w0",
            "-j",
            "MASQUERADE",
        ],
    );
    exec_ok(
        &vm.nat,
        &[
            "iptables",
            "-t",
            "nat",
            "-A",
            "PREROUTING",
            "-i",
            "w0",
            "-p",
            "udp",
            "--dport",
            MESH_PORT,
            "-j",
            "DNAT",
            "--to-destination",
            &format!("{lan}:{MESH_PORT}"),
        ],
    );
}

fn roster(vms: &[&Vm]) -> serde_json::Value {
    let peers: Vec<serde_json::Value> = vms
        .iter()
        .map(|vm| {
            serde_json::json!({
                "name": vm.name,
                "endpoint": vm.endpoint,
                "cidrs": [format!("{}/32", vm.overlay())],
                "addrs": [format!("{}:{MESH_PORT}", vm.public())],
                "names": [format!("svc-{}", vm.name)],
            })
        })
        .collect();
    serde_json::json!({ "peers": peers })
}

fn publish(dir: &Path, roster: &serde_json::Value) {
    let record = serde_json::json!({
        "uri": "at://did:plc:test/com.publicdomainrelay.temp.meshRoster/run",
        "cid": "bafy",
        "value": roster,
    });
    for (name, value) in [("roster.json", roster), ("record.json", &record)] {
        let tmp = dir.join(format!("{name}.tmp"));
        std::fs::write(&tmp, value.to_string()).unwrap();
        std::fs::rename(&tmp, dir.join(name)).unwrap();
    }
}

fn start(bin: &str, vm: &mut Vm) {
    let file = File::create(&vm.log).unwrap();
    let child = Command::new("sudo")
        .args(["-n", "ip", "netns", "exec", &vm.ns, bin, "run"])
        .args(["--roster", &vm.roster])
        .arg("--secret-file")
        .arg(&vm.secret)
        .args(["--relay", "disabled", "--discovery", "none"])
        .args(["--bind", &format!("{}:{MESH_PORT}", vm.lan())])
        .args(["--metrics-listen", "127.0.0.1:9469"])
        .args(["--dns-zone", ZONE])
        .args(["--dns-listen", &format!("{}:53", vm.overlay())])
        .args(["--dns-upstream", &format!("{UPSTREAM}:53")])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::from(file))
        .spawn()
        .expect("spawn meshd");
    vm.child = Some(child);
}

fn dig(ns: &str, server: &str, name: &str) -> String {
    exec_ok(
        ns,
        &[
            "dig",
            "+short",
            "+time=2",
            "+tries=2",
            &format!("@{server}"),
            name,
        ],
    )
    .trim()
    .to_string()
}

fn ping(ns: &str, target: &str) -> bool {
    exec(
        ns,
        &["ping", "-n", "-c", "2", "-i", "0.2", "-W", "2", target],
    )
    .status
    .success()
}

fn route_nodes(ns: &str) -> Vec<String> {
    status(ns)["routes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["node"].as_str().unwrap().to_string())
        .collect()
}

#[test]
fn three_standalone_meshd_behind_three_nats_share_one_roster_and_one_zone() {
    if !gate(GATE) {
        return;
    }
    let bin_path = common::meshd_bin();
    let bin = bin_path.as_str();
    let dir = scratch_dir("standalone");
    let pid = std::process::id();
    let url = format!("http://{ROSTER_HOST}:{ROSTER_PORT}/record.json");
    let file = dir.join("roster.json").to_string_lossy().into_owned();
    let vm = |name: &'static str, n: u8, roster: &str| Vm {
        name,
        n,
        nat: format!("swmesh-{pid}-nat-{name}"),
        ns: format!("swmesh-{pid}-vm-{name}"),
        secret: dir.join(format!("{name}.secret")),
        log: dir.join(format!("{name}.log")),
        endpoint: String::new(),
        roster: roster.to_string(),
        child: None,
    };
    let mut rig = Rig {
        wan: format!("swmesh-{pid}-wan"),
        vms: vec![vm("a", 1, &file), vm("b", 2, &file), vm("c", 3, &url)],
        children: Vec::new(),
        dir: dir.clone(),
    };
    for ns in rig.namespaces() {
        must(&["ip", "netns", "add", &ns]);
    }
    ip(&rig.wan, &["link", "set", "lo", "up"]);
    exec_ok(&rig.wan, &["sysctl", "-qw", "net.ipv4.ip_forward=1"]);
    ip(&rig.wan, &["link", "add", "svc0", "type", "dummy"]);
    for addr in [UPSTREAM, ROSTER_HOST] {
        ip(
            &rig.wan,
            &["addr", "add", &format!("{addr}/32"), "dev", "svc0"],
        );
    }
    ip(&rig.wan, &["link", "set", "svc0", "up"]);
    for vm in &rig.vms {
        plumb(&rig.wan, vm);
    }

    let dnsmasq = spawn_in(
        &rig.wan,
        &[
            "dnsmasq",
            "--keep-in-foreground",
            "--conf-file=/dev/null",
            "--no-resolv",
            "--no-hosts",
            "--bind-interfaces",
            &format!("--listen-address={UPSTREAM}"),
            "--address=/upstream.test/192.0.2.53",
            "--pid-file",
        ],
        &dir.join("dnsmasq.log"),
    );
    rig.children.push(dnsmasq);
    let http = spawn_in(
        &rig.wan,
        &[
            "python3",
            "-m",
            "http.server",
            "--bind",
            ROSTER_HOST,
            "--directory",
            dir.to_str().unwrap(),
            ROSTER_PORT,
        ],
        &dir.join("http.log"),
    );
    rig.children.push(http);

    for vm in &mut rig.vms {
        vm.endpoint = identity(bin, &vm.secret).0;
    }
    publish(&dir, &roster(&rig.vms.iter().collect::<Vec<_>>()));
    for vm in &mut rig.vms {
        start(bin, vm);
    }
    for vm in &mut rig.vms {
        let child = vm.child.as_mut().unwrap();
        wait_for_log(child, &vm.log, "\"event\":\"ready\"", READY_WAIT);
        wait_for_log(child, &vm.log, "\"event\":\"dns_listening\"", READY_WAIT);
    }

    let a_routes = must(&[
        "ip",
        "-n",
        &rig.vms[0].ns,
        "route",
        "show",
        "dev",
        "swmesh0",
    ]);
    assert!(
        a_routes.contains("10.44.0.2 ") && a_routes.contains("src 10.44.0.1"),
        "vm a routes the other overlay addresses through swmesh0 from its own: {a_routes}"
    );

    for from in &rig.vms {
        for to in &rig.vms {
            if from.n == to.n {
                continue;
            }
            assert!(
                ping(&from.ns, &to.overlay()),
                "ping {} -> {}",
                from.name,
                to.name
            );
        }
    }

    for vm in &rig.vms {
        let reply = format!("from-{}", vm.name);
        let server = spawn_in(
            &vm.ns,
            &[
                "socat",
                &format!("TCP-LISTEN:7000,bind={},fork,reuseaddr", vm.overlay()),
                &format!("SYSTEM:echo {reply}"),
            ],
            &dir.join(format!("socat-{}.log", vm.name)),
        );
        rig.children.push(server);
    }
    for from in &rig.vms {
        for to in &rig.vms {
            if from.n == to.n {
                continue;
            }
            let target = format!("TCP:{}:7000", to.overlay());
            let want = format!("from-{}", to.name);
            eventually(
                Duration::from_secs(10),
                &format!("tcp {} -> {}", from.name, to.name),
                || {
                    let out = exec(&from.ns, &["socat", "-T", "3", "-", &target]);
                    String::from_utf8_lossy(&out.stdout).trim() == want
                },
            );
        }
    }

    for from in &rig.vms {
        for server in &rig.vms {
            for to in &rig.vms {
                for name in [to.name.to_string(), format!("svc-{}", to.name)] {
                    assert_eq!(
                        dig(&from.ns, &server.overlay(), &format!("{name}.{ZONE}")),
                        to.overlay(),
                        "vm {} asks vm {}'s DNS for {name}",
                        from.name,
                        server.name
                    );
                }
            }
        }
        assert_eq!(
            dig(&from.ns, &from.overlay(), "upstream.test"),
            "192.0.2.53",
            "names outside the zone are forwarded upstream"
        );
    }

    let a = &rig.vms[0];
    let resolv = format!("/etc/netns/{}", a.ns);
    must(&["mkdir", "-p", &resolv]);
    must(&[
        "sh",
        "-c",
        &format!("echo 'nameserver {}' > {resolv}/resolv.conf", a.overlay()),
    ]);
    let hosts = exec_ok(&a.ns, &["host", "-W", "2", &format!("svc-c.{ZONE}")]);
    assert!(
        hosts.contains("has address 10.44.0.3"),
        "resolv.conf on the own overlay address: {hosts}"
    );

    publish(&dir, &roster(&[&rig.vms[0], &rig.vms[1]]));
    let (a_ns, b_ns, c_ns) = (
        rig.vms[0].ns.clone(),
        rig.vms[1].ns.clone(),
        rig.vms[2].ns.clone(),
    );
    eventually(
        Duration::from_secs(15),
        "c leaves a's and b's routes",
        || route_nodes(&a_ns) == vec!["b"] && route_nodes(&b_ns) == vec!["a"],
    );
    eventually(
        Duration::from_secs(15),
        "c drops every route and is not ready",
        || {
            let c = status(&c_ns);
            c["routes"].as_array().unwrap().is_empty() && c["ready"] == false
        },
    );
    assert!(!ping(&a_ns, "10.44.0.3"), "a no longer reaches c");
    assert!(!ping(&c_ns, "10.44.0.1"), "c no longer reaches a");
    assert_eq!(dig(&a_ns, "10.44.0.1", &format!("c.{ZONE}")), "");
    assert!(ping(&a_ns, "10.44.0.2"), "a still reaches b");

    publish(&dir, &roster(&rig.vms.iter().collect::<Vec<_>>()));
    eventually(Duration::from_secs(15), "c rejoins", || {
        route_nodes(&a_ns) == vec!["b", "c"] && route_nodes(&c_ns) == vec!["a", "b"]
    });
    assert!(ping(&a_ns, "10.44.0.3"), "a reaches c again");
    assert!(ping(&c_ns, "10.44.0.2"), "c reaches b again");
    assert_eq!(dig(&b_ns, "10.44.0.3", &format!("a.{ZONE}")), "10.44.0.1");
}
