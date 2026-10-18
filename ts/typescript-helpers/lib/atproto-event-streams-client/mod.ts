import {
  eventKey,
  DEFAULT_RELAY_URLS,
  DEFAULT_JETSTREAM_URLS,
  resumeCursor,
  streamSource,
} from "@publicdomainrelay/atproto-event-stream-common";
import type {
  ATProtoRelay,
  ATProtoJetstream,
  ATProtoEventStreamsClient,
  ATProtoEventStreamWatcher,
} from "@publicdomainrelay/atproto-event-stream-abc";
import type {
  FirehoseProgress,
  FirehoseRecordEvent,
  StreamCursorStore,
  StreamKind,
  StreamResumeOverlap,
} from "@publicdomainrelay/atproto-event-stream-common";
import { createATProtoRelay } from "@publicdomainrelay/atproto-event-stream-relay";
import { createATProtoJetstream } from "@publicdomainrelay/atproto-event-stream-jetstream";
import type { LoggerInterface } from "@publicdomainrelay/logger";

const MAX_DEDUP_ENTRIES = 10_000;

export type {
  ATProtoRelay,
  ATProtoJetstream,
  ATProtoEventStreamsClient,
  ATProtoEventStreamWatcher,
} from "@publicdomainrelay/atproto-event-stream-abc";
export type {
  FirehoseOperation,
  FirehoseProgress,
  FirehoseRecordEvent,
  StreamCursorStore,
  StreamResumeOverlap,
} from "@publicdomainrelay/atproto-event-stream-common";

export type StreamProgressListener = (source: string, progress: FirehoseProgress) => void;

export function createATProtoEventStreamsClient(opts: {
  relays: ATProtoRelay[];
  jetstreams: ATProtoJetstream[];
  dedupWindowMs?: number;
  onProgress?: StreamProgressListener;
  cursorStore?: StreamCursorStore;
  resumeOverlap?: StreamResumeOverlap;
  log?: LoggerInterface;
}): ATProtoEventStreamsClient {
  const { relays, jetstreams, dedupWindowMs = 15_000 } = opts;
  const latest = new Map<string, number>();
  const progressOf = (source: string) => (progress: FirehoseProgress) => {
    latest.set(source, Math.max(latest.get(source) ?? 0, progress.cursor));
    opts.cursorStore?.save(source, progress);
    opts.onProgress?.(source, progress);
  };
  const resumeOf = (kind: StreamKind, source: string): number | undefined => {
    const stored = opts.cursorStore?.load(source);
    const from = resumeCursor(kind, stored, opts.resumeOverlap);
    if (stored !== undefined) opts.log?.info?.("event_stream_resume", { source, stored, from: from ?? null });
    return from;
  };
  const liveOf = (kind: StreamKind, source: string): number | undefined =>
    resumeCursor(kind, latest.get(source) ?? opts.cursorStore?.load(source), { subscribeReposEvents: 0, jetstreamMicros: 0 });
  const sources = () => [
    ...relays.map((r) => ({ kind: "relay" as const, name: streamSource("relay", r.url), stream: r })),
    ...jetstreams.map((j) => ({ kind: "jetstream" as const, name: streamSource("jetstream", j.url), stream: j })),
  ];
  const watchers: ATProtoEventStreamWatcher[] = [];
  const forget = (list: ATProtoEventStreamWatcher[]) => {
    for (const w of list) {
      try { w.close(); } catch { /* ignore */ }
      const i = watchers.indexOf(w);
      if (i >= 0) watchers.splice(i, 1);
    }
  };

  // Batching: all watch() calls in the same synchronous block share one set
  // of underlying connections. Each registration specifies its own wantedCollections;
  // the union is passed to each stream (Jetstream filters server-side via repeated
  // wantedCollections params, relay filters client-side).
  let started = false;
  let _sharedTeardown: (() => void) | null = null;
  const registrations: Array<{
    wantedCollections: string[];
    onEvent(event: FirehoseRecordEvent): void | Promise<void>;
  }> = [];
  const sharedWatchers: ATProtoEventStreamWatcher[] = [];

  function createDedicatedWatcher(
    wo: { wantedCollections: string[]; onEvent(event: FirehoseRecordEvent): void | Promise<void>; log?: LoggerInterface },
  ): ATProtoEventStreamWatcher {
    const seen = new Map<string, number>();
    const dedicated: ATProtoEventStreamWatcher[] = [];

    const purgeTimer = setInterval(() => {
      const cutoff = Date.now() - dedupWindowMs;
      for (const [key, ts] of seen) {
        if (ts < cutoff) seen.delete(key);
      }
    }, Math.max(dedupWindowMs, 5_000));

    const onEvent = (event: FirehoseRecordEvent): void | Promise<void> => {
      const key = eventKey(event);
      const now = Date.now();
      const last = seen.get(key);
      if (last !== undefined && (now - last) < dedupWindowMs) return;
      seen.set(key, now);
      if (seen.size > MAX_DEDUP_ENTRIES) seen.delete(seen.keys().next().value!);
      const r = wo.onEvent(event);
      if (r instanceof Promise) r.catch(() => {});
    };

    for (const { kind, name, stream } of sources()) {
      const w = stream.watch({
        wantedCollections: wo.wantedCollections,
        cursor: liveOf(kind, name),
        onEvent,
        onProgress: progressOf(name),
        log: wo.log,
      });
      dedicated.push(w);
      watchers.push(w);
    }

    return {
      close() {
        clearInterval(purgeTimer);
        forget(dedicated);
        dedicated.length = 0;
        seen.clear();
      },
    };
  }

  function startStreams(): void {
    if (started || registrations.length === 0) return;
    started = true;

    const allCollections = [...new Set(registrations.flatMap((r) => r.wantedCollections))];
    const seen = new Map<string, number>();
    const purgeTimer = setInterval(() => {
      const cutoff = Date.now() - dedupWindowMs;
      for (const [key, ts] of seen) {
        if (ts < cutoff) seen.delete(key);
      }
    }, Math.max(dedupWindowMs, 5_000));

    const onEvent = (event: FirehoseRecordEvent): void | Promise<void> => {
      const key = eventKey(event);
      const now = Date.now();
      const last = seen.get(key);
      if (last !== undefined && (now - last) < dedupWindowMs) return;
      seen.set(key, now);
      if (seen.size > MAX_DEDUP_ENTRIES) seen.delete(seen.keys().next().value!);

      for (const reg of registrations) {
        if (reg.wantedCollections.includes(event.collection)) {
          const r = reg.onEvent(event);
          if (r instanceof Promise) r.catch(() => {});
        }
      }
    };

    for (const { kind, name, stream } of sources()) {
      const cursor = latest.has(name) ? liveOf(kind, name) : resumeOf(kind, name);
      const w = stream.watch({ wantedCollections: allCollections, cursor, onEvent, onProgress: progressOf(name) });
      sharedWatchers.push(w);
      watchers.push(w);
    }

    // Called when last registration closes.
    function teardown(): void {
      clearInterval(purgeTimer);
      forget(sharedWatchers);
      sharedWatchers.length = 0;
      started = false;
    }

    _sharedTeardown = teardown;
  }

  return {
    relays,
    jetstreams,

    watch(wo): ATProtoEventStreamWatcher {
      const reg = { wantedCollections: [...wo.wantedCollections], onEvent: wo.onEvent };

      if (started) {
        // Late registration after initial batch — open dedicated connections.
        return createDedicatedWatcher(wo);
      }

      registrations.push(reg);
      queueMicrotask(startStreams);

      let closed = false;

      return {
        close() {
          if (closed) return;
          closed = true;
          const idx = registrations.indexOf(reg);
          if (idx >= 0) registrations.splice(idx, 1);

          if (registrations.length === 0 && _sharedTeardown) {
            _sharedTeardown();
            _sharedTeardown = null;
          }
        },
      };
    },

    close() {
      registrations.length = 0;
      for (const w of watchers) {
        try { w.close(); } catch { /* ignore */ }
      }
      watchers.length = 0;
    },
  };
}

export interface DefaultClientOpts {
  /** Additional relay URLs beyond defaults. */
  additionalRelays?: string[];
  /** Additional Jetstream URLs beyond defaults. */
  additionalJetstreams?: string[];
  /** Relay URLs to remove from the default set. */
  removeRelays?: string[];
  /** Jetstream URLs to remove from the default set. */
  removeJetstreams?: string[];
  onlyRelays?: readonly string[];
  /**
   * Time window in ms for deduplication. Events with the same AT-URI + CID
   * within this window are suppressed. Default 15 seconds.
   */
  dedupWindowMs?: number;
  log?: LoggerInterface;
  onProgress?: StreamProgressListener;
  cursorStore?: StreamCursorStore;
  resumeOverlap?: StreamResumeOverlap;
}

export function createDefaultATProtoEventStreamsClient(
  opts: DefaultClientOpts = {},
): ATProtoEventStreamsClient {
  const removeRelay = new Set(opts.removeRelays ?? []);
  const removeJetstream = new Set(opts.removeJetstreams ?? []);

  const relayUrls = opts.onlyRelays ? [...opts.onlyRelays] : [
    ...DEFAULT_RELAY_URLS.filter((u) => !removeRelay.has(u)),
    ...(opts.additionalRelays ?? []),
  ];

  const jetstreamUrls = opts.onlyRelays ? [...(opts.additionalJetstreams ?? [])] : [
    ...DEFAULT_JETSTREAM_URLS.filter((u) => !removeJetstream.has(u)),
    ...(opts.additionalJetstreams ?? []),
  ];

  const relays = relayUrls.map((url) => createATProtoRelay(url, { log: opts.log }));
  const jetstreams = jetstreamUrls.map((url) => createATProtoJetstream(url, { log: opts.log }));

  opts.log?.info?.("event_streams_client_defaults", {
    relayCount: relays.length,
    relayUrls: relays.map((r) => r.url),
    jetstreamCount: jetstreams.length,
    jetstreamUrls: jetstreams.map((j) => j.url),
  });

  return createATProtoEventStreamsClient({
    relays,
    jetstreams,
    dedupWindowMs: opts.dedupWindowMs,
    onProgress: opts.onProgress,
    cursorStore: opts.cursorStore,
    resumeOverlap: opts.resumeOverlap,
    log: opts.log,
  });
}
