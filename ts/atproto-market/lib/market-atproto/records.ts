import type { Agent } from "@atproto/api";
import { getPdsEndpoint } from "@atproto/common-web";
import type { IdResolver } from "@atproto/identity";
import { parseAtUri, type RecordRef, type RecordResolver } from "@publicdomainrelay/market-abc";
import { strongRef, OFFERING_NSID, RECEIPT_NSID, type Logger, type Resolved, type StrongRef } from "@publicdomainrelay/market-common";
import { fetchWithRetry } from "@publicdomainrelay/http-retry-fetch";
import { createRemoteProofRecord, type RecordSigner } from "./signing.ts";

const PUBLIC_READ_RETRY = { maxTotalWaitMs: 30_000, maxAttempts: 5 };

export function publicGet(url: string, timeoutMs = 10_000): Promise<Response> {
  return fetchWithRetry(() => fetch(url, { signal: AbortSignal.timeout(timeoutMs) }), {
    label: `GET ${new URL(url).pathname}`,
    policy: PUBLIC_READ_RETRY,
  });
}

/**
 * Create a record in the agent's own repo and return a strongRef to it. The
 * record should carry its own `$type` matching `collection`, the same as a
 * direct `com.atproto.repo.createRecord` call.
 */
export async function createRecord(
  agent: Agent,
  collection: string,
  record: Record<string, unknown>,
): Promise<StrongRef> {
  const res = await agent.com.atproto.repo.createRecord({
    repo: agent.assertDid,
    collection,
    record,
  });
  return strongRef(res.data.uri, res.data.cid);
}

/** The strongRef'd records a market.receipt binds together. */
export interface ReceiptRefs {
  rfp: RecordRef;
  bid: RecordRef;
  accept: RecordRef;
  /** Proof-of-settlement record (the bid's payment/grant receipt). Optional -- free settlement has no separate receipt. */
  payload?: RecordRef;
  /** Service DID reference (did:web:HOST#compute_event) where teardown events are sent. */
  submitEvent: string;
}

/** The accept this receipt attests, as it lives in the requester's repo. */
export interface ReceiptSubject {
  /** The resolved accept record value (its `signatures` array is ignored). */
  acceptRecord: Record<string, unknown>;
  /** DID of the repository the accept lives in (the requester). */
  acceptRepositoryDid: string;
}

/**
 * Mint a market.receipt in the agent's own repo, binding the rfp, bid, accept,
 * and proof-of-settlement payload, and advertising where to send teardown
 * events. The receipt is a badge.blue remote attestation proof over the accept
 * (its `cid`) and carries the provider's inline signature. Returns a strongRef
 * to the created receipt.
 */
export function createReceiptRecord(
  agent: Agent,
  refs: ReceiptRefs,
  subject: ReceiptSubject,
  signer: RecordSigner,
): Promise<StrongRef> {
  return createRemoteProofRecord(
    agent,
    RECEIPT_NSID,
    {
      $type: RECEIPT_NSID,
      rfp: strongRef(refs.rfp.uri, refs.rfp.cid),
      bid: strongRef(refs.bid.uri, refs.bid.cid),
      accept: strongRef(refs.accept.uri, refs.accept.cid),
      payload: refs.payload ? strongRef(refs.payload.uri, refs.payload.cid) : null,
      submitEvent: refs.submitEvent,
      createdAt: new Date().toISOString(),
    },
    { subjectRecord: subject.acceptRecord, subjectRepositoryDid: subject.acceptRepositoryDid },
    signer,
  );
}

/** Delete a record the agent authored, addressed by its strongRef (uri parsed). */
export async function deleteRecord(agent: Agent, ref: { uri: string }): Promise<void> {
  const { repo, collection, rkey } = parseAtUri(ref.uri);
  await agent.com.atproto.repo.deleteRecord({ repo, collection, rkey });
}

/** A record as returned by com.atproto.repo.listRecords. */
export type ListedRecord = { uri: string; cid: string; value: Record<string, unknown> };

/**
 * List every record in a repo's collection, following the cursor to exhaustion.
 * Reads over plain HTTP against the given PDS (no auth needed for public repos),
 * matching how producers discover, e.g., a bidder's offering collection. Stops
 * (returning what it has) on the first non-OK response.
 */
export async function listRecordsAll(
  pdsUrl: string,
  repo: string,
  collection: string,
  opts: { limit?: number; timeoutMs?: number } = {},
): Promise<ListedRecord[]> {
  const limit = opts.limit ?? 100;
  const timeoutMs = opts.timeoutMs ?? 10000;
  const out: ListedRecord[] = [];
  let cursor: string | undefined;
  // PDS caps listRecords at 100/page; a larger requested limit must paginate,
  // not get sent verbatim (the PDS 400s and the read comes back empty).
  const pageLimit = Math.min(limit, 100);
  do {
    const url = new URL(`${pdsUrl}/xrpc/com.atproto.repo.listRecords`);
    url.searchParams.set("repo", repo);
    url.searchParams.set("collection", collection);
    url.searchParams.set("limit", String(pageLimit));
    if (cursor) url.searchParams.set("cursor", cursor);
    const res = await publicGet(url.toString(), timeoutMs);
    if (!res.ok) break;
    const data = await res.json() as { records: Array<{ uri: string; cid: string; value: unknown }>; cursor?: string };
    for (const r of data.records) out.push({ uri: r.uri, cid: r.cid, value: r.value as Record<string, unknown> });
    cursor = data.cursor;
  } while (cursor);
  return out;
}

export async function getRecordAt(
  pdsUrl: string,
  uri: string,
  opts: { timeoutMs?: number } = {},
): Promise<ListedRecord | null> {
  const { repo, collection, rkey } = parseAtUri(uri);
  const url = new URL(`${pdsUrl}/xrpc/com.atproto.repo.getRecord`);
  url.searchParams.set("repo", repo);
  url.searchParams.set("collection", collection);
  url.searchParams.set("rkey", rkey);
  const res = await publicGet(url.toString(), opts.timeoutMs ?? 10000);
  if (!res.ok) {
    await res.body?.cancel();
    return null;
  }
  const data = await res.json() as { uri?: string; cid?: string; value?: unknown };
  if (!data.value || typeof data.value !== "object") return null;
  return { uri: data.uri ?? uri, cid: data.cid ?? "", value: data.value as Record<string, unknown> };
}

export async function scanRecords(
  pdsUrl: string,
  repo: string,
  collection: string,
  visit: (record: ListedRecord) => boolean | void | Promise<boolean | void>,
  opts: { pageLimit?: number; timeoutMs?: number } = {},
): Promise<boolean> {
  const pageLimit = Math.min(opts.pageLimit ?? 100, 100);
  const timeoutMs = opts.timeoutMs ?? 10000;
  let cursor: string | undefined;
  do {
    const url = new URL(`${pdsUrl}/xrpc/com.atproto.repo.listRecords`);
    url.searchParams.set("repo", repo);
    url.searchParams.set("collection", collection);
    url.searchParams.set("limit", String(pageLimit));
    if (cursor) url.searchParams.set("cursor", cursor);
    const res = await publicGet(url.toString(), timeoutMs);
    if (!res.ok) {
      await res.body?.cancel();
      return false;
    }
    const data = await res.json() as { records: Array<{ uri: string; cid: string; value: unknown }>; cursor?: string };
    for (const r of data.records ?? []) {
      if (await visit({ uri: r.uri, cid: r.cid, value: r.value as Record<string, unknown> })) return true;
    }
    cursor = data.records?.length ? data.cursor : undefined;
  } while (cursor);
  return false;
}

export async function findRecord(
  pdsUrl: string,
  repo: string,
  collection: string,
  match: (record: ListedRecord) => boolean,
  opts: { pageLimit?: number; timeoutMs?: number } = {},
): Promise<ListedRecord | null> {
  let found: ListedRecord | null = null;
  await scanRecords(pdsUrl, repo, collection, (r) => {
    if (!match(r)) return false;
    found = r;
    return true;
  }, opts);
  return found;
}

/** Resolve a DID to its PDS service endpoint via the supplied IdResolver. */
export async function resolvePds(idResolver: IdResolver, did: string): Promise<string> {
  const doc = await idResolver.did.resolve(did);
  if (!doc) throw new Error(`could not resolve did ${did}`);
  const pds = getPdsEndpoint(doc);
  if (!pds) throw new Error(`no pds endpoint for ${did}`);
  return pds;
}

/** Read all records from a repo's collection via plain HTTP (no auth needed for public repos). Resolves DID to PDS, then calls listRecordsAll. */
export async function listRecordsPublic(
  idResolver: IdResolver,
  repo: string,
  collection: string,
  opts?: { limit?: number; timeoutMs?: number },
): Promise<ListedRecord[]> {
  const pdsUrl = await resolvePds(idResolver, repo);
  return listRecordsAll(pdsUrl, repo, collection, opts);
}

export interface OfferingRefreshHandle {
  stop(): void;
}

/**
 * Periodically re-commit a record (timer owned here, in the impl layer). The
 * caller's `refresh` should rewrite the offering with a bumped timestamp so the
 * record bytes change and the PDS emits a fresh `#commit`, keeping the offering
 * live in relay/collectiondir indexes and the firehose connection warm.
 */
export function startOfferingRefresh(opts: {
  intervalMs: number;
  refresh: () => Promise<void>;
  log?: Logger;
}): OfferingRefreshHandle {
  const { intervalMs, refresh, log } = opts;
  const timer = setInterval(() => {
    refresh().catch((err) => log?.("warn", "offering refresh failed", { err: String(err) }));
  }, intervalMs);
  (Deno as { unrefTimer?: (id: number) => void }).unrefTimer?.(timer as unknown as number);
  return { stop: () => clearInterval(timer) };
}

export interface OwnRepo {
  did: string;
  getRecord(did: string, collection: string, rkey: string): Promise<{ uri: string; cid: string; value: Record<string, unknown> } | null>;
}

export function createRecordResolver(idResolver: IdResolver, own?: OwnRepo): RecordResolver {
  return {
    async resolve<T>(ref: RecordRef): Promise<Resolved<T>> {
      const { repo, collection, rkey } = parseAtUri(ref.uri);
      if (own && repo === own.did) {
        const rec = await own.getRecord(repo, collection, rkey);
        if (!rec) throw new Error(`getRecord failed: ${ref.uri} is not in the own repo`);
        return { ...rec.value, _uri: rec.uri, _cid: rec.cid || ref.cid } as Resolved<T>;
      }
      const doc = await idResolver.did.resolve(repo);
      if (!doc) throw new Error(`could not resolve did ${repo}`);
      const pds = getPdsEndpoint(doc);
      if (!pds) throw new Error(`no pds endpoint for ${repo}`);
      const url = `${pds}/xrpc/com.atproto.repo.getRecord?repo=${repo}&collection=${collection}&rkey=${rkey}`;
      const res = await publicGet(url);
      if (!res.ok) {
        await res.body?.cancel();
        throw new Error(`getRecord failed: ${res.status}`);
      }
      const data = await res.json() as { uri: string; cid?: string; value: T };
      return { ...data.value, _uri: data.uri, _cid: data.cid ?? ref.cid } as Resolved<T>;
    },
  };
}
