// Market bidder factory -- lifecycle orchestration, callback merging, route wiring.
// Does NOT own I/O (no Deno.serve, signals, WS connect). Takes atproto + serve +
// providers; wires everything in beginServe().

import { TID } from "@atproto/common";
import { getPdsEndpoint } from "@atproto/common-web";
import type { RepoApi } from "@publicdomainrelay/atproto-repo-abc";
import { createRecordResolver, createRfpDispatcher, startOfferingRefresh, listRecordsPublic, getRecordAt, resolvePds } from "@publicdomainrelay/market-atproto";
import type { MarketServerDeps, OfferingRefreshHandle, RfpObservation } from "@publicdomainrelay/market-atproto";
import { createMarketFactory } from "@publicdomainrelay/hono-factory-market-atproto";
import { BID_NSID, RECEIPT_NSID, RFP_NSID, VOUCH_NSID } from "@publicdomainrelay/market-common";
import { createBidLoop } from "./bid-loop.ts";
import type {
  ATProtoEventStreamsClient,
} from "@publicdomainrelay/atproto-event-streams-client";
import type { Logger } from "@publicdomainrelay/market-common";
import {
  DEFAULT_MARKET_SERVICE_ID,
} from "@publicdomainrelay/market-common";
import { OAuthSessionExpiredError } from "@publicdomainrelay/atproto-helpers";
import {
  OFFERING_NSID,
  ALLOWLIST_RBAC_DID_NSID,
  BADGE_BLUE_KEYS_NSID,
  ACCEPT_NSID,
  EVENT_NSID,
  BIDDER_ASSOCIATION_NSID,
} from "@publicdomainrelay/market-lexicons";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import type { IngressRef, ServeHandle } from "@publicdomainrelay/serve";
import type { VouchResolver } from "@publicdomainrelay/trust-graph-abc";
import { createTangledGraphVouchResolver } from "@publicdomainrelay/trust-graph-tangled-graph";
import { createBadgeBlueKeysDelegatedTrustResolver } from "@publicdomainrelay/delegated-trust-badge-blue-keys";
import { createPolicyEvaluator } from "@publicdomainrelay/policy-engine-evaluator";
import type { PolicyEvaluator } from "@publicdomainrelay/policy-engine-evaluator";
import { createScopeCache } from "@publicdomainrelay/policy-engine-scope-cache";
import { createPolicyRegistry } from "@publicdomainrelay/policy-deno-typescript";
import { resolvePolicyName } from "@publicdomainrelay/policy-deno-typescript-shared";
import type { PolicyArgs } from "@publicdomainrelay/policy-deno-typescript-shared";
import { WORKFLOWS } from "@publicdomainrelay/policies-gha-lite";
import { GhaLiteExecutor } from "@publicdomainrelay/policy-engine-executor-gha-lite";
import { createRemoteEngineRegistry } from "@publicdomainrelay/policy-engine-client-http";
import { TypescriptExecutor } from "@publicdomainrelay/policy-engine-executor-typescript";
import { POLICY_GHA_LITE_NSID, POLICY_TYPESCRIPT_NSID } from "@publicdomainrelay/policy-engine-abc";
import { parseAtUri, type ATProto } from "@publicdomainrelay/atproto-helpers";
import type {
  ActiveContract,
  BidLedger,
  CallbackFactoryDeps,
  CallbackSet,
  ContractEvent,
  ContractStore,
  MarketBidderProviderRef,
  PolicyExecOptions,
} from "@publicdomainrelay/market-bidder-abc";
import { activeContractOf, TerminatedReceipts } from "@publicdomainrelay/market-bidder-abc";
import { declineReasonOf, type HandlerResult, type MarketLoopRecorder } from "@publicdomainrelay/market-abc";
import type { PdsWriteBudget } from "@publicdomainrelay/pds-write-budget-abc";
export type {
  ActiveContract,
  CallbackFactoryDeps,
  CallbackSet,
  ContractEvent,
  ContractStore,
  MarketBidderProviderRef,
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function deepMergeCallbacks<T>(
  a: Record<string, Record<string, T>>,
  b: Record<string, Record<string, T>>,
): Record<string, Record<string, T>> {
  const result: Record<string, Record<string, T>> = {};
  for (const key of new Set([...Object.keys(a), ...Object.keys(b)])) {
    result[key] = { ...(a[key] ?? {}), ...(b[key] ?? {}) };
  }
  return result;
}

// ---------------------------------------------------------------------------
// Config types
// ---------------------------------------------------------------------------

export interface MarketBidderConfig {
  logger: StructuredLoggerInterface;
  serve: ServeHandle;
  atproto: ATProto;
  relay?: IngressRef;
  providers?: MarketBidderProviderRef[];
  setup?(): Promise<void>;
  teardown?(): Promise<void>;
  callbackFactory?: (deps: CallbackFactoryDeps) => CallbackSet | Promise<CallbackSet>;
  /** Fires on contract lifecycle changes (accepted, provisioned, terminated). */
  onContractChange?: (event: ContractEvent) => void | Promise<void>;
  contractStore?: ContractStore;
  /** Registry name of the policy gating which RFPs the bidder responds to. */
  policy?: string | null;
  /** Arguments handed to that policy. */
  policyArgs?: PolicyArgs;
  /** How this bidder is willing to execute an RFP's attached policy. */
  policyExec?: PolicyExecOptions;
  policyEngineUrl?: string;
  /**
   * ATProto event streams client for firehose-based record discovery.
   * When set, the bidder self-discovers RFP/ACCEPT/EVENT records via firehose
   * (pull mode), no inbound submit* XRPC required. The client handles cross-source
   * deduplication by AT-URI + CID.
   */
  eventStreams?: ATProtoEventStreamsClient;
  /** Period for re-committing the offering record to stay discoverable. */
  offeringRefreshMs?: number;
  /**
   * RFP NSIDs to advertise in the offering record. Overrides the union of
   * provider `appliesTo`. Set for callbackFactory-only bidders with no providers.
   */
  appliesTo?: string[];
  /**
   * When true, the caller already started the serve (e.g. a desktop app with its
   * own HTTP server). Route mounting, offering setup, and firehose watchers still
   * run -- only the serve.beginServe() call is skipped.
   */
  skipServeBegin?: boolean;
  /**
   * Called when the OAuth session expires (refresh token consumed/revoked).
   * The session is dead -- delete the file and re-authenticate.
   */
  onSessionExpired?: (err: OAuthSessionExpiredError) => void;
  /** Pre-created acceptToContract map -- shared with providers for guest event routes. */
  acceptToContract?: Map<string, import("@publicdomainrelay/market-bidder-abc").GuestContractEntry>;
  loop?: MarketLoopRecorder;
  bids?: BidLedger;
  bidRecordTtlMs?: number;
  bidRetentionScanMs?: number;
  writeBudget?: PdsWriteBudget;
}

export interface MarketBidder {
  beginServe(): Promise<void>;
  shutdown(): void;
  /** Re-commits the offering record so relays re-index it immediately. */
  refreshOffering(): Promise<void>;
}

function logAdapter(logger: StructuredLoggerInterface): Logger {
  return (level: string, message: string, meta?: Record<string, unknown>) => {
    logger[level as "info" | "warn" | "error" | "debug"]?.(message, meta);
  };
}


export async function createMarketBidder(config: MarketBidderConfig): Promise<MarketBidder> {
  const { logger, serve, atproto, relay, providers, setup, teardown, callbackFactory, onContractChange, eventStreams, offeringRefreshMs, skipServeBegin, onSessionExpired } = config;
  const policyRegistry = createPolicyRegistry();
  const policyArgs = config.policyArgs ?? {};
  // Legacy --policy name canonicalized to the bidder-side variant
  // (only-me -> bidder-only-me). Unknown/wrong-side names warn and fall back to
  // open (no scope gate), matching the old registry-miss behavior.
  let canonical: string | undefined;
  if (config.policy) {
    try {
      canonical = resolvePolicyName(policyRegistry, config.policy, "bidder");
    } catch (err) {
      logger.warn("bidder scope policy not found in registry", {
        policy: config.policy,
        known: policyRegistry.names(),
        error: String(err),
      });
    }
  }
  // The bidder's own in-memory gha-lite policy record backing the scope gate.
  // Stable synthetic uri/cid so the scope cache keys predictably per canonical
  // name + counterparty + args.
  const scopeRecord = canonical && WORKFLOWS[canonical]
    ? {
        uri: `at://${atproto.did}/policy-gha-lite/${canonical}`,
        cid: canonical,
        value: {
          $type: POLICY_GHA_LITE_NSID,
          name: canonical,
          workflow: WORKFLOWS[canonical],
          createdAt: new Date().toISOString(),
        },
      }
    : undefined;
  const log = logAdapter(logger);
  const activeContracts = new Map<string, ActiveContract>();
  const acceptToContract = config.acceptToContract ?? new Map<string, import("@publicdomainrelay/market-bidder-abc").GuestContractEntry>();
  const terminatedReceipts = new TerminatedReceipts();
  const contractStore = config.contractStore;
  const loop = config.loop;
  config.writeBudget?.trackContracts(() => activeContracts.size);
  const listOwnRecords = async (collection: string, cursor: string | undefined) => {
    const pdsUrl = await resolvePds(atproto.idResolver, atproto.did);
    const url = new URL(`${pdsUrl}/xrpc/com.atproto.repo.listRecords`);
    url.searchParams.set("repo", atproto.did);
    url.searchParams.set("collection", collection);
    url.searchParams.set("limit", "100");
    if (cursor) url.searchParams.set("cursor", cursor);
    const res = await fetch(url, { signal: AbortSignal.timeout(10_000) });
    if (!res.ok) {
      await res.body?.cancel();
      throw new Error(`listRecords ${collection}: ${res.status}`);
    }
    const page = await res.json() as { records?: Array<{ uri: string; cid: string; value: Record<string, unknown> }>; cursor?: string };
    return { records: page.records ?? [], cursor: page.records?.length ? page.cursor : undefined };
  };
  const listOwnBidRecords = (cursor: string | undefined) => listOwnRecords(BID_NSID, cursor);
  const bidLoop = createBidLoop({
    selfDid: atproto.did,
    claimant: `pid-${Deno.pid}-${crypto.randomUUID().slice(0, 8)}`,
    bids: config.bids,
    loop,
    writeBudget: config.writeBudget,
    logger,
    retention: {
      ttlMs: config.bidRecordTtlMs,
      intervalMs: config.bidRetentionScanMs,
      sharedUris: atproto.sharedRecordUris,
      deleteRecord: atproto.deleteRecord,
      deleteRecords: async (records) => {
        await atproto.applyWrites(atproto.did, records.map((r) => ({ action: "delete" as const, ...r })));
      },
      listOwnBidRecords,
      listOwnRecords,
    },
    listOwnBids: async () => {
      const out: Array<{ uri: string; cid: string; rfpUri?: string }> = [];
      let cursor: string | undefined;
      for (let page = 0; page < 50; page++) {
        const got = await listOwnBidRecords(cursor);
        for (const r of got.records) out.push({ uri: r.uri, cid: r.cid, rfpUri: (r.value.rfp as { uri?: string } | undefined)?.uri });
        cursor = got.cursor;
        if (!cursor) break;
      }
      return out;
    },
    knownAccept: (acceptUri, acceptCid) => acceptToContract.has(`${acceptUri}#${acceptCid}`),
    endedAccept: (acceptUri, acceptCid) => {
      const contract = acceptToContract.get(`${acceptUri}#${acceptCid}`);
      const ended = contract ? terminatedReceipts.get(contract.receiptUri) : undefined;
      return contract && ended ? { uri: contract.receiptUri, ended: `${ended.reason} at ${ended.at}` } : undefined;
    },
    ownReceiptFor: async (acceptUri) => {
      const pdsUrl = await resolvePds(atproto.idResolver, atproto.did);
      let receiptUri: string | undefined;
      let cursor: string | undefined;
      do {
        const url = new URL(`${pdsUrl}/xrpc/com.atproto.repo.listRecords`);
        url.searchParams.set("repo", atproto.did);
        url.searchParams.set("collection", RECEIPT_NSID);
        url.searchParams.set("limit", "100");
        if (cursor) url.searchParams.set("cursor", cursor);
        const res = await fetch(url, { signal: AbortSignal.timeout(5_000) });
        if (!res.ok) {
          await res.body?.cancel();
          throw new Error(`listRecords ${RECEIPT_NSID}: ${res.status}`);
        }
        const page = await res.json() as { records?: Array<{ uri: string; value?: { accept?: { uri?: string } } }>; cursor?: string };
        receiptUri = page.records?.find((r) => r.value?.accept?.uri === acceptUri)?.uri;
        cursor = page.records?.length ? page.cursor : undefined;
      } while (!receiptUri && cursor);
      if (!receiptUri) return null;
      const ended = terminatedReceipts.get(receiptUri);
      return { uri: receiptUri, ...(ended ? { ended: `${ended.reason} at ${ended.at}` } : {}) };
    },
  });
  const rfpGate = bidLoop.rfpGate;
  const observeRfp = loop
    ? (o: RfpObservation): void => {
      if (o.outcome === "seen") loop.rfpSeen();
      else if (o.outcome === "scoped") loop.rfpScoped();
      else if (o.outcome === "bid") loop.bid();
      else if (o.outcome === "duplicate") return;
      else loop.declined(declineReasonOf(o.reason ?? "unknown"), o.rfpUri, new Date());
    }
    : undefined;
  const saveContractChange = contractStore || onContractChange || loop
    ? async (event: ContractEvent): Promise<void> => {
      if (event.type === "accepted") loop?.win();
      await contractStore?.save(event);
      await onContractChange?.(event);
      loop?.contractsActive(activeContracts.size);
    }
    : undefined;

  async function loadContracts(): Promise<void> {
    if (!contractStore) return;
    const stored = await contractStore.load();
    const sorted = [...stored.tombstones].sort((a, b) => a.at.localeCompare(b.at));
    for (const t of sorted) {
      terminatedReceipts.add(t.receiptUri, { acceptAuthor: t.acceptAuthor, reason: t.reason, at: t.at });
      if (t.acceptUri && t.acceptCid && t.receiptCid) {
        acceptToContract.set(`${t.acceptUri}#${t.acceptCid}`, {
          receiptKey: `${t.receiptUri}#${t.receiptCid}`, receiptUri: t.receiptUri, receiptCid: t.receiptCid,
        });
      }
    }
    let restored = 0;
    for (const c of stored.contracts) {
      if (terminatedReceipts.has(c.receiptUri)) continue;
      activeContracts.set(c.key, activeContractOf(c));
      if (c.acceptUri && c.acceptCid) {
        acceptToContract.set(`${c.acceptUri}#${c.acceptCid}`, {
          receiptKey: c.key, receiptUri: c.receiptUri, receiptCid: c.receiptCid, submitEventUrl: c.submitEventUrl,
        });
      }
      restored++;
    }
    logger.info("bidder_contracts_loaded", { contracts: restored, tombstones: terminatedReceipts.size });
    loop?.contractsActive(activeContracts.size);
  }
  const idResolver = atproto.idResolver;
  let offeringRefresher: OfferingRefreshHandle | null = null;

  const selfVouchResolver: VouchResolver = createTangledGraphVouchResolver({
    listRecords: async (_repo, coll) => {
      const result = await atproto.listRecords(atproto.did, coll, { limit: 100 });
      return (result?.records as Array<{ uri: string; value: Record<string, unknown> }>) ?? [];
    },
    log: (level, msg, meta) => logger[level as "info" | "warn"]?.(msg, meta),
  });

  const publicVouchResolver: VouchResolver = createTangledGraphVouchResolver({
    listRecords: async (repo, coll) =>
      repo === atproto.did
        ? ((await atproto.listRecords(repo, coll, { limit: 100 }))?.records as Array<{ uri: string; value: Record<string, unknown> }>) ?? []
        : listRecordsPublic(idResolver, repo, coll),
    log: (level, msg, meta) => logger[level as "info" | "warn"]?.(msg, meta),
  });

  // -- Policy engine scope cache ----------------------------------------------
  // Hot-path verdict cache keyed by (policy identity, counterparty DID, args).
  // Firehose trust events (badgeBlueKeys / vouch / association) invalidate the
  // affected DIDs' cached verdicts so a changed operator or vouch cannot leave a
  // stale "no" (or "yes") behind. Negatives carry a short TTL, positives longer.
  const scopeCache = createScopeCache();

  // Network-based operator discovery: read the subject's badgeBlueKeys records
  // and return the CHALLENGE whose keyId is the subject DID and whose service is
  // bidder_associate / requester_associate (that challenge is the operator DID).
  // Canonical shape is {challenge: operator, keyId: associated} -- the operator
  // acknowledges the associated bidder/requester. The old inverted shape
  // {challenge: subject, keyId: operator} is a DIFFERENT subject's
  // acknowledgment and must not be read as this subject's operator.
  // No local trust snapshot to warm -- the scope cache absorbs per-counterparty
  // verdicts. Self-owned operators fall through to the evaluator's
  // `?? selfDid` handling when this returns null.
  const resolveOperatorDid = async (bidderDid: string): Promise<string | null> => {
    try {
      const records = await listRecordsPublic(idResolver, bidderDid, BADGE_BLUE_KEYS_NSID, { limit: 200 });
      for (const rec of records) {
        const v = rec.value as Record<string, unknown>;
        if (v.service !== "bidder_associate" && v.service !== "requester_associate") continue;
        // Canonical: {challenge: operator, keyId: associated} -- the subject is
        // the associated party; the challenge is its operator.
        if (v.keyId === bidderDid && typeof v.challenge === "string" && v.challenge.startsWith("did:")) {
          logger.info("operator resolved from the association record", { did: bidderDid, operatorDid: v.challenge, uri: rec.uri });
          return v.challenge;
        }
      }
      // Legacy inverted shape: {challenge: subject, keyId: operator}. An
      // ephemeral requester/bidder declares its operator with challenge=self.
      // (An OPERATOR's own repo has acknowledgments of the same shape -- those
      // keyIds are its associated DIDs, not its operator -- so this fallback is
      // only a tiebreak for subjects whose canonical lookup found nothing.)
      for (const rec of records) {
        const v = rec.value as Record<string, unknown>;
        if (v.challenge === bidderDid && (v.service === "bidder_associate" || v.service === "requester_associate")) {
          const keyId = v.keyId;
          if (typeof keyId === "string" && keyId.startsWith("did:")) return keyId;
        }
      }
    } catch {
      // non-critical -- no operator association readable for this did
    }
    return null;
  };

  // Policy vouch lookups are operator-delegated (same rationale as the
  // requester side): the ephemeral bidder DID has no vouch records of its own,
  // so a raw lookup would leave tangled-vouch with an empty set. Resolve self
  // -> operator via badgeBlueKeys (bidder_associate / requester_associate),
  // then merge the operator's vouches.
  const delegatedVouchResolver = createBadgeBlueKeysDelegatedTrustResolver({
    vouchResolver: publicVouchResolver,
    listOwnRecords: async (collection, opts) => {
      const result = await atproto.listRecords(atproto.did, collection, { limit: opts?.limit ?? 100 });
      return (result?.records as Array<{ uri: string; value: Record<string, unknown> }>) ?? [];
    },
    log: (level, msg, meta) => logger[level as "info" | "warn"]?.(msg, meta),
  });
  const getVouchedDids = (did: string): Promise<Set<string>> => delegatedVouchResolver.getDelegatedTrustedDids(did);

  async function ensureOperatorAllowlist(service: string): Promise<void> {
    const result = await atproto.listRecords(atproto.did, ALLOWLIST_RBAC_DID_NSID, { limit: 100 });
    for (const rec of result?.records ?? []) {
      const v = rec.value as Record<string, unknown>;
      const protects = v.protects as Record<string, { service: string; scope?: string }> | undefined;
      for (const p of Object.values(protects ?? {})) {
        if (
          (p.service === service || p.service === "*") &&
          (p.scope === "account.auth" || p.scope === "*" || !p.scope)
        ) {
          log("info", "bidder allowlist exists", { uri: rec.uri });
          return;
        }
      }
    }
    const allowlistRef = await atproto.createRecord(ALLOWLIST_RBAC_DID_NSID, {
      $type: ALLOWLIST_RBAC_DID_NSID,
      protects: { allowSelf: { service, scope: "account.auth" } },
      allowed: { allowSelf: [atproto.did] },
      createdAt: new Date().toISOString(),
    });
    log("info", "bidder allowlist created", { uri: allowlistRef.uri, service });
  }

  function buildOffering(createdAt: string): Record<string, unknown> {
    const appliesTo = config.appliesTo ??
      [...new Set((providers ?? []).flatMap((p) => p.appliesTo))];
    return {
      $type: OFFERING_NSID,
      endpointUrl: relay?.ingressUrl || `${atproto.did}#pdr_temp_market`,
      appliesTo,
      createdAt,
      refreshedAt: new Date().toISOString(),
    };
  }

  // Ensures exactly one offering record per bidder DID (one active service =
  // one endpoint). The first (oldest) existing record's rkey becomes the
  // canonical rkey for this bidder's lifetime -- every subsequent write
  // (correction or refresh) updates that same record in place via
  // atproto.updateRecord rather than creating a new one, so the collection
  // never grows past one record.
  async function ensureOffering(): Promise<{ rkey: string; createdAt: string } | null> {
    try {
      const wanted = config.appliesTo ??
        [...new Set((providers ?? []).flatMap((p) => p.appliesTo))];
      const existing = await atproto.listRecords(atproto.did, OFFERING_NSID, { limit: 20 });
      const records = existing?.records ?? [];
      if (records.length) {
        const rec = records[0];
        const rkey = rec.uri.split("/").pop() ?? "";
        const createdAt = (rec.value.createdAt as string) ?? new Date().toISOString();
        const val = rec.value as Record<string, unknown>;
        const appliesTo = (val.appliesTo as string[] | undefined) ?? [];
        const endpointUrl = val.endpointUrl as string | undefined;
        const wantedStr = [...wanted].sort().join(",");
        const haveStr = [...appliesTo].sort().join(",");
        const hasGoodEp = endpointUrl?.startsWith("https://") ?? false;
        const wantedEp = relay?.ingressUrl || `${atproto.did}#pdr_temp_market`;
        const epSame = endpointUrl === wantedEp;
        if (haveStr === wantedStr && hasGoodEp && epSame) {
          log("info", "bidder offering exists (matched)", { uri: rec.uri, appliesTo: haveStr, endpointUrl });
        } else {
          await atproto.updateRecord(OFFERING_NSID, rkey, buildOffering(createdAt));
          log("info", "bidder offering corrected", { uri: rec.uri, appliesTo: wantedStr, endpointUrl: wantedEp, reason: haveStr === wantedStr ? (hasGoodEp ? "endpoint_changed" : "bad_endpoint") : "appliesTo_mismatch" });
        }
        return { rkey, createdAt };
      }
      const createdAt = new Date().toISOString();
      const offeringRef = await atproto.createRecord(OFFERING_NSID, buildOffering(createdAt));
      const rkey = offeringRef.uri.split("/").pop() ?? "";
      log("info", "bidder offering created", { uri: offeringRef.uri });
      return { rkey, createdAt };
    } catch (err) {
      if (err instanceof OAuthSessionExpiredError) {
        logger.error("bidder oauth session expired", {
          sessionPath: err.sessionPath,
          error: err.message,
        });
        onSessionExpired?.(err);
        return null;
      }
      throw err;
    }
  }

  async function beginServe(): Promise<void> {
    await loadContracts();
    const recordResolver = createRecordResolver(idResolver, atproto);

    // Policy engine: registry (record $type -> executor), evaluator, and the
    // bidder's own scope gate. The scope gate is the hot-path engagement check:
    // on a scope-cache miss the evaluator runs the scope-mode workflow against
    // the counterparty DID, otherwise it serves the cached verdict.
    const ghaLiteExecutor = new GhaLiteExecutor({ sandbox: { netOnly: true, fsApi: false } });
    const typescriptExecutor = new TypescriptExecutor();
    const engineRegistry = {
      get: ($type: string) =>
        $type === POLICY_GHA_LITE_NSID ? ghaLiteExecutor
          : $type === POLICY_TYPESCRIPT_NSID ? typescriptExecutor
          : undefined,
      kinds: () => [POLICY_GHA_LITE_NSID, POLICY_TYPESCRIPT_NSID],
    };
    const attachedRegistry = config.policyEngineUrl
      ? createRemoteEngineRegistry({ baseUrl: config.policyEngineUrl })
      : undefined;
    if (config.policyEngineUrl) logger.info("bidder_policy_engine_remote", { url: config.policyEngineUrl });
    const evaluator: PolicyEvaluator = createPolicyEvaluator({
      registry: engineRegistry,
      ...(attachedRegistry ? { attachedRegistry } : {}),
      resolve: (ref) => recordResolver.resolve(ref),
      resolveOperatorDid,
      getVouchedDids,
      scopeCache,
      log: (level, msg, meta) => logger[level as "info" | "warn" | "error" | "debug"]?.(msg, meta),
    });
    const scopeGate = async (counterpartyDid: string): Promise<boolean> => {
      if (!scopeRecord) return true;
      const r = await evaluator.scope({
        policyRecord: scopeRecord,
        perspective: "bidder",
        selfDid: atproto.did,
        counterpartyDid,
        args: policyArgs,
      });
      return r.allow;
    };

    const deps: CallbackFactoryDeps = {
      did: atproto.did,
      repoApi: {} as RepoApi,
      signer: atproto.signer,
      attestationKp: atproto.attestationKp,
      idResolver,
      relay: relay ?? { ingressRef: "", ingressUrl: "", ingressHost: "" },
      ingressProxyHost: "",
      log,
      activeContracts,
      acceptToContract,
      createRecord: atproto.createRecord,
      createRepoRecord: atproto.createRepoRecord,
      createSharedRecord: atproto.createSharedRecord,
      createSignedRepoRecord: atproto.createSignedRepoRecord,
      deleteRecord: atproto.deleteRecord,
      callService: atproto.callService,
      resolve: recordResolver,
      onContractChange: saveContractChange,
      terminatedReceipts,
      saveTombstone: contractStore ? (t) => contractStore.saveTombstone(t) : undefined,
      policyExec: config.policyExec,
      getVouchedDids,
      resolveOperatorDid,
      evaluator,
      bids: bidLoop.ledger,
      scopePolicy: canonical,
    };

    for (const p of providers ?? []) {
      await p.setup?.();
    }
    await setup?.();

    const merged: CallbackSet = {};
    const restores: Array<() => Promise<void>> = [];
    const acceptsInFlight = new Map<string, Promise<HandlerResult>>();
    for (const p of providers ?? []) {
      const cb = p.buildCallbacks(deps);
      if (cb.restore) restores.push(() => cb.restore!());
      if (cb.rfpCallbacks) {
        merged.rfpCallbacks = deepMergeCallbacks(merged.rfpCallbacks ?? {}, cb.rfpCallbacks);
      }
      if (cb.onAccept) {
        merged.onAccept = merged.onAccept ?? cb.onAccept;
      }
      if (cb.eventCallbacks) {
        merged.eventCallbacks = deepMergeCallbacks(merged.eventCallbacks ?? {}, cb.eventCallbacks);
      }
    }

    // Firehose-only mode: initialize empty callbacks so RFP watcher can run
    if (eventStreams && !merged.rfpCallbacks) merged.rfpCallbacks = {};

    if (callbackFactory) {
      const cb = await callbackFactory(deps);
      if (cb.restore) restores.push(() => cb.restore!());
      if (cb.rfpCallbacks) merged.rfpCallbacks = deepMergeCallbacks(merged.rfpCallbacks ?? {}, cb.rfpCallbacks);
      if (cb.onAccept) merged.onAccept = merged.onAccept ?? cb.onAccept;
      if (cb.eventCallbacks) merged.eventCallbacks = deepMergeCallbacks(merged.eventCallbacks ?? {}, cb.eventCallbacks);
    }

    for (const restore of restores) await restore();
    await bidLoop.restore().catch((err: unknown) => logger.warn("bids_restore_failed", { error: String(err) }));

    if (merged.onAccept) {
      const raw = merged.onAccept;
      merged.onAccept = (opts) => {
        const running = acceptsInFlight.get(opts.acceptUri);
        if (running) {
          logger.info("accept_duplicate_delivery", { acceptUri: opts.acceptUri, via: opts.req ? "push" : "firehose", answer: "the accept in flight" });
          return running;
        }
        const work = (async (): Promise<HandlerResult> => {
          const refused = await bidLoop.checkAccept({
            acceptUri: opts.acceptUri,
            acceptCid: opts.acceptCid,
            issuerDid: opts.issuerDid,
            rfp: opts.accept.rfp as { uri: string } | undefined,
            bid: opts.accept.bid as { uri: string } | undefined,
          });
          if (refused) return refused;
          return await raw(opts);
        })();
        acceptsInFlight.set(opts.acceptUri, work);
        const settle = () => {
          if (acceptsInFlight.get(opts.acceptUri) === work) acceptsInFlight.delete(opts.acceptUri);
        };
        work.then(settle, settle);
        return work;
      };
    }

    const marketDeps: MarketServerDeps = {
      hostname: () => relay?.ingressHost || "",
      idResolver,
      resolve: recordResolver,
      log,
    };

    if (merged.rfpCallbacks || merged.onAccept || merged.eventCallbacks || eventStreams) {
      const factory = createMarketFactory(marketDeps, {
        rfp: merged.rfpCallbacks,
        rfpScopeFilter: async ({ issuerDid }) => scopeGate(issuerDid),
        rfpObserve: observeRfp,
        rfpGate,
        accept: merged.onAccept
          ? { serviceIds: [DEFAULT_MARKET_SERVICE_ID], onAccept: merged.onAccept }
          : undefined,
        event: merged.eventCallbacks
          ? { callbacks: merged.eventCallbacks, background: merged.eventBackground ?? true }
          : undefined,
      });
      serve.app.route("/", factory.createApp() as never);
    }

    if (eventStreams) {
      // Trust invalidation: association/vouch/badgeBlueKeys commits drop the
      // scope cache's verdicts for the affected DID, so a changed operator or
      // vouch cannot leave a stale verdict behind. Only Jetstream delivers custom
      // collections; other transports get the scope cache's TTL instead.
      eventStreams.watch({
        wantedCollections: [BADGE_BLUE_KEYS_NSID, VOUCH_NSID, BIDDER_ASSOCIATION_NSID],
        onEvent: (e) => {
          scopeCache.applyEvent({ did: e.did, rkey: e.rkey });
        },
      });

      const dispatchCallbacks = merged.rfpCallbacks ?? {};
      const dispatch = createRfpDispatcher({
        deps: marketDeps,
        callbacks: dispatchCallbacks,
        acceptScopeFilter: ({ issuerDid }) => scopeGate(issuerDid),
        observe: observeRfp,
        gate: rfpGate,
      });
      bidLoop.attachReplay(dispatch);
      eventStreams.watch({
        wantedCollections: [RFP_NSID],
        onEvent: (e) => {
          if (e.operation !== "create" && e.operation !== "update") return;
          log("info", "rfp watch discovered", { rfpUri: e.uri });
          dispatch({ rfpUri: e.uri, rfpCid: e.cid, issuerDid: e.did })
            .catch((err) => log("error", "rfp watch dispatch failed", { rfpUri: e.uri, err: String(err) }));
        },
      });
      logger.info("bidder rfp firehose watches started via eventStreams client");
    }

    // Firehose watcher for ACCEPT_NSID -- fallback when bid.submitAccept is absent.
    // Discovers accept records referencing our bids, dispatches to merged.onAccept.
    if (merged.onAccept && eventStreams) {
      eventStreams.watch({
        wantedCollections: [ACCEPT_NSID],
        onEvent: async (e) => {
          if (e.operation !== "create") return;
          if (!await scopeGate(e.did)) return;
          log("info", "accept watch discovered", { acceptUri: e.uri });
          try {
            const doc = await idResolver.did.resolve(e.did);
            if (!doc) return;
            const pdsUrl = getPdsEndpoint(doc);
            if (!pdsUrl) return;
            const acceptRec = await getRecordAt(pdsUrl, e.uri);
            if (!acceptRec) return;
            const value = acceptRec.value as Record<string, unknown>;
            const bidRef = value.bid as { uri?: string } | undefined;
            if (!bidRef?.uri) return;
            const ownBid = parseAtUri(bidRef.uri).repo === atproto.did;
            if (bidLoop.ledger) {
              const seen = await bidLoop.acceptSeen({
                acceptUri: e.uri,
                acceptCid: e.cid,
                issuerDid: e.did,
                rfp: value.rfp as { uri: string } | undefined,
                bid: { uri: bidRef.uri },
              });
              if (seen !== "ours" || !ownBid) return;
            } else {
              try {
                const { repo, collection, rkey } = parseAtUri(bidRef.uri);
                if (!ownBid) return;
                const existing = await atproto.getRecord(repo, collection, rkey);
                if (!existing) return;
              } catch { return; }
            }
            log("info", "accept watch matched own bid", { acceptUri: e.uri, bidUri: bidRef.uri });
            await merged.onAccept!({
              acceptUri: e.uri,
              acceptCid: e.cid,
              accept: value as Parameters<typeof merged.onAccept>[0]["accept"],
              issuerDid: e.did,
              resolve: recordResolver,
              log,
              req: new Request("https://localhost"),
            });
          } catch (err) {
            log("error", "accept watch dispatch failed", { acceptUri: e.uri, err: String(err) });
          }
        },
      });
      logger.info("bidder accept firehose watch started via eventStreams client");
    }

    // Firehose watcher for EVENT_NSID -- fallback when accept.submitEvent is absent.
    // Routes lifecycle events to merged.eventCallbacks by payload NSID.
    if (merged.eventCallbacks && eventStreams) {
      eventStreams.watch({
        wantedCollections: [EVENT_NSID],
        onEvent: async (e) => {
          if (e.operation !== "create") return;
          log("info", "event watch discovered", { eventUri: e.uri });
          try {
            const doc = await idResolver.did.resolve(e.did);
            if (!doc) return;
            const pdsUrl = getPdsEndpoint(doc);
            if (!pdsUrl) return;
            const eventRec = await getRecordAt(pdsUrl, e.uri);
            if (!eventRec) return;
            const value = eventRec.value as Record<string, unknown>;
            const payload = value.payload as { $type?: string; uri?: string; cid?: string } | undefined;
            if (!payload) return;
            // market.event.payload is a strongRef, so its $type is the ref type
            // ("com.atproto.repo.strongRef"), not the payload's NSID -- keying the
            // handlers off it silently matched nothing and dropped every event
            // (vm.delete included). The NSID is the referenced URI's collection.
            const payloadNsid = payload.$type && payload.$type !== "com.atproto.repo.strongRef"
              ? payload.$type
              : payload.uri?.split("/")[3];
            if (!payloadNsid) return;
            const handlers = merged.eventCallbacks!["pdr_temp_compute_event"];
            if (!handlers) return;
            const handler = handlers[payloadNsid];
            if (!handler) return;
            log("info", "event watch dispatching", { eventUri: e.uri, payloadNsid });
            const ctx = {
              uri: eventRec.uri,
              cid: eventRec.cid,
              event: value as Parameters<typeof handler>[0]["event"],
              payloadNsid,
              issuerDid: e.did,
              serviceId: "pdr_temp_compute_event",
              resolve: recordResolver,
              log,
              req: new Request("https://localhost"),
            };
            await handler(ctx);
          } catch (err) {
            log("error", "event watch dispatch failed", { eventUri: e.uri, err: String(err) });
          }
        },
      });
      logger.info("bidder event firehose watch started via eventStreams client");
    }

    serve.onConnected(async () => {
      await ensureOperatorAllowlist("");
      const offeringResult = await ensureOffering();
      if (!offeringResult) {
        logger.warn("bidder offering skipped -- session expired");
        return;
      }
      const { rkey: offeringRkey, createdAt: offeringCreatedAt } = offeringResult;
      if (offeringRefreshMs && offeringRkey) {
        offeringRefresher = startOfferingRefresh({
          intervalMs: offeringRefreshMs,
          log,
          refresh: async () => {
            try {
              await atproto.updateRecord(OFFERING_NSID, offeringRkey, buildOffering(offeringCreatedAt));
            } catch (err) {
              if (err instanceof OAuthSessionExpiredError) {
                logger.error("bidder offering refresh failed -- session expired", {
                  sessionPath: err.sessionPath,
                });
                onSessionExpired?.(err);
                return;
              }
              /* best-effort for other errors */
            }
            log("info", "bidder offering refreshed", { rkey: offeringRkey });
          },
        });
        logger.info("bidder offering refresh started", { intervalMs: offeringRefreshMs });
      }
    });

    if (!skipServeBegin) {
      logger.info("bidder starting serve");
      await serve.beginServe();
      // offering already created by onConnected callback during serve.beginServe()
    }

    logger.info("bidder ready", { did: atproto.did });
  }

  function shutdown(): void {
    eventStreams?.close();
    bidLoop.close();
    offeringRefresher?.stop();
    activeContracts.clear();
    for (const p of providers ?? []) {
      p.teardown?.().catch(() => {});
    }
    teardown?.().catch(() => {});
    if (!skipServeBegin) serve.shutdown();
  }

  const refreshOffering = () => ensureOffering().then((r) => { if (!r) logger.warn("bidder refreshOffering skipped -- session expired"); });
  return { beginServe, shutdown, refreshOffering };
}
