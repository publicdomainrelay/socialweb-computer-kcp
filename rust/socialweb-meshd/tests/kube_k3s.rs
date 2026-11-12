mod common;

use std::fs::File;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use common::{
    READY_WAIT, eventually, gate, identity, must, reap, scratch_dir, status,
    stop_namespace_processes, sudo, wait_exit, wait_for_log,
};

const GATE: &str = "SOCIALWEB_MESHD_K3S";
const K3S_READY_WAIT: Duration = Duration::from_secs(180);
const APISERVER_PORT: &str = "16443";

struct Rig {
    ns: String,
    dir: PathBuf,
    children: Vec<Child>,
    extra: Vec<String>,
}

impl Drop for Rig {
    fn drop(&mut self) {
        for ns in self.extra.iter().chain([&self.ns]) {
            stop_namespace_processes(ns);
        }
        for child in &mut self.children {
            reap(child, Duration::from_secs(20));
        }
        for ns in self.extra.iter().chain([&self.ns]) {
            let _ = sudo(&["ip", "netns", "del", ns]);
        }
        let _ = sudo(&["rm", "-rf", self.dir.to_str().unwrap()]);
    }
}

fn kubectl(rig: &Rig, args: &[&str]) -> std::process::Output {
    let admin = rig.dir.join("admin.yaml");
    let mut full = vec![
        "ip",
        "netns",
        "exec",
        &rig.ns,
        "k3s",
        "kubectl",
        "--kubeconfig",
        admin.to_str().unwrap(),
    ];
    full.extend_from_slice(args);
    sudo(&full)
}

fn kubectl_ok(rig: &Rig, args: &[&str]) -> String {
    let out = kubectl(rig, args);
    assert!(
        out.status.success(),
        "kubectl {args:?}: {}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).into_owned()
}

fn node_manifest(name: &str, cidr: &str, mesh: Option<(&str, &str)>) -> serde_json::Value {
    let mut annotations = serde_json::Map::new();
    if let Some((endpoint, did)) = mesh {
        annotations.insert(
            "socialweb.computer/mesh-endpoint".to_string(),
            endpoint.into(),
        );
        annotations.insert("socialweb.computer/mesh-did".to_string(), did.into());
    }
    serde_json::json!({
        "apiVersion": "v1",
        "kind": "Node",
        "metadata": {"name": name, "annotations": annotations},
        "spec": {"podCIDR": cidr, "podCIDRs": [cidr]}
    })
}

fn apply(rig: &Rig, file: &str, manifest: &serde_json::Value) {
    let path = rig.dir.join(file);
    std::fs::write(&path, manifest.to_string()).unwrap();
    kubectl_ok(rig, &["apply", "-f", path.to_str().unwrap()]);
}

fn mint_node_credentials(rig: &Rig, node: &str) -> PathBuf {
    let tls = rig.dir.join("data/server/tls");
    let key_pkcs8 = rig.dir.join(format!("{node}.pkcs8.key"));
    let key = rig.dir.join(format!("{node}.key"));
    let csr = rig.dir.join(format!("{node}.csr"));
    let crt = rig.dir.join(format!("{node}.crt"));
    must(&[
        "openssl",
        "req",
        "-new",
        "-newkey",
        "ec",
        "-pkeyopt",
        "ec_paramgen_curve:prime256v1",
        "-nodes",
        "-keyout",
        key_pkcs8.to_str().unwrap(),
        "-out",
        csr.to_str().unwrap(),
        "-subj",
        &format!("/O=system:nodes/CN=system:node:{node}"),
    ]);
    must(&[
        "openssl",
        "ec",
        "-in",
        key_pkcs8.to_str().unwrap(),
        "-out",
        key.to_str().unwrap(),
    ]);
    must(&[
        "openssl",
        "x509",
        "-req",
        "-in",
        csr.to_str().unwrap(),
        "-CA",
        tls.join("client-ca.crt").to_str().unwrap(),
        "-CAkey",
        tls.join("client-ca.key").to_str().unwrap(),
        "-CAcreateserial",
        "-days",
        "1",
        "-out",
        crt.to_str().unwrap(),
    ]);
    let kubeconfig = rig.dir.join(format!("{node}.kubeconfig"));
    let text = format!(
        "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://127.0.0.1:{APISERVER_PORT}\n    certificate-authority: data/server/tls/server-ca.crt\n  name: local\ncontexts:\n- context:\n    cluster: local\n    namespace: default\n    user: user\n  name: Default\ncurrent-context: Default\npreferences: {{}}\nusers:\n- name: user\n  user:\n    client-certificate: {node}.crt\n    client-key: {node}.key\n"
    );
    std::fs::write(&kubeconfig, text).unwrap();
    kubeconfig
}

fn start_meshd(
    rig: &mut Rig,
    bin: &str,
    node: &str,
    kubeconfig: &Path,
    secret: &Path,
    log: &Path,
    extra: &[&str],
) -> usize {
    let file = File::create(log).unwrap();
    let child = Command::new("sudo")
        .args(["-n", "ip", "netns", "exec", &rig.ns, bin, "run"])
        .args(["--node-name", node])
        .arg("--kubeconfig")
        .arg(kubeconfig)
        .arg("--secret-file")
        .arg(secret)
        .args(["--relay", "disabled", "--discovery", "none"])
        .args(["--metrics-listen", "127.0.0.1:9469"])
        .args(extra)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::from(file))
        .spawn()
        .expect("spawn meshd");
    rig.children.push(child);
    rig.children.len() - 1
}

fn route_nodes(ns: &str) -> Vec<String> {
    status(ns)["routes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["node"].as_str().unwrap().to_string())
        .collect()
}

fn installed(ns: &str) -> String {
    must(&["ip", "-n", ns, "route", "show", "dev", "swmesh0"])
}

fn start_k3s(rig: &mut Rig) {
    must(&["ip", "-n", &rig.ns, "link", "set", "lo", "up"]);
    must(&[
        "ip", "-n", &rig.ns, "link", "add", "uplink0", "type", "dummy",
    ]);
    must(&[
        "ip",
        "-n",
        &rig.ns,
        "addr",
        "add",
        "10.250.0.2/24",
        "dev",
        "uplink0",
    ]);
    must(&["ip", "-n", &rig.ns, "link", "set", "uplink0", "up"]);
    must(&[
        "ip",
        "-n",
        &rig.ns,
        "route",
        "add",
        "default",
        "via",
        "10.250.0.1",
    ]);

    let data = rig.dir.join("data");
    let admin = rig.dir.join("admin.yaml");
    let k3s_log = rig.dir.join("k3s.log");
    let k3s = Command::new("sudo")
        .args(["-n", "ip", "netns", "exec", &rig.ns, "k3s", "server"])
        .args(["--disable-agent", "--disable-cloud-controller"])
        .args([
            "--disable",
            "traefik,servicelb,metrics-server,local-storage,coredns",
        ])
        .arg("--data-dir")
        .arg(&data)
        .args([
            "--bind-address",
            "127.0.0.1",
            "--advertise-address",
            "127.0.0.1",
        ])
        .args(["--https-listen-port", APISERVER_PORT])
        .arg("--write-kubeconfig")
        .arg(&admin)
        .args(["--write-kubeconfig-mode", "0644"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::from(File::create(&k3s_log).unwrap()))
        .spawn()
        .expect("spawn k3s");
    rig.children.push(k3s);
    eventually(K3S_READY_WAIT, "k3s apiserver /readyz", || {
        kubectl(rig, &["get", "--raw", "/readyz"]).status.success()
    });
}

#[test]
fn with_a_kubelet_identity_meshd_annotates_its_own_node_and_follows_the_node_list() {
    if !gate(GATE) {
        return;
    }
    let bin_path = common::meshd_bin();
    let bin = bin_path.as_str();
    let dir = scratch_dir("k3s");
    let mut rig = Rig {
        ns: format!("swmesh-{}-k3s", std::process::id()),
        dir,
        children: Vec::new(),
        extra: Vec::new(),
    };
    must(&["ip", "netns", "add", &rig.ns]);
    start_k3s(&mut rig);

    let secret_a = rig.dir.join("a.secret");
    let (endpoint_a, did_a) = identity(bin, &secret_a);
    let (endpoint_b, did_b) = identity(bin, &rig.dir.join("b.secret"));
    let (endpoint_c, did_c) = identity(bin, &rig.dir.join("c.secret"));
    apply(
        &rig,
        "node-a.json",
        &node_manifest("node-a", "10.42.1.0/24", None),
    );
    apply(
        &rig,
        "node-b.json",
        &node_manifest("node-b", "10.42.2.0/24", Some((&endpoint_b, &did_b))),
    );
    let kubeconfig_a = mint_node_credentials(&rig, "node-a");

    let log_a = rig.dir.join("meshd-a.log");
    let a = start_meshd(
        &mut rig,
        bin,
        "node-a",
        &kubeconfig_a,
        &secret_a,
        &log_a,
        &[],
    );
    let before_rbac = wait_for_log(
        &mut rig.children[a],
        &log_a,
        "\"event\":\"list_nodes_forbidden\"",
        READY_WAIT,
    );
    assert!(
        before_rbac.contains("cannot read all nodes, only its own Node object"),
        "{before_rbac}"
    );
    assert_eq!(status(&rig.ns)["ready"], false);
    let rbac = Path::new(env!("CARGO_MANIFEST_DIR")).join("deploy/node-reader-rbac.yaml");
    kubectl_ok(&rig, &["apply", "-f", rbac.to_str().unwrap()]);
    wait_for_log(
        &mut rig.children[a],
        &log_a,
        "\"event\":\"ready\"",
        READY_WAIT,
    );

    let annotations = kubectl_ok(
        &rig,
        &[
            "get",
            "node",
            "node-a",
            "-o",
            "jsonpath={.metadata.annotations}",
        ],
    );
    let annotations: serde_json::Value = serde_json::from_str(&annotations).unwrap();
    assert_eq!(annotations["socialweb.computer/mesh-endpoint"], endpoint_a);
    assert_eq!(annotations["socialweb.computer/mesh-did"], did_a);
    assert_eq!(route_nodes(&rig.ns), vec!["node-b"]);
    assert!(installed(&rig.ns).contains("10.42.2.0/24"));

    apply(
        &rig,
        "node-c.json",
        &node_manifest("node-c", "10.42.3.0/24", Some((&endpoint_c, &did_c))),
    );
    eventually(Duration::from_secs(15), "watch adds node-c", || {
        route_nodes(&rig.ns) == vec!["node-b", "node-c"]
            && installed(&rig.ns).contains("10.42.3.0/24")
    });
    kubectl_ok(&rig, &["delete", "node", "node-b", "--wait=true"]);
    eventually(Duration::from_secs(15), "watch drops node-b", || {
        route_nodes(&rig.ns) == vec!["node-c"] && !installed(&rig.ns).contains("10.42.2.0/24")
    });

    stop_namespace_processes_named(&rig, "socialweb-meshd");
    assert_eq!(wait_exit(&mut rig.children[a], Duration::from_secs(10)), 0);

    let log_wrong = rig.dir.join("meshd-wrong.log");
    let secret_wrong = rig.dir.join("wrong.secret");
    let wrong = start_meshd(
        &mut rig,
        bin,
        "node-c",
        &kubeconfig_a,
        &secret_wrong,
        &log_wrong,
        &[],
    );
    let code = wait_exit(&mut rig.children[wrong], Duration::from_secs(30));
    let text = std::fs::read_to_string(&log_wrong).unwrap_or_default();
    assert_eq!(
        code, 4,
        "patching another node's Node is forbidden:\n{text}"
    );
    assert!(text.contains("node_patch_forbidden"), "{text}");
}

fn stop_namespace_processes_named(rig: &Rig, name: &str) {
    let pids = sudo(&["ip", "netns", "pids", &rig.ns]);
    for pid in String::from_utf8_lossy(&pids.stdout).split_whitespace() {
        let comm = std::fs::read_to_string(format!("/proc/{pid}/comm")).unwrap_or_default();
        if comm.trim() == name {
            let _ = sudo(&["kill", "-TERM", pid]);
        }
    }
}

fn ip(ns: &str, args: &[&str]) {
    let mut full = vec!["ip", "-n", ns];
    full.extend_from_slice(args);
    must(&full);
}

fn netns_exec(ns: &str, args: &[&str]) -> std::process::Output {
    let mut full = vec!["ip", "netns", "exec", ns];
    full.extend_from_slice(args);
    sudo(&full)
}

fn ping_ok(ns: &str, target: &str) {
    let out = netns_exec(
        ns,
        &["ping", "-n", "-c", "3", "-i", "0.2", "-W", "3", target],
    );
    assert!(
        out.status.success(),
        "ping {target} from {ns}: {}",
        String::from_utf8_lossy(&out.stdout)
    );
}

fn dig(ns: &str, server: &str, name: &str) -> String {
    let out = netns_exec(
        ns,
        &[
            "dig",
            "+short",
            "+time=2",
            "+tries=2",
            &format!("@{server}"),
            name,
        ],
    );
    String::from_utf8_lossy(&out.stdout).trim().to_string()
}

#[test]
fn a_kube_mode_node_and_a_standalone_vm_share_one_overlay_and_one_zone() {
    if !gate(GATE) {
        return;
    }
    let bin_path = common::meshd_bin();
    let bin = bin_path.as_str();
    let dir = scratch_dir("k3s-roster");
    let pid = std::process::id();
    let vm_ns = format!("swmesh-{pid}-k3sr-vm");
    let pod_ns = format!("swmesh-{pid}-k3sr-pod");
    let mut rig = Rig {
        ns: format!("swmesh-{pid}-k3sr"),
        dir,
        children: Vec::new(),
        extra: vec![vm_ns.clone(), pod_ns.clone()],
    };
    for ns in [&rig.ns, &vm_ns, &pod_ns] {
        must(&["ip", "netns", "add", ns]);
    }
    start_k3s(&mut rig);
    let node_ns = rig.ns.clone();
    for ns in [&vm_ns, &pod_ns] {
        ip(ns, &["link", "set", "lo", "up"]);
    }
    ip(
        &node_ns,
        &[
            "link", "add", "u0", "type", "veth", "peer", "name", "u0", "netns", &vm_ns,
        ],
    );
    ip(&node_ns, &["addr", "add", "10.251.0.1/24", "dev", "u0"]);
    ip(&node_ns, &["link", "set", "u0", "up"]);
    ip(&vm_ns, &["addr", "add", "10.251.0.2/24", "dev", "u0"]);
    ip(&vm_ns, &["link", "set", "u0", "up"]);
    ip(
        &pod_ns,
        &[
            "link", "add", "eth0", "type", "veth", "peer", "name", "pod0", "netns", &node_ns,
        ],
    );
    ip(&pod_ns, &["addr", "add", "10.42.1.2/24", "dev", "eth0"]);
    ip(&pod_ns, &["link", "set", "eth0", "up"]);
    ip(&pod_ns, &["route", "add", "default", "via", "10.42.1.1"]);
    ip(&node_ns, &["addr", "add", "10.42.1.1/24", "dev", "pod0"]);
    ip(&node_ns, &["link", "set", "pod0", "up"]);
    must(&[
        "ip",
        "netns",
        "exec",
        &node_ns,
        "sysctl",
        "-qw",
        "net.ipv4.ip_forward=1",
    ]);

    let secret_a = rig.dir.join("a.secret");
    let secret_s = rig.dir.join("s.secret");
    let (endpoint_a, _) = identity(bin, &secret_a);
    let (endpoint_s, _) = identity(bin, &secret_s);
    let roster = rig.dir.join("roster.json");
    std::fs::write(
        &roster,
        serde_json::json!({"peers": [
            {"name": "node-a", "endpoint": endpoint_a, "cidrs": ["10.42.1.0/24"],
             "addrs": ["10.251.0.1:47001"], "names": ["k3s-a"]},
            {"name": "vm-s", "endpoint": endpoint_s, "cidrs": ["10.44.0.9/32"],
             "addrs": ["10.251.0.2:47001"], "names": ["svc"]},
        ]})
        .to_string(),
    )
    .unwrap();

    let rbac = Path::new(env!("CARGO_MANIFEST_DIR")).join("deploy/node-reader-rbac.yaml");
    kubectl_ok(&rig, &["apply", "-f", rbac.to_str().unwrap()]);
    let mut node = node_manifest("node-a", "10.42.1.0/24", None);
    node["metadata"]["annotations"]["socialweb.computer/mesh-roster"] =
        roster.to_string_lossy().into_owned().into();
    apply(&rig, "node-a.json", &node);
    let kubeconfig_a = mint_node_credentials(&rig, "node-a");

    let log_a = rig.dir.join("meshd-a.log");
    let a = start_meshd(
        &mut rig,
        bin,
        "node-a",
        &kubeconfig_a,
        &secret_a,
        &log_a,
        &[
            "--bind",
            "10.251.0.1:47001",
            "--dns-zone",
            "staging.internal",
            "--dns-listen",
            "10.42.1.0:53",
        ],
    );
    let log_s = rig.dir.join("meshd-s.log");
    rig.children.push(
        Command::new("sudo")
            .args(["-n", "ip", "netns", "exec", &vm_ns, bin, "run"])
            .arg("--roster")
            .arg(&roster)
            .arg("--secret-file")
            .arg(&secret_s)
            .args(["--relay", "disabled", "--discovery", "none"])
            .args(["--bind", "10.251.0.2:47001"])
            .args(["--metrics-listen", "127.0.0.1:9469"])
            .args(["--dns-zone", "staging.internal"])
            .args(["--dns-listen", "10.44.0.9:53"])
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::from(File::create(&log_s).unwrap()))
            .spawn()
            .expect("spawn standalone meshd"),
    );
    let s = rig.children.len() - 1;
    wait_for_log(
        &mut rig.children[a],
        &log_a,
        "\"event\":\"ready\"",
        READY_WAIT,
    );
    wait_for_log(
        &mut rig.children[s],
        &log_s,
        "\"event\":\"ready\"",
        READY_WAIT,
    );
    eventually(Duration::from_secs(15), "the roster reaches node-a", || {
        route_nodes(&node_ns) == vec!["vm-s"]
    });
    assert_eq!(route_nodes(&vm_ns), vec!["node-a"]);
    assert_eq!(status(&vm_ns)["node"], "vm-s");
    assert!(installed(&node_ns).contains("10.44.0.9"));

    ping_ok(&pod_ns, "10.44.0.9");
    ping_ok(&vm_ns, "10.42.1.2");
    ping_ok(&node_ns, "10.44.0.9");
    eventually(Duration::from_secs(15), "the node's DNS answers", || {
        dig(&vm_ns, "10.42.1.0", "svc.staging.internal") == "10.44.0.9"
    });
    assert_eq!(
        dig(&pod_ns, "10.42.1.0", "k3s-a.staging.internal"),
        "10.42.1.0"
    );
    assert_eq!(
        dig(&pod_ns, "10.44.0.9", "vm-s.staging.internal"),
        "10.44.0.9"
    );
    assert_eq!(
        dig(&node_ns, "10.44.0.9", "node-a.staging.internal"),
        "10.42.1.0"
    );
}
