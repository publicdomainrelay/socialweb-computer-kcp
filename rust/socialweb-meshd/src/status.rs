use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::watch;

use crate::identity::endpoint_id_hex;
use crate::mesh::Connections;
use crate::routes::RouteTable;
use crate::stats::Stats;

pub const REQUEST_LIMIT: usize = 8192;
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(5);

pub struct StatusInfo {
    pub endpoint_hex: String,
    pub did: String,
    pub tun: String,
    pub mtu: u32,
    pub source: String,
    pub ready: AtomicBool,
    pub routes: watch::Receiver<Arc<RouteTable>>,
    pub connections: Arc<Connections>,
    pub stats: Arc<Stats>,
    pub installed_routes: std::sync::Mutex<Vec<String>>,
}

impl StatusInfo {
    pub fn is_ready(&self) -> bool {
        self.ready.load(Ordering::Relaxed)
    }

    pub fn status_json(&self) -> serde_json::Value {
        let table = Arc::clone(&self.routes.borrow());
        let connected = self.connections.connected();
        let routes: Vec<serde_json::Value> = table
            .routes
            .iter()
            .map(|route| {
                let live = connected.iter().find(|(peer, _)| *peer == route.endpoint);
                serde_json::json!({
                    "node": route.node,
                    "podCIDR": route.cidr.to_string(),
                    "endpoint": endpoint_id_hex(&route.endpoint),
                    "connected": live.is_some(),
                    "maxDatagramSize": live.and_then(|(_, size)| *size),
                })
            })
            .collect();
        let rejected: Vec<serde_json::Value> = table
            .rejected
            .iter()
            .map(|r| serde_json::json!({"node": r.node, "reason": r.reason}))
            .collect();
        let counters: serde_json::Map<String, serde_json::Value> = self
            .stats
            .counters()
            .into_iter()
            .map(|(name, value)| (name, serde_json::Value::from(value)))
            .collect();
        let installed = self
            .installed_routes
            .lock()
            .map(|routes| routes.clone())
            .unwrap_or_default();
        serde_json::json!({
            "ready": self.is_ready(),
            "node": table.self_node,
            "endpoint": self.endpoint_hex,
            "did": self.did,
            "tun": self.tun,
            "mtu": self.mtu,
            "source": self.source,
            "ownNodeSeen": table.self_view.seen,
            "ownNodeAnnotated": table.self_annotated_with(&self.endpoint_hex, &self.did),
            "ownPodCIDRs": table.own_cidrs.iter().map(ToString::to_string).collect::<Vec<_>>(),
            "routes": routes,
            "installedRoutes": installed,
            "rejected": rejected,
            "counters": counters,
        })
    }
}

pub fn respond(info: &StatusInfo, request_line: &str) -> (u16, &'static str, String) {
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("");
    let path = parts.next().unwrap_or("");
    if method != "GET" {
        return (405, "text/plain", "method not allowed\n".to_string());
    }
    match path {
        "/status" => (200, "application/json", format!("{}\n", info.status_json())),
        "/metrics" => {
            let mut text = info.stats.prometheus();
            text.push_str(&format!(
                "# TYPE socialweb_meshd_ready gauge\nsocialweb_meshd_ready {}\n# TYPE socialweb_meshd_routes gauge\nsocialweb_meshd_routes {}\n# TYPE socialweb_meshd_connections gauge\nsocialweb_meshd_connections {}\n",
                u8::from(info.is_ready()),
                info.routes.borrow().routes.len(),
                info.connections.connected().len()
            ));
            (200, "text/plain; version=0.0.4", text)
        }
        "/healthz" | "/readyz" => {
            if info.is_ready() {
                (200, "text/plain", "ok\n".to_string())
            } else {
                (503, "text/plain", "not ready\n".to_string())
            }
        }
        _ => (404, "text/plain", "not found\n".to_string()),
    }
}

fn reason(code: u16) -> &'static str {
    match code {
        200 => "OK",
        404 => "Not Found",
        405 => "Method Not Allowed",
        503 => "Service Unavailable",
        _ => "Error",
    }
}

async fn serve_one(info: Arc<StatusInfo>, mut stream: TcpStream) -> std::io::Result<()> {
    let mut buf = Vec::with_capacity(1024);
    let mut chunk = [0u8; 1024];
    loop {
        let n = stream.read(&mut chunk).await?;
        if n == 0 {
            return Ok(());
        }
        buf.extend_from_slice(&chunk[..n]);
        if buf.windows(4).any(|w| w == b"\r\n\r\n") || buf.windows(2).any(|w| w == b"\n\n") {
            break;
        }
        if buf.len() > REQUEST_LIMIT {
            return Ok(());
        }
    }
    let text = String::from_utf8_lossy(&buf);
    let request_line = text.lines().next().unwrap_or("");
    let (code, content_type, body) = respond(&info, request_line);
    let response = format!(
        "HTTP/1.1 {code} {}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        reason(code),
        body.len()
    );
    stream.write_all(response.as_bytes()).await?;
    stream.shutdown().await
}

pub async fn bind(addr: SocketAddr) -> std::io::Result<TcpListener> {
    TcpListener::bind(addr).await
}

pub async fn serve(listener: TcpListener, info: Arc<StatusInfo>) {
    loop {
        let Ok((stream, _)) = listener.accept().await else {
            continue;
        };
        let info = Arc::clone(&info);
        tokio::spawn(async move {
            let _ = tokio::time::timeout(REQUEST_TIMEOUT, serve_one(info, stream)).await;
        });
    }
}
