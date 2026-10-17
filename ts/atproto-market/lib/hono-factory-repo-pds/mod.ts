import { IdResolver } from "@atproto/identity";
import { Secp256k1Keypair } from "@atproto/crypto";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { createServe } from "@publicdomainrelay/serve";
import { createLocalPDSAgent } from "@publicdomainrelay/atproto-helpers";
import type { LocalPDSAgent } from "@publicdomainrelay/atproto-helpers";
import { decideAssociation, vaultRefs } from "@publicdomainrelay/market-identity-abc";
import { createKcpIdentityPrompt } from "@publicdomainrelay/identity-prompt-kcp";
import { createOpenBaoIdentityVault } from "@publicdomainrelay/market-identity-vault-openbao";
import { createOpenBaoServerResolver } from "@publicdomainrelay/openbao-server-kcp";
import { createKcpRestClient } from "@publicdomainrelay/kcp-client-rest";
import { startAtprotoRecordReconciler, startAtprotoRecordWatchReconciler } from "@publicdomainrelay/atproto-record-kcp";
import type { AtprotoRecordReconciler } from "@publicdomainrelay/atproto-record-kcp";
import { installMarketNetworkFetch } from "@publicdomainrelay/market-network-fetch";
import type { MarketNetwork } from "@publicdomainrelay/market-network-common";
import { BADGE_BLUE_KEYS_NSID } from "@publicdomainrelay/market-lexicons";

export const REPO_PDS_NAME = "pds";
export const REPO_KEY_FIELD = "repoKeyHex";

export interface RepoPdsSpec {
  network: MarketNetwork;
  identityName: string;
  associateServiceId: string;
  kcp: { server: string; workspace: string; tokenFile?: string; caFile?: string };
  vault: { server: string; client: string; clientDir?: string; kcpServer: string; mount: string; kvMountClient: string };
  storagePath: string;
  serve: { addr: string; port: number };
  reports: { portFile?: string; listenersFile?: string; statusFile?: string };
}

export interface RepoPdsApp {
  start(): Promise<{ did: string; ingressHost: string; servePort: number }>;
  shutdown(): Promise<void>;
}

export function createRepoPdsApp(spec: RepoPdsSpec, ports: { logger: StructuredLoggerInterface }): RepoPdsApp {
  const { logger } = ports;
  let pds: LocalPDSAgent | undefined;
  let records: AtprotoRecordReconciler | undefined;
  let watches: AtprotoRecordReconciler | undefined;
  const caCert = spec.kcp.caFile ? Deno.readTextFileSync(spec.kcp.caFile) : undefined;
  const prompt = createKcpIdentityPrompt({ server: spec.kcp.server, workspace: spec.kcp.workspace, tokenFile: spec.kcp.tokenFile, caCert });
  const kcp = createKcpRestClient({ server: spec.kcp.server, tokenFile: spec.kcp.tokenFile, caFile: spec.kcp.caFile });
  const resolve = createOpenBaoServerResolver({
    server: spec.vault.kcpServer,
    tokenFile: spec.kcp.tokenFile,
    caCert,
    name: spec.vault.server,
    client: spec.vault.client,
    clientDir: spec.vault.clientDir,
    kvMountClient: spec.vault.kvMountClient,
    onWaiting: (reason) => logger.info("repo_pds_vault_waiting", { openbaoserver: spec.vault.server, reason }),
  });
  const vault = createOpenBaoIdentityVault({
    endpoint: async () => {
      const r = await resolve();
      return { address: r.address, caCert: r.caCert, clientCert: r.clientCert, clientKey: r.clientKey, certLogin: r.certLogin };
    },
  });
  const idResolver = new IdResolver({ plcUrl: spec.network.plcDirectoryUrl });

  async function identitySpec() {
    while (true) {
      const o = await prompt.get(spec.identityName).catch((err) => {
        logger.warn("repo_pds_identity_read_failed", { name: spec.identityName, error: String(err) });
        return null;
      });
      if (o?.spec) return o.spec;
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  async function repoKeyHex(): Promise<string> {
    const refs = vaultRefs(spec.identityName, await identitySpec(), spec.vault.mount);
    let waiting = false;
    while (true) {
      const held = await vault.read(refs.keys).catch((err) => {
        logger.warn("repo_pds_key_read_failed", { mount: refs.keys.mount, path: refs.keys.path, error: String(err) });
        return null;
      });
      const hex = held?.[REPO_KEY_FIELD];
      if (typeof hex === "string" && hex) return hex;
      if (!waiting) logger.info("repo_pds_key_waiting", { mount: refs.keys.mount, path: refs.keys.path, hint: "the application's core writes the repo key into its OpenBao once" });
      waiting = true;
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  async function callerHandle(did: string): Promise<string | undefined> {
    const doc = await idResolver.did.resolve(did).catch(() => null) as { alsoKnownAs?: string[] } | null;
    return doc?.alsoKnownAs?.[0]?.replace(/^at:\/\//, "");
  }

  async function onAssociate(callerDid: string): Promise<void> {
    const id = await identitySpec();
    const handle = await callerHandle(callerDid);
    const decision = decideAssociation(id, callerDid, handle);
    logger.info("repo_pds_association_asked", { callerDid, handle, decision });
    if (decision === "reject") throw new Error(`${handle ?? callerDid} is in spec.rejectedAssociations of MarketIdentity ${spec.identityName}`);
    if (decision !== "approve") {
      throw new Error(`${handle ?? callerDid} (${callerDid}) is not approved: set spec.associateWith or add the DID to spec.approvedAssociations of MarketIdentity ${spec.identityName}, then scan again`);
    }
    if (!id.approvedAssociations?.includes(callerDid)) await prompt.answerAssociation(spec.identityName, callerDid, "approve");
    records!.wake();
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const page = await pds!.repoApi.listRecords(pds!.did as never, BADGE_BLUE_KEYS_NSID, { limit: 100 });
      const found = page.records.some((r) => {
        const v = r.value as Record<string, unknown>;
        return v.keyId === pds!.did && v.challenge === callerDid && v.service === spec.associateServiceId;
      });
      if (found) return;
      await new Promise((r) => setTimeout(r, 250));
    }
    throw new Error(`${callerDid} is approved in MarketIdentity ${spec.identityName}, but no AtprotoRecord of this application wrote {challenge: ${callerDid}, keyId: ${pds!.did}, service: ${spec.associateServiceId}} within 30 s; the application's manifest set declares that record`);
  }

  return {
    async start() {
      installMarketNetworkFetch({ network: spec.network });
      const keypair = await Secp256k1Keypair.import(await repoKeyHex(), { exportable: true });
      const serve = createServe({
        logger,
        tcp: { addr: spec.serve.addr, port: spec.serve.port },
        portFile: spec.reports.portFile,
        listenersFile: spec.reports.listenersFile,
      });
      pds = await createLocalPDSAgent({
        logger,
        keypair,
        serve,
        plcDirectoryUrl: spec.network.plcDirectoryUrl,
        ingressProxyHost: spec.network.ingressProxyHost,
        storagePath: spec.storagePath,
        associateServiceId: spec.associateServiceId,
        onAssociate,
      });
      records = startAtprotoRecordReconciler({ kcp, workspace: spec.kcp.workspace, repoName: REPO_PDS_NAME, did: pds.did, repo: pds.repoApi, logger });
      watches = startAtprotoRecordWatchReconciler({ kcp, workspace: spec.kcp.workspace, repoName: REPO_PDS_NAME, plcDirectoryUrl: spec.network.plcDirectoryUrl, logger });
      await pds.beginServe();
      const status = { did: pds.did, ingressHost: pds.relay.ingressHost ?? "", servePort: serve.tcpPort, keyDid: keypair.did() };
      if (spec.reports.statusFile) {
        await Deno.writeTextFile(`${spec.reports.statusFile}.tmp`, JSON.stringify(status) + "\n");
        await Deno.rename(`${spec.reports.statusFile}.tmp`, spec.reports.statusFile);
      }
      logger.info("repo_pds_ready", status);
      return status;
    },
    async shutdown() {
      records?.stop();
      watches?.stop();
      pds?.stop();
    },
  };
}
