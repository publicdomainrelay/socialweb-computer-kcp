mod common;

use std::fs::File;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Output, Stdio};
use std::time::{Duration, Instant};

use common::{
    READY_WAIT, gate, identity, must, reap, scratch_dir, status, stop_namespace_processes, sudo,
    wait_exit, wait_for_log,
};

const GATE: &str = "SOCIALWEB_MESHD_NETNS";
const UNDERLAY_MTU: &str = "1228";

struct Side {
    node_ns: String,
    pod_ns: String,
    node_name: &'static str,
    underlay: &'static str,
    pod_cidr: &'static str,
    pod_gateway: &'static str,
    pod_ip: &'static str,
    secret: PathBuf,
    log: PathBuf,
    endpoint: String,
    child: Option<Child>,
}

struct Rig {
    dir: PathBuf,
    peers: PathBuf,
    a: Side,
    b: Side,
}

impl Rig {
    fn namespaces(&self) -> Vec<String> {
        vec![
            self.a.node_ns.clone(),
            self.b.node_ns.clone(),
            self.a.pod_ns.clone(),
            self.b.pod_ns.clone(),
        ]
    }
}

impl Drop for Rig {
    fn drop(&mut self) {
        for ns in self.namespaces() {
            stop_namespace_processes(&ns);
        }
        for side in [&mut self.a, &mut self.b] {
            if let Some(mut child) = side.child.take() {
                reap(&mut child, Duration::from_secs(5));
            }
        }
        for ns in self.namespaces() {
            let _ = sudo(&["ip", "netns", "del", &ns]);
        }
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn plumb(side: &Side, peer_underlay_ns: Option<&str>) {
    for ns in [&side.node_ns, &side.pod_ns] {
        must(&["ip", "-n", ns, "link", "set", "lo", "up"]);
    }
    if let Some(peer_ns) = peer_underlay_ns {
        must(&[
            "ip",
            "-n",
            &side.node_ns,
            "link",
            "add",
            "u0",
            "type",
            "veth",
            "peer",
            "name",
            "u0",
            "netns",
            peer_ns,
        ]);
    }
    must(&[
        "ip",
        "-n",
        &side.node_ns,
        "addr",
        "add",
        &format!("{}/24", side.underlay),
        "dev",
        "u0",
    ]);
    must(&[
        "ip",
        "-n",
        &side.node_ns,
        "link",
        "set",
        "u0",
        "mtu",
        UNDERLAY_MTU,
        "up",
    ]);
    must(&[
        "ip",
        "-n",
        &side.pod_ns,
        "link",
        "add",
        "eth0",
        "type",
        "veth",
        "peer",
        "name",
        "pod0",
        "netns",
        &side.node_ns,
    ]);
    must(&[
        "ip",
        "-n",
        &side.pod_ns,
        "addr",
        "add",
        &format!("{}/24", side.pod_ip),
        "dev",
        "eth0",
    ]);
    must(&["ip", "-n", &side.pod_ns, "link", "set", "eth0", "up"]);
    must(&[
        "ip",
        "-n",
        &side.pod_ns,
        "route",
        "add",
        "default",
        "via",
        side.pod_gateway,
    ]);
    must(&[
        "ip",
        "-n",
        &side.node_ns,
        "addr",
        "add",
        &format!("{}/24", side.pod_gateway),
        "dev",
        "pod0",
    ]);
    must(&["ip", "-n", &side.node_ns, "link", "set", "pod0", "up"]);
    must(&[
        "ip",
        "netns",
        "exec",
        &side.node_ns,
        "sysctl",
        "-qw",
        "net.ipv4.ip_forward=1",
    ]);
}

fn peer_json(side: &Side) -> serde_json::Value {
    serde_json::json!({
        "name": side.node_name,
        "endpoint": side.endpoint,
        "cidrs": [side.pod_cidr],
        "addrs": [format!("{}:47001", side.underlay)],
    })
}

fn start(bin: &str, side: &mut Side, peers: &Path) {
    let log = File::create(&side.log).expect("log file");
    let child = Command::new("sudo")
        .args(["-n", "ip", "netns", "exec", &side.node_ns, bin, "run"])
        .arg("--roster")
        .arg(peers)
        .arg("--secret-file")
        .arg(&side.secret)
        .args(["--relay", "disabled", "--discovery", "none"])
        .args(["--bind", &format!("{}:47001", side.underlay)])
        .args(["--metrics-listen", "127.0.0.1:9469"])
        .args(["--log-filter", "info,socialweb_meshd=debug"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::from(log))
        .spawn()
        .expect("spawn meshd");
    side.child = Some(child);
}

fn wait_ready(side: &mut Side) {
    let child = side.child.as_mut().expect("running");
    wait_for_log(child, &side.log, "\"event\":\"ready\"", READY_WAIT);
}

fn ping(ns: &str, target: &str, extra: &[&str]) -> Output {
    let mut args = vec![
        "ip", "netns", "exec", ns, "ping", "-n", "-c", "3", "-i", "0.2", "-W", "3",
    ];
    args.extend_from_slice(extra);
    args.push(target);
    sudo(&args)
}

fn assert_ping(ns: &str, target: &str, extra: &[&str]) {
    let out = ping(ns, target, extra);
    assert!(
        out.status.success(),
        "ping {target} from {ns} {extra:?} failed:\n{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
}

fn stop(side: &mut Side) -> i32 {
    stop_namespace_processes(&side.node_ns);
    let mut child = side.child.take().expect("running");
    wait_exit(&mut child, Duration::from_secs(10))
}

fn side(dir: &Path, tag: &str, node_name: &'static str, underlay: &'static str, n: u8) -> Side {
    let pid = std::process::id();
    let (pod_cidr, pod_gateway, pod_ip): (&'static str, &'static str, &'static str) = match n {
        1 => ("10.42.1.0/24", "10.42.1.1", "10.42.1.2"),
        _ => ("10.42.2.0/24", "10.42.2.1", "10.42.2.2"),
    };
    Side {
        node_ns: format!("swmesh-{pid}-node-{tag}"),
        pod_ns: format!("swmesh-{pid}-pod-{tag}"),
        node_name,
        underlay,
        pod_cidr,
        pod_gateway,
        pod_ip,
        secret: dir.join(format!("{tag}.secret")),
        log: dir.join(format!("{tag}.log")),
        endpoint: String::new(),
        child: None,
    }
}

#[test]
fn two_meshd_in_two_netns_carry_ping_between_two_pod_cidrs() {
    if !gate(GATE) {
        return;
    }
    let bin_path = common::meshd_bin();
    let bin = bin_path.as_str();
    let dir = scratch_dir("netns");
    let mut rig = Rig {
        peers: dir.join("roster.json"),
        a: side(&dir, "a", "node-a", "10.199.0.1", 1),
        b: side(&dir, "b", "node-b", "10.199.0.2", 2),
        dir,
    };
    for ns in rig.namespaces() {
        must(&["ip", "netns", "add", &ns]);
    }
    let b_ns = rig.b.node_ns.clone();
    plumb(&rig.a, Some(&b_ns));
    plumb(&rig.b, None);

    rig.a.endpoint = identity(bin, &rig.a.secret).0;
    rig.b.endpoint = identity(bin, &rig.b.secret).0;
    let peers = serde_json::json!({"peers": [peer_json(&rig.a), peer_json(&rig.b)]});
    std::fs::write(&rig.peers, peers.to_string()).unwrap();

    let peers_path = rig.peers.clone();
    start(bin, &mut rig.a, &peers_path);
    start(bin, &mut rig.b, &peers_path);
    wait_ready(&mut rig.a);
    wait_ready(&mut rig.b);

    let routes = must(&[
        "ip",
        "-n",
        &rig.a.node_ns,
        "route",
        "show",
        "dev",
        "swmesh0",
    ]);
    assert!(
        routes.contains("10.42.2.0/24"),
        "node a routes via swmesh0: {routes}"
    );
    assert!(
        routes.contains("src 10.42.1.0"),
        "node a route source: {routes}"
    );

    assert_ping(&rig.a.pod_ns, rig.b.pod_ip, &[]);
    assert_ping(&rig.b.pod_ns, rig.a.pod_ip, &[]);
    assert_ping(&rig.a.node_ns, rig.b.pod_ip, &[]);

    let a_status = status(&rig.a.node_ns);
    eprintln!("node a status: {a_status}");
    let mtu = a_status["mtu"].as_u64().unwrap();
    let largest_icmp_payload = (mtu - 28).to_string();
    assert_ping(
        &rig.a.pod_ns,
        rig.b.pod_ip,
        &["-M", "do", "-s", &largest_icmp_payload],
    );
    let max_datagram = a_status["routes"][0]["maxDatagramSize"].as_u64();
    eprintln!("tun mtu {mtu}; max datagram size reported on the live connection: {max_datagram:?}");
    assert!(
        max_datagram.is_some_and(|size| size >= mtu),
        "the tun MTU {mtu} must fit the connection's datagram size {max_datagram:?}"
    );
    assert_ping(&rig.a.pod_ns, rig.b.pod_ip, &["-M", "dont", "-s", "1400"]);
    let oversized = ping(
        &rig.a.pod_ns,
        rig.b.pod_ip,
        &["-c", "1", "-M", "do", "-s", "1400"],
    );
    assert!(
        !oversized.status.success(),
        "a don't-fragment packet above the tun MTU must be refused by the node, not sent"
    );

    let unknown = ping(&rig.a.pod_ns, "10.42.9.9", &["-c", "1"]);
    assert!(
        !unknown.status.success(),
        "a destination outside every pod CIDR must not answer"
    );

    let a_status = status(&rig.a.node_ns);
    assert_eq!(a_status["ready"], true);
    assert_eq!(a_status["routes"][0]["node"], "node-b");
    assert_eq!(a_status["routes"][0]["connected"], true);
    assert!(a_status["counters"]["datagrams_sent"].as_u64().unwrap() >= 9);
    assert!(a_status["counters"]["datagrams_received"].as_u64().unwrap() >= 9);
    assert_eq!(a_status["counters"]["dropped_too_large"], 0);

    assert_eq!(stop(&mut rig.b), 0, "SIGTERM is a clean exit");
    let gone = sudo(&["ip", "-n", &rig.b.node_ns, "link", "show", "swmesh0"]);
    assert!(
        !gone.status.success(),
        "the tun device leaves with the process"
    );
    start(bin, &mut rig.b, &peers_path);
    wait_ready(&mut rig.b);
    assert_ping(&rig.a.pod_ns, rig.b.pod_ip, &["-c", "5"]);
    assert_ping(&rig.b.pod_ns, rig.a.pod_ip, &[]);

    let pids = sudo(&["ip", "netns", "pids", &rig.b.node_ns]);
    for pid in String::from_utf8_lossy(&pids.stdout).split_whitespace() {
        let _ = sudo(&["kill", "-KILL", pid]);
    }
    let mut crashed = rig.b.child.take().expect("running");
    assert_ne!(wait_exit(&mut crashed, Duration::from_secs(10)), 0);
    start(bin, &mut rig.b, &peers_path);
    wait_ready(&mut rig.b);
    let since_restart = Instant::now();
    let one_way = sudo(&[
        "ip",
        "netns",
        "exec",
        &rig.a.pod_ns,
        "ping",
        "-n",
        "-c",
        "1",
        "-w",
        "60",
        rig.b.pod_ip,
    ]);
    eprintln!(
        "after peer b was SIGKILLed and restarted, pod a reached pod b again in {:?}",
        since_restart.elapsed()
    );
    assert!(
        one_way.status.success(),
        "pod a must reach pod b again after b crashed and restarted:\n{}",
        String::from_utf8_lossy(&one_way.stdout)
    );
}
