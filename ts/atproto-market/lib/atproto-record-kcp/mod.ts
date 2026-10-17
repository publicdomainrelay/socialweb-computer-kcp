import type { KcpClient } from "@publicdomainrelay/kcp-client-abc";
import type { KcpResource } from "@publicdomainrelay/kcp-client-common";
import type { Did, RepoApi } from "@publicdomainrelay/atproto-repo-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { IdResolver, MemoryCache } from "@atproto/identity";
import { createDefaultATProtoEventStreamsClient } from "@publicdomainrelay/atproto-event-streams-client";
import {
  API_RESOURCES,
  ATPROTO_RECORD_RESOURCE,
  ATPROTO_RECORD_WATCH_RESOURCE,
  type AtprotoRecord,
  type AtprotoRecordStatus,
  type AtprotoRecordWatch,
  type AtprotoRecordWatchStatus,
  type Condition,
  FINALIZER_ATPROTO_RECORD,
  type WatchedRecord,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export const CONDITION_REFERENCES_RESOLVED = "ReferencesResolved";

export interface ValueFrom {
  kind?: string;
  name: string;
  fieldPath: string;
  apiVersion?: string;
  resource?: string;
  workspace?: string;
  namespace?: string;
}

export interface AtprotoRecordReconcilerOpts {
  kcp: KcpClient;
  workspace: string;
  repoName: string;
  did: string;
  repo: RepoApi;
  logger: StructuredLoggerInterface;
  intervalMs?: number;
}

export interface AtprotoRecordReconciler {
  wake(): void;
  stop(): void;
}

const DENO_POD_RESOURCE: KcpResource = { group: "deno.computer", version: "v1alpha1", plural: "denopods", kind: "DenoPod" };

type Spec = NonNullable<AtprotoRecord["spec"]>;
type Held = AtprotoRecord & { spec: Spec };

function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v && typeof v === "object") {
    return `{${Object.keys(v as Record<string, unknown>).sort().map((k) => `${JSON.stringify(k)}:${canonical((v as Record<string, unknown>)[k])}`).join(",")}}`;
  }
  return JSON.stringify(v);
}

export function valueFromOf(v: unknown): ValueFrom | null {
  if (!v || typeof v !== "object" || Array.isArray(v)) return null;
  const keys = Object.keys(v);
  if (keys.length !== 1 || keys[0] !== "valueFrom") return null;
  const r = (v as { valueFrom: ValueFrom }).valueFrom;
  return r && (typeof r.kind === "string" || typeof r.resource === "string") && typeof r.name === "string" && typeof r.fieldPath === "string" ? r : null;
}

export function fieldAt(obj: unknown, path: string): unknown {
  let cur: unknown = obj;
  for (const part of path.replace(/\[(\d+)\]/g, ".$1").split(".").filter(Boolean)) {
    if (cur === null || cur === undefined || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
}

function resourceOf(r: ValueFrom): KcpResource {
  const known = [...API_RESOURCES, DENO_POD_RESOURCE].find((a) => r.resource ? a.plural === r.resource : a.kind === r.kind);
  if (known && !r.apiVersion) return known;
  if (r.resource && r.apiVersion) {
    const [group, version] = r.apiVersion.includes("/") ? r.apiVersion.split("/") : ["", r.apiVersion];
    return { group, version, plural: r.resource, kind: r.kind };
  }
  throw new Error(`valueFrom ${r.resource ?? r.kind} is not a socialweb.computer resource or denopods; name its apiVersion and resource`);
}

export interface MissingReference {
  what: string;
  unreadable: boolean;
}

export async function resolveValueFrom(kcp: KcpClient, workspace: string, v: unknown, missing: MissingReference[]): Promise<unknown> {
  const ref = valueFromOf(v);
  if (ref) {
    const resource = resourceOf(ref);
    const ws = ref.workspace ?? workspace;
    const what = `${resource.plural} ${ref.name} ${ref.fieldPath} in ${ws}`;
    const t = { resource, workspace: ws, namespace: ref.namespace ?? (resource.plural === DENO_POD_RESOURCE.plural ? "default" : undefined) };
    let obj: unknown;
    try {
      obj = await kcp.get(t, ref.name);
    } catch (err) {
      missing.push({ what: `${what}: ${err instanceof Error ? err.message : String(err)}`, unreadable: true });
      return undefined;
    }
    const value = obj ? fieldAt(obj, ref.fieldPath) : undefined;
    if (value === undefined || value === null || value === "") {
      missing.push({ what: `${what}${obj ? " is empty" : " does not exist"}`, unreadable: false });
      return undefined;
    }
    return value;
  }
  if (Array.isArray(v)) return await Promise.all(v.map((x) => resolveValueFrom(kcp, workspace, x, missing)));
  if (v && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) out[k] = await resolveValueFrom(kcp, workspace, x, missing);
    return out;
  }
  return v;
}

export function startAtprotoRecordReconciler(opts: AtprotoRecordReconcilerOpts): AtprotoRecordReconciler {
  const { kcp, logger, repo, repoName } = opts;
  const did = opts.did as Did;
  const target = { resource: ATPROTO_RECORD_RESOURCE, workspace: opts.workspace };
  const ac = new AbortController();
  let wake: (() => void) | undefined;

  async function setStatus(o: Held, next: AtprotoRecordStatus, cond: { status: "True" | "False"; reason: string; message: string }): Promise<void> {
    const prior = (o.status?.conditions ?? []).find((c) => c.type === CONDITION_REFERENCES_RESOLVED);
    const changed = !prior || prior.status !== cond.status || prior.reason !== cond.reason || prior.message !== cond.message;
    const condition = changed
      ? { type: CONDITION_REFERENCES_RESOLVED, ...cond, observedGeneration: o.metadata?.generation, lastTransitionTime: new Date().toISOString() }
      : prior;
    const status = { ...(o.status ?? {}), ...next, conditions: [condition, ...(o.status?.conditions ?? []).filter((c) => c.type !== CONDITION_REFERENCES_RESOLVED)] };
    if (canonical(o.status ?? {}) === canonical(status)) return;
    await kcp.patchStatus(target, o.metadata!.name!, (current) => ({ ...((current as AtprotoRecord)?.status ?? {}), ...status }));
  }

  async function reconcile(o: Held): Promise<void> {
    const name = o.metadata!.name!;
    const finalizers = o.metadata?.finalizers ?? [];
    const { collection, rkey } = o.spec;
    if (o.metadata?.deletionTimestamp) {
      if (!finalizers.includes(FINALIZER_ATPROTO_RECORD)) return;
      if (await repo.getRecord(did, collection, rkey)) await repo.applyWrites(did, [{ action: "delete", collection, rkey }]);
      await kcp.patch(target, name, { type: "merge", body: { metadata: { finalizers: finalizers.filter((f) => f !== FINALIZER_ATPROTO_RECORD) } } });
      logger.info("atproto_record_deleted", { name, collection, rkey });
      return;
    }
    if (!finalizers.includes(FINALIZER_ATPROTO_RECORD)) {
      await kcp.patch(target, name, { type: "merge", body: { metadata: { finalizers: [...finalizers, FINALIZER_ATPROTO_RECORD] } } });
    }
    const missing: MissingReference[] = [];
    const resolved = await resolveValueFrom(kcp, opts.workspace, o.spec.record ?? {}, missing) as Record<string, unknown>;
    if (missing.length) {
      const removed = o.spec.onMissingReference === "Delete" && missing.every((m) => !m.unreadable) && await repo.getRecord(did, collection, rkey);
      if (removed) {
        await repo.applyWrites(did, [{ action: "delete", collection, rkey }]);
        logger.info("atproto_record_withdrawn", { name, collection, rkey, missing: missing.map((m) => m.what) });
      }
      return await setStatus(o, { phase: "Pending", observedGeneration: o.metadata?.generation, uri: "", cid: "" }, {
        status: "False",
        reason: "MissingReference",
        message: `waiting for ${missing.map((m) => m.what).join("; ")}`,
      });
    }
    const record = { $type: collection, ...resolved };
    const held = await repo.getRecord(did, collection, rkey);
    if (!held || canonical(held.value) !== canonical(record)) {
      await repo.applyWrites(did, [{ action: held ? "update" : "create", collection, rkey, record }]);
      logger.info("atproto_record_applied", { name, collection, rkey, action: held ? "update" : "create" });
    }
    const now = await repo.getRecord(did, collection, rkey);
    await setStatus(o, {
      phase: now ? "Applied" : "Failed",
      observedGeneration: o.metadata?.generation,
      did,
      uri: now?.uri ?? "",
      cid: String(now?.cid ?? ""),
      message: now ? "" : "the record is not in the repo after the write",
    }, { status: "True", reason: "Resolved", message: "" });
  }

  async function pass(): Promise<void> {
    const list = await kcp.list(target);
    for (const raw of list.items) {
      const o = raw as Held;
      if (o.spec?.repo !== repoName) continue;
      try {
        await reconcile(o);
      } catch (err) {
        logger.warn("atproto_record_reconcile_failed", { name: o.metadata?.name, error: String(err) });
        await setStatus(o, { phase: "Failed", observedGeneration: o.metadata?.generation, message: String(err) }, {
          status: "False",
          reason: "Error",
          message: String(err),
        }).catch(() => {});
      }
    }
  }

  (async () => {
    while (!ac.signal.aborted) {
      await pass().catch((err) => logger.warn("atproto_record_list_failed", { error: String(err) }));
      await new Promise<void>((r) => {
        wake = r;
        // ponytail: a 5 s list poll of the workspace's AtprotoRecords and their references; a kcp watch when the poll costs too much
        const t = setTimeout(r, opts.intervalMs ?? 5_000);
        Deno.unrefTimer(t);
      });
    }
  })();

  return {
    wake: () => wake?.(),
    stop: () => {
      ac.abort();
      wake?.();
    },
  };
}

export const CONDITION_VERIFIED = "Verified";

export interface AtprotoRecordWatchReconcilerOpts {
  kcp: KcpClient;
  workspace: string;
  repoName: string;
  plcDirectoryUrl: string;
  logger: StructuredLoggerInterface;
  intervalMs?: number;
  reverifyMs?: number;
  maxRecords?: number;
}

export interface RecordMatch {
  fieldPath: string;
  equals: unknown;
}

type WatchSpec = NonNullable<AtprotoRecordWatch["spec"]>;
type HeldWatch = AtprotoRecordWatch & { spec: WatchSpec };

interface Watching {
  key: string;
  collection: string;
  match: RecordMatch[];
  records: Map<string, WatchedRecord>;
  cursors: Map<string, number>;
  queue: Promise<void>;
  verified: boolean;
  reverifiedAt: number;
  sources: number;
  close(): void;
}

const GONE_ERRORS = ["RecordNotFound", "RepoNotFound", "RepoDeactivated", "RepoTakendown", "RepoSuspended"];

export function recordMatches(value: unknown, match: RecordMatch[]): boolean {
  return !!value && typeof value === "object" && match.every((m) => canonical(fieldAt(value, m.fieldPath)) === canonical(m.equals));
}

export function parseRecordUri(uri: string): { did: string; collection: string; rkey: string } | null {
  const m = uri.match(/^at:\/\/([^/]+)\/([^/]+)\/([^/]+)$/);
  return m ? { did: m[1], collection: m[2], rkey: m[3] } : null;
}

function withCondition(
  list: Condition[] | undefined,
  next: { type: string; status: "True" | "False"; reason: string; message: string },
  generation: number | undefined,
): Condition[] {
  const prior = (list ?? []).find((c) => c.type === next.type);
  const same = prior && prior.status === next.status && prior.reason === next.reason && prior.message === next.message;
  const cond = same ? prior! : { ...next, observedGeneration: generation, lastTransitionTime: new Date().toISOString().replace(/\.\d{3}Z$/, "Z") };
  return [cond, ...(list ?? []).filter((c) => c.type !== next.type)];
}

export function startAtprotoRecordWatchReconciler(opts: AtprotoRecordWatchReconcilerOpts): AtprotoRecordReconciler {
  const { kcp, logger, repoName } = opts;
  const target = { resource: ATPROTO_RECORD_WATCH_RESOURCE, workspace: opts.workspace };
  const idResolver = new IdResolver({ plcUrl: opts.plcDirectoryUrl, didCache: new MemoryCache(5 * 60_000, 60 * 60_000) });
  const maxRecords = opts.maxRecords ?? 200;
  const reverifyMs = opts.reverifyMs ?? 5 * 60_000;
  const held = new Map<string, Watching>();
  const ac = new AbortController();
  let wake: (() => void) | undefined;

  async function author(did: string): Promise<{ pds?: string; handle?: string }> {
    const doc = await idResolver.did.resolve(did) as { service?: Array<{ id: string; serviceEndpoint: unknown }>; alsoKnownAs?: string[] } | null;
    const pds = doc?.service?.find((s) => s.id === "#atproto_pds" || s.id === `${did}#atproto_pds`)?.serviceEndpoint;
    const handle = doc?.alsoKnownAs?.find((a) => a.startsWith("at://"))?.slice(5);
    return { pds: typeof pds === "string" ? pds.replace(/\/+$/, "") : undefined, handle };
  }

  async function fetchRecord(did: string, collection: string, rkey: string): Promise<{ cid: string; value: unknown; handle?: string } | null> {
    const a = await author(did);
    if (!a.pds) throw new Error(`the DID document of ${did} names no #atproto_pds service`);
    const q = new URLSearchParams({ repo: did, collection, rkey });
    const res = await fetch(`${a.pds}/xrpc/com.atproto.repo.getRecord?${q}`, { signal: AbortSignal.timeout(10_000) });
    const body = await res.json().catch(() => ({})) as { cid?: string; value?: unknown; error?: string; message?: string };
    if (res.ok && body.value) return { cid: body.cid ?? "", value: body.value, handle: a.handle };
    if (res.status === 404 || GONE_ERRORS.includes(body.error ?? "") || (res.status === 400 && /not (be )?found|could not locate/i.test(body.message ?? ""))) return null;
    throw new Error(`getRecord at://${did}/${collection}/${rkey} from ${a.pds}: ${res.status} ${body.error ?? ""} ${body.message ?? ""}`);
  }

  async function verify(w: Watching, did: string, rkey: string): Promise<void> {
    const uri = `at://${did}/${w.collection}/${rkey}`;
    let got: Awaited<ReturnType<typeof fetchRecord>>;
    try {
      got = await fetchRecord(did, w.collection, rkey);
    } catch (err) {
      logger.warn("atproto_record_watch_verify_failed", { uri, error: String(err) });
      return;
    }
    const before = w.records.get(uri);
    if (got && recordMatches(got.value, w.match)) {
      if (!before && w.records.size >= maxRecords) {
        logger.warn("atproto_record_watch_full", { uri, maxRecords });
        return;
      }
      w.records.set(uri, { did, handle: got.handle, uri, cid: got.cid, value: got.value as Record<string, unknown>, verifiedAt: new Date().toISOString() });
      if (!before || before.cid !== got.cid) logger.info("atproto_record_watch_verified", { uri, cid: got.cid, handle: got.handle });
    } else if (before) {
      w.records.delete(uri);
      logger.info("atproto_record_watch_removed", { uri, reason: got ? "the record no longer matches" : "the record is gone from its repo" });
    }
  }

  function enqueue(w: Watching, did: string, rkey: string): void {
    w.queue = w.queue.then(() => verify(w, did, rkey)).catch(() => {}).finally(() => wake?.());
  }

  function reverify(w: Watching): void {
    w.reverifiedAt = Date.now();
    for (const uri of [...w.records.keys()]) {
      const at = parseRecordUri(uri);
      if (at) enqueue(w, at.did, at.rkey);
    }
    w.queue = w.queue.then(() => {
      if (!w.verified) logger.info("atproto_record_watch_reverified", { collection: w.collection, records: w.records.size });
      w.verified = true;
    });
  }

  function start(o: HeldWatch, match: RecordMatch[], key: string): Watching {
    const relayUrls = o.spec.source?.relayUrls ?? [];
    const jetstreamUrls = o.spec.source?.jetstreamUrls ?? [];
    const w: Watching = {
      key,
      collection: o.spec.collection,
      match,
      records: new Map((o.status?.records ?? []).map((r) => [r.uri, r])),
      cursors: new Map((o.status?.cursors ?? []).map((c) => [c.source, c.cursor])),
      queue: Promise.resolve(),
      verified: false,
      reverifiedAt: 0,
      sources: relayUrls.length + jetstreamUrls.length,
      close: () => {},
    };
    if (w.sources) {
      const streams = createDefaultATProtoEventStreamsClient({
        onlyRelays: relayUrls,
        additionalJetstreams: jetstreamUrls,
        log: logger,
        cursorStore: { load: (s) => w.cursors.get(s), save: (s, p) => void w.cursors.set(s, p.cursor) },
      });
      const sub = streams.watch({
        wantedCollections: [w.collection],
        onEvent: (ev) => {
          if (ev.collection === w.collection) enqueue(w, ev.did, ev.rkey);
        },
      });
      w.close = () => {
        sub.close();
        streams.close();
      };
    }
    logger.info("atproto_record_watch_started", { name: o.metadata?.name, collection: w.collection, relayUrls, jetstreamUrls, records: w.records.size, cursors: Object.fromEntries(w.cursors) });
    return w;
  }

  async function writeStatus(o: HeldWatch, w: Watching | undefined, cond: { status: "True" | "False"; reason: string; message: string }): Promise<void> {
    const status: AtprotoRecordWatchStatus = {
      ...(o.status ?? {}),
      observedGeneration: o.metadata?.generation,
      records: w ? [...w.records.values()].sort((a, b) => a.uri.localeCompare(b.uri)) : o.status?.records,
      cursors: w ? [...w.cursors].map(([source, cursor]) => ({ source, cursor })).sort((a, b) => a.source.localeCompare(b.source)) : o.status?.cursors,
      message: cond.message,
      conditions: withCondition(o.status?.conditions, { type: CONDITION_VERIFIED, ...cond }, o.metadata?.generation),
    };
    if (!status.records?.length) delete status.records;
    if (!status.cursors?.length) delete status.cursors;
    if (canonical(o.status ?? {}) === canonical(status)) return;
    await kcp.patchStatus(target, o.metadata!.name!, () => status);
  }

  async function reconcile(o: HeldWatch): Promise<void> {
    const name = o.metadata!.name!;
    const missing: MissingReference[] = [];
    const match: RecordMatch[] = [];
    for (const m of o.spec.match ?? []) {
      match.push({ fieldPath: m.fieldPath, equals: m.valueFrom ? await resolveValueFrom(kcp, opts.workspace, { valueFrom: m.valueFrom }, missing) : (m.value ?? "") });
    }
    if (missing.length) {
      held.get(name)?.close();
      held.delete(name);
      return await writeStatus(o, undefined, { status: "False", reason: "MissingReference", message: `waiting for ${missing.map((m) => m.what).join("; ")}` });
    }
    const key = canonical({ collection: o.spec.collection, source: o.spec.source ?? {}, match });
    let w = held.get(name);
    if (w && w.key !== key) {
      w.close();
      held.delete(name);
      w = undefined;
    }
    if (!w) {
      w = start(o, match, key);
      held.set(name, w);
    }
    if (Date.now() - w.reverifiedAt > reverifyMs) reverify(w);
    if (!w.sources) return await writeStatus(o, w, { status: "False", reason: "NoSource", message: "spec.source names no relayUrls and no jetstreamUrls" });
    await writeStatus(o, w, w.verified
      ? { status: "True", reason: "Verified", message: `${w.records.size} verified record(s); every one was fetched from its author's PDS` }
      : { status: "False", reason: "Reverifying", message: `re-verifying ${w.records.size} record(s) from status against their authors' PDSes` });
  }

  async function pass(): Promise<void> {
    const list = await kcp.list(target);
    const seen = new Set<string>();
    for (const raw of list.items) {
      const o = raw as HeldWatch;
      if (o.spec?.repo !== repoName || o.metadata?.deletionTimestamp) continue;
      seen.add(o.metadata!.name!);
      try {
        await reconcile(o);
      } catch (err) {
        logger.warn("atproto_record_watch_reconcile_failed", { name: o.metadata?.name, error: String(err) });
      }
    }
    for (const [name, w] of held) {
      if (seen.has(name)) continue;
      w.close();
      held.delete(name);
    }
  }

  (async () => {
    while (!ac.signal.aborted) {
      await pass().catch((err) => logger.warn("atproto_record_watch_list_failed", { error: String(err) }));
      await new Promise<void>((r) => {
        wake = r;
        const t = setTimeout(r, opts.intervalMs ?? 5_000);
        Deno.unrefTimer(t);
      });
    }
  })();

  return {
    wake: () => wake?.(),
    stop: () => {
      ac.abort();
      for (const w of held.values()) w.close();
      held.clear();
      wake?.();
    },
  };
}
