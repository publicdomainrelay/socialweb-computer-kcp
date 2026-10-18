import type { BidEntry, BidLedger } from "@publicdomainrelay/market-bidder-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { isRateLimitError } from "@publicdomainrelay/pds-write-budget-abc";

export const DEFAULT_BID_RECORD_TTL_MS = 24 * 60 * 60 * 1000;
export const DEFAULT_BID_RETENTION_SCAN_MS = 10 * 60 * 1000;
export const DEFAULT_BID_RETENTION_PASS_MS = 5 * 60 * 1000;
export const DEFAULT_BID_RETENTION_FIRST_MS = 30_000;
export const BID_RETENTION_BATCH = 100;

export interface OwnBidRecordPage {
  records: Array<{ uri: string; cid: string; value: Record<string, unknown> }>;
  cursor?: string;
}

export const RECEIPT_COLLECTION = "com.publicdomainrelay.temp.market.receipt";
export const EVENT_COLLECTION = "com.publicdomainrelay.temp.market.event";

export interface BidRetentionOptions {
  bids: BidLedger;
  selfDid: string;
  ttlMs?: number;
  intervalMs?: number;
  passMs?: number;
  listOwnBidRecords(cursor: string | undefined): Promise<OwnBidRecordPage>;
  listOwnRecords?(collection: string, cursor: string | undefined): Promise<OwnBidRecordPage>;
  deleteRecord(collection: string, rkey: string): Promise<void>;
  deleteRecords?(records: Array<{ collection: string; rkey: string }>): Promise<void>;
  keepBidUri?(uri: string): boolean;
  sharedUris?(): ReadonlySet<string>;
  logger: StructuredLoggerInterface;
  now?: () => number;
}

export interface BidRetentionReport {
  scanned: number;
  deleted: number;
  dependents: number;
  trail: number;
  kept: number;
  halted?: boolean;
  timedOut?: boolean;
}

export interface BidRetention {
  sweep(): Promise<BidRetentionReport>;
  start(): void;
  stop(): void;
}

function refOf(value: unknown): string | undefined {
  const uri = (value as { uri?: unknown } | undefined)?.uri;
  return typeof uri === "string" ? uri : undefined;
}

export function parseAtUri(uri: string): { did: string; collection: string; rkey: string } | undefined {
  const m = /^at:\/\/([^/]+)\/([^/]+)\/([^/]+)$/.exec(uri);
  return m ? { did: m[1], collection: m[2], rkey: m[3] } : undefined;
}

interface Removal {
  done: Set<string>;
  halted: boolean;
  timedOut: boolean;
}

export function createBidRetention(opts: BidRetentionOptions): BidRetention {
  const ttlMs = opts.ttlMs ?? DEFAULT_BID_RECORD_TTL_MS;
  const passMs = opts.passMs ?? DEFAULT_BID_RETENTION_PASS_MS;
  const now = opts.now ?? Date.now;
  let timer: ReturnType<typeof setInterval> | undefined;
  let first: ReturnType<typeof setTimeout> | undefined;
  let running: Promise<BidRetentionReport> | undefined;

  async function removeOwn(uri: string): Promise<void> {
    const at = parseAtUri(uri);
    if (!at || at.did !== opts.selfDid) return;
    await opts.deleteRecord(at.collection, at.rkey).catch((err: unknown) => {
      if (isRateLimitError(err) || !/404|not ?found|RecordNotFound/i.test(String(err))) throw err;
      opts.logger.warn("bid_record_already_absent", { uri, error: String(err) });
    });
  }

  async function removeMany(uris: string[], deadline: number): Promise<Removal> {
    const out: Removal = { done: new Set(), halted: false, timedOut: false };
    const own: Array<{ uri: string; collection: string; rkey: string }> = [];
    for (const uri of new Set(uris)) {
      const at = parseAtUri(uri);
      if (!at || at.did !== opts.selfDid) out.done.add(uri);
      else own.push({ uri, collection: at.collection, rkey: at.rkey });
    }
    for (let i = 0; i < own.length; i += BID_RETENTION_BATCH) {
      if (now() >= deadline) {
        out.timedOut = true;
        return out;
      }
      const chunk = own.slice(i, i + BID_RETENTION_BATCH);
      if (opts.deleteRecords) {
        try {
          await opts.deleteRecords(chunk.map(({ collection, rkey }) => ({ collection, rkey })));
          for (const c of chunk) out.done.add(c.uri);
          continue;
        } catch (err) {
          if (isRateLimitError(err)) {
            opts.logger.warn("bid_record_retention_failed", { records: chunk.length, error: String(err), halted: true });
            out.halted = true;
            return out;
          }
        }
      }
      for (const c of chunk) {
        try {
          await removeOwn(c.uri);
          out.done.add(c.uri);
        } catch (err) {
          out.halted = isRateLimitError(err);
          opts.logger.warn("bid_record_retention_failed", { uri: c.uri, error: String(err), halted: out.halted });
          if (out.halted) return out;
        }
      }
    }
    return out;
  }

  function due(record: { value: Record<string, unknown> }, entry: BidEntry | undefined): boolean {
    if (entry) {
      if (!entry.settledAt) return false;
      return now() - Date.parse(entry.settledAt) >= ttlMs;
    }
    const created = Date.parse(String(record.value.createdAt ?? ""));
    return Number.isFinite(created) && now() - created >= ttlMs;
  }

  function addDependents(into: Set<string>, record: { value: Record<string, unknown> }): void {
    for (const uri of [refOf(record.value.payload), refOf(record.value.bidConfig)]) if (uri) into.add(uri);
  }

  async function listAll(list: (cursor: string | undefined) => Promise<OwnBidRecordPage>, deadline: number): Promise<OwnBidRecordPage["records"] | undefined> {
    const out: OwnBidRecordPage["records"] = [];
    let cursor: string | undefined;
    do {
      if (now() >= deadline) return undefined;
      const got = await list(cursor);
      out.push(...got.records);
      cursor = got.cursor;
    } while (cursor);
    return out;
  }

  async function sweepTrail(
    list: NonNullable<BidRetentionOptions["listOwnRecords"]>,
    liveBids: Set<string>,
    report: BidRetentionReport,
    deadline: number,
  ): Promise<Removal> {
    const receipts = await listAll((c) => list(RECEIPT_COLLECTION, c), deadline);
    const events = receipts ? await listAll((c) => list(EVENT_COLLECTION, c), deadline) : undefined;
    if (!receipts || !events) return { done: new Set(), halted: false, timedOut: true };
    const liveReceipts = new Set<string>();
    const goneReceipts: string[] = [];
    for (const r of receipts) {
      const bidUri = refOf(r.value.bid);
      const created = Date.parse(String(r.value.createdAt ?? ""));
      const old = !Number.isFinite(created) || now() - created >= ttlMs;
      if (bidUri && !liveBids.has(bidUri) && old) goneReceipts.push(r.uri);
      else liveReceipts.add(r.uri);
    }
    const shared = opts.sharedUris?.() ?? new Set<string>();
    const trail: string[] = [];
    for (const e of events) {
      const receiptUri = refOf(e.value.receipt);
      if (!receiptUri || liveReceipts.has(receiptUri)) continue;
      trail.push(e.uri);
      const payload = refOf(e.value.payload);
      if (payload && !shared.has(payload)) trail.push(payload);
    }
    const removal = await removeMany([...trail, ...goneReceipts], deadline);
    report.trail += removal.done.size;
    return removal;
  }

  async function run(): Promise<BidRetentionReport> {
    const deadline = now() + passMs;
    const report: BidRetentionReport = { scanned: 0, deleted: 0, kept: 0, dependents: 0, trail: 0 };
    const byRfp = new Map<string, BidEntry>();
    for (const e of await opts.bids.list()) byRfp.set(e.rfp.uri, e);
    const records: OwnBidRecordPage["records"] = [];
    let cursor: string | undefined;
    do {
      if (now() >= deadline) break;
      const out = await opts.listOwnBidRecords(cursor);
      records.push(...out.records);
      cursor = out.cursor;
    } while (cursor);
    const scanComplete = cursor === undefined;
    const liveBids = new Set<string>();
    const live = new Set<string>();
    const dueRecords: OwnBidRecordPage["records"] = [];
    for (const record of records) {
      report.scanned++;
      const rfpUri = refOf(record.value.rfp);
      const entry = rfpUri ? byRfp.get(rfpUri) : undefined;
      if (opts.keepBidUri?.(record.uri) || !due(record, entry)) {
        report.kept++;
        liveBids.add(record.uri);
        addDependents(live, record);
      } else {
        dueRecords.push(record);
      }
    }
    const bids = await removeMany(dueRecords.map((r) => r.uri), deadline);
    const orphaned = new Set<string>();
    for (const record of dueRecords) {
      if (bids.done.has(record.uri)) {
        report.deleted++;
        addDependents(orphaned, record);
      } else {
        report.kept++;
        liveBids.add(record.uri);
        addDependents(live, record);
      }
    }
    let halted = bids.halted;
    let timedOut = bids.timedOut || !scanComplete;
    if (!halted) {
      const shared = opts.sharedUris?.() ?? new Set<string>();
      const dependents = await removeMany([...orphaned].filter((uri) => !live.has(uri) && !shared.has(uri)), deadline);
      report.dependents += dependents.done.size;
      halted = dependents.halted;
      timedOut ||= dependents.timedOut;
    }
    if (!halted && !timedOut && opts.listOwnRecords) {
      const trail = await sweepTrail(opts.listOwnRecords, liveBids, report, deadline);
      halted = trail.halted;
      timedOut ||= trail.timedOut;
    }
    if (halted) report.halted = true;
    if (timedOut) report.timedOut = true;
    if (report.deleted > 0 || report.dependents > 0 || report.trail > 0 || timedOut) opts.logger.info("bid_records_removed", { ...report, ttlMs, passMs });
    return report;
  }

  function sweep(): Promise<BidRetentionReport> {
    running ??= run().finally(() => {
      running = undefined;
    });
    return running;
  }

  return {
    sweep,
    start() {
      if (timer !== undefined) return;
      const run = () => sweep().catch((err: unknown) => opts.logger.warn("bid_record_sweep_failed", { error: String(err) }));
      timer = setInterval(run, opts.intervalMs ?? DEFAULT_BID_RETENTION_SCAN_MS);
      first = setTimeout(run, DEFAULT_BID_RETENTION_FIRST_MS);
      Deno.unrefTimer(timer as unknown as number);
      Deno.unrefTimer(first as unknown as number);
    },
    stop() {
      if (timer !== undefined) clearInterval(timer);
      if (first !== undefined) clearTimeout(first);
      timer = undefined;
      first = undefined;
    },
  };
}
