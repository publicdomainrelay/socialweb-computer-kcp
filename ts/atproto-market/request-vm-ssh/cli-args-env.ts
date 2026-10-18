import { ALLOW_UNTRUSTED_POLICY_EXEC_CLI_OPTION, ONLY_REMOTE_POLICY_EXEC_CLI_OPTION, POLICY_ENGINE_URL_CLI_OPTION } from "@publicdomainrelay/policy-engine-cli-options";

export default {
  name: "request-vm-ssh",
  description:
    "The requester controller DenoPod of an application. It runs only inside the application's own kcp and carries out every ComputeContract object of its workspace: for a MarketRequester application (`socialweb-computer run request`) with the application's own identity, for a Gateway application with the gateway's own repo, associated once with its operator account. Its keys and secrets live in the application's own OpenBao.",
  options: {
    "status-file": {
      type: "string" as const,
      description: "File the requester's DID and its contract's phase are written to; the DenoPod service template sets it, and without it the entry refuses to start",
      env: "STATUS_FILE",
    },
    "kcp-server": {
      type: "string" as const,
      description: "The DenoPod's own kcp (the deno runtime sets KCP_SERVER); its base address is the server of every kcp object this requester reads unless an option names another",
      env: "KCP_SERVER",
    },
    "port": {
      type: "number" as const,
      description: "Port the requester's repo and callbacks listen on (0: any free port, reported to the port file)",
      env: "PORT",
      default: 0,
    },
    "serve-addr": {
      type: "string" as const,
      description: "Address the requester listens on",
      env: "SERVE_ADDR",
      default: "127.0.0.1",
    },
    "port-file": {
      type: "string" as const,
      description: "File the bound port is written to",
      env: "PORT_FILE",
    },
    "listeners-file": {
      type: "string" as const,
      description: "File every bound listener is reported to as JSON once listening",
      env: "LISTENERS_FILE",
    },
    "tls-cert-file": {
      type: "string" as const,
      description: "TLS certificate the listener serves",
    },
    "tls-key-file": {
      type: "string" as const,
      description: "TLS key the listener serves",
    },
    "label": {
      type: "string" as const,
      description: "Service name in the structured log and the requester's ingress label",
      default: "requester",
    },
    "plc-directory-url": {
      type: "string" as const,
      description: "PLC directory of the market network",
      default: "https://plc.directory",
    },
    "ingress-proxy-host": {
      type: "string" as const,
      description: "Ingress proxy (dispatcher) host the requester and the guest register with",
      default: "xrpc.fedproxy.com",
    },
    "relay-url": {
      type: "string" as const,
      description: "Comma-separated relay URLs the requester watches and announces its repo to",
    },
    "firehose-url": {
      type: "string" as const,
      description: "Comma-separated firehose URLs of the market network",
    },
    "guest-tls-port": {
      type: "number" as const,
      description: "Port of the dispatcher's TLS front; with --repo-pds-url the requester asks each relay to crawl its repo PDS at did-key-<repo key>.<ingress proxy host>:<this port> (unset: the ingress proxy host's own port)",
    },
    "network-hosts": {
      type: "string" as const,
      description: "Comma-separated name=address pairs of the market network's service names (its PDSes, PLC, relay) and the loopback address each answers on, copied from the network value; another application's names never resolve through this application's own kcp",
    },
    "firehose-mode": {
      type: "string" as const,
      description: "off | subscriberepos",
      default: "off",
    },
    "accept-copies": {
      type: "number" as const,
      description: "How many copies of the first submitAccept the requester sends at once; more than one proves that the bidder makes one contract per accept",
      default: 1,
    },
    "qr-origin": {
      type: "string" as const,
      description: "Origin of the QR sign-in page",
    },
    "no-ingress-proxy": {
      type: "boolean" as const,
      description: "Do not register the requester's repo with the ingress proxy",
    },
    "market-identity-name": {
      type: "string" as const,
      description: "The MarketIdentity object the sign-in follows; it names the identity's vault paths",
      default: "requester",
    },
    "market-identity-kcp-server": {
      type: "string" as const,
      description: "kcp server holding the MarketIdentity object (the application's own kcp)",
    },
    "market-identity-kcp-workspace": {
      type: "string" as const,
      description: "kcp workspace holding the MarketIdentity object",
    },
    "market-identity-kcp-token-file": {
      type: "string" as const,
      description: "Bearer token file for the MarketIdentity object, re-read on every call",
      env: "KCP_PROJECTED_KCP_TOKEN",
    },
    "market-identity-kcp-ca-cert-file": {
      type: "string" as const,
      description: "CA certificate of that kcp",
      env: "KCP_PROJECTED_KCP_CA",
    },
    "compute-contract-kcp-server": {
      type: "string" as const,
      description: "kcp server holding the ComputeContract objects (the application's own kcp)",
    },
    "compute-contract-kcp-workspace": {
      type: "string" as const,
      description: "kcp workspace holding the ComputeContract objects; the controller carries out every one of them",
    },
    "max-concurrent": {
      type: "number" as const,
      description: "How many contracts the controller carries out at once; the others wait Pending",
      default: 64,
    },
    "per-account-max": {
      type: "number" as const,
      description: "How many contracts of one account the controller carries out at once",
      default: 16,
    },
    "unclaimed-grace-ms": {
      type: "number" as const,
      description: "How long a VM receipt of a known account that no contract names waits before the controller releases it",
      default: 600000,
    },
    "orphan-scan-interval-ms": {
      type: "number" as const,
      description: "How often the controller looks for VM receipts that no contract names",
      default: 300000,
    },
    "record-retention-sec": {
      type: "number" as const,
      description: "Seconds after a contract is Released or Failed before the requester removes its own rfp, payload and accept records; 0 keeps them",
      default: 604800,
    },
    "record-retention-scan-ms": {
      type: "number" as const,
      description: "How often the controller looks for finished contracts whose records are due for removal",
      default: 600000,
    },
    "memory-log-ms": {
      type: "number" as const,
      description: "How often the requester logs its V8 heap and the sizes of its per-contract maps (requester_memory)",
      default: 60000,
    },
    "market-root-name": {
      type: "string" as const,
      description: "Name of the MarketRequester root object in the ComputeContract's kcp workspace; the requester writes its loop counters to its status.market",
      env: "MARKET_ROOT_NAME",
    },
    "compute-contract-kcp-token-file": {
      type: "string" as const,
      description: "Bearer token file for the ComputeContract object, re-read on every call",
      env: "KCP_PROJECTED_KCP_TOKEN",
    },
    "compute-contract-kcp-ca-cert-file": {
      type: "string" as const,
      description: "CA certificate of that kcp",
      env: "KCP_PROJECTED_KCP_CA",
    },
    "identity-vault-mount": {
      type: "string" as const,
      description: "kv mount of the requester's OpenBao client; the identity keys, the session, the secrets bundle and each contract's secrets-server key live there",
      default: "requester",
    },
    "identity-vault-openbao-server": {
      type: "string" as const,
      description: "Name of the OpenBaoServer object of this application; its status gives the vault's address, CA, client certificate and kv mount",
    },
    "identity-vault-openbao-client": {
      type: "string" as const,
      description: "Client of that OpenBaoServer the requester logs in as",
      default: "requester",
    },
    "identity-vault-openbao-client-dir": {
      type: "string" as const,
      description: "Directory holding that client's certificate (tls.crt), key (tls.key) and the vault CA (ca.crt), kept current by the DenoPod's projected credentials; unset, they are read from the files the OpenBaoServer status names",
      env: "KCP_PROJECTED_VAULT",
    },
    "identity-vault-kcp-server": {
      type: "string" as const,
      description: "kcp server holding the OpenBaoServer object",
    },
    "identity-vault-kcp-workspace": {
      type: "string" as const,
      description: "kcp workspace holding the OpenBaoServer object",
    },
    "identity-vault-kcp-token-file": {
      type: "string" as const,
      description: "Bearer token file for the OpenBaoServer object, re-read on every call",
      env: "KCP_PROJECTED_KCP_TOKEN",
    },
    "identity-vault-kcp-ca-cert-file": {
      type: "string" as const,
      description: "CA certificate of that kcp",
      env: "KCP_PROJECTED_KCP_CA",
    },
    "oauth-client-id": {
      type: "string" as const,
      description: "OAuth client_id of the browser sign-in",
      default: "http://localhost",
    },
    "oauth-redirect-uri": {
      type: "string" as const,
      description: "OAuth redirect URI of the browser sign-in",
      default: "http://127.0.0.1:0/callback",
    },
    "oauth-session-client-id": {
      type: "string" as const,
      description: "OAuth client_id a restored session was issued to; a refresh token is bound to the client that obtained it",
    },
    "repo-pds-url": {
      type: "string" as const,
      description: "The application's own PDS DenoPod by kcp service name (hono-repo-pds); set, every record the requester writes and reads goes to that repo, signed with the repo key from the application's OpenBao, and the requester answers bids and events on a callback DID of its own",
      env: "REPO_PDS_URL",
    },
    "pds-state-path": {
      type: "string" as const,
      description: "Storage of the requester's own repo (localRepo mode); it lives in the application's state dir so a restarted kcp resumes the same repo",
    },
    "bidder-dids": {
      type: "string" as const,
      description: "Comma-separated bidder DIDs the RFP is sent to besides the ones the relays announce",
    },
    "guest-host-aliases": {
      type: "string" as const,
      description: "Comma-separated /etc/hosts entries for the guest, each '<ip> <name>'",
    },
    "guest-ca-file": {
      type: "string" as const,
      description: "PEM CA the guest trusts (cloud-init ca_certs), for a market network whose TLS front has its own CA",
      env: "KCP_PROJECTED_NETWORK_CA",
    },
    "guest-curl-home": {
      type: "string" as const,
      description: "CURL_HOME the guest's systemd manager exports at boot",
    },
    "vm-ready-timeout-sec": {
      type: "number" as const,
      description: "How long the requester waits for the guest's onNetwork when the ComputeContract names no timeout",
      default: 120,
    },
    "skip-rbac": {
      type: "boolean" as const,
      description: "Do not publish the ssh-key rbac record for the winning bidder",
    },
    "policy-engine": {
      type: "string" as const,
      description: "Policy engine the requester evaluates its ComputeContract policy with",
    },
    "only-remote-policy-exec": ONLY_REMOTE_POLICY_EXEC_CLI_OPTION,
    "allow-untrusted-policy-exec": ALLOW_UNTRUSTED_POLICY_EXEC_CLI_OPTION,
    "policy-engine-url": POLICY_ENGINE_URL_CLI_OPTION,
  },
};
