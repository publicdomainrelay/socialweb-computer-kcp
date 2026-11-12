use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::watch;

use crate::cli::{
    EXIT_ENDPOINT, EXIT_IDENTITY, EXIT_KUBE, EXIT_NETWORK_DEVICE, EXIT_OK, EXIT_RUNTIME,
    EXIT_STATUS_LISTEN, EXIT_USAGE, IdentityArgs, RunArgs,
};
use crate::dns::{self, DnsOptions};
use crate::identity::{self, Loaded};
use crate::mesh::{self, Connections, EndpointOptions, Mesh};
use crate::netconf::NetConf;
use crate::notify::sd_notify;
use crate::roster::{self, Roster};
use crate::routes::RouteTable;
use crate::source::{self, SelfIdentity, SourceError};
use crate::stats::Stats;
use crate::status::{self, StatusInfo};
use crate::tun::Tun;

pub const SHUTDOWN_GRACE: Duration = Duration::from_secs(3);

pub fn init_logging(filter: &str) {
    let filter = tracing_subscriber::EnvFilter::try_new(filter)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    let _ = tracing_subscriber::fmt()
        .json()
        .flatten_event(true)
        .with_current_span(false)
        .with_span_list(false)
        .with_target(true)
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .try_init();
}

pub fn identity(args: &IdentityArgs) -> i32 {
    match identity::load_or_create(&args.secret_file) {
        Ok(loaded) => {
            let created = matches!(loaded, Loaded::Created(_));
            let id = loaded.identity();
            println!(
                "{}",
                serde_json::json!({
                    "endpoint": id.endpoint_id_hex(),
                    "did": id.did_key(),
                    "secretFile": args.secret_file.display().to_string(),
                    "created": created,
                })
            );
            EXIT_OK
        }
        Err(err) => {
            eprintln!("socialweb-meshd: {err}");
            EXIT_IDENTITY
        }
    }
}

fn fatal(code: i32, event: &str, message: impl std::fmt::Display) -> i32 {
    tracing::error!(event, error = %message, exit_code = code);
    code
}

fn source_exit(result: Result<Result<(), SourceError>, tokio::task::JoinError>, task: &str) -> i32 {
    match result {
        Ok(Ok(())) => fatal(EXIT_RUNTIME, "task_stopped", format!("{task} ended")),
        Ok(Err(SourceError::Config(msg))) => fatal(EXIT_KUBE, "kubeconfig_unusable", msg),
        Ok(Err(SourceError::Forbidden(msg))) => fatal(EXIT_KUBE, "node_patch_forbidden", msg),
        Err(err) => fatal(EXIT_RUNTIME, "task_panicked", format!("{task}: {err}")),
    }
}

fn ready_now(table: &RouteTable, me: &SelfIdentity, kube: bool) -> bool {
    table.self_view.seen
        && !table.own_cidrs.is_empty()
        && (!kube || table.self_annotated_with(&me.endpoint_hex, &me.did))
}

pub async fn run(args: RunArgs) -> i32 {
    let loaded = match identity::load_or_create(&args.secret_file) {
        Ok(loaded) => loaded,
        Err(err) => return fatal(EXIT_IDENTITY, "secret_unusable", err),
    };
    if matches!(loaded, Loaded::Created(_)) {
        tracing::info!(event = "secret_created", path = %args.secret_file.display());
    }
    let id = loaded.identity().clone();
    let me = SelfIdentity {
        node_name: args
            .node_name
            .clone()
            .unwrap_or_else(|| id.endpoint_id_hex()),
        endpoint: id.public(),
        endpoint_hex: id.endpoint_id_hex(),
        did: id.did_key(),
    };
    tracing::info!(event = "identity", node = %me.node_name, endpoint = %me.endpoint_hex, did = %me.did);

    let http = match roster::http_client() {
        Ok(http) => http,
        Err(err) => return fatal(EXIT_RUNTIME, "http_client_failed", err),
    };
    if let Some(Err(err)) = args.dns_zone.as_deref().map(dns::origin) {
        return fatal(EXIT_USAGE, "dns_zone_invalid", err);
    }
    if let Some(Err(err)) = args.kubeconfig.as_deref().map(source::kube_client) {
        return fatal(EXIT_KUBE, "kubeconfig_unusable", err);
    }

    let tun = match Tun::create(&args.tun_name) {
        Ok(tun) => Arc::new(tun),
        Err(err) => {
            return fatal(
                EXIT_NETWORK_DEVICE,
                "tun_create_failed",
                format!("{}: {err}", args.tun_name),
            );
        }
    };
    let index = match tun.index() {
        Ok(index) => index,
        Err(err) => return fatal(EXIT_NETWORK_DEVICE, "tun_index_failed", err),
    };
    let mut netconf = match NetConf::connect(index) {
        Ok(netconf) => netconf,
        Err(err) => return fatal(EXIT_NETWORK_DEVICE, "netlink_failed", err),
    };
    if let Err(err) = netconf.bring_up(args.mtu).await {
        return fatal(EXIT_NETWORK_DEVICE, "tun_up_failed", err);
    }
    tracing::info!(event = "tun_up", tun = %tun.name(), index, mtu = args.mtu);

    let listener = match status::bind(args.metrics_listen).await {
        Ok(listener) => listener,
        Err(err) => {
            return fatal(
                EXIT_STATUS_LISTEN,
                "status_listen_failed",
                format!("{}: {err}", args.metrics_listen),
            );
        }
    };

    if !args.metrics_listen.ip().is_loopback() {
        tracing::warn!(
            event = "status_not_loopback",
            listen = %args.metrics_listen,
            reason = "the status endpoint names every peer endpoint and pod CIDR"
        );
    }

    let options = EndpointOptions {
        relay: args.relay.clone(),
        discovery: args.discovery.into(),
        bind: args.bind.clone(),
    };
    let endpoint = match mesh::bind_endpoint(id.secret.clone(), &options).await {
        Ok(endpoint) => endpoint,
        Err(err) => return fatal(EXIT_ENDPOINT, "endpoint_bind_failed", err),
    };
    tracing::info!(
        event = "endpoint_bound",
        endpoint = %me.endpoint_hex,
        sockets = ?endpoint.bound_sockets(),
        relay = ?options.relay,
        discovery = ?options.discovery
    );

    let stats = Arc::new(Stats::default());
    let connections = Arc::new(Connections::default());
    let initial = RouteTable {
        self_node: me.node_name.clone(),
        ..RouteTable::default()
    };
    let (routes_tx, routes_rx) = watch::channel(Arc::new(initial));

    let info = Arc::new(StatusInfo {
        endpoint_hex: me.endpoint_hex.clone(),
        did: me.did.clone(),
        tun: tun.name().to_string(),
        mtu: args.mtu,
        source: match (&args.kubeconfig, &args.roster) {
            (Some(_), Some(_)) => "kube+roster",
            (Some(_), None) => "kube",
            _ => "roster",
        }
        .to_string(),
        ready: Default::default(),
        routes: routes_rx.clone(),
        connections: Arc::clone(&connections),
        stats: Arc::clone(&stats),
        installed_routes: Default::default(),
    });
    tokio::spawn(status::serve(listener, Arc::clone(&info)));

    let mesh = Arc::new(Mesh {
        endpoint: endpoint.clone(),
        tun: Arc::clone(&tun),
        routes: routes_rx.clone(),
        connections: Arc::clone(&connections),
        stats: Arc::clone(&stats),
    });
    tokio::spawn(Arc::clone(&mesh).run_accept());
    let mut outbound = tokio::spawn(Arc::clone(&mesh).run_outbound());

    let kube_mode = args.kubeconfig.is_some();
    let mut annotator = args.kubeconfig.as_ref().map(|path| {
        tokio::spawn(source::run_annotator(
            path.clone(),
            me.clone(),
            routes_rx.clone(),
        ))
    });
    let (roster_tx, roster_rx) = watch::channel(Arc::new(Roster::default()));
    let (location_tx, location_rx) = watch::channel(args.roster.clone());
    let (kube_tx, kube_rx) = watch::channel(Arc::new(Vec::new()));
    let kubeconfig = args.kubeconfig.clone();
    let routes = source::run_routes(
        me.clone(),
        kube_mode.then_some(kube_rx),
        roster_rx.clone(),
        (kube_mode && args.roster.is_none()).then_some(location_tx),
        routes_tx,
        Arc::clone(&stats),
    );
    let mut source_task = tokio::spawn(async move {
        tokio::select! {
            () = routes => Ok(()),
            () = roster::run(http, location_rx, roster_tx) => Ok(()),
            result = async {
                match kubeconfig {
                    Some(path) => source::run_kube(path, kube_tx).await,
                    None => std::future::pending().await,
                }
            } => result,
        }
    });
    let mut dns_task = args.dns_zone.clone().map(|zone| {
        tokio::spawn(dns::run(
            DnsOptions {
                listen: args.dns_listen.clone(),
                zone,
                upstream: args.dns_upstream.clone(),
            },
            roster_rx.clone(),
        ))
    });

    let sync_info = Arc::clone(&info);
    let sync_stats = Arc::clone(&stats);
    let sync_me = me.clone();
    let mut sync_rx = routes_rx.clone();
    tokio::spawn(async move {
        loop {
            let table = Arc::clone(&sync_rx.borrow_and_update());
            let failures = netconf.sync(&table).await;
            for failure in &failures {
                Stats::bump(&sync_stats.route_sync_failures);
                tracing::warn!(event = "route_sync_failed", error = %failure);
            }
            if let Ok(mut installed) = sync_info.installed_routes.lock() {
                *installed = netconf
                    .installed_routes()
                    .iter()
                    .map(ToString::to_string)
                    .collect();
            }
            let ready = failures.is_empty() && ready_now(&table, &sync_me, kube_mode);
            let was = sync_info.ready.swap(ready, Ordering::Relaxed);
            if ready && !was {
                tracing::info!(
                    event = "ready",
                    own_pod_cidrs = ?table.own_cidrs.iter().map(ToString::to_string).collect::<Vec<_>>(),
                    routes = table.routes.len()
                );
                if let Err(err) = sd_notify("READY=1\nSTATUS=mesh ready") {
                    tracing::warn!(event = "sd_notify_failed", error = %err);
                }
            }
            if sync_rx.changed().await.is_err() {
                return;
            }
        }
    });

    let mut sigterm = match signal(SignalKind::terminate()) {
        Ok(s) => s,
        Err(err) => return fatal(EXIT_RUNTIME, "signal_setup_failed", err),
    };
    let mut sigint = match signal(SignalKind::interrupt()) {
        Ok(s) => s,
        Err(err) => return fatal(EXIT_RUNTIME, "signal_setup_failed", err),
    };

    let code = tokio::select! {
        _ = sigterm.recv() => { tracing::info!(event = "stopping", signal = "SIGTERM"); EXIT_OK }
        _ = sigint.recv() => { tracing::info!(event = "stopping", signal = "SIGINT"); EXIT_OK }
        result = &mut outbound => match result {
            Ok(Ok(())) => fatal(EXIT_RUNTIME, "outbound_stopped", "route source closed"),
            Ok(Err(err)) => fatal(EXIT_NETWORK_DEVICE, "tun_read_failed", err),
            Err(err) => fatal(EXIT_RUNTIME, "outbound_panicked", err),
        },
        result = &mut source_task => source_exit(result, "source"),
        result = async {
            match annotator.as_mut() {
                Some(handle) => handle.await,
                None => std::future::pending().await,
            }
        } => source_exit(result, "annotator"),
        result = async {
            match dns_task.as_mut() {
                Some(handle) => handle.await,
                None => std::future::pending().await,
            }
        } => match result {
            Ok(Ok(())) => fatal(EXIT_RUNTIME, "task_stopped", "dns ended"),
            Ok(Err(err)) => fatal(EXIT_RUNTIME, "dns_failed", err),
            Err(err) => fatal(EXIT_RUNTIME, "task_panicked", format!("dns: {err}")),
        },
    };
    let _ = sd_notify("STOPPING=1");
    connections.close_all();
    let _ = tokio::time::timeout(SHUTDOWN_GRACE, endpoint.close()).await;
    code
}
