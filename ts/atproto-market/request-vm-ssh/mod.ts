import { Command } from "@publicdomainrelay/cli-args-env";
import { createLogger } from "@publicdomainrelay/logger";
import { createServe } from "@publicdomainrelay/serve";
import { applyOAuthAgentToRequesterPDS, createOAuthRequester, createRequesterPDS } from "@publicdomainrelay/requester-xrpc";
import type { OAuthRequesterHandle } from "@publicdomainrelay/requester-xrpc";
import { createDelegatingAgent, createRepoPdsAgent } from "@publicdomainrelay/atproto-helpers";
import type { RepoPdsAgent } from "@publicdomainrelay/atproto-helpers";
import type { RequesterPDS } from "@publicdomainrelay/requester-abc";
import { OFFERING_NSID } from "@publicdomainrelay/market-common";
import { REQUESTER_OAUTH_SCOPE } from "@publicdomainrelay/oauth-scope";
import { createDefaultATProtoEventStreamsClient } from "@publicdomainrelay/atproto-event-streams-client";
import type { UserDataPatch } from "@publicdomainrelay/cloud-init-common";
import { Secp256k1Keypair } from "@atproto/crypto";
import { explicitRelayUrls, marketNetwork } from "@publicdomainrelay/market-network-common";
import { installMarketNetworkFetch } from "@publicdomainrelay/market-network-fetch";
import { vaultRefs } from "@publicdomainrelay/market-identity-abc";
import type { BrowserOAuthClient, IdentityVault, MarketIdentity, MarketIdentityMode, VaultRef } from "@publicdomainrelay/market-identity-abc";
import { requesterAssociatingRepo, startMarketIdentityReconciler, vaultOAuthSessionStore } from "@publicdomainrelay/market-identity-atproto";
import type { MarketIdentityHandle } from "@publicdomainrelay/market-identity-atproto";
import { createKcpIdentityPrompt } from "@publicdomainrelay/identity-prompt-kcp";
import { createOpenBaoIdentityVault } from "@publicdomainrelay/market-identity-vault-openbao";
import { createOpenBaoServerResolver } from "@publicdomainrelay/openbao-server-kcp";
import type { MarketLoopStatus, MarketRequester } from "@publicdomainrelay/socialweb-computer-k8s-api";
import { MARKET_REQUESTER_RESOURCE } from "@publicdomainrelay/socialweb-computer-k8s-api";
import { createCoalescedStatusWriter, createKcpRestClient, statusFieldWrite } from "@publicdomainrelay/kcp-client-rest";
import { MarketLoopTally, type MarketLoopView } from "@publicdomainrelay/market-abc";
import type { AccountPdsProvider } from "@publicdomainrelay/requester-controller-abc";
import { createRequesterController } from "@publicdomainrelay/requester-controller-kcp";
import { createNodeJoinProfile } from "@publicdomainrelay/socialweb-computer-k8s-node-contract";
import { createScopeCache } from "@publicdomainrelay/policy-engine-scope-cache";
import { createScopeRefresher } from "@publicdomainrelay/scope-refresh-timers";
import cliArgsEnv from "./cli-args-env.ts";

let runtimeConfig: Record<string, unknown> | null = null;
try {
  runtimeConfig = (await import("./config.json", { with: { type: "json" } })).default;
} catch {
  runtimeConfig = null;
}
const { options } = await new Command("CONFIG_PATH_REQUEST_VM_SSH", cliArgsEnv, runtimeConfig).resolve();
const o: Record<string, any> = options;

if (!o.statusFile) {
  console.error("request-vm-ssh runs only as a requester DenoPod, in its application's own kcp: start a requester with `socialweb-computer run request`");
  Deno.exit(2);
}
const kcpBase = typeof o.kcpServer === "string" ? o.kcpServer.split("/clusters/")[0] : undefined;
o.marketIdentityKcpServer ||= kcpBase;
o.computeContractKcpServer ||= kcpBase;
o.identityVaultKcpServer ||= kcpBase;
const missing = [
  ["market-identity-kcp-server", o.marketIdentityKcpServer],
  ["market-identity-kcp-workspace", o.marketIdentityKcpWorkspace],
  ["compute-contract-kcp-server", o.computeContractKcpServer],
  ["identity-vault-openbao-server", o.identityVaultOpenbaoServer],
  ["identity-vault-kcp-server", o.identityVaultKcpServer],
].filter(([, v]) => !v).map(([n]) => `--${n}`);
if (missing.length > 0) {
  console.error(`request-vm-ssh: ${missing.join(", ")} ${missing.length === 1 ? "is" : "are"} unset; the application's profile renders them`);
  Deno.exit(2);
}

const label = o.label as string;
const logger = createLogger({ serviceName: label });
const readCa = (path: string | undefined): string | undefined => path ? Deno.readTextFileSync(path) : undefined;

const network = marketNetwork({
  plcDirectoryUrl: o.plcDirectoryUrl,
  ingressProxyHost: o.ingressProxyHost,
  relayUrls: o.relayUrl,
  firehoseUrls: o.firehoseUrl,
  qrOrigin: o.qrOrigin,
  hosts: o.networkHosts,
});
installMarketNetworkFetch({ network });

const contractKcp = createKcpRestClient({
  server: o.computeContractKcpServer,
  tokenFile: o.computeContractKcpTokenFile,
  caCert: readCa(o.computeContractKcpCaCertFile),
});

async function marketLoop(): Promise<MarketLoopTally | undefined> {
  const root = o.marketRootName as string | undefined;
  if (!root) return undefined;
  const target = { resource: MARKET_REQUESTER_RESOURCE, workspace: o.computeContractKcpWorkspace };
  const prior = await contractKcp.get(target, root).then(
    (obj) => (obj as MarketRequester | null)?.status?.market,
    (err: unknown) => {
      logger.warn("requester_market_status_read_failed", { root, error: String(err) });
      return undefined;
    },
  );
  const write = statusFieldWrite(contractKcp, target, root, "market");
  const writer = createCoalescedStatusWriter<null>({
    write: () => {
      const market: MarketLoopStatus = tally.view(new Date());
      return write(market);
    },
    onError: (err) => logger.warn("requester_market_status_failed", { root, error: String(err) }),
  });
  const tally = new MarketLoopTally(() => writer.set(null), prior as Partial<MarketLoopView> | undefined);
  return tally;
}
const loop = await marketLoop();

const firehoseMode = (o.firehoseMode as string) || "off";
const eventStreams = createDefaultATProtoEventStreamsClient({
  onlyRelays: explicitRelayUrls(network, firehoseMode),
  log: logger,
  cursorStore: loop
    ? {
      load: (source) => loop.cursorOf(source),
      save: (source, p) => loop.streamProgress(source, p.cursor, p.eventTime, new Date()),
    }
    : undefined,
});
const relayUrls = eventStreams.relays.map((r) => r.url);

const serve = createServe({
  logger,
  tcp: { addr: o.serveAddr, port: o.port ?? 0, certFile: o.tlsCertFile, keyFile: o.tlsKeyFile },
  portFile: o.portFile,
  listenersFile: o.listenersFile,
});
let healthDid = "";
serve.app.get("/xrpc/_health", (c) => c.json({ ok: healthDid !== "", did: healthDid }, healthDid ? 200 : 503));

const vaultMount = o.identityVaultMount as string;
const resolveVault = createOpenBaoServerResolver({
  server: o.identityVaultKcpServer,
  workspace: o.identityVaultKcpWorkspace,
  tokenFile: o.identityVaultKcpTokenFile,
  caCert: readCa(o.identityVaultKcpCaCertFile),
  name: o.identityVaultOpenbaoServer,
  client: o.identityVaultOpenbaoClient,
  clientDir: o.identityVaultOpenbaoClientDir,
  onWaiting: (reason) => logger.info("requester_vault_waiting", { openbaoserver: o.identityVaultOpenbaoServer, reason }),
});
const vault: IdentityVault = createOpenBaoIdentityVault({
  endpoint: async () => {
    const r = await resolveVault();
    if (r.kvMount !== vaultMount) throw new Error(`openbaoserver client ${r.name} has kv mount ${r.kvMount}, not --identity-vault-mount ${vaultMount}`);
    return { address: r.address, caCert: r.caCert, clientCert: r.clientCert, clientKey: r.clientKey, certLogin: r.certLogin };
  },
});

const guestCaPem = o.guestCaFile ? (await Deno.readTextFile(o.guestCaFile)).trim() : "";
const guestNetwork: UserDataPatch = {
  ...(guestCaPem ? { ca_certs: { trusted: [guestCaPem] } } : {}),
  ...(o.guestCurlHome ? { bootcmd: [["systemctl", "set-environment", `CURL_HOME=${o.guestCurlHome}`]] } : {}),
};
const splitList = (s: string | undefined): string[] => s ? s.split(",").map((x) => x.trim()).filter(Boolean) : [];

let stopIdentity: (() => Promise<void>) | undefined;
let pdsProvider: AccountPdsProvider;
let statusDid = "";
let statusRepoDid = "";

{
  const identityName = o.marketIdentityName as string;
  const identityPrompt = createKcpIdentityPrompt({
    server: o.marketIdentityKcpServer,
    workspace: o.marketIdentityKcpWorkspace,
    tokenFile: o.marketIdentityKcpTokenFile,
    caCert: readCa(o.marketIdentityKcpCaCertFile),
  });

  async function identityObject(): Promise<MarketIdentity & { spec: NonNullable<MarketIdentity["spec"]> }> {
    let waiting = false;
    for (;;) {
      const obj = await identityPrompt.get(identityName).catch((err) => {
        logger.warn("requester_identity_read_failed", { name: identityName, error: String(err) });
        return null;
      });
      if (obj?.spec) return obj as MarketIdentity & { spec: NonNullable<MarketIdentity["spec"]> };
      if (!waiting) logger.info("requester_identity_waiting", { name: identityName });
      waiting = true;
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  const identitySpec = (await identityObject()).spec;
  const identityMode: MarketIdentityMode = identitySpec.mode;
  const identityRefs = vaultRefs(identityName, identitySpec, vaultMount);
  logger.info("requester_identity_mode", { mode: identityMode, name: identityName });

  async function heldKeyHex(ref: VaultRef, field: string): Promise<string> {
    const existing = await vault.read(ref);
    const held = typeof existing?.[field] === "string" ? existing[field] as string : undefined;
    if (held) return held;
    const kp = await Secp256k1Keypair.create({ exportable: true });
    const hex = Array.from(await kp.export()).map((b) => b.toString(16).padStart(2, "0")).join("");
    await vault.write(ref, { ...(existing ?? {}), [field]: hex });
    return hex;
  }

  const privateKeyHex = await heldKeyHex(identityRefs.keys, "repoKeyHex");

  function requesterPDS(keyHex: string) {
    return createRequesterPDS({
      logger,
      serve,
      privateKeyHex: keyHex,
      plcDirectoryUrl: network.plcDirectoryUrl,
      ingressProxyHost: network.ingressProxyHost,
      label,
      storagePath: o.pdsStatePath,
      skipIngress: !!o.noIngressProxy,
    });
  }

  let localPds: Awaited<ReturnType<typeof createRequesterPDS>> | undefined;
  let browserRequester: OAuthRequesterHandle | undefined;

  async function openBrowserClient(): Promise<BrowserOAuthClient> {
    const handle = identitySpec.handle;
    if (!handle) throw new Error("mode oauthBrowser needs spec.identity.handle on the MarketRequester");
    const { loadOrGenerateKeypair } = await import("@publicdomainrelay/market-atproto");
    const requester = await createOAuthRequester({
      handle,
      sessionStore: vaultOAuthSessionStore(vault, identityRefs.session),
      clientId: o.oauthClientId,
      redirectUri: o.oauthRedirectUri,
      scope: REQUESTER_OAUTH_SCOPE.join(" "),
      plcDirectoryUrl: network.plcDirectoryUrl,
      logger,
      attestationKp: await loadOrGenerateKeypair(privateKeyHex),
      privateKeyHex,
    });
    browserRequester = requester;
    return {
      restore: () => requester.restore(),
      startFlow: () => requester.startFlow(),
      completeFlow: (params) => requester.completeFlow(params),
      agent: { get did() { return requester.pds.did; } },
    };
  }

  async function repoPdsDid(url: string): Promise<string> {
    let waiting = false;
    while (true) {
      try {
        const res = await fetch(`${url.replace(/\/+$/, "")}/xrpc/com.atproto.server.describeServer`);
        if (res.ok) {
          const did = (await res.json() as { did?: string }).did;
          if (did) return did;
        } else await res.body?.cancel();
      } catch (err) {
        if (!waiting) logger.info("requester_repo_pds_waiting", { url, error: String(err) });
        waiting = true;
      }
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  let repoAgent: RepoPdsAgent | undefined;

  const identityHandle: MarketIdentityHandle = startMarketIdentityReconciler({
    name: identityName,
    prompt: identityPrompt,
    vault,
    logger,
    plcDirectoryUrl: network.plcDirectoryUrl,
    qrOrigin: network.qrOrigin,
    vaultMount,
    autoRefreshThresholdMs: 3_600_000,
    oauthClientId: o.oauthSessionClientId,
    localRepo: {
      qrParam: "plc",
      service: "requester_associate",
      open: async (repoKeyHex: string) => {
        if (o.repoPdsUrl) {
          const did = await repoPdsDid(o.repoPdsUrl);
          const agent = createRepoPdsAgent({ pdsUrl: o.repoPdsUrl, did, keypair: await Secp256k1Keypair.import(repoKeyHex, { exportable: true }) });
          repoAgent = agent;
          logger.info("requester_repo_pds", { url: o.repoPdsUrl, did });
          return requesterAssociatingRepo({
            did,
            associateCalled: new Promise<string>(() => {}),
            approveAssociation: () => {},
            rejectAssociation: () => {},
            api: { listRecords: (r, c, opts) => agent.listRecordsPage(r, c, opts) },
          });
        }
        localPds = await requesterPDS(repoKeyHex);
        await localPds.beginServe();
        return requesterAssociatingRepo(localPds);
      },
    },
    oauthBrowser: { open: openBrowserClient },
  });
  stopIdentity = () => identityHandle.stop();

  const ready = await identityHandle.ready();

  let pds: RequesterPDS;
  if (localPds) {
    pds = localPds;
  } else if (browserRequester) {
    pds = browserRequester.pds;
  } else if (repoAgent) {
    pds = await requesterPDS(await heldKeyHex({ mount: vaultMount, path: `identity/${identityName}/callback` }, "keyHex"));
    applyOAuthAgentToRequesterPDS(pds, Object.assign(repoAgent, { sessionData: { userDid: repoAgent.did } }), { log: (event, data) => logger.info(event, data) });
  } else {
    pds = await requesterPDS(privateKeyHex);
    const userAgent = Object.assign(createDelegatingAgent(ready.did, () => identityHandle.agent()), { sessionData: { userDid: ready.did } });
    applyOAuthAgentToRequesterPDS(pds, userAgent, { log: (event, data) => logger.info(event, data) });
  }
  const marketDid = (pds as unknown as { oauthSession?: { userDid?: string } }).oauthSession?.userDid ?? pds.did;
  pdsProvider = {
    accounts: false,
    pdsFor: () => Promise.resolve(pds),
    did: () => Promise.resolve(marketDid),
  };
  healthDid = marketDid;
  statusDid = marketDid;
  statusRepoDid = pds.did;
  logger.info("requester_identity_ready", { did: ready.did, handle: ready.handle, mode: identityMode, localRepo: !!localPds, repoPds: o.repoPdsUrl ?? null });
}

await serve.beginServe();
await Deno.writeTextFile(`${o.statusFile}.tmp`, JSON.stringify({ did: statusDid, repoDid: statusRepoDid, port: serve.tcpPort }) + "\n");
await Deno.rename(`${o.statusFile}.tmp`, o.statusFile);

const offeringDids = new Set<string>();
eventStreams.watch({
  wantedCollections: [OFFERING_NSID],
  onEvent: (e) => {
    if (e.operation !== "delete") offeringDids.add(e.did);
  },
  log: logger,
});

const scopeCache = createScopeCache();
const scopeRefresher = createScopeRefresher({ scopeCache });

const controller = createRequesterController({
  kcp: contractKcp,
  workspace: o.computeContractKcpWorkspace,
  logger,
  vault,
  vaultMount,
  pds: pdsProvider,
  profiles: [createNodeJoinProfile()],
  plcUrl: network.plcDirectoryUrl,
  ingressProxyHost: network.ingressProxyHost,
  relayUrls,
  eventStreams,
  scopeCache,
  scopeRefresher,
  loop,
  guestNetwork,
  guestHostAliases: splitList(o.guestHostAliases),
  extraBidderDids: splitList(o.bidderDids),
  defaultVmReadyTimeoutSec: o.vmReadyTimeoutSec as number,
  skipRbac: !!o.skipRbac,
  policyEngine: o.policyEngine,
  policyEngineUrl: o.policyEngineUrl,
  onlyRemotePolicyExec: o.onlyRemotePolicyExec,
  allowUntrustedPolicyExec: o.allowUntrustedPolicyExec,
  acceptCopies: o.acceptCopies,
  offeringWatcherDids: () => [...offeringDids],
  limits: { maxConcurrent: o.maxConcurrent as number, perAccount: o.perAccountMax as number },
  orphanScan: true,
  unclaimedGraceMs: o.unclaimedGraceMs as number,
  orphanScanIntervalMs: o.orphanScanIntervalMs as number,
  recordRetentionMs: (o.recordRetentionSec as number) * 1000,
  recordRetentionScanMs: o.recordRetentionScanMs as number,
});

function shutdown(): void {
  eventStreams.close();
  serve.shutdown();
  controller.stop()
    .then(() => stopIdentity?.())
    .finally(() => Deno.exit());
}
Deno.addSignalListener("SIGINT", shutdown);
Deno.addSignalListener("SIGTERM", shutdown);

controller.start();
logger.info("requester_controller_started", { maxConcurrent: o.maxConcurrent, perAccountMax: o.perAccountMax });
await new Promise<never>(() => {});
