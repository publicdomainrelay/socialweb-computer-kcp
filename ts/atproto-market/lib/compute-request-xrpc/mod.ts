import { Secp256k1Keypair } from "@atproto/crypto";
import { IdResolver, MemoryCache } from "@atproto/identity";
import { getPdsEndpoint } from "@atproto/common-web";
import {
  createRecordResolver,
  findRecord,
  getRecordAt,
  listRecordsAll,
  listRecordsPublic,
  publicGet,
  scanRecords,
  type ListedRecord,
  verifyRecordSignatures,
  verifyRemoteProof,
} from "@publicdomainrelay/market-atproto";
import { atUriAuthority, stripResolved } from "@publicdomainrelay/market-abc";
import { TtlMemo } from "@publicdomainrelay/ttl-memo-common";
import {
  ACCEPT_NSID,
  BID_NSID,
  COMPUTE_EVENTS_VM_DELETE_NSID,
  COMPUTE_EVENTS_VM_ONNETWORK_NSID,
  COMPUTE_EVENTS_VM_REGISTER_IDENTITY_NSID,
  COMPUTE_EVENTS_VM_TERMINATED_NSID,
  COMPUTE_EVENTS_VM_HOST_KEYS_NSID,
  COMPUTE_VM_NSID,
  EVENT_NSID,
  HEARTBEAT_INTERVAL_MS,
  HEARTBEAT_NSID,
  HEARTBEAT_RKEY,
  HEARTBEAT_TTL_MS,
  OFFERING_NSID,
  RECEIPT_NSID,
  RELAYS_NSID,
  RFP_NSID,
  SUBMIT_ACCEPT_LXM,
  SUBMIT_ACCEPT_NSID,
  SUBMIT_EVENT_LXM,
  SUBMIT_EVENT_NSID,
  SUBMIT_RFP_LXM,
  SUBMIT_RFP_NSID,
  VOUCH_NSID,
} from "@publicdomainrelay/market-common";
import { bidWindowSecOf, firstFreeOf } from "@publicdomainrelay/policy-engine-cli-options";
import {
  createPolicyEvaluator,
  resolvePolicyName,
} from "@publicdomainrelay/policy-engine-evaluator";
import type { PolicyEvaluator, ScopeCache } from "@publicdomainrelay/policy-engine-evaluator";
import type { ScopeRefresher } from "@publicdomainrelay/scope-refresh-timers";
import { DEFAULT_POLICY_CONCURRENCY, mapWithConcurrency } from "@publicdomainrelay/policy-common";
import type { PolicyArgs, PolicyRecord } from "@publicdomainrelay/policy-common";
import { WORKFLOWS } from "@publicdomainrelay/policies-gha-lite";
import { createPolicyRegistry } from "@publicdomainrelay/policy-deno-typescript";
import { GhaLiteExecutor } from "@publicdomainrelay/policy-engine-executor-gha-lite";
import { createRemoteEngineRegistry } from "@publicdomainrelay/policy-engine-client-http";
import { TypescriptExecutor } from "@publicdomainrelay/policy-engine-executor-typescript";
import {
  POLICY_GHA_LITE_NSID,
  POLICY_TYPESCRIPT_NSID,
  type PolicyResult,
} from "@publicdomainrelay/policy-engine-abc";
import { buildUserData, GUEST_TRANSPORT_MODULE, LegacyTransportError } from "@publicdomainrelay/cloud-init-common";
import type { CloudInitContext } from "@publicdomainrelay/cloud-init-common";
import { buildSshKeyRbacRecord, FEDPROXY_RBAC_NSID, rbacRecordKey } from "@publicdomainrelay/fedproxy-rbac-common";
import { deriveGrantVars, isWifSimpleConfig } from "@publicdomainrelay/guest-capability-abc";
import type {
  GuestCapability,
  GuestFetchedEvent,
  PrepareContext,
  WifSimpleConfig,
} from "@publicdomainrelay/guest-capability-abc";
import type {
  CollectedBid,
  ContractFlowOptions,
  RequesterPDS,
} from "@publicdomainrelay/requester-abc";
import { BidCollector, bidPayloadNsid, selectWinner } from "@publicdomainrelay/requester-abc";
import {
  advanceContract,
  CONTRACT_PHASES,
  createContractState,
  guestAddressKind,
  hasContract,
  isTerminated,
  patchContract,
  receiptKeyOf,
  releaseTargetOf,
  ReturnLatch,
  hostKeysOf,
  terminationOf,
} from "@publicdomainrelay/compute-request-abc";
import type {
  BidDecision,
  ComputeEventEnvelope,
  ContractHostKeys,
  ContractTermination,
  ComputeRequestHandlers,
  ContractOutcome,
  ContractReleaser,
  ContractState,
  ContractStatePatch,
  FlowDecision,
  NetworkEvent,
  RecordRef,
  WinnerReason,
} from "@publicdomainrelay/compute-request-abc";
import type { LoggerInterface, StructuredLoggerInterface } from "@publicdomainrelay/logger";
import {
  BADGE_BLUE_KEYS_NSID,
  BIDS_FREE_NSID,
  BIDDER_ASSOCIATION_NSID,
} from "@publicdomainrelay/market-lexicons";
import { createTangledGraphVouchResolver } from "@publicdomainrelay/trust-graph-tangled-graph";
import { createBadgeBlueKeysDelegatedTrustResolver } from "@publicdomainrelay/delegated-trust-badge-blue-keys";
import { createBadgeBlueKeysOperatorDiscovery, discoverOperatedDids } from "@publicdomainrelay/operator-discovery-badge-blue-keys";

type EventStreamsClient =
  import("@publicdomainrelay/atproto-event-streams-client").ATProtoEventStreamsClient;

export const DEFAULT_TRANSPORT_MODULE = GUEST_TRANSPORT_MODULE;

export function assertGuestTransport(transport: unknown): void {
  if (transport === undefined || transport === GUEST_TRANSPORT_MODULE) return;
  throw new LegacyTransportError(String(transport));
}
export const DEFAULT_VM_DISK = "50G";
export const DEFAULT_VM_READY_TIMEOUT_SEC = 120;
export const RECEIPT_FIREHOSE_TIMEOUT_MS = 30_000;
export const RECEIPT_REPO_ATTEMPTS = 5;
export const RECEIPT_REPO_RETRY_MS = 1_000;
export const BID_WINDOW_MS = 300_000;
export const ACCEPT_RESEND_MAX_MS = 120_000;
export const ACCEPT_RESEND_FIRST_DELAY_MS = 250;
export const ACCEPT_RESEND_MAX_DELAY_MS = 2_000;

export function acceptResendDeadline(bidCreatedAt: unknown, now: number): number {
  const created = typeof bidCreatedAt === "string" ? Date.parse(bidCreatedAt) : Number.NaN;
  const cap = now + ACCEPT_RESEND_MAX_MS;
  return Number.isFinite(created) ? Math.min(cap, created + BID_WINDOW_MS) : cap;
}

export const ACCEPT_IN_PROGRESS = "AcceptInProgress";

export function acceptRetryable(status: number, body: unknown): boolean {
  const error = (body as { error?: unknown } | null)?.error;
  if (error === ACCEPT_IN_PROGRESS) return true;
  if (status >= 500 || status === 408 || status === 429) return true;
  if (status === 404 && error === "NotFound") return true;
  return status >= 200 && status < 300;
}

export function defaultVmName(): string {
  const b = new Uint8Array(4);
  crypto.getRandomValues(b);
  return `compute-${Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("")}`;
}

export const RELAY_DISCOVERY_CACHE_MS = 180_000;
export const OFFERINGS_CACHE_MS = 120_000;
export const DID_DOC_STALE_MS = 60_000;
export const DID_DOC_MAX_MS = 300_000;

const relayDiscoveryMemo = new TtlMemo<string[]>({ ttlMs: RELAY_DISCOVERY_CACHE_MS });
const offeringsMemo = new TtlMemo<ListedRecord[]>({ ttlMs: OFFERINGS_CACHE_MS });
const idResolvers = new Map<string, IdResolver>();

export function sharedIdResolver(plcUrl?: string): IdResolver {
  const key = plcUrl ?? "";
  let resolver = idResolvers.get(key);
  if (!resolver) {
    resolver = new IdResolver({
      ...(plcUrl ? { plcUrl } : {}),
      didCache: new MemoryCache(DID_DOC_STALE_MS, DID_DOC_MAX_MS),
    });
    idResolvers.set(key, resolver);
  }
  return resolver;
}

export function clearRequesterReadCaches(): void {
  relayDiscoveryMemo.clear();
  offeringsMemo.clear();
  idResolvers.clear();
}

export function requesterReadCacheStats(): Record<string, unknown> {
  return { relayDiscovery: relayDiscoveryMemo.stats(), offerings: offeringsMemo.stats() };
}

function listOfferingsCached(pdsUrl: string, bidderDid: string): Promise<ListedRecord[]> {
  return offeringsMemo.get(
    `${pdsUrl}|${bidderDid}|${OFFERING_NSID}`,
    () => listRecordsAll(pdsUrl, bidderDid, OFFERING_NSID),
    (records) => records.length > 0 ? OFFERINGS_CACHE_MS : false,
  );
}

export async function discoverBiddersFromRelay(opts: {
  relayUrl: string;
  collection: string;
  log?: LoggerInterface;
  timeoutMs?: number;
}): Promise<string[]> {
  const { relayUrl, collection, log, timeoutMs } = opts;
  const key = `${relayUrl}|${collection}`;
  const cached = relayDiscoveryMemo.peek(key);
  if (cached) {
    log?.info("relay_discovery_cached", { relayUrl, collection, count: cached.length });
    return cached;
  }
  return await relayDiscoveryMemo.get(
    key,
    () => queryRelayForBidders({ relayUrl, collection, log, timeoutMs }),
    (dids) => dids.length > 0 ? RELAY_DISCOVERY_CACHE_MS : false,
  );
}

async function queryRelayForBidders(opts: {
  relayUrl: string;
  collection: string;
  log?: LoggerInterface;
  timeoutMs?: number;
}): Promise<string[]> {
  const { relayUrl, collection, log, timeoutMs } = opts;
  try {
    const url = `${
      relayUrl.replace(/\/+$/, "")
    }/xrpc/com.atproto.sync.listReposByCollection?collection=${
      encodeURIComponent(collection)
    }&limit=1000`;
    log?.info("relay_discovery_query", { url, collection });
    const res = await publicGet(url, timeoutMs ?? 10_000);
    if (!res.ok) {
      log?.warn("relay_discovery_http_error", { relayUrl, status: res.status, collection });
      return [];
    }
    const data = await res.json() as { repos?: Array<{ did: string }> };
    const dids = [...new Set((data.repos ?? []).map((r) => r.did).filter(Boolean))];
    log?.info("relay_discovery_result", { relayUrl, collection, count: dids.length });
    return dids;
  } catch (err) {
    log?.warn("relay_discovery_error", { relayUrl, collection, error: String(err) });
    return [];
  }
}

export async function discoverBiddersFromRelays(opts: {
  relayUrls: string[];
  collection: string;
  log?: LoggerInterface;
  timeoutMs?: number;
}): Promise<string[]> {
  const { relayUrls, collection, log, timeoutMs } = opts;
  if (relayUrls.length === 0) return [];
  const results = await Promise.all(
    relayUrls.map((url) => discoverBiddersFromRelay({ relayUrl: url, collection, log, timeoutMs })),
  );
  return [...new Set(results.flat())];
}

export async function autoDiscoverRelayUrls(opts: {
  atprotoDid?: string;
  log?: LoggerInterface;
}): Promise<string[]> {
  const did = opts.atprotoDid ?? Deno.env.get("ATPROTO_DID");
  if (!did) return [];
  const log = opts.log;
  log?.info("relay_autodiscover_lookup", { did });
  try {
    const resolver = new IdResolver();
    const doc = await resolver.did.resolve(did);
    if (!doc) {
      log?.warn("relay_autodiscover_did_unresolvable", { did });
      return [];
    }
    const pdsUrl = getPdsEndpoint(doc);
    if (!pdsUrl) {
      log?.warn("relay_autodiscover_no_pds", { did });
      return [];
    }
    const records = await listRecordsAll(pdsUrl, did, RELAYS_NSID);
    const urls: string[] = [];
    for (const r of records) {
      const relays = (r.value as Record<string, unknown>).relays;
      if (Array.isArray(relays)) {
        for (const item of relays) {
          if (
            typeof item === "string" && item.trim() &&
            (item.startsWith("https://") || item.startsWith("http://"))
          ) urls.push(item.trim());
        }
      }
    }
    const deduped = [...new Set(urls)];
    log?.info("relay_autodiscover_result", { did, sources: records.length, urls: deduped.length });
    return deduped;
  } catch (err) {
    log?.warn("relay_autodiscover_error", { did, error: String(err) });
    return [];
  }
}

export async function withDeadline<T>(
  what: string,
  ms: number,
  work: Promise<T>,
  onExpiry: (what: string, ms: number) => void,
): Promise<T | undefined> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      work,
      new Promise<undefined>((resolve) => {
        timer = setTimeout(() => {
          onExpiry(what, ms);
          resolve(undefined);
        }, ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

export type FlowLog = (event: string, extra?: Record<string, unknown>) => void;

function flowLog(logger?: StructuredLoggerInterface): FlowLog {
  return (event, extra = {}) =>
    logger ? logger.info(event, extra) : console.log(JSON.stringify({ event, ...extra }));
}

function bestEffort(work: () => Promise<void>): void {
  work().catch(() => undefined);
}

export interface CandidateScopeGateOptions {
  evaluator: PolicyEvaluator;
  selfDid: string;
  args: PolicyArgs;
  policyRecord?: PolicyRecord;
  ref?: RecordRef;
  refresher?: ScopeRefresher;
  log: FlowLog;
}

export function createCandidateScopeGate(
  opts: CandidateScopeGateOptions,
): (counterpartyDid: string) => Promise<PolicyResult> {
  const { evaluator, selfDid, args, policyRecord, ref, refresher, log } = opts;
  const target: { policyRecord: PolicyRecord } | { ref: RecordRef } | undefined = policyRecord
    ? { policyRecord }
    : ref
    ? { ref }
    : undefined;
  return async (counterpartyDid) => {
    if (!target) return { allow: true, violations: [] };
    const startedAt = Date.now();
    const result = await evaluator.scope({
      ...target,
      perspective: "requester",
      selfDid,
      counterpartyDid,
      args,
    });
    if (result.allow) {
      refresher?.noteUse({
        perspective: "requester",
        selfDid,
        counterpartyDid,
        args,
        ...target,
      });
    }
    log("candidate_scope", {
      bidderDid: counterpartyDid,
      allow: result.allow,
      durationMs: Date.now() - startedAt,
    });
    return result;
  };
}

function wireTrustInvalidation(
  eventStreams: EventStreamsClient | undefined,
  scopeCache: ScopeCache | undefined,
  refresher: ScopeRefresher | undefined,
  log: FlowLog,
): void {
  if (!eventStreams || !scopeCache) return;
  if (trustInvalidationWired.has(eventStreams)) return;
  trustInvalidationWired.add(eventStreams);
  eventStreams.watch({
    wantedCollections: [BADGE_BLUE_KEYS_NSID, VOUCH_NSID, BIDDER_ASSOCIATION_NSID],
    onEvent: (e) => {
      scopeCache.applyEvent({ did: e.did, rkey: e.rkey });
      refresher?.invalidate({ did: e.did, rkey: e.rkey });
    },
  });
  log("scope_invalidation_wired", {
    collections: [BADGE_BLUE_KEYS_NSID, VOUCH_NSID, BIDDER_ASSOCIATION_NSID],
  });
}

const trustInvalidationWired = new WeakSet<object>();

const HEARTBEAT_READ_TIMEOUT_MS = 5_000;
const HEARTBEAT_MAX_SKEW_MS = 60_000;
const HEARTBEAT_EVENT_DEBOUNCE_MS = 15_000;
const heartbeats = new Map<string, { atMs: number; checkedAtMs: number }>();
const heartbeatWatchWired = new WeakSet<object>();

async function readHeartbeat(idResolver: IdResolver, did: string): Promise<number> {
  const checkedAtMs = Date.now();
  let atMs = 0;
  try {
    const pdsUrl = await pdsUrlOf(idResolver, did);
    if (pdsUrl) {
      const res = await fetch(
        `${pdsUrl}/xrpc/com.atproto.repo.getRecord?repo=${encodeURIComponent(did)}&collection=${HEARTBEAT_NSID}&rkey=${HEARTBEAT_RKEY}`,
        { signal: AbortSignal.timeout(HEARTBEAT_READ_TIMEOUT_MS) },
      );
      if (res.ok) {
        const data = await res.json() as { value?: { createdAt?: unknown } };
        const t = Date.parse(String(data.value?.createdAt ?? ""));
        if (Number.isFinite(t) && t <= checkedAtMs + HEARTBEAT_MAX_SKEW_MS) atMs = t;
      } else {
        await res.body?.cancel();
      }
    }
  } catch {
    atMs = 0;
  }
  heartbeats.set(did, { atMs, checkedAtMs });
  return atMs;
}

function heartbeatFresh(atMs: number, now: number): boolean {
  return now - atMs <= HEARTBEAT_TTL_MS;
}

export async function freshBidderDids(idResolver: IdResolver, dids: string[]): Promise<string[]> {
  const now = Date.now();
  const fresh = await Promise.all(dids.map(async (did) => {
    const held = heartbeats.get(did);
    if (held && (heartbeatFresh(held.atMs, now) || now - held.checkedAtMs < HEARTBEAT_INTERVAL_MS)) {
      return heartbeatFresh(held.atMs, now);
    }
    return heartbeatFresh(await readHeartbeat(idResolver, did), now);
  }));
  return dids.filter((_, i) => fresh[i]);
}

function wireHeartbeatWatch(eventStreams: EventStreamsClient | undefined, idResolver: IdResolver, log: FlowLog): void {
  if (!eventStreams || heartbeatWatchWired.has(eventStreams)) return;
  heartbeatWatchWired.add(eventStreams);
  eventStreams.watch({
    wantedCollections: [HEARTBEAT_NSID],
    onEvent: (e) => {
      if (e.operation === "delete") {
        heartbeats.set(e.did, { atMs: 0, checkedAtMs: Date.now() });
        return;
      }
      const held = heartbeats.get(e.did);
      if (held && Date.now() - held.checkedAtMs < HEARTBEAT_EVENT_DEBOUNCE_MS) return;
      void readHeartbeat(idResolver, e.did);
    },
  });
  log("heartbeat_watch_wired", { collection: HEARTBEAT_NSID, ttlMs: HEARTBEAT_TTL_MS });
}

const POLICY_LIST_MEMO_MS = 15_000;

function nowIso(): string {
  return new Date().toISOString();
}

interface FetchedRecord {
  uri: string;
  cid: string;
  value: Record<string, unknown>;
}

async function pdsUrlOf(idResolver: IdResolver, did: string): Promise<string | undefined> {
  const doc = await idResolver.did.resolve(did);
  if (!doc) return undefined;
  return getPdsEndpoint(doc) ?? undefined;
}

async function getRecordFrom(
  pdsUrl: string,
  repo: string,
  collection: string,
  rkey: string,
): Promise<FetchedRecord | null> {
  const recordUrl = `${pdsUrl}/xrpc/com.atproto.repo.getRecord?repo=${
    encodeURIComponent(repo)
  }&collection=${encodeURIComponent(collection)}&rkey=${rkey}`;
  const res = await publicGet(recordUrl);
  const data = await res.json();
  const value = data.value as Record<string, unknown> | undefined;
  if (!value) return null;
  return { uri: data.uri as string, cid: data.cid as string, value };
}

interface FirehoseReceipt {
  receiptUri: string;
  receiptCid: string;
  submitEventRef?: string;
}

type OnNetworkPds = Pick<
  RequesterPDS,
  | "setOnNetworkResolved"
  | "clearOnNetworkResolved"
  | "resolveIrohNodeId"
  | "setOnTerminated"
  | "clearOnTerminated"
  | "setOnHostKeys"
  | "clearOnHostKeys"
>;

export type ComputeEventVerifier = (envelope: ComputeEventEnvelope) => Promise<boolean>;

export function createComputeEventVerifier(): ComputeEventVerifier {
  return async ({ uri, record }) =>
    await verifyRecordSignatures({
      record: stripResolved(record as never) as Record<string, unknown>,
      repositoryDid: atUriAuthority(uri),
    });
}

interface ContractWatch {
  readonly firehose: boolean;
  readonly address: Promise<NetworkEvent>;
  readonly terminated: Promise<ContractTermination>;
  readonly hostKeys: Promise<ContractHostKeys>;
  releaseNetwork(): void;
  setRfp(uri: string): void;
  setReceipt(receipt: RecordRef): void;
  bidsFor(rfpUri: string): CollectedBid[];
  receiptFor(acceptUri: string): FirehoseReceipt | undefined;
  backfill(winnerDid: string, until?: "network" | "terminated"): Promise<void>;
  close(): void;
}

function openContractWatch(opts: {
  pds: OnNetworkPds;
  eventStreams?: EventStreamsClient;
  idResolver: IdResolver;
  logger?: StructuredLoggerInterface;
  log: FlowLog;
  verifyEvent?: ComputeEventVerifier;
}): ContractWatch {
  const { pds, eventStreams, idResolver, logger, log } = opts;
  const verifyEvent = opts.verifyEvent ?? createComputeEventVerifier();
  const discoveredBids = new Map<string, CollectedBid[]>();
  const discoveredReceipts = new Map<string, FirehoseReceipt>();
  const contractOnNetworkUris = new Set<string>();
  const addressReady = Promise.withResolvers<NetworkEvent>();
  const terminatedReady = Promise.withResolvers<ContractTermination>();
  let terminatedSettled = false;
  let terminatedKey = "";
  const hostKeysReady = Promise.withResolvers<ContractHostKeys>();
  let hostKeysSettled = false;
  let hostKeysKey = "";
  let rfpUri = "";
  let receiptUri = "";
  let receiptCid = "";
  let vmAddress = "";
  let registeredKey = "";
  let watcher: { close(): void } | undefined;

  const resolveGuestAddress = (address: string, source: string) => {
    if (!address || vmAddress) return;
    const kind = guestAddressKind(address);
    if (kind === "refused") {
      log("vm_onnetwork_address_refused", { address, kind, source, reason: "not an iroh ticket" });
      return;
    }
    if (kind === "ip" || kind === "hostname") {
      log("vm_onnetwork_ip_skipped", { address, kind, source, hint: "waiting for the guest's iroh ticket" });
      return;
    }
    vmAddress = address;
    addressReady.resolve({ address, kind, source });
    log("vm_fqdn_discovered", { fqdn: address, kind, source });
  };

  const onNetworkPayload = async (pdsUrl: string, payloadUri: string, eventUri: string) => {
    const [, , payloadRepo, payloadColl] = payloadUri.split("/");
    const rkey = payloadUri.split("/").pop()!;
    const payload = await getRecordFrom(pdsUrl, payloadRepo, payloadColl, rkey);
    const address = payload?.value.address as string | undefined;
    if (address) resolveGuestAddress(address, `firehose:${eventUri}`);
  };

  const matchesReceipt = (receiptRef: { uri?: string; cid?: string } | undefined): boolean => {
    if (
      receiptRef?.uri && receiptRef.cid && receiptRef.uri === receiptUri &&
      receiptRef.cid === receiptCid
    ) {
      return true;
    }
    if (receiptRef?.uri && receiptRef.uri === receiptUri && receiptRef.cid !== receiptCid) {
      log("receipt_cid_mismatch", {
        receiptUri,
        eventCid: receiptRef.cid,
        contractCid: receiptCid,
      });
    }
    return false;
  };

  const onTerminatedEnvelope = async (envelope: ComputeEventEnvelope, source: string) => {
    if (terminatedSettled || !receiptUri) return;
    const bound = terminationOf(envelope, { uri: receiptUri, cid: receiptCid });
    if (!bound.ok) {
      log("vm_terminated_rejected", { eventUri: envelope.uri, source, reason: bound.reason });
      return;
    }
    let sigOk = false;
    try {
      sigOk = await verifyEvent(envelope);
    } catch (err) {
      log("vm_terminated_verify_error", { eventUri: envelope.uri, source, error: String(err) });
    }
    if (!sigOk) {
      log("vm_terminated_rejected", { eventUri: envelope.uri, source, reason: "signature invalid" });
      return;
    }
    if (terminatedSettled) return;
    terminatedSettled = true;
    const t = bound.termination;
    log("vm_terminated_by_bidder", {
      receiptUri,
      reason: t.reason,
      exitCode: t.exitCode,
      observedAt: t.observedAt,
      request: t.request?.uri,
      eventUri: t.event.uri,
      source,
    });
    terminatedReady.resolve(t);
  };

  const onHostKeysEnvelope = async (envelope: ComputeEventEnvelope, source: string) => {
    if (hostKeysSettled || !receiptUri) return;
    const bound = hostKeysOf(envelope, { uri: receiptUri, cid: receiptCid });
    if (!bound.ok) {
      log("vm_hostkeys_rejected", { eventUri: envelope.uri, source, reason: bound.reason });
      return;
    }
    let sigOk = false;
    try {
      sigOk = await verifyEvent(envelope);
    } catch (err) {
      log("vm_hostkeys_verify_error", { eventUri: envelope.uri, source, error: String(err) });
    }
    if (!sigOk) {
      log("vm_hostkeys_rejected", { eventUri: envelope.uri, source, reason: "signature invalid" });
      return;
    }
    if (hostKeysSettled) return;
    hostKeysSettled = true;
    log("vm_hostkeys_received", {
      receiptUri,
      fingerprints: bound.hostKeys.keys.map((k) => `${k.type} ${k.fingerprint}`),
      observedAt: bound.hostKeys.observedAt,
      eventUri: bound.hostKeys.event.uri,
      source,
    });
    hostKeysReady.resolve(bound.hostKeys);
  };

  const onWrappedEvent = async (pdsUrl: string, event: FetchedRecord) => {
    const wrappedReceipt = event.value.receipt as { uri?: string; cid?: string } | undefined;
    const wrappedPayload = event.value.payload as { uri?: string } | undefined;
    if (
      receiptUri && wrappedReceipt?.uri === receiptUri &&
      wrappedPayload?.uri?.split("/")[3] === COMPUTE_EVENTS_VM_TERMINATED_NSID
    ) {
      const [, , payloadRepo, payloadColl] = wrappedPayload.uri.split("/");
      const payload = await getRecordFrom(pdsUrl, payloadRepo, payloadColl, wrappedPayload.uri.split("/").pop()!);
      if (!payload) return;
      await onTerminatedEnvelope(
        { uri: event.uri, cid: event.cid, record: event.value, payload: payload.value },
        `firehose:${event.uri}`,
      );
      return;
    }
    if (
      receiptUri && wrappedReceipt?.uri === receiptUri &&
      wrappedPayload?.uri?.split("/")[3] === COMPUTE_EVENTS_VM_HOST_KEYS_NSID
    ) {
      const [, , payloadRepo, payloadColl] = wrappedPayload.uri.split("/");
      const payload = await getRecordFrom(pdsUrl, payloadRepo, payloadColl, wrappedPayload.uri.split("/").pop()!);
      if (!payload) return;
      await onHostKeysEnvelope(
        { uri: event.uri, cid: event.cid, record: event.value, payload: payload.value },
        `firehose:${event.uri}`,
      );
      return;
    }
    if (!matchesReceipt(wrappedReceipt)) return;
    const payloadRef = wrappedPayload;
    if (!payloadRef?.uri) return;
    if (payloadRef.uri.split("/")[3] !== COMPUTE_EVENTS_VM_ONNETWORK_NSID) return;
    contractOnNetworkUris.add(payloadRef.uri);
    log("firehose_onnetwork_wrapped", { eventUri: event.uri, payloadUri: payloadRef.uri });
    await onNetworkPayload(pdsUrl, payloadRef.uri, event.uri);
  };

  if (eventStreams) {
    const watched = [BID_NSID, EVENT_NSID, COMPUTE_EVENTS_VM_ONNETWORK_NSID, RECEIPT_NSID];
    watcher = eventStreams.watch({
      wantedCollections: watched,
      onEvent: (e) => {
        if (e.operation !== "create") return;
        if (!rfpUri && !receiptUri) return;
        if (e.collection === BID_NSID && rfpUri) {
          bestEffort(async () => {
            const pdsUrl = await pdsUrlOf(idResolver, e.did);
            if (!pdsUrl) return;
            const data = await getRecordFrom(pdsUrl, e.did, e.collection, e.rkey);
            if (!data) return;
            if ((data.value.rfp as { uri?: string } | undefined)?.uri !== rfpUri) return;
            const queue = discoveredBids.get(rfpUri) ?? [];
            queue.push({ did: e.did, uri: data.uri, cid: data.cid, record: data.value });
            discoveredBids.set(rfpUri, queue);
            log("firehose_bid_discovered", { bidUri: data.uri, rfpUri, bidderDid: e.did });
          });
        }
        if (e.collection === EVENT_NSID) {
          bestEffort(async () => {
            const pdsUrl = await pdsUrlOf(idResolver, e.did);
            if (!pdsUrl) return;
            const data = await getRecordFrom(pdsUrl, e.did, e.collection, e.rkey);
            if (!data) return;
            await onWrappedEvent(pdsUrl, data);
          });
        }
        if (e.collection === COMPUTE_EVENTS_VM_ONNETWORK_NSID) {
          log("firehose_onnetwork_raw", { uri: e.uri, did: e.did, rkey: e.rkey });
          if (!receiptUri || !contractOnNetworkUris.has(e.uri)) return;
          bestEffort(async () => {
            const pdsUrl = await pdsUrlOf(idResolver, e.did);
            if (!pdsUrl) return;
            const data = await getRecordFrom(pdsUrl, e.did, e.collection, e.rkey);
            const address = data?.value.address;
            if (typeof address === "string" && address) {
              resolveGuestAddress(address, `firehose:${data!.uri}`);
            }
          });
        }
        if (e.collection === COMPUTE_EVENTS_VM_REGISTER_IDENTITY_NSID) {
          bestEffort(async () => {
            const pdsUrl = await pdsUrlOf(idResolver, e.did);
            if (!pdsUrl) return;
            const data = await getRecordFrom(pdsUrl, e.did, e.collection, e.rkey);
            const identity = data?.value.computeIdentity as Record<string, unknown> | undefined;
            if (identity?.nodeId) {
              log("iroh_node_id_firehose", { nodeId: identity.nodeId });
              pds.resolveIrohNodeId?.(String(identity.nodeId));
            }
          });
        }
        if (e.collection === RECEIPT_NSID) {
          bestEffort(async () => {
            const pdsUrl = await pdsUrlOf(idResolver, e.did);
            if (!pdsUrl) return;
            const rec = await getRecordAt(pdsUrl, e.uri, { timeoutMs: 10_000 });
            if (!rec) return;
            const val = rec.value as Record<string, unknown>;
            const acceptRef = val.accept as { uri?: string } | undefined;
            if (!acceptRef?.uri) return;
            discoveredReceipts.set(acceptRef.uri, {
              receiptUri: rec.uri,
              receiptCid: rec.cid || e.cid,
              submitEventRef: val.submitEvent as string | undefined,
            });
          });
        }
      },
      log: logger,
    });
    log("firehose_market_watch_started", {
      relayCount: eventStreams.relays.length,
      jetstreamCount: eventStreams.jetstreams.length,
      collections: watched,
    });
  }

  const clearRegistration = () => {
    if (registeredKey) pds.clearOnNetworkResolved?.(registeredKey);
    registeredKey = "";
  };

  const clearTerminated = () => {
    if (terminatedKey) pds.clearOnTerminated?.(terminatedKey);
    terminatedKey = "";
  };

  const clearHostKeys = () => {
    if (hostKeysKey) pds.clearOnHostKeys?.(hostKeysKey);
    hostKeysKey = "";
  };

  return {
    firehose: watcher !== undefined,
    address: addressReady.promise,
    terminated: terminatedReady.promise,
    hostKeys: hostKeysReady.promise,
    releaseNetwork() {
      clearRegistration();
    },
    setRfp(uri) {
      rfpUri = uri;
    },
    setReceipt(receipt) {
      receiptUri = receipt.uri;
      receiptCid = receipt.cid;
      if (!receiptUri || !receiptCid) return;
      clearRegistration();
      registeredKey = receiptKeyOf(receipt);
      pds.setOnNetworkResolved?.(
        registeredKey,
        (address) => resolveGuestAddress(address, "submitEvent"),
      );
      clearTerminated();
      terminatedKey = receiptUri;
      pds.setOnTerminated?.(
        terminatedKey,
        (envelope) => void onTerminatedEnvelope(envelope, "submitEvent"),
      );
      clearHostKeys();
      hostKeysKey = receiptUri;
      pds.setOnHostKeys?.(
        hostKeysKey,
        (envelope) => void onHostKeysEnvelope(envelope, "submitEvent"),
      );
    },
    bidsFor(uri) {
      return discoveredBids.get(uri) ?? [];
    },
    receiptFor(acceptUri) {
      return discoveredReceipts.get(acceptUri);
    },
    async backfill(winnerDid, until = "network") {
      try {
        const pdsUrl = await pdsUrlOf(idResolver, winnerDid);
        if (!pdsUrl) return;
        const settled = () => terminatedSettled || (vmAddress && hostKeysSettled && until === "network");
        await scanRecords(pdsUrl, winnerDid, EVENT_NSID, async (event) => {
          if (settled()) return true;
          await onWrappedEvent(pdsUrl, event as FetchedRecord);
          return !!settled();
        }, { timeoutMs: 10_000 });
      } catch (err) {
        log("onnetwork_backfill_failed", { winnerDid, error: String(err) });
      }
    },
    close() {
      clearRegistration();
      clearTerminated();
      clearHostKeys();
      watcher?.close();
      watcher = undefined;
    },
  };
}

type AddressWait =
  | { kind: "network"; network: NetworkEvent }
  | { kind: "terminated"; termination: ContractTermination };

async function waitForAddress(
  watch: ContractWatch,
  timeoutMs: number,
  latch?: ReturnLatch,
): Promise<AddressWait | null> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      watch.address.then((network): AddressWait => ({ kind: "network", network })),
      watch.terminated.then((termination): AddressWait => ({ kind: "terminated", termination })),
      new Promise<null>((resolve) => {
        timer = setTimeout(() => resolve(null), timeoutMs);
      }),
      ...(latch ? [latch.returned.then(() => null)] : []),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

export interface ReceiptVerification {
  ok: boolean;
  sigOk?: boolean;
  bindOk?: boolean;
}

export type ReceiptVerifier = (
  refs: { receipt: RecordRef; accept: RecordRef },
) => Promise<ReceiptVerification>;

export type RecordResolver = (ref: RecordRef) => Promise<Record<string, unknown>>;

export function createIdRecordResolver(idResolver: IdResolver): RecordResolver {
  return async (ref) =>
    await createRecordResolver(idResolver).resolve({ uri: ref.uri, cid: ref.cid } as never);
}

export function createReceiptVerifier(resolveRecord: RecordResolver): ReceiptVerifier {
  return async ({ receipt: receiptRef, accept: acceptRef }) => {
    const receipt = await resolveRecord({ uri: receiptRef.uri, cid: receiptRef.cid });
    const accept = await resolveRecord({ uri: acceptRef.uri, cid: acceptRef.cid });
    const receiptBare = stripResolved(receipt as never) as Record<string, unknown>;
    const sigOk = await verifyRecordSignatures({
      record: receiptBare,
      repositoryDid: atUriAuthority(receiptRef.uri),
    });
    const bindOk = verifyRemoteProof({
      subjectRecord: stripResolved(accept as never) as Record<string, unknown>,
      subjectRepositoryDid: atUriAuthority(acceptRef.uri),
      proofRecord: receiptBare,
    });
    return { ok: sigOk && bindOk, sigOk, bindOk };
  };
}

export type ComputeRequestOptions =
  & Omit<
    ContractFlowOptions,
    | "execProgram"
    | "keepVm"
    | "hold"
    | "holdAbort"
    | "onSshStart"
    | "onSshEnd"
    | "sshProxyCommandFn"
  >
  & {
    relayUrl?: string;
    returnSignal?: AbortSignal;
    acceptCopies?: number;
    onRfpAnswer?: (answer: { bidderDid: string; status: number; error?: string }) => void;
    offeringWatcherDids?: () => string[];
    logger?: StructuredLoggerInterface;
    payloadFactory?: () => Promise<{ uri: string; cid: string }>;
    vmDisk?: string;
    eventStreams?: EventStreamsClient;
    scopeCache?: ScopeCache;
    scopeRefresher?: ScopeRefresher;
    sshPublicKey?: string;
    receiptVerifier?: ReceiptVerifier;
    recordResolver?: RecordResolver;
    receiptFinder?: ReceiptFinder;
    eventVerifier?: ComputeEventVerifier;
    resume?: ContractState;
    bidFinder?: BidFinder;
    acceptFinder?: AcceptFinder;
  };

export type BidFinder = (bidderDid: string, rfpUri: string) => Promise<CollectedBid[]>;

export interface FoundAccept {
  accept: RecordRef;
  bid: RecordRef;
}

export type AcceptFinder = (rfpUri: string) => Promise<FoundAccept | null>;

export function createRepoBidFinder(idResolver: IdResolver): BidFinder {
  return async (bidderDid, rfpUri) => {
    const pdsUrl = await pdsUrlOf(idResolver, bidderDid);
    if (!pdsUrl) return [];
    const found: CollectedBid[] = [];
    for (const collection of [BID_NSID, BIDS_FREE_NSID]) {
      const rec = await findRecord(
        pdsUrl,
        bidderDid,
        collection,
        (r) => (r.value.rfp as { uri?: string } | undefined)?.uri === rfpUri && !!r.uri && !!r.cid,
        { timeoutMs: 10_000 },
      );
      if (rec) found.push({ did: bidderDid, uri: rec.uri, cid: rec.cid, record: rec.value });
    }
    return found;
  };
}

export interface FoundReceipt {
  uri: string;
  cid: string;
  submitEvent?: string;
}

export type ReceiptFinder = (bidderDid: string, acceptUri: string) => Promise<FoundReceipt | null>;

export function createRepoReceiptFinder(idResolver: IdResolver): ReceiptFinder {
  return async (bidderDid, acceptUri) => {
    const pdsUrl = await pdsUrlOf(idResolver, bidderDid);
    if (!pdsUrl) return null;
    const rec = await findRecord(
      pdsUrl,
      bidderDid,
      RECEIPT_NSID,
      (r) => (r.value.accept as { uri?: string } | undefined)?.uri === acceptUri && !!r.uri && !!r.cid,
      { timeoutMs: 10_000 },
    );
    if (!rec) return null;
    return {
      uri: rec.uri,
      cid: rec.cid,
      ...(typeof rec.value.submitEvent === "string" ? { submitEvent: rec.value.submitEvent } : {}),
    };
  };
}

export class ComputeRequestFailed extends Error {
  constructor(readonly state: ContractState, readonly reason: unknown) {
    super(reason instanceof Error ? reason.message : String(reason));
  }
}

export interface ComputeRequestHandle {
  state: ContractState;
  capabilities: GuestCapability[];
  dispose(): Promise<void>;
  terminated?: Promise<ContractState>;
  hostKeys?: Promise<ContractHostKeys>;
}

type HandlerName = keyof ComputeRequestHandlers;

async function disposeAll(capabilities: GuestCapability[], log: FlowLog): Promise<void> {
  for (const cap of capabilities) {
    try {
      await cap.dispose?.();
    } catch (err) {
      log("capability_dispose_failed", { capability: cap.id, error: String(err) });
    }
  }
}

function capabilityHandle(
  state: ContractState,
  capabilities: GuestCapability[],
  log: FlowLog,
  extra: { terminated?: Promise<ContractState>; hostKeys?: Promise<ContractHostKeys>; close?: () => void } = {},
): ComputeRequestHandle {
  let disposed: Promise<void> | undefined;
  return {
    state,
    capabilities,
    dispose: () =>
      disposed ??= (async () => {
        extra.close?.();
        await disposeAll(capabilities, log);
      })(),
    ...(extra.terminated ? { terminated: extra.terminated } : {}),
    ...(extra.hostKeys ? { hostKeys: extra.hostKeys } : {}),
  };
}

export function flowViewOf(state: ContractState): Record<string, unknown> {
  return {
    vmUri: state.payload?.uri,
    vmCid: state.payload?.cid,
    rfpUri: state.rfp?.uri,
    rfpCid: state.rfp?.cid,
    acceptUri: state.accept?.uri,
    acceptCid: state.accept?.cid,
    bidUri: state.bid?.uri,
    bidCid: state.bid?.cid,
    winnerDid: state.winner?.did,
    receiptUri: state.receipt?.uri,
    receiptCid: state.receipt?.cid,
    submitEventRef: state.submitEventRef,
    receiptOk: state.receiptOk ?? false,
    bids: state.bids,
    ...(state.termination
      ? {
        terminated: {
          reason: state.termination.reason,
          ...(state.termination.exitCode !== undefined ? { exitCode: state.termination.exitCode } : {}),
          observedAt: state.termination.observedAt,
        },
      }
      : {}),
  };
}

export async function requestCompute(
  pds: RequesterPDS,
  opts: ComputeRequestOptions = {},
  handlers: ComputeRequestHandlers = {},
): Promise<ComputeRequestHandle> {
  const resume = opts.resume;
  const vmName = resume?.vmName ?? opts.vmName ?? defaultVmName();
  const policySpec = opts.policy;
  const policyArgs = policySpec?.args ?? {};
  const bidWindowSec = bidWindowSecOf(policyArgs);
  const firstFree = firstFreeOf(policyArgs);
  const skipSsh = opts.skipSsh ?? false;
  const vmReadyTimeoutSec = opts.vmReadyTimeoutSec ?? DEFAULT_VM_READY_TIMEOUT_SEC;
  const extraBidderDids = opts.extraBidderDids ?? [];
  const denyBidderDids = opts.denyBidderDids ?? [];
  const capabilities = opts.capabilities ?? [];
  const relayUrls = opts.relayUrls ?? (opts.relayUrl ? [opts.relayUrl] : []);
  assertGuestTransport(opts.userData?.transport);
  const transport = DEFAULT_TRANSPORT_MODULE;
  const logger = opts.logger;
  const log = flowLog(logger);
  const scopeCache = opts.scopeCache;
  const scopeRefresher = opts.scopeRefresher;
  wireTrustInvalidation(opts.eventStreams, scopeCache, scopeRefresher, log);
  const idResolver = sharedIdResolver(opts.plcUrl);
  wireHeartbeatWatch(opts.eventStreams, idResolver, log);
  const resolveRecord = opts.recordResolver ?? createIdRecordResolver(idResolver);
  const verifyReceipt = opts.receiptVerifier ?? createReceiptVerifier(resolveRecord);
  const findReceipt = opts.receiptFinder ?? createRepoReceiptFinder(idResolver);
  const findBids = opts.bidFinder ?? createRepoBidFinder(idResolver);

  let capKeypair: Promise<Secp256k1Keypair> | null = null;
  const capabilitySigner: PrepareContext["signer"] = {
    did: () => pds.did,
    sign: async (bytes: Uint8Array) => {
      capKeypair ??= Secp256k1Keypair.import(pds.privateKeyHex);
      return await (await capKeypair).sign(bytes);
    },
  };

  const marketDid =
    (pds as unknown as { oauthSession?: { userDid?: string } }).oauthSession?.userDid ??
      pds.did;
  const ownApi = (pds as unknown as {
    api: { listRecords(repo: string, coll: string): Promise<{ records?: unknown } | undefined> };
  }).api;
  const oauthAgent = (pds as unknown as Record<string, unknown>).oauthAgent as
    | {
      listRecords(
        did: string,
        collection: string,
        opts?: { limit?: number },
      ): Promise<{ records: Array<{ uri: string; cid?: string; value: Record<string, unknown> }> }>;
    }
    | undefined;

  const findAccept: AcceptFinder = opts.acceptFinder ?? (async (rfpUri) => {
    const listed = oauthAgent && marketDid !== pds.did
      ? await oauthAgent.listRecords(marketDid, ACCEPT_NSID, { limit: 100 })
      : await ownApi.listRecords(marketDid, ACCEPT_NSID);
    const records = (listed?.records ?? []) as Array<{ uri: string; cid?: string; value: Record<string, unknown> }>;
    for (const r of records) {
      const rfp = r.value.rfp as { uri?: string } | undefined;
      const bid = r.value.bid as { uri?: string; cid?: string } | undefined;
      if (rfp?.uri !== rfpUri || !bid?.uri || !bid.cid || !r.cid) continue;
      return { accept: { uri: r.uri, cid: r.cid }, bid: { uri: bid.uri, cid: bid.cid } };
    }
    return null;
  });

  let state = resume
    ? patchContract(structuredClone(resume), { outcome: undefined, error: undefined })
    : createContractState({ vmName, requesterDid: pds.did, marketDid, at: nowIso() });
  const latch = new ReturnLatch();
  let ended = false;
  if (opts.returnSignal?.aborted) latch.request("return_requested");
  else opts.returnSignal?.addEventListener("abort", () => latch.request("return_requested"), { once: true });

  const advance = (phase: ContractState["phase"], patch: ContractStatePatch) => {
    if (CONTRACT_PHASES.indexOf(phase) < CONTRACT_PHASES.indexOf(state.phase)) {
      state = patchContract(state, patch);
      return;
    }
    state = advanceContract(state, phase, patch, state.timestamps[phase] ?? nowIso());
  };

  const runHandler = async <E, D extends FlowDecision | BidDecision>(
    name: HandlerName,
    fn: ((event: E, state: ContractState) => D | Promise<D>) | undefined,
    event: E,
  ): Promise<D | undefined> => {
    if (!fn) return undefined;
    try {
      return await fn(event, structuredClone(state));
    } catch (err) {
      log("compute_handler_failed", { handler: name, error: String(err) });
      state = patchContract(state, { error: `${name} failed: ${String(err)}` });
      latch.request(`${name}_failed`);
      return undefined;
    }
  };

  const decideFlow = async <E>(
    name: HandlerName,
    fn: ((event: E, state: ContractState) => FlowDecision | Promise<FlowDecision>) | undefined,
    event: E,
  ): Promise<boolean> => {
    const decision = await runHandler(name, fn, event);
    return latch.apply(decision, name);
  };

  const onGuestFetched = (event: GuestFetchedEvent) => {
    void (async () => {
      const decision = await runHandler("onSecretsFetched", handlers.onSecretsFetched, event);
      if (!ended) latch.apply(decision, "onSecretsFetched");
    })();
  };

  const watch = openContractWatch({
    pds,
    eventStreams: opts.eventStreams,
    idResolver,
    logger,
    log,
    verifyEvent: opts.eventVerifier,
  });

  const terminatedState = Promise.withResolvers<ContractState>();
  void watch.terminated.then(async (termination) => {
    state = patchContract(state, { termination });
    latch.request("bidder_terminated");
    try {
      await handlers.onTerminated?.(termination, structuredClone(state));
    } catch (err) {
      log("compute_handler_failed", { handler: "onTerminated", error: String(err) });
    }
    terminatedState.resolve(structuredClone(state));
  });

  const hostKeysState = watch.hostKeys.then(async (hostKeys) => {
    state = patchContract(state, { hostKeys });
    try {
      await handlers.onHostKeys?.(hostKeys, structuredClone(state));
    } catch (err) {
      log("compute_handler_failed", { handler: "onHostKeys", error: String(err) });
    }
    return hostKeys;
  });

  const finish = async (
    outcome: ContractOutcome,
    patch: ContractStatePatch = {},
  ): Promise<ComputeRequestHandle> => {
    ended = true;
    if (isTerminated(state)) await terminatedState.promise;
    state = patchContract(state, { ...patch, outcome });
    if (outcome === "returned") {
      log("compute_request_returned", { phase: state.phase, by: latch.reason });
    }
    if (!hasContract(state)) {
      watch.close();
      await disposeAll(capabilities, log);
      return capabilityHandle(structuredClone(state), [], log);
    }
    if (isTerminated(state)) {
      watch.close();
      return capabilityHandle(structuredClone(state), capabilities, log, {
        terminated: terminatedState.promise,
      });
    }
    watch.releaseNetwork();
    return capabilityHandle(structuredClone(state), capabilities, log, {
      terminated: terminatedState.promise,
      hostKeys: hostKeysState,
      close: () => watch.close(),
    });
  };

  try {
    const ingressRef = pds.relay.ingressRef;
    const ingressProxyHost = opts.ingressProxyHost ??
      (pds.relaySubdomain.includes(".")
        ? pds.relaySubdomain.substring(pds.relaySubdomain.indexOf(".") + 1)
        : "xrpc.fedproxy.com");
    log("relay_ready_for_rfp", { ingressRef });

    let cloudInit: string;
    if (!skipSsh) {
      const capCtx: Partial<CloudInitContext> = {};
      for (const cap of capabilities) {
        const prepared = await cap.prepare?.({
          vmName,
          requesterDid: marketDid,
          ingressProxyHost,
          signer: capabilitySigner,
          log: (event, extra) => log(event, extra ?? {}),
          onGuestFetched,
        });
        Object.assign(capCtx, prepared?.ctx ?? {});
      }
      const ud = opts.userData;
      cloudInit = buildUserData({
        ctx: {
          vmName,
          hostAliases: opts.guestHostAliases,
          sshAuthorizedKey: [opts.sshPublicKey, ...(opts.sshAuthorizedKeys ?? [])]
            .filter((k): k is string => !!k)
            .join("\n"),
          ...capCtx,
        },
        base: ud?.base ?? opts.baseUserData,
        modules: [
          transport,
          ...(ud?.modules ?? []),
          ...capabilities.flatMap((c) => c.userDataModule ? [c.userDataModule] : []),
        ],
        overrides: ud?.overrides,
      });
      if (opts.userDataFactory) {
        cloudInit = opts.userDataFactory(opts.sshPublicKey ?? "");
      }
    } else {
      cloudInit = `#cloud-config
runcmd:
  - echo "test VM (no sshd) ready" | tee /tmp/ready
`;
    }

    const payload: RecordRef = resume?.payload
      ? resume.payload
      : opts.payloadFactory
      ? await opts.payloadFactory()
      : await pds.createRepoRecord(COMPUTE_VM_NSID, {
        $type: COMPUTE_VM_NSID,
        role: vmName.trim() || "compute",
        disk: opts.vmDisk ?? DEFAULT_VM_DISK,
        user_data: cloudInit,
        createdAt: nowIso(),
      });
    state = patchContract(state, { payload: { uri: payload.uri, cid: payload.cid } });
    log(resume?.payload ? "vm_record_resumed" : "vm_record_created", { uri: payload.uri, cid: payload.cid });

    const rfpRecord: Record<string, unknown> = {
      $type: RFP_NSID,
      domain: "compute",
      payload: { $type: "com.atproto.repo.strongRef", uri: payload.uri, cid: payload.cid },
      submitBid: `${pds.did}#pdr_temp_market`,
      createdAt: nowIso(),
    };

    const resolveRecordForPolicy = (
      ref: { uri: string; cid: string },
    ): Promise<Record<string, unknown>> => resolveRecord({ uri: ref.uri, cid: ref.cid });

    type PolicyListed = Array<{ uri: string; value: Record<string, unknown> }>;
    const policyListMemo = new Map<string, { at: number; read: Promise<PolicyListed> }>();
    const listRecordsForPolicy = (repo: string, coll: string): Promise<PolicyListed> => {
      const key = `${repo}|${coll}`;
      const memo = policyListMemo.get(key);
      if (memo && Date.now() - memo.at < POLICY_LIST_MEMO_MS) return memo.read;
      const read = readRecordsForPolicy(repo, coll);
      policyListMemo.set(key, { at: Date.now(), read });
      read.then(
        () => undefined,
        () => {
          if (policyListMemo.get(key)?.read === read) policyListMemo.delete(key);
        },
      );
      return read;
    };
    const readRecordsForPolicy = async (repo: string, coll: string): Promise<PolicyListed> => {
      type Listed = PolicyListed;
      const ownReads: Array<Promise<Listed | undefined>> = repo === marketDid
        ? [
          (async () => (await oauthAgent?.listRecords(repo, coll, { limit: 100 }))?.records)(),
          (async () => (await ownApi.listRecords(repo, coll))?.records as Listed | undefined)(),
        ]
        : [];
      const [publicRead, ...ownSettled] = await Promise.allSettled([
        listRecordsPublic(idResolver, repo, coll),
        ...ownReads,
      ]);
      const merged = new Map<string, { uri: string; value: Record<string, unknown> }>();
      for (const read of ownSettled) {
        if (read.status !== "fulfilled") continue;
        for (const r of read.value ?? []) merged.set(r.uri, r);
      }
      if (publicRead.status === "fulfilled") {
        for (const r of publicRead.value) merged.set(r.uri, r);
      } else {
        log("policy_public_records_failed", {
          repo,
          collection: coll,
          error: String(publicRead.reason),
        });
      }
      return [...merged.values()];
    };

    const operatorDiscovery = createBadgeBlueKeysOperatorDiscovery({
      listRecordsOwn: (collection) => listRecordsForPolicy(marketDid, collection),
      listRecordsPublic: (repo, collection) => listRecordsPublic(idResolver, repo, collection),
      log: (level, msg, meta) => log(`operator_discovery_${level}`, { msg, ...(meta ?? {}) }),
    });

    const resolveOperatorDid = async (bidderDid: string): Promise<string | null> => {
      try {
        const opDids = await operatorDiscovery.discoverOperatorDids(bidderDid);
        if (opDids.length > 0) return opDids[0];
      } catch (err) {
        log("operator_did_resolution_failed", { bidderDid, error: String(err) });
      }
      return bidderDid;
    };

    const policyVouchResolver = createBadgeBlueKeysDelegatedTrustResolver({
      vouchResolver: createTangledGraphVouchResolver({
        listRecords: listRecordsForPolicy,
        log: (level, msg, meta) => log(`policy_vouch_${level}`, { msg, ...(meta ?? {}) }),
      }),
      listOwnRecords: (collection, _opts) => listRecordsForPolicy(marketDid, collection),
      log: (level, msg, meta) => log(`policy_vouch_${level}`, { msg, ...(meta ?? {}) }),
    });

    const policyRegistry = createPolicyRegistry();
    const ghaLiteExecutor = new GhaLiteExecutor({ sandbox: { netOnly: true, fsApi: false } });
    const typescriptExecutor = new TypescriptExecutor();
    const attachedRegistry = opts.policyEngineUrl
      ? createRemoteEngineRegistry({ baseUrl: opts.policyEngineUrl })
      : undefined;
    const evaluator = createPolicyEvaluator({
      ...(attachedRegistry ? { attachedRegistry } : {}),
      registry: {
        get: ($t: string) =>
          $t === POLICY_GHA_LITE_NSID
            ? ghaLiteExecutor
            : $t === POLICY_TYPESCRIPT_NSID
            ? typescriptExecutor
            : undefined,
        kinds: () => [POLICY_GHA_LITE_NSID, POLICY_TYPESCRIPT_NSID],
      },
      resolve: (ref) => resolveRecordForPolicy(ref),
      resolveOperatorDid: (did) => resolveOperatorDid(did),
      getVouchedDids: (did) => policyVouchResolver.getDelegatedTrustedDids(did),
      policies: policyRegistry,
      scopeCache,
      log: (level, msg, meta) => log(`policy_eval_${level}`, { msg, ...(meta ?? {}) }),
    });
    scopeRefresher?.setEvaluator(evaluator);

    let policyRef: RecordRef | undefined;
    let scopeRecord: PolicyRecord | undefined;
    if (policySpec) {
      try {
        const canonical = resolvePolicyName(policyRegistry, policySpec.name, "requester");
        const workflow = WORKFLOWS[canonical];
        scopeRecord = workflow
          ? {
            uri: `at://${marketDid}/policy-gha-lite/${canonical}`,
            cid: canonical,
            value: {
              $type: POLICY_GHA_LITE_NSID,
              name: canonical,
              workflow,
              createdAt: nowIso(),
            },
          }
          : undefined;
        const { nsid, record } = evaluator.buildPolicyRecord({
          name: policySpec.name,
          description: policySpec.description,
          args: policyArgs,
          requesterDid: marketDid,
          perspective: "requester",
          kind: workflow ? "gha-lite" : "typescript",
          workflow,
          policies: [{ name: canonical, args: policyArgs }],
        });
        if (resume?.rfp) {
          policyRef = resume.policy;
        } else {
          policyRef = await pds.createRepoRecord(nsid, record);
          rfpRecord.policies = [{
            $type: "com.atproto.repo.strongRef",
            uri: policyRef.uri,
            cid: policyRef.cid,
          }];
          log("policy_attached", {
            policy: policySpec.name,
            policyNsid: nsid,
            policyUris: [policyRef.uri],
          });
        }
      } catch (err) {
        log("policy_create_error", { error: String(err) });
      }
    }

    const scopeGate = createCandidateScopeGate({
      evaluator,
      selfDid: marketDid,
      args: policyArgs,
      ...(scopeRecord ? { policyRecord: scopeRecord } : {}),
      ...(policyRef ? { ref: policyRef } : {}),
      ...(scopeRefresher ? { refresher: scopeRefresher } : {}),
      log,
    });

    const rfp = resume?.rfp ?? await pds.createSignedRepoRecord(RFP_NSID, rfpRecord, pds.attestationKp, pds.did);
    const rfpUri = rfp.uri;
    const rfpCid = rfp.cid;
    watch.setRfp(rfpUri);
    log(resume?.rfp ? "rfp_resumed" : "rfp_created", { uri: rfpUri, cid: rfpCid, hasPolicy: !!policyRef });

    let vouchedDids: string[] = [];
    try {
      const publicVouchResolver = createTangledGraphVouchResolver({
        listRecords: (repo, coll) => listRecordsPublic(idResolver, repo, coll),
      });
      const delegatedTrust = createBadgeBlueKeysDelegatedTrustResolver({
        vouchResolver: publicVouchResolver,
        listOwnRecords: async (collection, _opts) => {
          const result = oauthAgent && marketDid !== pds.did
            ? await oauthAgent.listRecords(marketDid, collection, { limit: 100 })
            : await ownApi.listRecords(marketDid, collection);
          const records =
            (result?.records as Array<{ uri: string; value: Record<string, unknown> }>) ?? [];
          for (const r of records) {
            log("delegated_trust_record", {
              challenge: r.value.challenge,
              service: r.value.service,
              keyId: r.value.keyId,
              selfDid: marketDid,
            });
          }
          log("delegated_trust_badgeBlueKeys", {
            did: marketDid,
            collection,
            count: records.length,
          });
          return records;
        },
      });
      vouchedDids = [...await delegatedTrust.getDelegatedTrustedDids(marketDid)];
      log("vouch_discovery", { count: vouchedDids.length });
    } catch (err) {
      log("vouch_discovery_error", { error: String(err) });
    }
    const operatedDids = await discoverOperatedDids({
      operators: vouchedDids,
      service: "bidder_associate",
      listRecordsPublic: (repo, coll) => listRecordsPublic(idResolver, repo, coll),
      log: (level, msg, meta) => log(`operated_discovery_${level}`, { msg, ...(meta ?? {}) }),
    });
    if (operatedDids.length > 0) log("operated_discovery", { count: operatedDids.length, bidders: operatedDids });

    const autoRelayUrls = await autoDiscoverRelayUrls({ log: logger });
    const allRelayUrls = [...new Set([...relayUrls, ...autoRelayUrls])];
    let relayDids: string[] = [];
    if (allRelayUrls.length > 0) {
      relayDids = await discoverBiddersFromRelays({
        relayUrls: allRelayUrls,
        collection: OFFERING_NSID,
        log: logger,
        timeoutMs: 15_000,
      });
      const counts = {
        relays: allRelayUrls.length,
        configured: relayUrls.length,
        autodiscovered: autoRelayUrls.length,
      };
      if (relayDids.length > 0) log("relay_discovery", { count: relayDids.length, ...counts });
      else log("relay_discovery_empty", counts);
    }

    const watcherDids = opts.offeringWatcherDids?.() ?? [];
    if (watcherDids.length > 0) log("offering_watch_discovery", { count: watcherDids.length });

    const bidderDids = Array.from(
      new Set([...relayDids, ...watcherDids, ...vouchedDids, ...operatedDids, ...extraBidderDids]),
    );
    const deniedSet = new Set(denyBidderDids);
    const foundBidderDids = bidderDids.filter((d) => !deniedSet.has(d));
    const filteredBidderDids = await freshBidderDids(idResolver, foundBidderDids);
    const freshSet = new Set(filteredBidderDids);
    log("bidder_discovery", {
      total: filteredBidderDids.length,
      found: foundBidderDids.length,
      fresh: filteredBidderDids.length,
      stale: foundBidderDids.length - filteredBidderDids.length,
      heartbeatTtlMs: HEARTBEAT_TTL_MS,
      relay: relayDids.length,
      watch: watcherDids.length,
      vouched: vouchedDids.length,
      operated: operatedDids.length,
      extra: extraBidderDids.length,
      denied: bidderDids.length - filteredBidderDids.length,
    });

    const prewarmDids = [...new Set([...vouchedDids, ...operatedDids, ...extraBidderDids])]
      .filter((d) => freshSet.has(d));
    if (scopeCache && prewarmDids.length > 0) {
      const startedAt = Date.now();
      log("scope_prewarm", { candidates: prewarmDids.length, discovered: filteredBidderDids.length });
      bestEffort(async () => {
        await mapWithConcurrency(prewarmDids, DEFAULT_POLICY_CONCURRENCY, (did) => scopeGate(did));
        log("scope_prewarm_done", {
          candidates: prewarmDids.length,
          entries: scopeCache.stats().entries,
          durationMs: Date.now() - startedAt,
        });
      });
    }

    const seen = new Set<string>();
    const rfpDeadlineMs = Math.max(5_000, bidWindowSec * 1_000);
    const submitRfpTo = resume?.rfp ? [] : filteredBidderDids;
    await Promise.allSettled(submitRfpTo.map((bidderDid) =>
      withDeadline(
        bidderDid,
        rfpDeadlineMs,
        (async () => {
          try {
            const pdsUrl = await pdsUrlOf(idResolver, bidderDid);
            if (!pdsUrl) return;
            const offerings = await listOfferingsCached(pdsUrl, bidderDid);
            for (const offering of offerings) {
              const appliesTo = offering.value.appliesTo as string[] | undefined;
              const endpointUrl = offering.value.endpointUrl as string | undefined;
              if (
                !endpointUrl || !Array.isArray(appliesTo) ||
                !appliesTo.includes(opts.appliesToNsid ?? COMPUTE_VM_NSID)
              ) continue;
              const target = await pds.resolveBidderEndpoint(endpointUrl);
              if (!target) {
                log("bidder_unknown_endpoint", { endpointUrl });
                continue;
              }
              const dedupKey = `${bidderDid}::${target.targetUrl}`;
              if (seen.has(dedupKey)) continue;
              seen.add(dedupKey);
              log("submitting_rfp", { bidderDid, endpointUrl });
              const r = await pds.callBidder(
                target.targetUrl,
                SUBMIT_RFP_NSID,
                SUBMIT_RFP_LXM,
                target.audDid,
                {
                  rfpUri,
                  rfpCid,
                },
              );
              log("submitRfp_result", { bidderDid, status: r.status, ok: r.ok });
              const answered = (r.body as { error?: unknown } | null | undefined)?.error;
              opts.onRfpAnswer?.({ bidderDid, status: r.status, ...(typeof answered === "string" ? { error: answered } : {}) });
            }
          } catch (err) {
            log("bidder_error", { bidderDid, error: String(err) });
          }
        })(),
        (what, ms) => log("bidder_deadline_exceeded", { what, ms }),
      )
    ));

    advance("rfp_submitted", {
      rfp: { uri: rfpUri, cid: rfpCid },
      ...(policyRef ? { policy: policyRef } : {}),
    });
    if (
      await decideFlow("onRfpSubmitted", handlers.onRfpSubmitted, {
        rfp: { uri: rfpUri, cid: rfpCid },
        payload,
        policy: policyRef,
        bidderDids: filteredBidderDids,
      })
    ) {
      pds.pendingBids.delete(rfpUri);
      return await finish("returned");
    }

    const evaluateCandidate = async (candidate: CollectedBid): Promise<PolicyResult> => {
      if (!policyRef) return { allow: true, violations: [] };
      const candidateDid = candidate.did;
      const payloadRef = candidate.record.payload as { uri: string; cid: string } | undefined;
      const startedAt = Date.now();
      const result = await evaluator.evaluatePolicies({
        refs: [policyRef],
        ctx: {
          policyName: "requester-recheck",
          args: policyArgs,
          perspective: "requester",
          selfDid: marketDid,
          subjectDid: candidateDid,
          rootRequesterDid: marketDid,
          counterpartyDid: candidateDid,
          resolve: (ref) => resolveRecordForPolicy(ref),
          resolveOperatorDid,
          getVouchedDids: (did) => policyVouchResolver.getDelegatedTrustedDids(did),
          log: (level, msg, meta) => log(`policy_eval_${level}`, { msg, ...(meta ?? {}) }),
          ...(payloadRef
            ? {
              offer: {
                bidRef: { uri: candidate.uri, cid: candidate.cid },
                payloadRef,
                payloadNsid: bidPayloadNsid(candidate),
              },
            }
            : {}),
        },
      });
      log("candidate_evaluate", {
        bidderDid: candidateDid,
        allow: result.allow,
        durationMs: Date.now() - startedAt,
      });
      return result;
    };

    let resumedAccept: FoundAccept | null = null;
    if (resume?.accept && resume.bid) {
      resumedAccept = { accept: resume.accept, bid: resume.bid };
    } else if (resume?.acceptPending) {
      try {
        resumedAccept = await findAccept(rfpUri);
      } catch (err) {
        log("accept_lookup_failed", { rfpUri, error: String(err) });
      }
      log("accept_pending_resolved", { rfpUri, bidUri: resume.acceptPending.bidUri, found: resumedAccept !== null });
    }

    let earlyWinner: CollectedBid | undefined;
    let bidCount = resume?.bids ?? 0;
    let winner: CollectedBid;
    let winnerReason: WinnerReason = "resumed";
    if (resumedAccept) {
      const found = resumedAccept;
      const bidRecord = await resolveRecord(found.bid).catch((err): Record<string, unknown> => {
        log("resumed_bid_unresolvable", { bidUri: found.bid.uri, error: String(err) });
        return {};
      });
      winner = {
        did: atUriAuthority(found.bid.uri),
        uri: found.bid.uri,
        cid: found.bid.cid,
        record: {
          ...(resume?.winner?.submitAccept ? { submitAccept: resume.winner.submitAccept } : {}),
          ...bidRecord,
        },
      };
      log("accept_resumed", { acceptUri: found.accept.uri, bidUri: found.bid.uri, winnerDid: winner.did });
    } else {
      if (resume?.rfp) {
        const found = await Promise.allSettled(filteredBidderDids.map((did) => findBids(did, rfpUri)));
        const backfilled = found.flatMap((r) => r.status === "fulfilled" ? r.value : []);
        const known = pds.pendingBids.get(rfpUri) ?? [];
        pds.pendingBids.set(rfpUri, [...known, ...backfilled.filter((b) => !known.some((k) => k.uri === b.uri))]);
        log("bids_backfilled", { rfpUri, count: backfilled.length, bidders: filteredBidderDids.length });
      }
      const bidDecisions = new Map<string, Promise<BidDecision>>();
      const decideBid = (bid: CollectedBid): Promise<BidDecision> => {
        let decision = bidDecisions.get(bid.uri);
        if (!decision) {
          const arrivedAt = nowIso();
          decision = (async () => {
            const startedAt = Date.now();
            const d = await runHandler("onBid", handlers.onBid, { bid, arrivedAt });
            const result: BidDecision = d === "deny" ? "deny" : "accept";
            log("bid_decide", {
              uri: bid.uri,
              bidderDid: bid.did,
              decision: result,
              durationMs: Date.now() - startedAt,
            });
            return result;
          })();
          bidDecisions.set(bid.uri, decision);
        }
        return decision;
      };

      log("waiting_for_bids", { bidWindowSec, firstFree });

      const collector = new BidCollector({
        freeBidNsid: BIDS_FREE_NSID,
        firstFree,
        allow: async (bid) =>
          (await decideBid(bid)) === "accept" && (await scopeGate(bid.did)).allow,
        onEarlyWinner: (bid) => {
          earlyWinner = bid;
          log("first_free_winner", { uri: bid.uri, did: bid.did });
        },
      });

      const notDenied = (bid: CollectedBid) => !deniedSet.has(bid.did);
      const collect = () => {
        collector.addAll((pds.pendingBids.get(rfpUri) ?? []).filter(notDenied));
        collector.addAll(watch.bidsFor(rfpUri).filter(notDenied));
        for (const bid of collector.all()) void decideBid(bid);
      };

      let windowTimer: ReturnType<typeof setTimeout> | undefined;
      const windowElapsed = new Promise<void>((resolve) => {
        windowTimer = setTimeout(resolve, bidWindowSec * 1000);
      });
      const poller = setInterval(collect, 250);
      try {
        await Promise.race([
          windowElapsed,
          ...(firstFree ? [collector.earlyWinner.then(() => {})] : []),
        ]);
      } finally {
        clearInterval(poller);
        if (windowTimer !== undefined) clearTimeout(windowTimer);
      }

      collect();
      pds.pendingBids.delete(rfpUri);
      const bids = collector.all();
      bidCount = bids.length;
      log("bids_collected", { count: bids.length, earlyExit: !!earlyWinner });

      if (bids.length === 0) {
        const error = `no bids received within ${bidWindowSec}s`;
        log("no_bids", { event: "no_bids", error });
        return await finish("no_bids", { error, bids: 0 });
      }

      const candidates: CollectedBid[] = [];
      for (const bid of bids) {
        if ((await decideBid(bid)) === "accept") candidates.push(bid);
      }
      if (candidates.length === 0) {
        const error = `all ${bids.length} bids denied by onBid`;
        log("bids_denied", { event: "bids_denied", error });
        return await finish("bids_denied", { error, bids: bids.length });
      }

      winner = earlyWinner ?? selectWinner(candidates)!;
      winnerReason = earlyWinner ? "firstFree" : "bestWithinWindow";
    }
    log("winner", { uri: winner.uri, did: winner.did, viaFirstFree: !!earlyWinner });
    const submitAcceptTarget = winner.record.submitAccept as string | undefined;
    advance("winner_selected", {
      bid: { uri: winner.uri, cid: winner.cid },
      winner: {
        did: winner.did,
        ...(submitAcceptTarget ? { submitAccept: submitAcceptTarget } : {}),
      },
      bids: bidCount,
    });

    const serviceName = vmName.trim() || "compute";
    let wifConfig: WifSimpleConfig | undefined;
    if ((opts.rbac && !skipSsh) || capabilities.length > 0) {
      const bidConfigRef = (winner.record.config ?? winner.record.bidConfig) as
        | { uri?: string; cid?: string }
        | undefined;
      if (bidConfigRef?.uri && bidConfigRef?.cid) {
        try {
          const cfg = await resolveRecord({ uri: bidConfigRef.uri, cid: bidConfigRef.cid });
          if (isWifSimpleConfig(cfg)) wifConfig = cfg;
          else {log("bid_config_incomplete", {
              reason: "missing issuer_uri/actx",
              bidConfigUri: bidConfigRef.uri,
            });}
        } catch (err) {
          log("bid_config_resolve_failed", { reason: String(err), bidConfigUri: bidConfigRef.uri });
        }
      } else {
        log("bid_config_missing", { reason: "winner bid has no bidConfig ref" });
      }
    }

    if (opts.rbac && !skipSsh && !resumedAccept) {
      if (wifConfig) {
        try {
          const rbacRecord = buildSshKeyRbacRecord({
            serviceName,
            issuerUri: wifConfig.issuer_uri,
            actx: wifConfig.actx,
            requesterDid: marketDid,
            subjectTemplate: wifConfig.subject,
          });
          const { uri: rbacUri } = await pds.createRepoRecord(FEDPROXY_RBAC_NSID, rbacRecord, rbacRecordKey("ssh", serviceName));
          log("rbac_created", { uri: rbacUri, serviceName, issuerUri: wifConfig.issuer_uri });
        } catch (err) {
          log("rbac_skipped", { reason: String(err) });
        }
      } else {
        log("rbac_skipped", { reason: "no usable bidConfig" });
      }
    }

    if (capabilities.length > 0) {
      if (wifConfig) {
        const grantVars = deriveGrantVars({
          cfg: wifConfig,
          subjectDid: atUriAuthority(rfpUri),
          audienceDid: marketDid,
          role: serviceName,
        });
        log("capability_grants", {
          subject: grantVars.subject,
          issuerUri: grantVars.issuerUri,
          aud: grantVars.expectedAud,
          capabilities: capabilities.map((c) => c.id),
        });
        for (const cap of capabilities) {
          try {
            await cap.onContract?.(grantVars);
          } catch (err) {
            log("capability_grant_failed", { capability: cap.id, error: String(err) });
          }
          const record = resumedAccept ? undefined : cap.rbacRecord?.(grantVars);
          if (record) {
            try {
              const { uri } = await pds.createRepoRecord(FEDPROXY_RBAC_NSID, record, rbacRecordKey(cap.id, serviceName));
              log("capability_rbac_created", { capability: cap.id, uri });
            } catch (err) {
              log("capability_rbac_failed", { capability: cap.id, error: String(err) });
            }
          }
        }
      } else {
        log("capability_grants_skipped", {
          reason: "no usable bidConfig",
          capabilities: capabilities.map((c) => c.id),
        });
      }
    }

    if (policyRef && !earlyWinner && !resumedAccept) {
      const evalResult = await evaluateCandidate(winner);
      if (!evalResult.allow) {
        log("policy_rejected", { violations: evalResult.violations, winnerDid: winner.did });
        return await finish("policy_rejected", {
          error: `winner rejected by policy: ${evalResult.violations.map((v) => v.msg).join("; ")}`,
          bids: bidCount,
        });
      }
    }

    if (policyRef && winnerReason === "bestWithinWindow") winnerReason = "policy";
    if (!resumedAccept) {
      try {
        await handlers.onWinner?.({
          bid: { uri: winner.uri, cid: winner.cid },
          did: winner.did,
          reason: winnerReason,
          considered: bidCount,
        }, structuredClone(state));
      } catch (err) {
        log("compute_handler_failed", { handler: "onWinner", error: String(err) });
      }
    }

    let acceptUri: string;
    let acceptCid: string;
    if (resumedAccept) {
      acceptUri = resumedAccept.accept.uri;
      acceptCid = resumedAccept.accept.cid;
    } else {
      state = patchContract(state, { acceptPending: { bidUri: winner.uri } });
      if (
        await decideFlow("onAccepting", handlers.onAccepting, {
          rfp: { uri: rfpUri, cid: rfpCid },
          bid: { uri: winner.uri, cid: winner.cid },
          winnerDid: winner.did,
        })
      ) {
        return await finish("returned");
      }
      const accept = await pds.createSignedRepoRecord(
        ACCEPT_NSID,
        {
          $type: ACCEPT_NSID,
          rfp: { $type: "com.atproto.repo.strongRef", uri: rfpUri, cid: rfpCid },
          bid: { $type: "com.atproto.repo.strongRef", uri: winner.uri, cid: winner.cid },
          submitEvent: `${pds.did}#pdr_temp_compute_event`,
          createdAt: nowIso(),
        },
        pds.attestationKp,
        pds.did,
      );
      acceptUri = accept.uri;
      acceptCid = accept.cid;
      log("accept_created", { uri: acceptUri, cid: acceptCid });
    }

    let receiptUri: string | undefined;
    let receiptCid: string | undefined;
    let submitEventRef: string | undefined;
    const takeReceipt = (found: { uri?: string; cid?: string; submitEvent?: string }) => {
      receiptUri = found.uri;
      receiptCid = found.cid;
      submitEventRef = found.submitEvent;
      watch.setReceipt({ uri: receiptUri ?? "", cid: receiptCid ?? "" });
    };
    if (resume?.receipt) {
      takeReceipt({ uri: resume.receipt.uri, cid: resume.receipt.cid, submitEvent: resume.submitEventRef });
      log("receipt_resumed", { receiptUri, submitEventRef });
    }

    let acceptPushed = false;
    if (submitAcceptTarget && !receiptUri) {
      const target = await pds.resolveBidderEndpoint(submitAcceptTarget);
      if (target) {
        acceptPushed = true;
        let deadline = acceptResendDeadline(winner.record.createdAt, Date.now());
        let inProgressSince: number | undefined;
        let delay = ACCEPT_RESEND_FIRST_DELAY_MS;
        const sendAccept = () =>
          pds.callBidder(target.targetUrl, SUBMIT_ACCEPT_NSID, SUBMIT_ACCEPT_LXM, target.audDid, { acceptUri, acceptCid });
        const sendCopies = async (copies: number) => {
          const settled = await Promise.allSettled(Array.from({ length: copies }, sendAccept));
          const answers = settled.map((x) =>
            x.status === "fulfilled"
              ? { status: x.value.status, receipt: (x.value.body as { uri?: string } | null)?.uri, error: (x.value.body as { error?: string } | null)?.error }
              : { error: String(x.reason) }
          );
          log("submitAccept_copies", { copies, receipts: [...new Set(answers.map((a) => a.receipt).filter(Boolean))], answers });
          const fulfilled = settled.flatMap((x) => x.status === "fulfilled" ? [x.value] : []);
          const withReceipt = fulfilled.find((r) => r.ok && (r.body as { uri?: string } | null)?.uri);
          if (withReceipt ?? fulfilled[0]) return withReceipt ?? fulfilled[0];
          throw (settled[0] as PromiseRejectedResult).reason;
        };
        log("submitting_accept", { target: submitAcceptTarget, resendUntil: new Date(deadline).toISOString() });
        for (let attempt = 1; !receiptUri && !latch.requested; attempt++) {
          const fromFirehose = watch.firehose ? watch.receiptFor(acceptUri) : undefined;
          if (fromFirehose) {
            takeReceipt({ uri: fromFirehose.receiptUri, cid: fromFirehose.receiptCid, submitEvent: fromFirehose.submitEventRef });
            log("receipt_from_firehose", { receiptUri, receiptCid, submitEventRef, attempt });
            break;
          }
          let refused: string | undefined;
          try {
            const copies = attempt === 1 ? Math.max(1, Math.floor(opts.acceptCopies ?? 1)) : 1;
            const r = copies > 1 ? await sendCopies(copies) : await sendAccept();
            const body = r.body as { uri?: string; cid?: string; submitEvent?: string; error?: string; message?: string } | null;
            if (body?.error === ACCEPT_IN_PROGRESS && inProgressSince === undefined) {
              inProgressSince = Date.now();
              deadline = Math.max(deadline, inProgressSince + ACCEPT_RESEND_MAX_MS);
              log("submitAccept_in_progress", { attempt, detail: body.message, waitUntil: new Date(deadline).toISOString() });
            }
            if (r.ok && body?.uri) {
              takeReceipt(body);
              log("submitAccept_result", { status: r.status, receiptUri, receiptCid, submitEventRef, attempt });
              break;
            }
            if (acceptRetryable(r.status, body)) {
              log("submitAccept_retry", { attempt, status: r.status, error: body?.error, detail: body?.message });
            } else {
              refused = `${r.status} ${body?.error ?? ""}: ${body?.message ?? ""}`;
            }
          } catch (err) {
            log("submitAccept_retry", { attempt, error: String(err) });
          }
          if (refused) {
            log("submitAccept_refused", { target: submitAcceptTarget, attempt, refused });
            break;
          }
          try {
            const found = await findReceipt(winner.did, acceptUri);
            if (found) {
              takeReceipt(found);
              log("receipt_from_bidder_repo", { receiptUri, receiptCid, submitEventRef, attempt });
              break;
            }
          } catch (err) {
            log("receipt_repo_lookup_failed", { bidderDid: winner.did, attempt, error: String(err) });
          }
          if (Date.now() + delay > deadline) {
            log("submitAccept_gave_up", { attempt, deadline: new Date(deadline).toISOString() });
            break;
          }
          await new Promise((r) => setTimeout(r, delay));
          delay = Math.min(delay * 2, ACCEPT_RESEND_MAX_DELAY_MS);
        }
      } else {
        log("accept_target_unresolvable", { submitAcceptTarget });
      }
    }

    for (let attempt = 1; !acceptPushed && !receiptUri && attempt <= RECEIPT_REPO_ATTEMPTS && !latch.requested; attempt++) {
      try {
        const found = await findReceipt(winner.did, acceptUri);
        if (found) {
          takeReceipt(found);
          log("receipt_from_bidder_repo", { receiptUri, receiptCid, submitEventRef, attempt });
          break;
        }
      } catch (err) {
        log("receipt_repo_lookup_failed", { bidderDid: winner.did, attempt, error: String(err) });
      }
      if (attempt < RECEIPT_REPO_ATTEMPTS) await new Promise((r) => setTimeout(r, RECEIPT_REPO_RETRY_MS));
    }

    const receiptPatch = (): ContractStatePatch => ({
      ...(receiptUri && receiptCid ? { receipt: { uri: receiptUri, cid: receiptCid } } : {}),
      ...(submitEventRef ? { submitEventRef } : {}),
    });

    advance("accepted", { accept: { uri: acceptUri, cid: acceptCid }, acceptPending: undefined, ...receiptPatch() });
    if (
      await decideFlow("onAccepted", handlers.onAccepted, {
        accept: { uri: acceptUri, cid: acceptCid },
        bid: { uri: winner.uri, cid: winner.cid },
        winnerDid: winner.did,
      })
    ) {
      return await finish("returned");
    }

    if (!acceptPushed && !receiptUri && watch.firehose) {
      log("receipt_firehose_fallback", { acceptUri });
      const deadline = Date.now() + RECEIPT_FIREHOSE_TIMEOUT_MS;
      let fromFirehose: FirehoseReceipt | undefined;
      while (Date.now() < deadline && !latch.requested) {
        fromFirehose = watch.receiptFor(acceptUri);
        if (fromFirehose) break;
        await new Promise((r) => setTimeout(r, 500));
      }
      if (fromFirehose) {
        takeReceipt({
          uri: fromFirehose.receiptUri,
          cid: fromFirehose.receiptCid,
          submitEvent: fromFirehose.submitEventRef,
        });
        log("receipt_from_firehose", { receiptUri, receiptCid, submitEventRef });
      } else if (!latch.requested) {
        log("receipt_firehose_timeout", { acceptUri });
      }
    }
    if (latch.requested) return await finish("returned", receiptPatch());

    let receiptOk = false;
    if (receiptUri && receiptCid) {
      try {
        const v = await verifyReceipt({
          receipt: { uri: receiptUri, cid: receiptCid },
          accept: { uri: acceptUri, cid: acceptCid },
        });
        receiptOk = v.ok;
        log("receipt_verified", { receiptUri, sigOk: v.sigOk, bindOk: v.bindOk, ok: receiptOk });
      } catch (err) {
        log("receipt_verify_error", { receiptUri, error: String(err) });
      }
      advance("receipt", { ...receiptPatch(), receiptOk });
    } else {
      log("receipt_missing", { receiptUri, receiptCid });
      state = patchContract(state, { ...receiptPatch(), receiptOk });
    }
    log("compute_request_complete", { event: "compute_request_complete", ...flowViewOf(state) });

    if (
      await decideFlow("onReceipt", handlers.onReceipt, {
        receipt: state.receipt,
        receiptOk,
        submitEventRef,
      })
    ) {
      return await finish("returned");
    }

    if (skipSsh) return await finish("complete");
    if (!receiptOk) {
      log("vm_poll_bailed", { reason: "no valid receipt", receiptUri, receiptCid });
      return await finish("receipt_invalid");
    }

    const waited: AddressWait | null = state.vmAddress
      ? {
        kind: "network",
        network: { address: state.vmAddress, kind: guestAddressKind(state.vmAddress), source: "resume" },
      }
      : await waitForAddress(watch, vmReadyTimeoutSec * 1000, latch);
    if (!waited) {
      if (latch.requested) return await finish("returned");
      log("vm_fqdn_timeout", { timeoutSec: vmReadyTimeoutSec });
      return await finish("network_timeout");
    }
    if (waited.kind === "terminated") {
      await terminatedState.promise;
      return await finish("returned");
    }
    const network = waited.network;
    advance("network", { vmAddress: network.address });
    if (await decideFlow("onNetwork", handlers.onNetwork, network)) return await finish("returned");
    return await finish(latch.requested ? "returned" : "complete");
  } catch (err) {
    ended = true;
    watch.close();
    await disposeAll(capabilities, log);
    throw new ComputeRequestFailed(structuredClone(state), err);
  }
}

export interface AwaitComputeNetworkOptions {
  logger?: StructuredLoggerInterface;
  eventStreams?: EventStreamsClient;
  plcUrl?: string;
  timeoutMs?: number;
  backfill?: boolean;
  onNetwork?: ComputeRequestHandlers["onNetwork"];
  onTerminated?: ComputeRequestHandlers["onTerminated"];
  eventVerifier?: ComputeEventVerifier;
}

export async function awaitComputeNetwork(
  pds: OnNetworkPds,
  state: ContractState,
  opts: AwaitComputeNetworkOptions = {},
): Promise<ContractState> {
  const log = flowLog(opts.logger);
  if (state.vmAddress || isTerminated(state)) return state;
  if (!state.receipt) {
    log("onnetwork_resume_skipped", { reason: "no receipt", phase: state.phase });
    return patchContract(state, { error: "no receipt to resume from" });
  }
  const watch = openContractWatch({
    pds,
    eventStreams: opts.eventStreams,
    idResolver: sharedIdResolver(opts.plcUrl),
    logger: opts.logger,
    log,
    verifyEvent: opts.eventVerifier,
  });
  try {
    watch.setReceipt(state.receipt);
    log("onnetwork_resume", { receiptUri: state.receipt.uri, winnerDid: state.winner?.did });
    if (opts.backfill !== false && state.winner?.did) void watch.backfill(state.winner.did);
    const timeoutMs = opts.timeoutMs ?? DEFAULT_VM_READY_TIMEOUT_SEC * 1000;
    const waited = await waitForAddress(watch, timeoutMs);
    if (!waited) {
      log("vm_fqdn_timeout", { timeoutSec: timeoutMs / 1000 });
      return patchContract(state, { outcome: "network_timeout" });
    }
    if (waited.kind === "terminated") {
      const ended = patchContract(state, {
        termination: waited.termination,
        outcome: "returned",
      });
      try {
        await opts.onTerminated?.(waited.termination, structuredClone(ended));
      } catch (err) {
        log("compute_handler_failed", { handler: "onTerminated", error: String(err) });
      }
      return ended;
    }
    const network = waited.network;
    const next = advanceContract(state, "network", {
      vmAddress: network.address,
      outcome: "complete",
    }, nowIso());
    try {
      await opts.onNetwork?.(network, structuredClone(next));
    } catch (err) {
      log("compute_handler_failed", { handler: "onNetwork", error: String(err) });
    }
    return next;
  } finally {
    watch.close();
  }
}

export interface AwaitComputeTerminationOptions {
  logger?: StructuredLoggerInterface;
  eventStreams?: EventStreamsClient;
  plcUrl?: string;
  backfill?: boolean;
  signal?: AbortSignal;
  eventVerifier?: ComputeEventVerifier;
}

export async function awaitComputeTermination(
  pds: OnNetworkPds,
  state: ContractState,
  opts: AwaitComputeTerminationOptions = {},
): Promise<ContractState | null> {
  const log = flowLog(opts.logger);
  if (isTerminated(state)) return state;
  if (!state.receipt || opts.signal?.aborted) return null;
  const watch = openContractWatch({
    pds,
    eventStreams: opts.eventStreams,
    idResolver: sharedIdResolver(opts.plcUrl),
    logger: opts.logger,
    log,
    verifyEvent: opts.eventVerifier,
  });
  const aborted = Promise.withResolvers<null>();
  const onAbort = () => aborted.resolve(null);
  opts.signal?.addEventListener("abort", onAbort, { once: true });
  try {
    watch.setReceipt(state.receipt);
    watch.releaseNetwork();
    log("termination_watch", { receiptUri: state.receipt.uri, winnerDid: state.winner?.did });
    if (opts.backfill !== false && state.winner?.did) void watch.backfill(state.winner.did, "terminated");
    const termination = await Promise.race([watch.terminated, aborted.promise]);
    if (!termination) return null;
    return patchContract(state, { termination });
  } finally {
    opts.signal?.removeEventListener("abort", onAbort);
    watch.close();
  }
}

export interface AwaitComputeHostKeysOptions {
  logger?: StructuredLoggerInterface;
  eventStreams?: EventStreamsClient;
  plcUrl?: string;
  timeoutMs?: number;
  backfill?: boolean;
  signal?: AbortSignal;
  eventVerifier?: ComputeEventVerifier;
}

export async function awaitComputeHostKeys(
  pds: OnNetworkPds,
  state: ContractState,
  opts: AwaitComputeHostKeysOptions = {},
): Promise<ContractState> {
  const log = flowLog(opts.logger);
  if (state.hostKeys || !state.receipt || isTerminated(state) || opts.signal?.aborted) return state;
  const watch = openContractWatch({
    pds,
    eventStreams: opts.eventStreams,
    idResolver: sharedIdResolver(opts.plcUrl),
    logger: opts.logger,
    log,
    verifyEvent: opts.eventVerifier,
  });
  const stop = Promise.withResolvers<null>();
  const onAbort = () => stop.resolve(null);
  opts.signal?.addEventListener("abort", onAbort, { once: true });
  const timer = setTimeout(() => stop.resolve(null), opts.timeoutMs ?? DEFAULT_VM_READY_TIMEOUT_SEC * 1000);
  try {
    watch.setReceipt(state.receipt);
    watch.releaseNetwork();
    log("hostkeys_watch", { receiptUri: state.receipt.uri, winnerDid: state.winner?.did });
    if (opts.backfill !== false && state.winner?.did) void watch.backfill(state.winner.did, "terminated");
    const hostKeys = await Promise.race([watch.hostKeys, stop.promise]);
    if (!hostKeys) {
      log("vm_hostkeys_missing", { receiptUri: state.receipt.uri });
      return state;
    }
    return patchContract(state, { hostKeys });
  } finally {
    clearTimeout(timer);
    opts.signal?.removeEventListener("abort", onAbort);
    watch.close();
  }
}

export interface ReleaseComputeOptions {
  logger?: StructuredLoggerInterface;
  capabilities?: GuestCapability[];
  reason?: string;
  plcUrl?: string;
}

async function submitRelease(
  pds: ContractReleaser,
  state: ContractState,
  target: NonNullable<ReturnType<typeof releaseTargetOf>>,
  release: { event: { uri: string; cid: string }; payload: { uri: string; cid: string } },
  record: Record<string, unknown>,
  log: FlowLog,
): Promise<ContractState> {
  const bidder = await pds.resolveBidderEndpoint(target.submitEventRef);
  if (!bidder) {
    log("vm_delete_target_unresolvable", { submitEventRef: target.submitEventRef });
    return patchContract(state, { release: { ...release, ok: false }, error: `vm.delete target unresolvable: ${target.submitEventRef}` });
  }
  log("submitting_vm_delete", { submitEventRef: target.submitEventRef, eventUri: release.event.uri });
  const r = await pds.callBidder(bidder.targetUrl, SUBMIT_EVENT_NSID, SUBMIT_EVENT_LXM, bidder.audDid, {
    uri: release.event.uri,
    cid: release.event.cid,
    record,
  });
  log("vm_delete_result", { status: r.status, ok: r.ok });
  const done = { release: { ...release, ok: r.ok, status: r.status } };
  return r.ok
    ? advanceContract(state, "released", done, nowIso())
    : patchContract(state, { ...done, error: `vm.delete answered ${r.status}` });
}

export async function resubmitRelease(
  pds: ContractReleaser,
  state: ContractState,
  opts: ReleaseComputeOptions = {},
): Promise<ContractState> {
  const log = flowLog(opts.logger);
  const release = state.release;
  const target = releaseTargetOf(state);
  if (!release || !target) return state;
  try {
    const [repo, collection, rkey] = release.event.uri.replace(/^at:\/\//, "").split("/");
    const pdsUrl = await pdsUrlOf(sharedIdResolver(opts.plcUrl), repo);
    if (!pdsUrl) throw new Error(`no PDS resolves for ${repo}`);
    const rec = await getRecordFrom(pdsUrl, repo, collection, rkey);
    if (!rec || rec.cid !== release.event.cid) {
      throw new Error(`the vm.delete event ${release.event.uri} is not readable at cid ${release.event.cid}`);
    }
    log("resubmitting_vm_delete", { eventUri: release.event.uri, lastStatus: release.status ?? null });
    return await submitRelease(pds, state, target, { event: release.event, payload: release.payload }, rec.value, log);
  } catch (err) {
    log("vm_delete_error", { error: String(err) });
    return patchContract(state, { error: `vm.delete resend failed: ${String(err)}` });
  }
}

async function deleteRbacRecords(
  pds: ContractReleaser,
  state: ContractState,
  capabilities: GuestCapability[],
  log: FlowLog,
): Promise<void> {
  if (!pds.deleteRepoRecord) return;
  const keys = [rbacRecordKey("ssh", state.vmName), ...capabilities.filter((c) => c.rbacRecord).map((c) => rbacRecordKey(c.id, state.vmName))];
  for (const rkey of keys) {
    try {
      await pds.deleteRepoRecord(FEDPROXY_RBAC_NSID, rkey);
      log("rbac_deleted", { rkey });
    } catch (err) {
      log("rbac_delete_skipped", { rkey, reason: String(err) });
    }
  }
}

export async function releaseCompute(
  pds: ContractReleaser,
  state: ContractState,
  opts: ReleaseComputeOptions = {},
): Promise<ContractState> {
  const log = flowLog(opts.logger);
  if (state.release?.event && !isTerminated(state)) return await resubmitRelease(pds, state, opts);
  await deleteRbacRecords(pds, state, opts.capabilities ?? [], log);
  if (isTerminated(state)) {
    log("vm_delete_skipped", {
      reason: "terminated_by_bidder",
      terminatedReason: state.termination?.reason,
      receiptUri: state.receipt?.uri,
    });
    for (const cap of opts.capabilities ?? []) {
      try {
        await cap.onRevoke?.();
      } catch (err) {
        log("capability_revoke_failed", { capability: cap.id, error: String(err) });
      }
    }
    return state;
  }
  const target = releaseTargetOf(state);
  if (!target) {
    log("vm_delete_skipped", {
      reason: "missing receipt refs",
      receiptUri: state.receipt?.uri,
      receiptCid: state.receipt?.cid,
      submitEventRef: state.submitEventRef,
    });
    return state;
  }
  let next = state;
  try {
    const at = nowIso();
    const deletion = await pds.createSignedRepoRecord(
      COMPUTE_EVENTS_VM_DELETE_NSID,
      {
        $type: COMPUTE_EVENTS_VM_DELETE_NSID,
        reason: opts.reason ?? "session_ended",
        createdAt: at,
      },
      pds.attestationKp,
      pds.did,
    );
    const eventRecord = {
      $type: EVENT_NSID,
      receipt: {
        $type: "com.atproto.repo.strongRef",
        uri: target.receipt.uri,
        cid: target.receipt.cid,
      },
      payload: { $type: "com.atproto.repo.strongRef", uri: deletion.uri, cid: deletion.cid },
      createdAt: at,
    };
    const event = await pds.createSignedRepoRecord(
      EVENT_NSID,
      eventRecord,
      pds.attestationKp,
      pds.did,
    );
    const release = { event: { uri: event.uri, cid: event.cid }, payload: { uri: deletion.uri, cid: deletion.cid } };
    next = patchContract(state, { release: { ...release, ok: false } });
    next = await submitRelease(pds, next, target, release, eventRecord, log);
  } catch (err) {
    log("vm_delete_error", { error: String(err) });
    next = patchContract(next, { error: `vm.delete failed: ${String(err)}` });
  }
  for (const cap of opts.capabilities ?? []) {
    try {
      await cap.onRevoke?.();
    } catch (err) {
      log("capability_revoke_failed", { capability: cap.id, error: String(err) });
    }
  }
  return next;
}

export interface FindVmContractOptions {
  plcUrl?: string;
  logger?: StructuredLoggerInterface;
  eventVerifier?: ComputeEventVerifier;
  backfillTimeoutMs?: number;
  payloadUri?: string;
}

export interface VmRecordView {
  uri: string;
  vmName: string;
  createdAt: string;
}

const OWN_RECORDS_LIMIT = 10_000;

type SessionLister = {
  listRecords(repo: string, collection: string, opts?: { limit?: number }): Promise<{ records?: unknown } | undefined>;
};

async function ownRecords(pds: Pick<RequesterPDS, "did">, plcUrl: string | undefined) {
  const idResolver = sharedIdResolver(plcUrl);
  const marketDid = (pds as unknown as { oauthSession?: { userDid?: string } }).oauthSession?.userDid ?? pds.did;
  const oauthAgent = (pds as unknown as Record<string, unknown>).oauthAgent as SessionLister | undefined;
  const ownApi = (pds as unknown as { api?: SessionLister }).api;
  const session = oauthAgent && marketDid !== pds.did ? oauthAgent : ownApi;
  const list = async (collection: string): Promise<FetchedRecord[]> => {
    if (session) {
      const listed = await session.listRecords(marketDid, collection, { limit: OWN_RECORDS_LIMIT });
      return (listed?.records ?? []) as FetchedRecord[];
    }
    const ownPds = await pdsUrlOf(idResolver, marketDid);
    if (!ownPds) throw new Error(`no PDS for ${marketDid}`);
    return await listRecordsAll(ownPds, marketDid, collection, { limit: OWN_RECORDS_LIMIT, timeoutMs: 10_000 });
  };
  return { idResolver, marketDid, list };
}

export async function listVmRecords(pds: Pick<RequesterPDS, "did">, opts: { plcUrl?: string } = {}): Promise<VmRecordView[]> {
  const own = await ownRecords(pds, opts.plcUrl);
  return (await own.list(COMPUTE_VM_NSID)).map((r) => ({
    uri: r.uri,
    vmName: String(r.value.role ?? ""),
    createdAt: String(r.value.createdAt ?? ""),
  }));
}

export interface OwnVmRecordSet {
  vm: VmRecordView;
  rfps: string[];
  accepts: string[];
}

export interface OwnContractRecords {
  sets: OwnVmRecordSet[];
  eventsByReceipt: Map<string, string[]>;
}

export async function listOwnContractRecords(pds: Pick<RequesterPDS, "did">, opts: { plcUrl?: string } = {}): Promise<OwnContractRecords> {
  const own = await ownRecords(pds, opts.plcUrl);
  const [vms, rfps, accepts, events] = await Promise.all([COMPUTE_VM_NSID, RFP_NSID, ACCEPT_NSID, EVENT_NSID].map((c) => own.list(c)));
  const sets = new Map<string, OwnVmRecordSet>();
  for (const r of vms) {
    sets.set(r.uri, { vm: { uri: r.uri, vmName: String(r.value.role ?? ""), createdAt: String(r.value.createdAt ?? "") }, rfps: [], accepts: [] });
  }
  const setOfRfp = new Map<string, OwnVmRecordSet>();
  for (const r of rfps) {
    const payload = refOf(r.value.payload).uri ?? "";
    let set = sets.get(payload);
    if (!set) {
      set = { vm: { uri: payload, vmName: "", createdAt: String(r.value.createdAt ?? "") }, rfps: [], accepts: [] };
      sets.set(payload || r.uri, set);
    }
    set.rfps.push(r.uri);
    setOfRfp.set(r.uri, set);
  }
  for (const r of accepts) setOfRfp.get(refOf(r.value.rfp).uri ?? "")?.accepts.push(r.uri);
  const eventsByReceipt = new Map<string, string[]>();
  for (const r of events) {
    const receipt = refOf(r.value.receipt).uri;
    if (!receipt) continue;
    const list = eventsByReceipt.get(receipt) ?? [];
    list.push(r.uri, ...(refOf(r.value.payload).uri ? [refOf(r.value.payload).uri!] : []));
    eventsByReceipt.set(receipt, list);
  }
  return { sets: [...sets.values()], eventsByReceipt };
}

export interface FoundVmContract {
  state: ContractState;
  deleteSubmitted: boolean;
}

function refOf(value: unknown): { uri?: string; cid?: string } {
  return (value ?? {}) as { uri?: string; cid?: string };
}

export async function findVmContract(
  pds: OnNetworkPds & Pick<RequesterPDS, "did">,
  vmName: string,
  opts: FindVmContractOptions = {},
): Promise<FoundVmContract | null> {
  const log = flowLog(opts.logger);
  const { idResolver, marketDid, list: own } = await ownRecords(pds, opts.plcUrl);
  const vms = (await own(COMPUTE_VM_NSID)).filter((r) =>
    r.value.role === vmName && (opts.payloadUri === undefined || r.uri === opts.payloadUri)
  );
  if (vms.length === 0) return null;
  const rfps = await own(RFP_NSID);
  const accepts = await own(ACCEPT_NSID);
  const events = await own(EVENT_NSID);
  const newestFirst = [...vms].sort((a, b) => String(b.value.createdAt ?? "").localeCompare(String(a.value.createdAt ?? "")));
  for (const vm of newestFirst) {
    const rfp = rfps.find((r) => refOf(r.value.payload).uri === vm.uri);
    if (!rfp) continue;
    const accept = accepts.find((r) => refOf(r.value.rfp).uri === rfp.uri);
    const bid = refOf(accept?.value.bid);
    if (!accept || !bid.uri || !bid.cid) continue;
    const winnerDid = atUriAuthority(bid.uri);
    const receipt = await createRepoReceiptFinder(idResolver)(winnerDid, accept.uri);
    if (!receipt) continue;
    let state = createContractState({ vmName, requesterDid: pds.did, marketDid, at: String(vm.value.createdAt ?? nowIso()) });
    state = advanceContract(state, "receipt", {
      payload: { uri: vm.uri, cid: vm.cid },
      rfp: { uri: rfp.uri, cid: rfp.cid },
      bid: { uri: bid.uri, cid: bid.cid },
      winner: { did: winnerDid },
      accept: { uri: accept.uri, cid: accept.cid },
      receipt: { uri: receipt.uri, cid: receipt.cid },
      ...(receipt.submitEvent ? { submitEventRef: receipt.submitEvent } : {}),
    }, nowIso());
    const deleteSubmitted = events.some((e) =>
      refOf(e.value.receipt).uri === receipt.uri &&
      (refOf(e.value.payload).uri ?? "").includes(`/${COMPUTE_EVENTS_VM_DELETE_NSID}/`)
    );
    const watch = openContractWatch({ pds, idResolver, logger: opts.logger, log, verifyEvent: opts.eventVerifier });
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      watch.setReceipt(state.receipt!);
      watch.releaseNetwork();
      await Promise.race([
        watch.backfill(winnerDid, "terminated"),
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, opts.backfillTimeoutMs ?? 20_000);
        }),
      ]);
      const settled = <T>(p: Promise<T>) => Promise.race([p, Promise.resolve(undefined)]);
      const termination = await settled(watch.terminated);
      const hostKeys = await settled(watch.hostKeys);
      state = patchContract(state, { ...(termination ? { termination } : {}), ...(hostKeys ? { hostKeys } : {}) });
    } finally {
      clearTimeout(timer);
      watch.close();
    }
    log("vm_contract_found", {
      vmName,
      receiptUri: receipt.uri,
      winnerDid,
      hostKeys: state.hostKeys?.keys.length ?? 0,
      terminated: state.termination?.reason ?? null,
      deleteSubmitted,
    });
    return { state, deleteSubmitted };
  }
  return null;
}
