use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::time::Duration;

use hickory_server::Server;
use hickory_server::proto::rr::rdata::{A, AAAA, SOA};
use hickory_server::proto::rr::{LowerName, Name, RData, Record};
use hickory_server::resolver::config::NameServerConfig;
use hickory_server::store::forwarder::{ForwardConfig, ForwardZoneHandler};
use hickory_server::store::in_memory::InMemoryZoneHandler;
use hickory_server::zone_handler::{AxfrPolicy, Catalog, ZoneType};
use ipnet::IpNet;
use tokio::net::{TcpListener, UdpSocket};
use tokio::sync::watch;

use crate::roster::Roster;

pub const TTL: u32 = 5;
pub const BIND_RETRY: Duration = Duration::from_secs(1);
pub const TCP_TIMEOUT: Duration = Duration::from_secs(5);

pub struct DnsOptions {
    pub listen: Vec<SocketAddr>,
    pub zone: String,
    pub upstream: Vec<SocketAddr>,
}

fn zone_records(origin: &Name, roster: &Roster, serial: u32) -> InMemoryZoneHandler {
    let mut zone = InMemoryZoneHandler::empty(origin.clone(), ZoneType::Primary, AxfrPolicy::Deny);
    let soa = SOA::new(origin.clone(), origin.clone(), serial, 60, 60, 3600, TTL);
    zone.upsert_mut(
        Record::from_rdata(origin.clone(), TTL, RData::SOA(soa)),
        serial,
    );
    for peer in &roster.peers {
        let addrs: Vec<IpAddr> = peer
            .cidrs
            .iter()
            .filter_map(|cidr| cidr.trim().parse::<IpNet>().ok())
            .map(|cidr| cidr.network())
            .collect();
        for label in std::iter::once(&peer.name).chain(&peer.names) {
            let Ok(name) = Name::from_ascii(label).and_then(|n| n.append_domain(origin)) else {
                tracing::warn!(event = "dns_name_invalid", name = %label);
                continue;
            };
            for addr in &addrs {
                let rdata = match addr {
                    IpAddr::V4(v4) => RData::A(A(*v4)),
                    IpAddr::V6(v6) => RData::AAAA(AAAA(*v6)),
                };
                zone.upsert_mut(Record::from_rdata(name.clone(), TTL, rdata), serial);
            }
        }
    }
    zone
}

fn forwarder(upstream: &[SocketAddr]) -> Result<ForwardZoneHandler, String> {
    let name_servers = upstream
        .iter()
        .map(|addr| {
            let mut server = NameServerConfig::udp_and_tcp(addr.ip());
            for connection in &mut server.connections {
                connection.port = addr.port();
            }
            server
        })
        .collect();
    ForwardZoneHandler::builder_tokio(ForwardConfig {
        name_servers,
        options: None,
    })
    .with_origin(Name::root())
    .build()
}

async fn bind(addr: SocketAddr) -> (UdpSocket, TcpListener) {
    let mut warned = false;
    loop {
        match (UdpSocket::bind(addr).await, TcpListener::bind(addr).await) {
            (Ok(udp), Ok(tcp)) => return (udp, tcp),
            (udp, tcp) => {
                if !warned {
                    let err = udp.err().or(tcp.err()).map(|e| e.to_string());
                    tracing::info!(event = "dns_bind_waiting", listen = %addr, error = ?err);
                    warned = true;
                }
                tokio::time::sleep(BIND_RETRY).await;
            }
        }
    }
}

pub fn origin(zone: &str) -> Result<Name, String> {
    Name::from_ascii(zone.trim_end_matches('.'))
        .and_then(|name| name.append_domain(&Name::root()))
        .map_err(|err| format!("--dns-zone {zone}: {err}"))
}

pub async fn run(
    options: DnsOptions,
    mut roster: watch::Receiver<Arc<Roster>>,
) -> Result<(), String> {
    let origin = origin(&options.zone)?;
    let mut serial: u32 = 1;
    let zone = Arc::new(zone_records(&origin, &roster.borrow_and_update(), serial));
    let mut catalog = Catalog::new();
    catalog.upsert(LowerName::new(&origin), vec![zone.clone()]);
    if !options.upstream.is_empty() {
        catalog.upsert(
            LowerName::new(&Name::root()),
            vec![Arc::new(forwarder(&options.upstream)?)],
        );
    }
    let mut server = Server::new(catalog);
    for addr in &options.listen {
        let (udp, tcp) = bind(*addr).await;
        server.register_socket(udp);
        server.register_listener(tcp, TCP_TIMEOUT, 16);
        tracing::info!(event = "dns_listening", listen = %addr, zone = %origin, upstream = ?options.upstream);
    }
    tokio::spawn(async move {
        while roster.changed().await.is_ok() {
            serial = serial.wrapping_add(1);
            let mut fresh = zone_records(&origin, &roster.borrow_and_update(), serial);
            *zone.records_mut().await = std::mem::take(fresh.records_get_mut());
            tracing::info!(event = "dns_zone", zone = %origin, serial);
        }
    });
    server
        .block_until_done()
        .await
        .map_err(|err| err.to_string())
}
