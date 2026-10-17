import { parsePolicyArgs } from "@publicdomainrelay/policy-engine-cli-options";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { createServe } from "@publicdomainrelay/serve";
import type { IngressRef, ServeHandle } from "@publicdomainrelay/serve";
import { createIngress, ingressUrlOf } from "@publicdomainrelay/did-key-ingress-proxy";
import { generatedKeyHex, vaultGuestKv, vaultRelayKeypair } from "@publicdomainrelay/market-bidder-vault-keys";
import { createMarketBidder } from "@publicdomainrelay/market-bidder";
import type { MarketBidder } from "@publicdomainrelay/market-bidder";
import type { MarketBidderProviderRef } from "@publicdomainrelay/market-bidder-abc";
import { createComputeProviderHooks } from "@publicdomainrelay/market-bidder-compute";
import { createKcpContractStore } from "@publicdomainrelay/market-bidder-contract-store-kcp";
import { createBidTombstoneStore, createKcpBidLedger } from "@publicdomainrelay/market-bidder-bids-kcp";
import type { BidLedger } from "@publicdomainrelay/market-bidder-abc";
import type { KcpComputeVMClient } from "@publicdomainrelay/compute-provider-kcp";
import type { ComputeVMAccepts } from "@publicdomainrelay/compute-provider-kcp-abc";
import { createComputeProviderDenoWorker, createWorkerProviderHooks } from "@publicdomainrelay/market-bidder-worker";
import {
  createATProto,
  createDelegatingAgent,
  createOAuthAgent,
  createRepoPdsAgent,
  registerKeypairPlcDid,
} from "@publicdomainrelay/atproto-helpers";
import type { RepoPdsAgent } from "@publicdomainrelay/atproto-helpers";
import { explicitRelayUrls, splitHostPort } from "@publicdomainrelay/market-network-common";
import type { MarketNetwork } from "@publicdomainrelay/market-network-common";
import { installMarketNetworkFetch } from "@publicdomainrelay/market-network-fetch";
import { vaultRefs } from "@publicdomainrelay/market-identity-abc";
import type {
  BrowserOAuthClient,
  IdentityPrompt,
  IdentityVault,
  MarketIdentity,
  MarketIdentityMode,
} from "@publicdomainrelay/market-identity-abc";
import {
  bidderAssociatingRepo,
  startMarketIdentityReconciler,
  vaultOAuthSessionStore,
} from "@publicdomainrelay/market-identity-atproto";
import type { MarketIdentityHandle } from "@publicdomainrelay/market-identity-atproto";
import { createMemoryIdentityVault } from "@publicdomainrelay/market-identity-memory";
import { createKcpIdentityPrompt } from "@publicdomainrelay/identity-prompt-kcp";
import { createOpenBaoIdentityVault } from "@publicdomainrelay/market-identity-vault-openbao";
import { createOpenBaoServerResolver } from "@publicdomainrelay/openbao-server-kcp";
import type { ResolvedOpenBaoServer } from "@publicdomainrelay/openbao-server-kcp";
import type { GuestKv } from "@publicdomainrelay/oidc-issuer-abc";
import type { GuestIdentityGrants } from "@publicdomainrelay/guest-identity-abc";
import { FEDPROXY_RBAC_NSID } from "@publicdomainrelay/fedproxy-rbac-common";
import { oauthClientMetadata } from "@publicdomainrelay/atproto-oauth-helpers";
import { createBadgeBlueKeysRecord, loadOrGenerateKeypair } from "@publicdomainrelay/market-atproto";
import { OFFERING_NSID } from "@publicdomainrelay/market-common";
import { BID_NSID, BIDS_FREE_NSID, COMPUTE_CONFIG_WIF_SIMPLE_NSID } from "@publicdomainrelay/market-lexicons";
import { DEFAULT_BID_WRITE_RESERVE, PdsWriteBudget } from "@publicdomainrelay/pds-write-budget-abc";
import { installPdsWriteObserver } from "@publicdomainrelay/pds-write-budget-fetch";
import { BIDDER_OAUTH_SCOPE } from "@publicdomainrelay/oauth-scope";
import { verifyRelayVisibility } from "@publicdomainrelay/relay-visibility-xrpc";
import { createDefaultATProtoEventStreamsClient } from "@publicdomainrelay/atproto-event-streams-client";
import { MarketLoopTally, type MarketLoopView } from "@publicdomainrelay/market-abc";
import { createCoalescedStatusWriter, createKcpRestClient, statusFieldWrite } from "@publicdomainrelay/kcp-client-rest";
import { MARKET_BIDDER_RESOURCE, type MarketBidder as MarketBidderObject, type MarketLoopStatus } from "@publicdomainrelay/socialweb-computer-k8s-api";
import { createPlcDirectoryClient } from "@publicdomainrelay/did-plc";
import type { ComputeAtproto } from "@publicdomainrelay/compute-provider-abc";
import { createComputeProviderKcpFactory } from "@publicdomainrelay/hono-factory-compute-provider-kcp";
import { Secp256k1Keypair } from "@atproto/crypto";
import { loadOrCreateAttestationKeyHex } from "@publicdomainrelay/utils-attestation-key";

export type MarketBidderIdentityPromptSpec = { kind: "kcp"; server: string; workspace: string; tokenFile?: string; caCertFile?: string };

export const BIDDER_SIGN_IN_ONLY_THROUGH_KCP =
  "the bidder signs in only through its MarketIdentity object in its own kcp; start it with socialweb-computer run bidder";

export interface MarketBidderOpenBaoServerRef {
  name: string;
  client: string;
  clientDir?: string;
  kcp: { server: string; workspace?: string; token?: string; tokenFile?: string; caCertFile?: string };
}

export type MarketBidderIdentityVaultSpec =
  { kind: "openbao"; address?: string; tokenFile?: string; caCertFile?: string; namespace?: string; server?: MarketBidderOpenBaoServerRef };

export interface MarketBidderIdentitySpec {
  name: string;
  vaultMount: string;
  prompt: MarketBidderIdentityPromptSpec;
  vault?: MarketBidderIdentityVaultSpec;
  handle?: string;
  pdsUrl?: string;
  password?: string;
  privateKeyHex?: string;
  privateKeyHexPath?: string;
  associateWith?: string;
  oauth?: {
    clientId?: string;
    redirectUri?: string;
    sessionPath?: string;
    sessionClientId?: string;
  };
}

export type MarketBidderRepoSpec = { kind: "pds"; url: string };

export interface KcpProviderSpec {
  kind: "kcp";
  server: string;
  workspace: string;
  handle: string;
  tokenFile?: string;
  caFile?: string;
  guestHostAddress?: string;
  guestLoginMount: string;
  guestIdentityResyncSec?: number;
}

export type MarketBidderProviderSpec =
  | KcpProviderSpec
  | { kind: "denoWorker"; permissionMode?: string };

export interface MarketBidderMarketSpec {
  serve: { addr?: string; port?: number; unix?: string; tlsCertFile?: string; tlsKeyFile?: string };
  ingressProxy: boolean;
  firehoseMode?: string;
  offeringRefreshSec?: number;
  bidWindowSec?: number;
  bidTtlSec?: number;
  bidQuota?: number;
  bidWriteReserve?: number;
  computeVMQuota?: number;
  policy?: string;
  policyArgs?: unknown;
  onlyRemotePolicyExec?: boolean;
  allowUntrustedPolicyExec?: boolean;
  policyEngineUrl?: string;
  ingressKeyDir?: string;
  root?: { name: string };
}

export interface MarketBidderAppSpec {
  network: MarketNetwork;
  identity: MarketBidderIdentitySpec;
  repo: MarketBidderRepoSpec;
  providers: MarketBidderProviderSpec[];
  market: MarketBidderMarketSpec;
  reports?: { portFile?: string; listenersFile?: string };
}

export interface MarketBidderAppPorts {
  logger: StructuredLoggerInterface;
  identityPrompt?: IdentityPrompt;
  identityVault?: IdentityVault;
}

export interface MarketBidderAppStatus {
  did: string;
  ingressRef?: string;
  servePort: number;
}

export interface MarketBidderApp {
  start(): Promise<MarketBidderAppStatus>;
  announce(): Promise<void>;
  shutdown(): Promise<void>;
  readonly relays: ReadonlyMap<string, IngressRef>;
}

export class BidderPolicyError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "BidderPolicyError";
  }
}

export const GUEST_DENOPOD_KCP = "guest-kcp";

const BIDDER_ASSOC_SERVICE = "bidder_associate";
const BIDDER_ASSOCIATE_RKEY = "bidder_associate";
const BADGE_BLUE_KEYS_NSID = "com.publicdomainrelay.temp.badgeBlueKeys";
const AUTO_REFRESH_THRESHOLD_MS = 3_600_000;
const DEFAULT_BID_WINDOW_SEC = 300;

type GuestContractEntry = { receiptKey: string; receiptUri: string; receiptCid: string; submitEventUrl?: string };
type Atproto = Awaited<ReturnType<typeof createATProto>>;

function guestTlsPortOf(network: MarketNetwork): number | undefined {
  if (!network.guestTlsUrl) return undefined;
  const port = new URL(network.guestTlsUrl).port;
  return port ? parseInt(port) : undefined;
}

function isCrawlRoot(u: string): boolean {
  try {
    const url = new URL(u);
    return (url.protocol === "http:" || url.protocol === "https:") && (url.pathname === "/" || url.pathname === "");
  } catch {
    return false;
  }
}

const REQUIRED_PROVIDER_FIELDS = {
  kcp: ["server", "workspace", "handle", "guestLoginMount"],
} as const;

export function missingProviderFields(p: MarketBidderProviderSpec): string[] {
  const required: readonly string[] = p.kind === "kcp" ? REQUIRED_PROVIDER_FIELDS[p.kind] : [];
  const values = p as unknown as Record<string, unknown>;
  return required.filter((k) => !values[k]);
}

export function marketBidderGuestTlsUrl(network: MarketNetwork, guestTlsPort: number | undefined): MarketNetwork {
  if (!guestTlsPort) return network;
  return { ...network, guestTlsUrl: `https://${splitHostPort(network.ingressProxyHost).hostname}:${guestTlsPort}` };
}

export async function resolveBidderPolicy(market: MarketBidderMarketSpec): Promise<Record<string, unknown>> {
  try {
    const policyArgs = parsePolicyArgs(market.policyArgs) as Record<string, unknown>;
    if (market.policy) {
      const { createPolicyRegistry } = await import("@publicdomainrelay/policy-deno-typescript");
      const { resolvePolicyName } = await import("@publicdomainrelay/policy-deno-typescript-shared");
      resolvePolicyName(createPolicyRegistry(), market.policy, "bidder");
    }
    return policyArgs;
  } catch (err) {
    throw new BidderPolicyError(`invalid --policy: ${err}`);
  }
}

export function createMarketBidderApp(spec: MarketBidderAppSpec, ports: MarketBidderAppPorts): MarketBidderApp {
  const { logger, network } = { logger: ports.logger, network: spec.network };
  const id = spec.identity;
  const listenersFile = spec.reports?.listenersFile;
  const guestTlsPort = guestTlsPortOf(network);
  const relays = new Map<string, IngressRef>();
  const acceptToContract = new Map<string, GuestContractEntry>();
  const ingressKeyDir = spec.market.ingressKeyDir;

  let identityHandle: MarketIdentityHandle | undefined;
  let bidder: MarketBidder | undefined;
  let atproto: Atproto | undefined;
  let keypair: Secp256k1Keypair | undefined;
  let ownPlcDid: string | undefined;
  let pdsHostname: string | undefined;
  let bidderServe: ServeHandle | undefined;
  let crawlRelayUrls: string[] = [];
  let localRelayUrls: string[] = [];
  let kcpContracts: { client: KcpComputeVMClient; accepts: ComputeVMAccepts; handle: string } | undefined;
  let kcpIdentities: GuestIdentityGrants | undefined;
  let bidLedger: BidLedger | undefined;
  let vaultMount = id.vaultMount;
  let keyVault: IdentityVault | undefined;
  const keysOutOfFiles = id.vault?.kind === "openbao";
  const vaultServerRef = id.vault?.kind === "openbao" ? id.vault.server : undefined;
  const resolveVaultServer: (() => Promise<ResolvedOpenBaoServer>) | undefined = vaultServerRef
    ? (() => {
      const resolve = createOpenBaoServerResolver({
        server: vaultServerRef.kcp.server,
        workspace: vaultServerRef.kcp.workspace,
        tokenFile: vaultServerRef.kcp.tokenFile,
        token: vaultServerRef.kcp.token,
        caCert: vaultServerRef.kcp.caCertFile ? Deno.readTextFileSync(vaultServerRef.kcp.caCertFile) : undefined,
        name: vaultServerRef.name,
        client: vaultServerRef.client,
        clientDir: vaultServerRef.clientDir,
        onWaiting: (reason) => logger.info("bidder_vault_waiting", { openbaoserver: vaultServerRef.name, reason }),
      });
      return async () => {
        const r = await resolve();
        logger.info("bidder_vault_resolved", { openbaoserver: r.name, address: r.address, kvMount: r.kvMount, role: r.certLogin.role });
        return r;
      };
    })()
    : undefined;

  const writeBudget = new PdsWriteBudget({
    reserve: spec.market.bidWriteReserve ?? DEFAULT_BID_WRITE_RESERVE,
    bidCollections: [BID_NSID, BIDS_FREE_NSID, COMPUTE_CONFIG_WIF_SIMPLE_NSID],
    sweepCollections: [OFFERING_NSID],
  });

  async function marketLoop(): Promise<MarketLoopTally | undefined> {
    const root = spec.market.root;
    if (!root || id.prompt?.kind !== "kcp" || !id.prompt.server) return undefined;
    const kcp = createKcpRestClient({ server: id.prompt.server, tokenFile: id.prompt.tokenFile, caFile: id.prompt.caCertFile });
    const target = { resource: MARKET_BIDDER_RESOURCE, workspace: id.prompt.workspace };
    const prior = await kcp.get(target, root.name).then(
      (o) => (o as MarketBidderObject | null)?.status?.market,
      (err: unknown) => {
        logger.warn("bidder_market_status_read_failed", { root: root.name, error: String(err) });
        return undefined;
      },
    );
    const write = statusFieldWrite(kcp, target, root.name, "market");
    const writer = createCoalescedStatusWriter<null>({
      write: () => {
        const market: MarketLoopStatus = { ...tally.view(new Date()), pdsWrites: writeBudget.view() };
        return write(market);
      },
      onError: (err) => logger.warn("bidder_market_status_failed", { root: root.name, error: String(err) }),
    });
    const tally = new MarketLoopTally(() => writer.set(null), prior as Partial<MarketLoopView> | undefined);
    writeBudget.onChange(() => writer.set(null));
    logger.info("bidder_market_status_root", {
      root: root.name,
      workspace: id.prompt.workspace,
      resumed: prior ? { bids: prior.bids ?? 0, streams: (prior.streams ?? []).map((st) => ({ source: st.source, cursor: st.cursor })) } : null,
    });
    return tally;
  }

  function kcpPrompt(): IdentityPrompt {
    if (id.prompt?.kind !== "kcp" || !id.prompt.server) throw new Error(BIDDER_SIGN_IN_ONLY_THROUGH_KCP);
    return createKcpIdentityPrompt({
      server: id.prompt.server,
      workspace: id.prompt.workspace,
      tokenFile: id.prompt.tokenFile,
      caCert: id.prompt.caCertFile ? Deno.readTextFileSync(id.prompt.caCertFile) : undefined,
    });
  }

  async function identityObject(prompt: IdentityPrompt): Promise<MarketIdentity & { spec: NonNullable<MarketIdentity["spec"]> }> {
    let waiting = false;
    while (true) {
      const obj = await prompt.get(id.name).catch((err) => {
        logger.warn("bidder_identity_read_failed", { name: id.name, error: String(err) });
        return null;
      });
      if (obj?.spec) return obj as MarketIdentity & { spec: NonNullable<MarketIdentity["spec"]> };
      if (!waiting) logger.info("bidder_identity_waiting", { name: id.name, hint: "no MarketIdentity object with a spec yet" });
      waiting = true;
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  function baseIdentityVault(): IdentityVault {
    if (ports.identityVault) return ports.identityVault;
    if (id.vault && id.password) logger.warn("atproto_password_ignored", { hint: "the identity vault holds the password" });
    if (id.vault?.kind === "openbao") {
      const resolve = resolveVaultServer;
      return createOpenBaoIdentityVault({
        address: id.vault.address,
        tokenFile: id.vault.tokenFile,
        caCert: id.vault.caCertFile ? Deno.readTextFileSync(id.vault.caCertFile) : undefined,
        namespace: id.vault.namespace,
        endpoint: resolve
          ? async () => {
            const r = await resolve();
            return { address: r.address, caCert: r.caCert, clientCert: r.clientCert, clientKey: r.clientKey, certLogin: r.certLogin };
          }
          : undefined,
      });
    }
    if (!id.password) logger.warn("bidder_identity_store_in_memory", { hint: "name an OpenBao identity vault to keep the sign-in across restarts" });
    return createMemoryIdentityVault();
  }

  async function readPrivateKeyHex(): Promise<string | undefined> {
    if (id.privateKeyHex || !id.privateKeyHexPath || keysOutOfFiles) return id.privateKeyHex;
    try {
      const content = (await Deno.readTextFile(id.privateKeyHexPath)).trim();
      if (!content) return undefined;
      logger.info("private_key_loaded_from_path", { path: id.privateKeyHexPath });
      return content;
    } catch {
      return undefined;
    }
  }

  async function persistPrivateKeyHex(hex: string, given: string | undefined): Promise<void> {
    const path = id.privateKeyHexPath;
    if (!path || keysOutOfFiles) return;
    try {
      await Deno.mkdir(path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : ".", { recursive: true });
      await Deno.writeTextFile(path, hex);
      logger.info(given ? "private_key_rewritten" : "private_key_generated_and_saved", { path });
    } catch (err) {
      logger.warn("private_key_save_failed", { path, error: String(err) });
    }
  }

  async function ingressKeypair(role: string): Promise<Secp256k1Keypair> {
    if (keyVault) return await vaultRelayKeypair(keyVault, vaultMount, role, logger);
    if (!ingressKeyDir) return await Secp256k1Keypair.create({ exportable: true });
    await Deno.mkdir(ingressKeyDir, { recursive: true, mode: 0o700 });
    const path = `${ingressKeyDir}/${role}.jwk`;
    const kp = await Secp256k1Keypair.import(await loadOrCreateAttestationKeyHex(path), { exportable: true });
    logger.info("bidder_ingress_key", { role, path, did: kp.did() });
    return kp;
  }

  async function heldRepoKeyHex(vault: IdentityVault, ref: { mount: string; path: string }): Promise<string> {
    let waiting = false;
    while (true) {
      const held = await vault.read(ref).catch((err) => {
        logger.warn("bidder_repo_key_read_failed", { mount: ref.mount, path: ref.path, error: String(err) });
        return null;
      });
      if (typeof held?.repoKeyHex === "string" && held.repoKeyHex) return held.repoKeyHex;
      if (!waiting) logger.info("bidder_repo_key_waiting", { mount: ref.mount, path: ref.path, hint: "the core writes the repo key into the bidder's OpenBao once" });
      waiting = true;
      await new Promise((r) => setTimeout(r, 1_000));
    }
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
        if (!waiting) logger.info("bidder_repo_pds_waiting", { url, error: String(err) });
        waiting = true;
      }
      await new Promise((r) => setTimeout(r, 1_000));
    }
  }

  function guestVault(): { kv: GuestKv; server: string; mount: string } {
    const vault = keyVault;
    if (!vault || !vaultServerRef) {
      throw new Error("the kcp provider keeps each guest's user data and provisioning record in the bidder's OpenBao; start the bidder with --identity-vault-openbao-server");
    }
    return { kv: vaultGuestKv(vault, vaultMount), server: vaultServerRef.name, mount: vaultMount };
  }

  async function openIngress(role: string): Promise<IngressRef> {
    const relayKeypair = await ingressKeypair(role);
    const bidderDid = ownPlcDid ?? atproto!.did;
    const signer = { did: () => bidderDid, sign: async (bytes: Uint8Array) => keypair!.sign(bytes) };
    const relay = createIngress({ logger, ingressProxyHost: network.ingressProxyHost, signer, keypair: relayKeypair });
    relays.set(role, relay);
    return relay;
  }

  async function signIn(): Promise<{ agent: unknown; attestationKp: Awaited<ReturnType<typeof loadOrGenerateKeypair>> }> {
    const prompt: IdentityPrompt = ports.identityPrompt ?? kcpPrompt();
    logger.info("bidder_identity_mode", { mode: "kcp", name: id.name });
    if (resolveVaultServer) {
      const r = await resolveVaultServer();
      if (r.kvMount !== vaultMount) logger.info("bidder_vault_mount", { option: vaultMount, openbaoserver: r.kvMount });
      vaultMount = r.kvMount;
    }

    const identitySpec = (await identityObject(prompt)).spec;
    const mode: MarketIdentityMode = identitySpec.mode;
    const refs = vaultRefs(id.name, identitySpec, vaultMount);

    const baseVault = baseIdentityVault();
    if (mode === "pdsAccount" && id.password && !id.vault && !ports.identityVault) {
      logger.warn("atproto_password_from_options", { hint: "development only; production keeps the password in the identity vault" });
      await baseVault.write(refs.keys, { password: id.password });
    }
    if (keysOutOfFiles && (id.oauth?.sessionPath || id.privateKeyHexPath)) {
      logger.warn("bidder_key_files_ignored", { hint: "with an OpenBao vault the repo key and the OAuth session live only in the vault" });
    }
    const identityVault: IdentityVault = baseVault;
    if (id.vault || ports.identityVault) keyVault = baseVault;

    const given = await readPrivateKeyHex();
    let privateKeyHex: string;
    if (mode === "localRepo") {
      privateKeyHex = await heldRepoKeyHex(identityVault, refs.keys);
    } else if (!keyVault && mode !== "oauthQr") {
      privateKeyHex = given ?? await generatedKeyHex();
    } else {
      const existing = await identityVault.read(refs.keys);
      const held = typeof existing?.repoKeyHex === "string" ? existing.repoKeyHex : undefined;
      if (held && given && given !== held) logger.warn("bidder_identity_key_from_vault", { hint: "the identity vault's key wins over --private-key-hex" });
      privateKeyHex = held ?? given ?? await generatedKeyHex();
      if (!held) await identityVault.write(refs.keys, { ...(existing ?? {}), repoKeyHex: privateKeyHex });
    }
    keypair = await Secp256k1Keypair.import(privateKeyHex, { exportable: true });
    const attestationKp = await loadOrGenerateKeypair(privateKeyHex);

    let repoAgent: RepoPdsAgent | undefined;
    const repo = spec.repo;
    identityHandle = startMarketIdentityReconciler({
      name: id.name,
      prompt,
      vault: identityVault,
      logger,
      plcDirectoryUrl: network.plcDirectoryUrl,
      qrOrigin: network.qrOrigin,
      vaultMount,
      autoRefreshThresholdMs: AUTO_REFRESH_THRESHOLD_MS,
      oauthClientId: id.oauth?.sessionClientId,
      localRepo: {
        qrParam: "bdr",
        service: BIDDER_ASSOC_SERVICE,
        open: async () => {
          const did = await repoPdsDid(repo.url);
          repoAgent = createRepoPdsAgent({ pdsUrl: repo.url, did, keypair: keypair! });
          logger.info("bidder_repo_pds", { url: repo.url, did });
          const lister = repoAgent;
          return bidderAssociatingRepo({
            did,
            associateCalled: new Promise<string>(() => {}),
            approveAssociation: () => {},
            rejectAssociation: () => {},
            repoApi: { listRecords: (r, c, o) => lister.listRecordsPage(r, c, o) },
          }, BIDDER_ASSOC_SERVICE);
        },
      },
      oauthBrowser: {
        open: async (): Promise<BrowserOAuthClient> => {
          const handle = identitySpec.handle ?? id.handle;
          if (!handle) throw new Error("mode oauthBrowser needs a handle: --atproto-handle or spec.handle");
          const sessionPath = keysOutOfFiles ? undefined : id.oauth?.sessionPath;
          const agent = await createOAuthAgent({
            handle,
            sessionPath,
            sessionStore: sessionPath ? undefined : vaultOAuthSessionStore(identityVault, refs.session) as never,
            clientId: id.oauth?.clientId,
            redirectUri: id.oauth?.redirectUri,
            scope: BIDDER_OAUTH_SCOPE.join(" "),
            plcDirectoryUrl: network.plcDirectoryUrl,
            logger,
          });
          return {
            restore: () => agent.restore(),
            startFlow: () => agent.startFlow(),
            completeFlow: (params) => agent.completeFlow(params),
            agent,
          };
        },
      },
    });

    const handle = identityHandle;
    const ready = await handle.ready();

    let agent: unknown;
    if (ready.repo && repoAgent) {
      agent = repoAgent;
      const crawlPort = guestTlsPortOf(network) ?? splitHostPort(network.ingressProxyHost).port;
      pdsHostname = new URL(ingressUrlOf(keypair.did(), network.ingressProxyHost)).hostname + (crawlPort ? `:${crawlPort}` : "");
    } else {
      agent = createDelegatingAgent(ready.did, () => handle.agent());
      ownPlcDid = await registerKeypairPlcDid({ keypair, attestationDid: attestationKp.did(), plcDirectoryUrl: network.plcDirectoryUrl, logger });
      pdsHostname = ready.pdsUrl && mode === "pdsAccount" ? new URL(ready.pdsUrl).host : undefined;
    }
    logger.info("bidder_identity_ready", { did: ready.did, handle: ready.handle, pdsUrl: ready.pdsUrl, localRepo: !!ready.repo });
    await persistPrivateKeyHex(privateKeyHex, given);
    return { agent, attestationKp };
  }

  async function bindKeys(agent: unknown, attestationKp: { did(): string }): Promise<void> {
    const at = atproto!;
    const writer = agent as { createRecord?: (repo: string, collection: string, rkey: string, record: Record<string, unknown>) => Promise<unknown> };
    try {
      await createBadgeBlueKeysRecord({
        did: at.did,
        keyId: attestationKp.did(),
        service: "bidder_attestation",
        writeRecord: async (repo, collection, rkey, record) => {
          const held = await at.getRecord(repo, collection, rkey).catch(() => null);
          if (held) return { uri: held.uri, cid: held.cid };
          return typeof writer.createRecord === "function"
            ? await writer.createRecord(repo, collection, rkey, record) as { uri: string; cid: string }
            : await at.updateRecord(collection, rkey, record);
        },
      } as Parameters<typeof createBadgeBlueKeysRecord>[0]);
      logger.info("badge_blue_key_bound", { did: at.did, keyId: attestationKp.did(), service: "bidder_attestation" });
    } catch (err) {
      logger.warn("badge_blue_key_bind_failed", { error: String(err) });
    }
    if (!id.associateWith) return;
    try {
      const held = await at.getRecord(at.did, BADGE_BLUE_KEYS_NSID, BIDDER_ASSOCIATE_RKEY).catch(() => null);
      if (held?.value.challenge !== id.associateWith) {
        await at.updateRecord(BADGE_BLUE_KEYS_NSID, BIDDER_ASSOCIATE_RKEY, {
          $type: BADGE_BLUE_KEYS_NSID,
          challenge: id.associateWith,
          keyId: at.did,
          service: "bidder_associate",
          createdAt: new Date().toISOString(),
        });
      }
      logger.info("bidder_associated_with_operator", { operatorDid: id.associateWith, bidderDid: at.did });
    } catch (err) {
      logger.warn("bidder_associate_failed", { operatorDid: id.associateWith, error: String(err) });
    }
  }

  async function openProvider(p: MarketBidderProviderSpec): Promise<MarketBidderProviderRef> {
    const at = atproto!;
    const computeAtproto = at as unknown as ComputeAtproto;
    const signed = at.createSignedRepoRecord.bind(at);
    const callService = at.callService.bind(at);
    switch (p.kind) {
      case "kcp": {
        const guest = guestVault();
        const issuerUrl = ingressUrlOf((await ingressKeypair("kcp")).did(), network.ingressProxyHost);
        logger.info("bidder_guest_endpoints", { provider: "kcp", denopod: GUEST_DENOPOD_KCP, issuerUrl });
        const kcp = createComputeProviderKcpFactory({
          logger,
          atproto: computeAtproto,
          credentials: { server: p.server, workspace: p.workspace, tokenFile: p.tokenFile, caFile: p.caFile },
          handle: p.handle,
          guestVault: guest,
          guestHostAddress: p.guestHostAddress,
          guestLoginMount: p.guestLoginMount,
          plcDirectoryUrl: network.plcDirectoryUrl,
          guestIdentityResyncMs: p.guestIdentityResyncSec ? p.guestIdentityResyncSec * 1_000 : undefined,
          getIssuerUrl: () => issuerUrl,
          guestTlsPort,
          createSignedRepoRecord: signed,
          callService,
          contractStoreReleases: true,
        });
        kcpContracts = { client: kcp.client, accepts: kcp.accepts, handle: p.handle };
        kcpIdentities = kcp.identities;
        bidLedger = createKcpBidLedger({
          kcp: createKcpRestClient({ server: p.server, tokenFile: p.tokenFile, caFile: p.caFile }),
          workspace: p.workspace,
          windowMs: (spec.market.bidWindowSec ?? DEFAULT_BID_WINDOW_SEC) * 1_000,
          ...(spec.market.bidTtlSec ? { ttl: `${spec.market.bidTtlSec}s` } : {}),
          bidQuota: spec.market.bidQuota,
          computeVMQuota: spec.market.computeVMQuota,
        });
        return createComputeProviderHooks({ provider: kcp.provider });
      }
      case "denoWorker": {
        let permissionPolicyHandler: import("@publicdomainrelay/compute-deno-abc").PermissionPolicyHandler | undefined;
        if ((p.permissionMode || "deny-all") === "allow-net") {
          const { createAllowNetOnlyPolicyHandler } = await import("@publicdomainrelay/compute-deno-atproto");
          permissionPolicyHandler = createAllowNetOnlyPolicyHandler();
        }
        return createWorkerProviderHooks({
          provider: await createComputeProviderDenoWorker({ logger, atproto: at }),
          permissionPolicyHandler,
        });
      }
    }
  }

  function openMarketServe(relay: IngressRef | undefined): ServeHandle {
    const s = spec.market.serve;
    const serve = createServe({
      logger,
      tcp: { addr: s.addr || "0.0.0.0", port: s.port ?? 0, certFile: s.tlsCertFile, keyFile: s.tlsKeyFile },
      unix: s.unix ? { socketPath: s.unix } : undefined,
      portFile: spec.reports?.portFile,
      listenersFile,
      relays: relay ? [relay] : [],
    });
    serve.app.get("/oauth-client-metadata.json", (c) =>
      c.json(oauthClientMetadata({
        clientId: id.oauth?.clientId,
        redirectUri: id.oauth?.redirectUri,
        scope: BIDDER_OAUTH_SCOPE.join(" "),
        clientName: "Compute Provider (hono-bidder)",
      })));
    return serve;
  }

  async function start(): Promise<MarketBidderAppStatus> {
    for (const p of spec.providers) {
      const missing = missingProviderFields(p);
      if (missing.length > 0) {
        throw new Error(`the ${p.kind} provider is enabled and ${missing.join(", ")} ${missing.length === 1 ? "is" : "are"} not set; it would fail at the first RFP instead of at startup`);
      }
    }
    const policyArgs = await resolveBidderPolicy(spec.market);
    installMarketNetworkFetch({ network });
    installPdsWriteObserver(writeBudget);

    let signedIn: Awaited<ReturnType<typeof signIn>>;
    try {
      signedIn = await signIn();
    } catch (err) {
      logger.error("bidder_sign_in_failed", { error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
    atproto = await createATProto({
      logger,
      badgeBlueSigner: signedIn.attestationKp,
      plcDirectory: createPlcDirectoryClient({ plcDirectoryUrl: network.plcDirectoryUrl }),
      agent: signedIn.agent as never,
      writeBudget,
    });
    await bindKeys(signedIn.agent, signedIn.attestationKp);

    const firehoseMode = spec.market.firehoseMode || "off";
    const loop = await marketLoop();
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
    crawlRelayUrls = eventStreams.relays.map((r) => r.url).filter(isCrawlRoot);
    localRelayUrls = crawlRelayUrls.filter((u) => u.startsWith("http://127.0.0.1:") || u.startsWith("http://localhost:"));

    const providers: MarketBidderProviderRef[] = [];
    for (const p of spec.providers) providers.push(await openProvider(p));
    if (kcpIdentities) {
      const identities = kcpIdentities;
      eventStreams.watch({
        wantedCollections: [FEDPROXY_RBAC_NSID],
        onEvent: (e) => {
          logger.info("requester_rbac_event", { did: e.did, rkey: e.rkey, operation: e.operation });
          identities.requesterRbacChanged(e.did);
        },
        log: logger,
      });
    }

    const marketRelay = spec.market.ingressProxy ? await openIngress("market") : undefined;
    bidderServe = openMarketServe(marketRelay);
    const contractStore = kcpContracts && bidLedger
      ? createKcpContractStore({ ...kcpContracts, tombstones: createBidTombstoneStore({ ledger: bidLedger, logger }), logger })
      : undefined;
    if (contractStore) logger.info("bidder_contract_store", { contracts: "ComputeVM spec.contract", tombstones: "Bid status.terminated" });
    const refreshSec = spec.market.offeringRefreshSec ?? 300;

    bidder = await createMarketBidder({
      logger,
      atproto,
      providers,
      relay: marketRelay,
      contractStore,
      eventStreams,
      offeringRefreshMs: refreshSec > 0 ? refreshSec * 1000 : undefined,
      serve: bidderServe,
      policy: spec.market.policy,
      policyArgs: policyArgs as never,
      policyExec: { onlyRemote: spec.market.onlyRemotePolicyExec, allowUntrusted: spec.market.allowUntrustedPolicyExec },
      policyEngineUrl: spec.market.policyEngineUrl,
      onSessionExpired: (err) =>
        logger.warn("bidder_session_expired", {
          name: id.name,
          error: err.message,
          hint: "the MarketIdentity shows SessionExpired with a sign-in-again action; the bidder stays up",
        }),
      acceptToContract,
      loop,
      writeBudget,
      bids: bidLedger,
      ...(spec.market.bidTtlSec ? { bidRecordTtlMs: spec.market.bidTtlSec * 1_000 } : {}),
    });
    await bidder.beginServe();
    return { did: atproto.did, ingressRef: marketRelay?.ingressRef, servePort: bidderServe.tcpPort };
  }

  async function announce(): Promise<void> {
    const at = atproto!;
    const localPort = bidderServe!.tcpPort;
    if (localRelayUrls.length > 0 && localPort > 0) {
      const hostname = `127.0.0.1:${localPort}`;
      for (const url of localRelayUrls) {
        await at.requestCrawl(url, hostname).then(
          () => logger.info("relay_local_reregistered", { registry: url, hostname }),
          (err: unknown) => logger.warn("relay_local_reregister_failed", { registry: url, error: String(err) }),
        );
      }
    }
    await bidder!.refreshOffering().then(
      () => logger.info("relay_offering_refreshed_after_local_registration", {}),
      (err: unknown) => logger.warn("relay_offering_refresh_failed", { error: String(err) }),
    );
    const allRelayUrls = [...crawlRelayUrls, ...localRelayUrls];
    const visibilityHostname = pdsHostname ?? (localPort > 0 ? `127.0.0.1:${localPort}` : undefined);
    if (!visibilityHostname) return;
    for (const url of crawlRelayUrls) {
      await at.requestCrawl(url, visibilityHostname).then(
        () => logger.info("relay_reregistered_after_offering_refresh", { url, hostname: visibilityHostname }),
        (err: unknown) => logger.warn("relay_reregister_failed", { url, error: String(err) }),
      );
    }
    if (allRelayUrls.length === 0) return;
    const result = await verifyRelayVisibility({ relayUrls: allRelayUrls, bidderDid: at.did, collection: OFFERING_NSID, log: logger });
    if (result.ok) {
      logger.info("relay_visibility_confirmed", { indexedBy: result.indexedBy, capableRelays: result.capableRelays });
    } else {
      logger.warn("relay_visibility_failed", {
        capableRelays: result.capableRelays,
        failures: result.failures,
        hint: "Requester-side discovery via listReposByCollection will not find this bidder. The periodic offering refresh may eventually fix this.",
      });
    }
  }

  async function shutdown(): Promise<void> {
    bidder?.shutdown();
    await identityHandle?.stop().catch(() => {});
  }

  return { start, announce, shutdown, relays };
}
