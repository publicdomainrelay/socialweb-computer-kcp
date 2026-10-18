import { TID } from "@atproto/common";
import { IdResolver } from "@atproto/identity";
import { Secp256k1Keypair } from "@atproto/crypto";
import { Agent, CredentialSession } from "@atproto/api";
import { OAuthClient } from "@atproto/oauth-client";
import type { SessionStore } from "@atproto/oauth-client";
import { webCryptoRuntime, memoryStateStore, jsonSessionStore } from "@publicdomainrelay/atproto-oauth-helpers";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import type { RepoApi, WriteOp } from "@publicdomainrelay/atproto-repo-abc";
import type { CommitEvent } from "@publicdomainrelay/atproto-repo-abc";
import { createRepoFactory } from "@publicdomainrelay/hono-factory-atproto-repo-deno";
import { MemoryStorage, DenoKvStorage, signServiceAuth } from "@publicdomainrelay/atproto-repo-deno";
import { PlcClient, PlcNotFoundError, createGenesisOp } from "@publicdomainrelay/did-plc";
import type { AttestationKeypair, InlineAttestation } from "@publicdomainrelay/market-atproto";
import { attestationFor, toStorableEntry, createRecordResolver } from "@publicdomainrelay/market-atproto";
import type { RecordResolver } from "@publicdomainrelay/market-abc";
import { createIngress } from "@publicdomainrelay/did-key-ingress-proxy";
import type { IngressRef, ServeHandle } from "@publicdomainrelay/serve";
import type { Logger, StrongRef } from "@publicdomainrelay/market-common";
import {
  DEFAULT_MARKET_SERVICE_ID,
  DEFAULT_COMPUTE_EVENT_SERVICE_ID,
} from "@publicdomainrelay/market-common";
import type { RecordMap } from "@atiproto/atproto-attestation";
import { ASSOCIATE_CONFIRM_NSID } from "@publicdomainrelay/market-lexicons";
import { verifyServiceAuth } from "@publicdomainrelay/market-atproto";
import { fetchWithRetry } from "@publicdomainrelay/http-retry-fetch";
import { retryingFetch } from "./misc.ts";
import { WRITE_POINTS, type PdsWriteBudget, type WriteAction } from "@publicdomainrelay/pds-write-budget-abc";

export interface AtprotoAgentLike {
  did: string;
  signer: { did(): string; sign(bytes: Uint8Array): Promise<Uint8Array> };
  applyWrites(did: string, writes: WriteOp[]): Promise<CommitEvent>;
  getRecord(did: string, collection: string, rkey: string): Promise<{ uri: string; cid: string; value: Record<string, unknown> } | null>;
  listRecords(did: string, collection: string, opts?: { limit?: number }): Promise<{ records: Array<{ uri: string; cid: string; value: Record<string, unknown> }> }>;
  getServiceAuth?(aud: string, lxm?: string): Promise<string>;
  /** Bypass applyWrites for Lexicons a PDS rejects unless created via the direct endpoint. */
  createRecord?(did: string, collection: string, rkey: string, record: Record<string, unknown>): Promise<{ uri: string; cid: string }>;
  putRecord?(did: string, collection: string, rkey: string, record: Record<string, unknown>): Promise<{ uri: string; cid: string }>;
}

export interface CreateATProtoOpts {
  logger: StructuredLoggerInterface;
  badgeBlueSigner: AttestationKeypair;
  plcDirectory: PlcClient;
  agent: AtprotoAgentLike;
  writeBudget?: PdsWriteBudget;
}

export interface ATProto {
  did: string;
  getAgentDid(): string;
  signer: { did(): string; sign(bytes: Uint8Array): Promise<Uint8Array> };
  attestationKp: AttestationKeypair;
  idResolver: IdResolver;
  plcClient: PlcClient;
  applyWrites(did: string, writes: WriteOp[]): Promise<CommitEvent>;
  getRecord(did: string, collection: string, rkey: string): Promise<{ uri: string; cid: string; value: Record<string, unknown> } | null>;
  listRecords(did: string, collection: string, opts?: { limit?: number }): Promise<{ records: Array<{ uri: string; cid: string; value: Record<string, unknown> }> }>;
  createRecord(collection: string, record: Record<string, unknown>): Promise<StrongRef>;
  createSharedRecord(collection: string, record: Record<string, unknown>): Promise<StrongRef>;
  sharedRecordUris(): ReadonlySet<string>;
  updateRecord(collection: string, rkey: string, record: Record<string, unknown>): Promise<StrongRef>;
  createRepoRecord(collection: string, record: Record<string, unknown>): Promise<{ uri: string; cid: string }>;
  createSignedRepoRecord(collection: string, record: Record<string, unknown>, issuer?: string): Promise<{ uri: string; cid: string; record: Record<string, unknown> }>;
  deleteRecord(collection: string, rkey: string): Promise<void>;
  callService(endpointUrl: string, nsid: string, lxm: string, body: Record<string, unknown>): Promise<{ status: number; ok: boolean; body: unknown }>;
  /** Register PDS hostname with a relay via requestCrawl. */
  requestCrawl(relayUrl: string, hostname: string): Promise<void>;
  /** Query a relay for DIDs that have records in the given collection. */
  listReposByCollection(relayUrl: string, collection: string): Promise<string[]>;
  resolve: ReturnType<typeof createRecordResolver>;
}

function makeLogger(logger: StructuredLoggerInterface): Logger {
  return (level: string, message: string, meta?: Record<string, unknown>) => {
    const l = level as "info" | "warn" | "error" | "debug";
    logger[l]?.(message, meta);
  };
}

export async function requestRelayCrawl(logger: StructuredLoggerInterface, relayUrl: string, hostname: string): Promise<void> {
  const base = relayUrl.replace(/\/+$/, "");
  logger.info("relay_registering_pds", { hostname, relay: relayUrl });
  try {
    const res = await fetch(`${base}/xrpc/com.atproto.sync.requestCrawl`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ hostname }),
    });
    if (res.ok) {
      await res.body?.cancel();
      logger.info("relay_registered_pds", { hostname, relay: relayUrl });
    } else {
      const text = await res.text().catch(() => "");
      logger.warn("relay_register_pds_failed", { hostname, relay: relayUrl, status: res.status, body: text.slice(0, 200) });
    }
  } catch (err) {
    logger.warn("relay_register_pds_error", { hostname, relay: relayUrl, error: String(err) });
  }
}

export async function createATProto(opts: CreateATProtoOpts): Promise<ATProto> {
  const { logger, badgeBlueSigner, plcDirectory, agent, writeBudget } = opts;
  const l = makeLogger(logger);
  const did = agent.did;
  const signer = agent.signer;
  const idResolver = new IdResolver();

  function gate(collection: string, action: WriteAction): void {
    if (writeBudget) writeBudget.check(writeBudget.classOf(collection, action), WRITE_POINTS[action]);
  }

  const shared = new Map<string, { key: string; ref: Promise<StrongRef>; uri?: string }>();

  function createSharedRecord(collection: string, record: Record<string, unknown>): Promise<StrongRef> {
    const { createdAt: _createdAt, ...content } = record;
    const key = JSON.stringify(content);
    const current = shared.get(collection);
    if (current?.key === key) return current.ref;
    const entry: { key: string; ref: Promise<StrongRef>; uri?: string } = { key, ref: createRecord(collection, record) };
    shared.set(collection, entry);
    entry.ref.then(
      (ref) => {
        entry.uri = ref.uri;
      },
      () => {
        if (shared.get(collection) === entry) shared.delete(collection);
      },
    );
    return entry.ref;
  }

  function sharedRecordUris(): ReadonlySet<string> {
    const uris = new Set<string>();
    for (const entry of shared.values()) if (entry.uri) uris.add(entry.uri);
    return uris;
  }

  async function createRecord(
    collection: string,
    record: Record<string, unknown>,
  ): Promise<StrongRef> {
    gate(collection, "create");
    const rkey = TID.next().toString();
    if (agent.createRecord) {
      const { uri, cid } = await agent.createRecord(did, collection, rkey, record);
      return { $type: "com.atproto.repo.strongRef", uri, cid } as StrongRef;
    }
    await agent.applyWrites(did, [{ action: "create", collection, rkey, record }]);
    const rec = await agent.getRecord(did, collection, rkey);
    return {
      $type: "com.atproto.repo.strongRef",
      uri: `at://${did}/${collection}/${rkey}`,
      cid: rec?.cid ?? "",
    } as StrongRef;
  }

  async function updateRecord(
    collection: string,
    rkey: string,
    record: Record<string, unknown>,
  ): Promise<StrongRef> {
    gate(collection, "update");
    if (agent.putRecord) {
      const { uri, cid } = await agent.putRecord(did, collection, rkey, record);
      return { $type: "com.atproto.repo.strongRef", uri, cid } as StrongRef;
    }
    await agent.applyWrites(did, [{ action: "update", collection, rkey, record }]);
    const rec = await agent.getRecord(did, collection, rkey);
    return {
      $type: "com.atproto.repo.strongRef",
      uri: `at://${did}/${collection}/${rkey}`,
      cid: rec?.cid ?? "",
    } as StrongRef;
  }

  async function createRepoRecord(
    collection: string,
    record: Record<string, unknown>,
  ): Promise<{ uri: string; cid: string }> {
    gate(collection, "create");
    const rkey = TID.next().toString();
    if (agent.createRecord) {
      const { uri, cid } = await agent.createRecord(did, collection, rkey, record);
      return { uri, cid };
    }
    await agent.applyWrites(did, [{ action: "create", collection, rkey, record }]);
    const rec = await agent.getRecord(did, collection, rkey);
    return { uri: `at://${did}/${collection}/${rkey}`, cid: rec?.cid ?? "" };
  }

  async function createSignedRepoRecord(
    collection: string,
    record: Record<string, unknown>,
    issuer?: string,
  ): Promise<{ uri: string; cid: string; record: Record<string, unknown> }> {
    gate(collection, "create");
    const rkey = TID.next().toString();
    const att = attestationFor(badgeBlueSigner, issuer);
    const entry = await att.sign({ record: record as RecordMap, repository: did }) as InlineAttestation;
    const signed = { ...record, signatures: [toStorableEntry(entry)] };
    if (agent.createRecord) {
      const { uri, cid } = await agent.createRecord(did, collection, rkey, signed);
      return { uri, cid, record: signed };
    }
    await agent.applyWrites(did, [{ action: "create", collection, rkey, record: signed }]);
    const rec = await agent.getRecord(did, collection, rkey);
    return { uri: `at://${did}/${collection}/${rkey}`, cid: rec?.cid ?? "", record: signed };
  }

  async function deleteRecord(collection: string, rkey: string): Promise<void> {
    gate(collection, "delete");
    await agent.applyWrites(did, [{ action: "delete", collection, rkey }]);
  }

  async function callService(
    endpointUrl: string,
    nsid: string,
    lxm: string,
    body: Record<string, unknown>,
  ): Promise<{ status: number; ok: boolean; body: unknown }> {
    let targetBase: string;
    let audDid: string;

    if (endpointUrl.startsWith("http://") || endpointUrl.startsWith("https://")) {
      targetBase = `${endpointUrl.replace(/\/+$/, "")}/xrpc`;
      audDid = `did:web:${new URL(endpointUrl).host}`;
    } else if (endpointUrl.startsWith("did:")) {
      const didPart = endpointUrl.split("#")[0];
      const svcDoc = await idResolver.did.resolve(didPart);
      const svcId = endpointUrl.includes("#") ? endpointUrl.split("#")[1] : DEFAULT_MARKET_SERVICE_ID;
      const svc = svcDoc?.service?.find?.((s: { id: string }) => s.id === `#${svcId}`);
      if (!svc) throw new Error(`service ${svcId} not found in DID doc for ${didPart}`);
      const ep = (svc as { serviceEndpoint: string }).serviceEndpoint.replace(/\/+$/, "");
      targetBase = `${ep}/xrpc`;
      audDid = `did:web:${new URL(ep).host}`;
    } else {
      throw new Error(`unresolvable endpoint: ${endpointUrl}`);
    }

    const token = agent.getServiceAuth
      ? await agent.getServiceAuth(audDid, lxm)
      : await signServiceAuth(signer, { aud: audDid, lxm });
    const res = await fetch(`${targetBase}/${nsid}`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body: JSON.stringify(body),
    });
    let resBody: unknown;
    try { resBody = await res.json(); } catch { resBody = await res.text(); }
    return { status: res.status, ok: res.ok, body: resBody };
  }

  function requestCrawl(relayUrl: string, hostname: string): Promise<void> {
    return requestRelayCrawl(logger, relayUrl, hostname);
  }

  async function listReposByCollection(relayUrl: string, collection: string): Promise<string[]> {
    try {
      const url = `${relayUrl.replace(/\/+$/, "")}/xrpc/com.atproto.sync.listReposByCollection?collection=${encodeURIComponent(collection)}`;
      const res = await fetch(url);
      if (!res.ok) {
        await res.body?.cancel();
        return [];
      }
      const data = await res.json() as { repos?: Array<{ did: string }> };
      return [...new Set((data.repos ?? []).map((r) => r.did).filter(Boolean))];
    } catch {
      return [];
    }
  }

  const recordResolver = createRecordResolver(idResolver);

  return {
    did,
    getAgentDid: () => did,
    signer,
    attestationKp: badgeBlueSigner,
    idResolver,
    plcClient: plcDirectory,
    applyWrites: (d: string, writes: WriteOp[]) => {
      for (const w of writes) gate(w.collection, w.action);
      return agent.applyWrites(d, writes);
    },
    getRecord: (d: string, collection: string, rkey: string) => agent.getRecord(d, collection, rkey),
    listRecords: (d: string, collection: string, opts?: { limit?: number }) => agent.listRecords(d, collection, opts),
    createRecord,
    createSharedRecord,
    sharedRecordUris,
    updateRecord,
    createRepoRecord,
    createSignedRepoRecord,
    deleteRecord,
    callService,
    requestCrawl,
    listReposByCollection,
    resolve: recordResolver,
  };
}

export interface CreateLocalPDSAgentOpts {
  logger: StructuredLoggerInterface;
  keypair: Secp256k1Keypair;
  serve: ServeHandle;
  plcDirectoryUrl: string;
  ingressProxyHost: string;
  tls?: boolean;
  storagePath?: string;
  associateServiceId: string;
  onAssociate(callerDid: string): Promise<void>;
}

export interface LocalPDSAgent {
  did: string;
  repoApi: RepoApi;
  serve: ServeHandle;
  relay: IngressRef;
  beginServe(): Promise<void>;
  stop(): void;
}

export const REPO_OWNER_WRITE_METHODS = [
  "com.atproto.repo.createRecord",
  "com.atproto.repo.putRecord",
  "com.atproto.repo.deleteRecord",
  "com.atproto.repo.applyWrites",
];

function b64urlDecode(s: string): Uint8Array {
  const std = s.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(s.length / 4) * 4, "=");
  return Uint8Array.from(atob(std), (ch) => ch.charCodeAt(0));
}

export async function verifyRepoOwnerToken(authHeader: string | undefined, keyDid: string, did: string, lxm: string): Promise<boolean> {
  const token = authHeader?.match(/^Bearer\s+(.+)$/i)?.[1]?.trim();
  if (!token) return false;
  const parts = token.split(".");
  if (parts.length !== 3) return false;
  try {
    const header = JSON.parse(new TextDecoder().decode(b64urlDecode(parts[0])));
    const payload = JSON.parse(new TextDecoder().decode(b64urlDecode(parts[1])));
    if (header.alg !== "ES256K" || payload.iss !== did || payload.aud !== did || payload.lxm !== lxm) return false;
    if (typeof payload.exp !== "number" || payload.exp < Math.floor(Date.now() / 1000)) return false;
    const { verifySignature } = await import("@atproto/crypto");
    return await verifySignature(keyDid, new TextEncoder().encode(`${parts[0]}.${parts[1]}`), b64urlDecode(parts[2]));
  } catch {
    return false;
  }
}

export async function localPdsGenesis(keypair: Secp256k1Keypair, ingressProxyHost: string, associateServiceId: string) {
  const signingKeyDid = keypair.did();
  const epHost = ingressProxyHost.replace(/:\d+$/, "");
  const sub = signingKeyDid.replace(/:/g, "-").toLowerCase();
  const exportBytes = await keypair.export();
  const hex = Array.from(exportBytes).map((b) => b.toString(16).padStart(2, "0")).join("");
  const { loadOrGenerateKeypair } = await import("@publicdomainrelay/market-atproto");
  const attestationKp = await loadOrGenerateKeypair(hex);
  const endpoint = `https://${sub}.${epHost}`;
  const { did, op } = await createGenesisOp({
    rotationKeys: [signingKeyDid],
    verificationMethods: { atproto: signingKeyDid, attestation: attestationKp.did() },
    alsoKnownAs: [`at://${sub}.${epHost}`],
    services: {
      atproto_pds: { type: "AtprotoPersonalDataServer", endpoint },
      pdr_temp_market: { type: "PDRTempMarket", endpoint },
      pdr_temp_compute_event: { type: "PDRTempComputeEvent", endpoint },
      [associateServiceId]: { type: "PDRRequesterAssociate", endpoint },
    },
    sign: (bytes) => keypair.sign(bytes),
  });
  return { did, op, attestationKp };
}

export async function createLocalPDSAgent(opts: CreateLocalPDSAgentOpts): Promise<LocalPDSAgent> {
  const { logger, keypair, serve, plcDirectoryUrl, ingressProxyHost, storagePath, associateServiceId } = opts;
  const plc = new PlcClient({ baseUrl: plcDirectoryUrl });
  const { did, op, attestationKp } = await localPdsGenesis(keypair, ingressProxyHost, associateServiceId);

  logger.info("localPDS plc registering", { did });
  try {
    await plc.resolve(did);
    logger.info("localPDS plc already registered", { did });
  } catch (err) {
    if (err instanceof PlcNotFoundError) {
      await plc.submitOp(did, op);
      logger.info("localPDS plc registered", { did });
    } else {
      throw err;
    }
  }

  const signer = {
    did: () => did,
    sign: (bytes: Uint8Array) => keypair.sign(bytes),
  };

  const { app, api, subscribe } = createRepoFactory({
    storage: storagePath ? await DenoKvStorage.create(storagePath) : new MemoryStorage(),
    signer,
    baseOrigin: `https://${keypair.did().replace(/:/g, "-").toLowerCase()}.${ingressProxyHost}`,
    didWebServices: [
      { id: DEFAULT_MARKET_SERVICE_ID, type: "PDRTempMarket" },
      { id: DEFAULT_COMPUTE_EVENT_SERVICE_ID, type: "PDRTempComputeEvent" },
      { id: associateServiceId, type: "PDRRequesterAssociate" },
    ],
    publicKeyDid: keypair.did(),
    attestationKeyDid: attestationKp.did(),
    readOnly: true,
  });

  for (const method of REPO_OWNER_WRITE_METHODS) {
    serve.app.use(`/xrpc/${method}`, async (c: { req: { header(name: string): string | undefined }; set(k: string, v: unknown): void }, next: () => Promise<void>) => {
      if (await verifyRepoOwnerToken(c.req.header("Authorization"), keypair.did(), did, method)) c.set("oauthUserDid", did);
      await next();
    });
  }

  const xrpcRelay = createIngress({
    logger,
    ingressProxyHost,
    tls: opts.tls,
    signer,
    keypair,
    directSubscriptionHandler: (subscriptionId, nsid, params, onEvent, _onData) => {
      const unsub = subscribe({ nsid, params }, (frame) => {
        try {
          onEvent(frame);
        } catch { /* relay closed */ }
      });
      return () => unsub?.();
    },
  });
  serve.addRelay(xrpcRelay);

  const idResolver = new IdResolver({ plcUrl: plcDirectoryUrl });
  serve.app.post(`/xrpc/${ASSOCIATE_CONFIRM_NSID}`, async (c: { req: { header(name: string): string | undefined }; json(obj: unknown, status?: number): Response }) => {
    const authHeader = c.req.header("Authorization");
    if (!authHeader) return c.json({ error: "Unauthorized" }, 401);
    let callerDid: string;
    try {
      const auth = await verifyServiceAuth({
        authHeader,
        hostname: xrpcRelay.ingressHost || ingressProxyHost,
        lxm: ASSOCIATE_CONFIRM_NSID,
        serviceIds: [associateServiceId, "pdr_temp_market"],
        extraAudienceDids: [did],
        idResolver,
      });
      callerDid = auth.issuerDid;
    } catch (err) {
      return c.json({ error: String(err) }, 401);
    }
    try {
      await opts.onAssociate(callerDid);
      logger.info("localPDS association recorded", { did, callerDid });
      return c.json({ ok: true, requesterDid: did });
    } catch (err) {
      logger.warn("localPDS association refused", { did, callerDid, error: String(err) });
      return c.json({ error: "AssociationRefused", message: err instanceof Error ? err.message : String(err) }, 403);
    }
  });

  serve.app.route("/", app as never);

  return {
    did,
    repoApi: api,
    serve,
    relay: xrpcRelay,
    beginServe: () => serve.beginServe(),
    stop: () => serve.shutdown(),
  };
}

export interface RepoPdsAgentOpts {
  pdsUrl: string;
  did: string;
  keypair: Secp256k1Keypair;
}

export interface RepoPdsAgent extends AtprotoAgentLike {
  listRecordsPage(repo: string, collection: string, opts?: { limit?: number; cursor?: string }): Promise<{ records: Array<{ uri: string; cid: string; value: Record<string, unknown> }>; cursor?: string }>;
}

export function createRepoPdsAgent(opts: RepoPdsAgentOpts): RepoPdsAgent {
  const base = opts.pdsUrl.replace(/\/+$/, "");
  const did = opts.did;
  const signer = { did: () => did, sign: (bytes: Uint8Array) => opts.keypair.sign(bytes) };
  async function post(method: string, body: unknown): Promise<Record<string, unknown>> {
    const token = await signServiceAuth(signer as never, { aud: did, lxm: method });
    const res = await retryingFetch(`${base}/xrpc/${method}`, {
      method: "POST",
      headers: { "content-type": "application/json", Authorization: `Bearer ${token}` },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`${method} on ${base}: ${res.status} ${await res.text()}`);
    return await res.json() as Record<string, unknown>;
  }
  async function listRecordsPage(repo: string, collection: string, o?: { limit?: number; cursor?: string }) {
    const params = new URLSearchParams({ repo, collection, limit: String(Math.min(o?.limit ?? 100, 100)) });
    if (o?.cursor) params.set("cursor", o.cursor);
    const res = await retryingFetch(`${base}/xrpc/com.atproto.repo.listRecords?${params}`);
    if (!res.ok) throw new Error(`listRecords on ${base}: ${res.status} ${await res.text()}`);
    return await res.json() as { records: Array<{ uri: string; cid: string; value: Record<string, unknown> }>; cursor?: string };
  }
  return {
    did,
    signer,
    async applyWrites(repo: string, writes: WriteOp[]) {
      const data = await post("com.atproto.repo.applyWrites", {
        repo,
        writes: writes.map((w) => ({
          $type: `com.atproto.repo.applyWrites#${w.action}`,
          collection: w.collection,
          rkey: w.rkey,
          ...(w.action !== "delete" ? { value: (w as { record: unknown }).record } : {}),
        })),
      }) as { commit?: { rev?: string; cid?: string } };
      const rev = data.commit?.rev ?? "";
      return { repo, commit: data.commit?.cid ?? rev, rev, since: null, blocks: new Uint8Array(), ops: [], prevData: null } as unknown as CommitEvent;
    },
    async getRecord(repo: string, collection: string, rkey: string) {
      const params = new URLSearchParams({ repo, collection, rkey });
      const res = await retryingFetch(`${base}/xrpc/com.atproto.repo.getRecord?${params}`);
      if (!res.ok) {
        await res.body?.cancel();
        return null;
      }
      const data = await res.json() as { uri: string; cid?: string; value: Record<string, unknown> };
      return { uri: data.uri, cid: data.cid ?? "", value: data.value };
    },
    async listRecords(repo: string, collection: string, o?: { limit?: number }) {
      const all: Array<{ uri: string; cid: string; value: Record<string, unknown> }> = [];
      const target = o?.limit ?? 100;
      let cursor: string | undefined;
      do {
        const page = await listRecordsPage(repo, collection, { limit: target - all.length, cursor });
        all.push(...page.records);
        cursor = page.cursor;
      } while (cursor && all.length < target);
      return { records: all };
    },
    listRecordsPage,
    getServiceAuth: (aud: string, lxm?: string) => signServiceAuth(signer as never, { aud, lxm }),
    async createRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const data = await post("com.atproto.repo.createRecord", { repo, collection, rkey, record }) as { uri: string; cid: string };
      return { uri: data.uri, cid: data.cid ?? "" };
    },
    async putRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const data = await post("com.atproto.repo.putRecord", { repo, collection, rkey, record }) as { uri: string; cid: string };
      return { uri: data.uri, cid: data.cid ?? "" };
    },
  };
}

export function createDelegatingAgent(did: string, current: () => AtprotoAgentLike | null): AtprotoAgentLike {
  const live = (): AtprotoAgentLike => {
    const a = current();
    if (!a) throw new Error(`identity ${did} is not signed in`);
    if (a.did !== did) throw new Error(`identity signed in as ${a.did}, not ${did}`);
    return a;
  };
  async function write(action: "create" | "update", repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
    const a = live();
    if (action === "create" && a.createRecord) return await a.createRecord(repo, collection, rkey, record);
    if (action === "update" && a.putRecord) return await a.putRecord(repo, collection, rkey, record);
    await a.applyWrites(repo, [{ action, collection, rkey, record }]);
    const rec = await a.getRecord(repo, collection, rkey);
    return { uri: `at://${repo}/${collection}/${rkey}`, cid: rec?.cid ?? "" };
  }
  return {
    did,
    signer: { did: () => did, sign: (bytes: Uint8Array) => live().signer.sign(bytes) },
    applyWrites: (d: string, writes: WriteOp[]) => live().applyWrites(d, writes),
    getRecord: (d: string, collection: string, rkey: string) => live().getRecord(d, collection, rkey),
    listRecords: (d: string, collection: string, opts?: { limit?: number }) => live().listRecords(d, collection, opts),
    getServiceAuth: (aud: string, lxm?: string) => {
      const a = live();
      return a.getServiceAuth ? a.getServiceAuth(aud, lxm) : signServiceAuth(a.signer, { aud, lxm });
    },
    createRecord: (repo: string, collection: string, rkey: string, record: Record<string, unknown>) => write("create", repo, collection, rkey, record),
    putRecord: (repo: string, collection: string, rkey: string, record: Record<string, unknown>) => write("update", repo, collection, rkey, record),
  };
}

export interface KeypairPlcDidOpts {
  keypair: Secp256k1Keypair;
  attestationDid: string;
  plcDirectoryUrl: string;
  logger?: StructuredLoggerInterface;
}

export async function registerKeypairPlcDid(opts: KeypairPlcDidOpts): Promise<string> {
  const { keypair } = opts;
  const genesis = await createGenesisOp({
    rotationKeys: [keypair.did()],
    verificationMethods: { atproto: keypair.did(), attestation: opts.attestationDid },
    sign: (bytes: Uint8Array) => keypair.sign(bytes),
  });
  const plc = new PlcClient({ baseUrl: opts.plcDirectoryUrl });
  try {
    await plc.resolve(genesis.did);
    opts.logger?.info("keypair_plc_did_resolved", { did: genesis.did });
  } catch (err) {
    if (!(err instanceof PlcNotFoundError)) throw err;
    await plc.submitOp(genesis.did, genesis.op);
    opts.logger?.info("keypair_plc_did_registered", { did: genesis.did });
  }
  return genesis.did;
}

export interface CreateRemoteAgentOpts {
  handle: string;
  password: string;
  pdsUrl: string;
}

export interface RemoteAgent extends AtprotoAgentLike {
  pdsUrl: string;
  handle: string;
}

export async function createRemoteAgent(opts: CreateRemoteAgentOpts): Promise<RemoteAgent> {
  const session = new CredentialSession(new URL(opts.pdsUrl), retryingFetch);
  await session.login({ identifier: opts.handle, password: opts.password });
  const agent = new Agent(session);
  const did = session.did ?? "";
  if (!did) throw new Error(`signed in to ${opts.pdsUrl} as ${opts.handle} but the session carries no DID`);

  const signer = {
    did: () => did,
    sign: async (_bytes: Uint8Array): Promise<Uint8Array> => {
      throw new Error("remote agent sign not supported");
    },
  };

  return {
    did,
    signer,
    pdsUrl: opts.pdsUrl,
    handle: session.session?.handle ?? opts.handle,
    async getServiceAuth(aud: string, lxm?: string): Promise<string> {
      const res = await agent.com.atproto.server.getServiceAuth({ aud, lxm });
      return res.data.token;
    },
    async createRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const res = await agent.com.atproto.repo.createRecord({ repo, collection, rkey, record, validate: false });
      return { uri: res.data.uri, cid: res.data.cid ?? "" };
    },
    async putRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const res = await agent.com.atproto.repo.putRecord({ repo, collection, rkey, record, validate: false });
      return { uri: res.data.uri, cid: res.data.cid ?? "" };
    },
    async applyWrites(repo: string, writes: WriteOp[]): Promise<CommitEvent> {
      const res = await agent.com.atproto.repo.applyWrites({
        repo,
        writes: writes.map((w) => {
          const base: Record<string, unknown> = {
            $type: `com.atproto.repo.applyWrites#${w.action}`,
            collection: w.collection,
            rkey: w.rkey,
          };
          if (w.action !== "delete") (base as Record<string, unknown>).value = (w as { record: unknown }).record;
          return base as never;
        }),
      });
      const rev = res.data.commit?.rev as string ?? "";
      return { repo: repo as never, commit: rev as never, rev: rev as never, since: null as never, blocks: new Uint8Array() as never, ops: [], prevData: null };
    },
    async getRecord(repo: string, collection: string, rkey: string) {
      try {
        const res = await agent.com.atproto.repo.getRecord({ repo, collection, rkey });
        return { uri: res.data.uri, cid: res.data.cid ?? "", value: res.data.value as Record<string, unknown> };
      } catch {
        return null;
      }
    },
    async listRecords(repo: string, collection: string, opts?: { limit?: number }) {
      const all: Array<{ uri: string; cid: string; value: Record<string, unknown> }> = [];
      let cursor: string | undefined;
      // The PDS caps com.atproto.repo.listRecords at 100 per page. A caller may
      // request more (e.g. limit: 200); send pages of ≤100 and paginate until the
      // target is collected. Sending limit>100 verbatim makes the PDS 400 and the
      // whole read silently comes back empty.
      const target = opts?.limit ?? 100;
      const pageLimit = Math.min(target, 100);
      do {
        const res = await agent.com.atproto.repo.listRecords({ repo, collection, limit: pageLimit, cursor });
        for (const r of res.data.records) {
          all.push({ uri: r.uri, cid: r.cid ?? "", value: r.value as Record<string, unknown> });
        }
        cursor = res.data.cursor;
        if (all.length >= target) break;
      } while (cursor);
      return { records: all.slice(0, target) };
    },
  };
}

// ---------------------------------------------------------------------------
// OAuth agent -- ATProto OAuth with @atproto/oauth-client
// ---------------------------------------------------------------------------

export interface CreateOAuthAgentOpts {
  handle: string;
  sessionPath?: string;
  sessionStore?: SessionStore;
  clientId?: string;
  redirectUri?: string;
  scope?: string;
  pdsUrl?: string;
  plcDirectoryUrl?: string;
  logger?: StructuredLoggerInterface;
}

export interface OAuthAgent extends AtprotoAgentLike {
  oauthClient: OAuthClient;
  startFlow(): Promise<string>; // returns auth URL
  completeFlow(params: Record<string, string>): Promise<void>;
  restore(): Promise<boolean>;
}

export async function createOAuthAgent(opts: CreateOAuthAgentOpts): Promise<OAuthAgent> {
  const clientId = opts.clientId ?? "http://localhost";
  const redirectUri = opts.redirectUri ?? "http://127.0.0.1:0/callback";
  const scope = opts.scope ?? "atproto";
  const log = opts.logger;

  const client = new OAuthClient({
    responseMode: "query",
    clientMetadata: {
      client_id: clientId,
      application_type: "web",
      dpop_bound_access_tokens: true,
      redirect_uris: [redirectUri],
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      scope,
      token_endpoint_auth_method: "none",
    },
    stateStore: memoryStateStore(),
    sessionStore: opts.sessionStore ?? jsonSessionStore(opts.sessionPath ?? ""),
    runtimeImplementation: webCryptoRuntime(),
    identityResolver: {
      resolve: async (identifier: string) => {
        const resolver = new IdResolver({ plcUrl: opts.plcDirectoryUrl ?? "https://plc.directory" });
        const did = identifier.startsWith("did:") ? identifier : (await resolver.handle.resolve(identifier)) ?? identifier;
        const didDoc = await resolver.did.resolve(did) as Record<string, unknown>;
        const handle = ((didDoc?.alsoKnownAs as string[] | undefined)?.[0] ?? "").replace("at://", "");
        return { did, didDoc, handle: handle || "handle.invalid" };
      },
    } as never,
    allowHttp: clientId === "http://localhost" || redirectUri.startsWith("http://127.0.0.1") || redirectUri.startsWith("http://localhost"),
  });

  let _session: Awaited<ReturnType<typeof client.restore>> | null = null;

  function getSession() {
    if (!_session) throw new Error("OAuth session not initialized");
    return _session;
  }

  let _did = "";
  const _signer = {
    did: () => _did,
    sign: async () => { throw new Error("OAuth agent uses getServiceAuth, not local signing"); },
  };

  const agent: OAuthAgent = {
    oauthClient: client,
    get did() { return _did; },
    signer: _signer,

    async startFlow(): Promise<string> {
      const result = await client.authorize(opts.handle, { scope });
      return String(result);
    },

    async completeFlow(params: Record<string, string>): Promise<void> {
      const result = await client.callback(new URLSearchParams(params));
      _session = result.session;
      _did = result.session.did;
      log?.info("oauth_session_complete", { did: _did });
    },

    async restore(): Promise<boolean> {
      try {
        _session = await client.restore(opts.handle);
        _did = _session.did;
        log?.info("oauth_session_restored", { did: _did });
        return true;
      } catch {
        return false;
      }
    },

    async getServiceAuth(aud: string, lxm?: string): Promise<string> {
      const s = await getSession();
      const res = await s.fetchHandler(
        `${s.server.issuer}/xrpc/com.atproto.server.getServiceAuth`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ aud, lxm: lxm ?? aud }),
        },
      );
      if (!res.ok) {
        throw new Error(`getServiceAuth failed: ${res.status}`);
      }
      const data = await res.json() as { token: string };
      return data.token;
    },

    async applyWrites(repo: string, writes: Parameters<AtprotoAgentLike["applyWrites"]>[1]) {
      const s = await getSession();
      const res = await s.fetchHandler(
        `${s.server.issuer}/xrpc/com.atproto.repo.applyWrites`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ repo, writes: writes.map((w) => {
            const base: Record<string, unknown> = {
              $type: `com.atproto.repo.applyWrites#${w.action}`,
              collection: w.collection,
              rkey: w.rkey,
            };
            if (w.action !== "delete") (base as Record<string, unknown>).value = (w as { record: unknown }).record;
            return base;
          }) }),
        },
      );
      if (!res.ok) {
        throw new Error(`applyWrites failed: ${res.status} ${await res.text()}`);
      }
      const data = await res.json() as { commit?: { rev?: string } };
      const rev = data.commit?.rev ?? "";
      return { repo, commit: rev, rev, since: null, blocks: new Uint8Array(), ops: [] } as unknown as CommitEvent;
    },

    async getRecord(repo: string, collection: string, rkey: string) {
      const s = await getSession();
      const res = await s.fetchHandler(
        `${s.server.issuer}/xrpc/com.atproto.repo.getRecord?repo=${encodeURIComponent(repo)}&collection=${encodeURIComponent(collection)}&rkey=${encodeURIComponent(rkey)}`,
      );
      if (!res.ok) {
        await res.body?.cancel();
        return null;
      }
      const data = await res.json() as { uri: string; cid?: string; value: Record<string, unknown> };
      return { uri: data.uri, cid: data.cid ?? "", value: data.value };
    },

    async listRecords(repo: string, collection: string, opts?: { limit?: number }) {
      const s = await getSession();
      const all: Array<{ uri: string; cid: string; value: Record<string, unknown> }> = [];
      let cursor: string | undefined;
      // PDS caps listRecords at 100/page; paginate pages of ≤100 for a larger target.
      const target = opts?.limit ?? 100;
      const pageLimit = Math.min(target, 100);
      do {
        const params = new URLSearchParams({ repo, collection, limit: String(pageLimit) });
        if (cursor) params.set("cursor", cursor);
        const res = await s.fetchHandler(
          `${s.server.issuer}/xrpc/com.atproto.repo.listRecords?${params.toString()}`,
        );
        if (!res.ok) break;
        const data = await res.json() as { records: Array<{ uri: string; cid?: string; value: unknown }>; cursor?: string };
        for (const r of data.records) {
          all.push({ uri: r.uri, cid: r.cid ?? "", value: r.value as Record<string, unknown> });
        }
        cursor = data.cursor;
      } while (cursor && all.length < target);
      return { records: all.slice(0, target) };
    },
  };

  return agent;
}

// -- QR-based OAuth: session transferred from browser ----------------------

/**
 * OAuth session data returned by qr.fedfork.com after browser completes OAuth
 * and the CLI polls the XRPC endpoint.
 */
export class OAuthSessionExpiredError extends Error {
  constructor(message: string, public readonly sessionPath?: string) {
    super(message);
    this.name = "OAuthSessionExpiredError";
  }
}

export interface OAuthSessionData {
  accessJwt: string;
  refreshJwt: string;
  userDid: string;
  handle: string;
  pds: string;
  dpopPublicJwk: Record<string, string>;
  dpopPrivateJwk: Record<string, string>;
}

/**
 * Poll qr.fedfork.com for an OAuth session transferred from a browser.
 * Signs a service auth JWT with the CLI's private key to prove DID ownership.
 */
export async function pollForOAuthSession(opts: {
  cliDid: string;
  signer: { did(): string; sign(bytes: Uint8Array): Promise<Uint8Array> };
  qrFedforkOrigin: string;
  pollIntervalMs?: number;
  timeoutMs?: number;
  logger?: StructuredLoggerInterface;
  signal?: AbortSignal;
}): Promise<OAuthSessionData> {
  const origin = opts.qrFedforkOrigin.replace(/\/+$/, "");
  const pollIntervalMs = opts.pollIntervalMs ?? 3_000;
  const timeoutMs = opts.timeoutMs ?? 5 * 60 * 1_000;
  const log = opts.logger;
  const deadline = Date.now() + timeoutMs;

  while (Date.now() < deadline) {
    opts.signal?.throwIfAborted();
    const token = await signServiceAuth(opts.signer, {
      aud: `did:web:${new URL(origin).host}`,
      lxm: "com.fedfork.atprotoOauthQR",
      expiresInSec: 60,
    });

    const res = await fetch(`${origin}/xrpc/com.fedfork.atprotoOauthQR`, {
      headers: { authorization: `Bearer ${token}` },
      signal: opts.signal,
    });

    if (res.ok) {
      const data = await res.json() as OAuthSessionData;
      log?.info?.("oauth_qr_session_received", {
        userDid: data.userDid,
        handle: data.handle,
      });
      return data;
    }

    if (res.status === 404 || res.status === 401) {
      // Not ready yet (404: no session stored; 401: session not yet posted, backend
      // can't verify the service auth JWT because no entry exists for this DID)
      log?.debug?.("oauth_qr_poll", { status: res.status });
      await res.body?.cancel();
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => {
          opts.signal?.removeEventListener("abort", onAbort);
          resolve();
        }, pollIntervalMs);
        const onAbort = () => {
          clearTimeout(timer);
          reject(opts.signal?.reason);
        };
        opts.signal?.addEventListener("abort", onAbort, { once: true });
      });
      continue;
    }

    // Unexpected error
    const body = await res.text().catch(() => "");
    throw new Error(`oauth-qr poll failed: ${res.status} ${body}`);
  }

  throw new Error("oauth-qr poll timed out waiting for session transfer");
}

// -- DPoP utilities (inline, same pattern as market-bidder-agent) ----------

interface DpopKey {
  bareJwk: Record<string, string>;
  algorithms?: string[];
  createJwt(header: Record<string, unknown>, payload: Record<string, unknown>): Promise<string>;
}

interface DpopNonceStore {
  get(key: string): Promise<string | undefined>;
  set(key: string, value: string): Promise<void>;
  del(key: string): Promise<void>;
}

function b64url(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function buildHtu_(url: string): string {
  const i = url.indexOf("?");
  const j = url.indexOf("#");
  const end = i === -1 ? j : j === -1 ? i : Math.min(i, j);
  return end === -1 ? url : url.slice(0, end);
}

async function sha256_(input: string): Promise<string> {
  const bytes = new TextEncoder().encode(input);
  const hash = await crypto.subtle.digest("SHA-256", bytes);
  return b64url(new Uint8Array(hash));
}

function createDpopNonceStore_(): DpopNonceStore {
  const map = new Map<string, string>();
  return {
    get: async (k) => map.get(k),
    set: async (k, v) => { map.set(k, v); },
    del: async (k) => { map.delete(k); },
  };
}

async function createDpopKey_(jwk: Record<string, string>, logger?: StructuredLoggerInterface): Promise<DpopKey> {
  // Strip browser-specific fields (key_ops, ext, use) that cause "Invalid key usage"
  const cleanJwk: Record<string, string> = {
    kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y,
  };
  if (jwk.d) cleanJwk.d = jwk.d;

  logger?.debug?.("dpop_key_import", {
    jwkKeys: Object.keys(jwk),
    cleanKeys: Object.keys(cleanJwk),
    jwkAlg: jwk.alg,
    jwkKty: jwk.kty,
    jwkCrv: jwk.crv,
    hasD: !!jwk.d,
    hasKeyOps: !!jwk.key_ops,
    hasExt: "ext" in jwk,
    hasUse: !!jwk.use,
  });

  const privateKey = await crypto.subtle.importKey(
    "jwk", cleanJwk, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"],
  );
  const bareJwk: Record<string, string> = { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y };
  if (jwk.kid) bareJwk.kid = jwk.kid;
  return {
    bareJwk,
    async createJwt(header, payload) {
      const headerB64 = b64url(new TextEncoder().encode(JSON.stringify(header)));
      const payloadB64 = b64url(new TextEncoder().encode(JSON.stringify(payload)));
      const signingInput = `${headerB64}.${payloadB64}`;
      const sig = await crypto.subtle.sign(
        { name: "ECDSA", hash: "SHA-256" },
        privateKey,
        new TextEncoder().encode(signingInput),
      );
      return `${signingInput}.${b64url(new Uint8Array(sig))}`;
    },
  };
}

function createDpopFetch_(opts: {
  key: DpopKey;
  nonces: DpopNonceStore;
  fetch?: typeof globalThis.fetch;
}): (url: string, init: RequestInit) => Promise<Response> {
  const { key, nonces } = opts;
  const fetcher = opts.fetch ?? globalThis.fetch;

  const dpopFetchOnce = async function dpopFetch(url: string, init: RequestInit): Promise<Response> {
    const authHdr = (init.headers as Record<string, string> | undefined)?.["Authorization"] ?? "";
    const ath = authHdr.startsWith("DPoP ")
      ? await sha256_(authHdr.slice(5))
      : undefined;

    const htm = init.method ?? "GET";
    const htu = buildHtu_(url);
    const origin = new URL(url).origin;

    let nonce: string | undefined;
    try { nonce = await nonces.get(origin); } catch { /* ignore */ }

    const proof = await key.createJwt(
      { typ: "dpop+jwt", alg: "ES256", jwk: key.bareJwk },
      { jti: crypto.randomUUID(), htm, htu, iat: Math.floor(Date.now() / 1000), ...(nonce ? { nonce } : {}), ...(ath ? { ath } : {}) },
    );

    const req = new Request(url, init);
    req.headers.set("DPoP", proof);

    let res = await fetcher(req);

    const nextNonce = res.headers.get("DPoP-Nonce");
    if (nextNonce && nextNonce !== nonce) {
      try { await nonces.set(origin, nextNonce); } catch { /* ignore */ }
      // Retry on use_dpop_nonce error
      if (res.status === 400 || res.status === 401) {
        try {
          const errBody = await res.clone().json().catch(() => null);
          if (errBody?.error === "use_dpop_nonce") {
            try { res.body?.cancel(); } catch { /* ignore */ }
            const retryProof = await key.createJwt(
              { typ: "dpop+jwt", alg: "ES256", jwk: key.bareJwk },
              { jti: crypto.randomUUID(), htm, htu, iat: Math.floor(Date.now() / 1000), nonce: nextNonce, ...(ath ? { ath } : {}) },
            );
            const retryReq = new Request(url, init);
            retryReq.headers.set("DPoP", retryProof);
            res = await fetcher(retryReq);
            const retryNonce = res.headers.get("DPoP-Nonce");
            if (retryNonce && retryNonce !== nextNonce) {
              try { await nonces.set(origin, retryNonce); } catch { /* ignore */ }
            }
          }
        } catch { /* clone failed, continue */ }
      }
    }

    return res;
  };

  return (url: string, init: RequestInit): Promise<Response> =>
    fetchWithRetry(() => dpopFetchOnce(url, init), {
      label: `${init.method ?? "GET"} ${new URL(url).pathname}`,
      ...(init.signal ? { signal: init.signal } : {}),
    });
}

function createTokenRefreshLock_() {
  let pending: Promise<unknown> | null = null;
  return function runLocked(fn: () => Promise<void>): Promise<void> {
    if (pending) return pending.then(() => runLocked(fn));
    pending = fn().finally(() => { pending = null; });
    return pending as Promise<void>;
  };
}

function decodeJwtExp(jwt: string): number | null {
  try {
    const parts = jwt.split(".");
    if (parts.length !== 3) return null;
    // JWT uses base64url (not standard base64). Convert to base64 for atob.
    const base64url = parts[1];
    const base64 = base64url.replace(/-/g, "+").replace(/_/g, "/");
    const payload = JSON.parse(atob(base64));
    return typeof payload.exp === "number" ? payload.exp * 1000 : null; // JWT exp is in seconds
  } catch {
    return null;
  }
}

interface OAuthAgentFromSessionOpts {
  logger?: StructuredLoggerInterface;
  /**
   * OAuth client_id to refresh as. A refresh token is issued to the client that
   * obtained it, so a session restored from a file must refresh as that same
   * client. Defaults to the qr.fedfork.com client that produced the QR sessions
   * this helper was written for.
   */
  clientId?: string;
  /** Persist refreshed tokens back to disk. */
  saveSession?: (session: OAuthSessionData) => Promise<void>;
  /** File path to the session JSON (used in error messages). */
  sessionPath?: string;
  /**
   * When set, a background timer proactively refreshes the access token before
   * it expires. Refresh fires when TTL drops below this threshold (ms).
   * Default recommendation: 3_600_000 (1 hour).
   */
  autoRefreshThresholdMs?: number;
  /**
   * Called when the refresh token is rejected (consumed/revoked). The session
   * is dead -- delete the file and re-authenticate.
   */
  onSessionExpired?: (err: OAuthSessionExpiredError) => void;
}

/**
 * Create an AtprotoAgentLike from a session transferred via QR code OAuth.
 * The DPoP private key (P-256) is imported from JWK. All PDS requests use
 * DPoP-bound access tokens. Token refresh is handled transparently.
 *
 * Pass saveSession to persist refreshed tokens back to disk so that a
 * restart does not replay an already-consumed refresh token.
 *
 * Pass autoRefreshThresholdMs to start a background keepalive timer that
 * proactively refreshes the token before it expires (prevents the refresh
 * token from going stale while the process is alive).
 */
export async function createOAuthAgentFromSession(
  sessionData: OAuthSessionData,
  opts?: OAuthAgentFromSessionOpts,
): Promise<AtprotoAgentLike & { sessionData: OAuthSessionData; dispose(): void; proactiveRefresh(): Promise<void> }> {
  const log = opts?.logger;
  const saveSession = opts?.saveSession;
  const clientId = opts?.clientId ?? "https://qr.fedfork.com/oauth-client-metadata.json";
  const sessionPath = opts?.sessionPath;
  const onSessionExpired = opts?.onSessionExpired;
  const nonces = createDpopNonceStore_();
  const key = await createDpopKey_(sessionData.dpopPrivateJwk, log);

  let accessJwt = sessionData.accessJwt;
  let refreshJwt = sessionData.refreshJwt;
  let dpopFetch = createDpopFetch_({ key, nonces });
  const refreshLock = createTokenRefreshLock_();
  let sessionExpired = false;
  let keepaliveTimer: ReturnType<typeof setInterval> | null = null;

  function checkSession(): void {
    if (sessionExpired) {
      const err = new OAuthSessionExpiredError(
        `OAuth session expired -- refresh token already consumed. Delete session file and re-authenticate: ${sessionPath ?? "unknown path"}`,
        sessionPath,
      );
      onSessionExpired?.(err);
      throw err;
    }
  }

  async function refreshTokens(): Promise<void> {
    checkSession();

    // Resolve PDS -> auth server for token endpoint
    const pdsUrl = sessionData.pds.replace(/\/+$/, "");
    const protRes = await fetch(`${pdsUrl}/.well-known/oauth-protected-resource`);
    if (!protRes.ok) throw new Error(`failed to fetch oauth-protected-resource: ${protRes.status}`);
    const protMeta = await protRes.json() as { authorization_servers?: string[] };
    const authServer = protMeta.authorization_servers?.[0];
    if (!authServer) throw new Error("no authorization server in protected resource metadata");

    // Fetch auth server metadata to get the actual token_endpoint URL.
    const authMetaRes = await fetch(`${authServer.replace(/\/+$/, "")}/.well-known/oauth-authorization-server`);
    const authMeta = await authMetaRes.json() as { token_endpoint?: string };
    const tokenUrl = authMeta.token_endpoint ?? `${authServer.replace(/\/+$/, "")}/token`;
    if (!authMeta.token_endpoint) log?.warn?.("token_endpoint_missing_from_auth_metadata", { authServer });

    const refreshDpopFetch = createDpopFetch_({ key, nonces });
    const body = new URLSearchParams({
      grant_type: "refresh_token",
      refresh_token: refreshJwt,
      client_id: clientId,
    });

    const res = await refreshDpopFetch(tokenUrl, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    });

    if (!res.ok) {
      const errText = await res.text().catch(() => "");
      const isInvalidGrant = errText.includes("invalid_grant") || errText.includes("Refresh token replayed");
      if (isInvalidGrant) {
        sessionExpired = true;
        log?.error?.("oauth_qr_session_expired", { sessionPath, error: errText });
        const expiredErr = new OAuthSessionExpiredError(
          `OAuth session expired -- refresh token already consumed. Delete session file and re-authenticate: ${sessionPath ?? "unknown path"}`,
          sessionPath,
        );
        onSessionExpired?.(expiredErr);
        throw expiredErr;
      }
      throw new Error(`token refresh failed: ${res.status} ${errText}`);
    }

    const data = await res.json() as { access_token: string; refresh_token?: string };
    accessJwt = data.access_token;
    if (data.refresh_token) refreshJwt = data.refresh_token;

    // Persist updated tokens so a restart won't replay the old refresh token.
    sessionData.accessJwt = accessJwt;
    if (data.refresh_token) sessionData.refreshJwt = refreshJwt;
    if (saveSession) {
      try { await saveSession(sessionData); } catch (e) { log?.warn?.("oauth_qr_session_save_failed", { error: String(e) }); }
    }

    // Rebuild dpopFetch with new key material (nonces reset)
    dpopFetch = createDpopFetch_({ key, nonces });
    log?.info?.("oauth_qr_tokens_refreshed", {});
  }

  // -- Background token keepalive --------------------------------------
  // Proactively refresh the access token when its TTL drops below the
  // threshold. This keeps the refresh token fresh and prevents it from
  // going stale while the process is alive.
  if (opts?.autoRefreshThresholdMs && opts.autoRefreshThresholdMs > 0) {
    const thresholdMs = opts.autoRefreshThresholdMs;
    const CHECK_INTERVAL_MS = 5 * 60 * 1000; // check every 5 minutes
    keepaliveTimer = setInterval(() => {
      if (sessionExpired) return;
      const exp = decodeJwtExp(accessJwt);
      if (exp === null) return;
      const remainingMs = exp - Date.now();
      if (remainingMs < thresholdMs) {
        log?.info?.("oauth_qr_background_refresh_triggered", {
          remainingMs: Math.round(remainingMs / 1000),
          thresholdMs: Math.round(thresholdMs / 1000),
        });
        refreshLock(() => refreshTokens()).catch((e) => {
          if (e instanceof OAuthSessionExpiredError) return; // onSessionExpired already called
          log?.warn?.("oauth_qr_background_refresh_failed", { error: String(e) });
        });
      }
    }, CHECK_INTERVAL_MS);
    Deno.unrefTimer?.(keepaliveTimer as unknown as number);
  }

  const _signer = {
    did: () => sessionData.userDid,
    sign: async () => { throw new Error("OAuth QR agent uses getServiceAuth, not local signing"); },
  };

  const agent: AtprotoAgentLike & { sessionData: OAuthSessionData; dispose(): void; proactiveRefresh(): Promise<void> } = {
    sessionData,
    get did() { return sessionData.userDid; },
    dispose() {
      if (keepaliveTimer !== null) {
        clearInterval(keepaliveTimer);
        keepaliveTimer = null;
      }
    },
    /** Force a token refresh to verify the refresh token is still valid. */
    async proactiveRefresh(): Promise<void> {
      return refreshLock(() => refreshTokens());
    },
    signer: _signer,

    async getServiceAuth(aud: string, lxm?: string): Promise<string> {
      const params = new URLSearchParams({ aud });
      if (lxm) params.set("lxm", lxm);
      const url = `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.server.getServiceAuth?${params}`;
      const doCall = async (): Promise<Response> => {
        return await dpopFetch(url, {
          method: "GET",
          headers: { Authorization: `DPoP ${accessJwt}` },
        });
      };
      // Same shape as the record ops: a token that expired since the last call
      // is recoverable, not a failed run. Without this, a token expiring
      // between the start burst and the accept or event burst fails the run
      // and skips vm.delete.
      let res = await doCall();
      if (res.status === 401) {
        await refreshLock(() => refreshTokens());
        res = await doCall();
      }
      if (!res.ok) {
        const errBody = await res.text().catch(() => "");
        throw new Error(`getServiceAuth failed: ${res.status} ${errBody}`);
      }
      const data = await res.json() as { token: string };
      return data.token;
    },

    async applyWrites(repo: string, writes: Parameters<AtprotoAgentLike["applyWrites"]>[1]) {
      const doCall = async (): Promise<Response> => {
        const res = await dpopFetch(
          `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.repo.applyWrites`,
          {
            method: "POST",
            headers: {
              "content-type": "application/json",
              Authorization: `DPoP ${accessJwt}`,
            },
            body: JSON.stringify({ repo, writes: writes.map((w) => {
              const base: Record<string, unknown> = {
                $type: `com.atproto.repo.applyWrites#${w.action}`, collection: w.collection, rkey: w.rkey,
              };
              if (w.action !== "delete") (base as Record<string, unknown>).value = (w as { record: unknown }).record;
              return base;
            }) }),
          },
        );
        return res;
      };

      let res = await doCall();
      if (res.status === 401) {
        await refreshLock(() => refreshTokens());
        res = await doCall();
      }
      if (!res.ok) throw new Error(`applyWrites failed: ${res.status} ${await res.text()}`);
      const data = await res.json() as { commit?: { rev?: string } };
      const rev = data.commit?.rev ?? "";
      return { repo, commit: rev, rev, since: null, blocks: new Uint8Array(), ops: [] } as unknown as CommitEvent;
    },

    async getRecord(repo: string, collection: string, rkey: string) {
      const params = new URLSearchParams({ repo, collection, rkey });
      const doCall = async (): Promise<Response> => dpopFetch(
        `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.repo.getRecord?${params.toString()}`,
        { headers: { Authorization: `DPoP ${accessJwt}` } },
      );
      let res = await doCall();
      if (res.status === 401) {
        await refreshLock(() => refreshTokens());
        res = await doCall();
      }
      if (!res.ok) {
        await res.body?.cancel();
        return null;
      }
      const data = await res.json() as { uri: string; cid?: string; value: Record<string, unknown> };
      return { uri: data.uri, cid: data.cid ?? "", value: data.value };
    },

    // Direct record CRUD -- bypasses applyWrites Lexicon validation on
    // remote PDSes that don't know our custom Lexicons.
    async createRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const doCall = async (): Promise<Response> => dpopFetch(
        `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.repo.createRecord`,
        { method: "POST", headers: { "content-type": "application/json", Authorization: `DPoP ${accessJwt}` }, body: JSON.stringify({ repo, collection, rkey, record }) },
      );
      let res = await doCall();
      if (res.status === 401) { await refreshLock(() => refreshTokens()); res = await doCall(); }
      if (!res.ok) throw new Error(`createRecord failed: ${res.status} ${await res.text()}`);
      const data = await res.json() as { uri: string; cid: string };
      return { uri: data.uri, cid: data.cid ?? "" };
    },

    async putRecord(repo: string, collection: string, rkey: string, record: Record<string, unknown>) {
      const doCall = async (): Promise<Response> => dpopFetch(
        `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.repo.putRecord`,
        { method: "POST", headers: { "content-type": "application/json", Authorization: `DPoP ${accessJwt}` }, body: JSON.stringify({ repo, collection, rkey, record }) },
      );
      let res = await doCall();
      if (res.status === 401) { await refreshLock(() => refreshTokens()); res = await doCall(); }
      if (!res.ok) throw new Error(`putRecord failed: ${res.status} ${await res.text()}`);
      const data = await res.json() as { uri: string; cid: string };
      return { uri: data.uri, cid: data.cid ?? "" };
    },

    async listRecords(repo: string, collection: string, opts?: { limit?: number }) {
      const all: Array<{ uri: string; cid: string; value: Record<string, unknown> }> = [];
      let cursor: string | undefined;
      // PDS caps listRecords at 100/page; paginate pages of ≤100 for a larger target.
      const target = opts?.limit ?? 100;
      const pageLimit = Math.min(target, 100);
      do {
        const params = new URLSearchParams({ repo, collection, limit: String(pageLimit) });
        if (cursor) params.set("cursor", cursor);
        const doCall = async (): Promise<Response> => dpopFetch(
          `${sessionData.pds.replace(/\/+$/, "")}/xrpc/com.atproto.repo.listRecords?${params.toString()}`,
          { headers: { Authorization: `DPoP ${accessJwt}` } },
        );
        let res = await doCall();
        if (res.status === 401) {
          await refreshLock(() => refreshTokens());
          res = await doCall();
        }
        if (!res.ok) break;
        const data = await res.json() as { records: Array<{ uri: string; cid?: string; value: unknown }>; cursor?: string };
        for (const r of data.records) {
          all.push({ uri: r.uri, cid: r.cid ?? "", value: r.value as Record<string, unknown> });
        }
        cursor = data.cursor;
      } while (cursor && all.length < target);
      return { records: all.slice(0, target) };
    },
  };

  return agent;
}
