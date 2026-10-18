import { assert, assertEquals } from "@std/assert";
import {
  createScopeCache,
  scopeCacheKey,
} from "@publicdomainrelay/policy-engine-scope-cache";
import type { ScopeCache } from "@publicdomainrelay/policy-engine-scope-cache";
import { createPolicyEvaluator } from "@publicdomainrelay/policy-engine-evaluator";
import {
  POLICY_GHA_LITE_NSID,
  type PolicyEngineExecutor,
} from "@publicdomainrelay/policy-engine-abc";
import { BADGE_BLUE_KEYS_NSID, BIDS_FREE_NSID } from "@publicdomainrelay/market-lexicons";
import { RFP_NSID, SUBMIT_ACCEPT_NSID } from "@publicdomainrelay/market-common";
import type { CollectedBid, RequesterPDS } from "@publicdomainrelay/requester-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import {
  createCandidateScopeGate,
  requestCompute,
} from "@publicdomainrelay/compute-request-xrpc";
import type { ComputeRequestOptions } from "@publicdomainrelay/compute-request-xrpc";
import { createScopeRefresher } from "@publicdomainrelay/scope-refresh-timers";

Deno.env.delete("ATPROTO_DID");

const REQUESTER_DID = "did:plc:requesterfake";
const BIDDER_DID = "did:plc:bidderone";
const BIDDER_ENDPOINT = "https://bidder.test";
const TICKET = "iroh://endpointfaketicket";
const POLICY_NAME = "open";
const SCOPE_URI = `at://${REQUESTER_DID}/policy-gha-lite/${POLICY_NAME}`;
const POLICY_ARGS = { bidWindowSec: 5, firstFree: true };
const ACCEPT_BODY = {
  uri: "at://did:plc:bidder/com.publicdomainrelay.temp.market.receipt/1",
  cid: "bafyreceipt",
  submitEvent: BIDDER_ENDPOINT,
};

interface LoggedEvent {
  event: string;
  extra: Record<string, unknown>;
  at: number;
}

function recordingLogger(events: LoggedEvent[]): StructuredLoggerInterface {
  const push = (event: string, extra?: Record<string, unknown>) => {
    events.push({ event, extra: extra ?? {}, at: Date.now() });
  };
  return { info: push, warn: push, error: push, debug: push } as unknown as StructuredLoggerInterface;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function waitForEvent(events: LoggedEvent[], event: string, timeoutMs = 60_000): Promise<LoggedEvent> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const found = events.find((e) => e.event === event);
    if (found) return found;
    await delay(25);
  }
  throw new Error(`timed out waiting for log event ${event}`);
}

function spyScopeCache() {
  const inner = createScopeCache();
  const gets: string[] = [];
  const sets: string[] = [];
  const hits: string[] = [];
  const cache = {
    get(identity: never, did: string, args: never) {
      const key = scopeCacheKey(identity, did, args);
      gets.push(key);
      const verdict = inner.get(identity, did, args);
      if (verdict) hits.push(key);
      return verdict;
    },
    set(identity: never, did: string, args: never, result: never) {
      sets.push(scopeCacheKey(identity, did, args));
      inner.set(identity, did, args, result);
    },
    once(identity: never, did: string, args: never, work: () => Promise<never>) {
      return inner.once(identity, did, args, () => {
        sets.push(scopeCacheKey(identity, did, args));
        return work();
      });
    },
    applyEvent(e: { did: string; rkey: string }) {
      inner.applyEvent(e);
    },
    stats: () => inner.stats(),
  };
  return { cache: cache as unknown as ScopeCache, gets, sets, hits, inner };
}

function fakePds(bidPayload: Record<string, unknown>) {
  const records: Array<{ uri: string; cid: string; record: Record<string, unknown> }> = [];
  const onNetwork = new Map<string, (address: string) => void>();
  const pendingBids = new Map<string, CollectedBid[]>();
  let seq = 0;

  const write = (collection: string, record: Record<string, unknown>) => {
    seq++;
    const entry = { uri: `at://${REQUESTER_DID}/${collection}/${seq}`, cid: `bafycid${seq}`, record };
    records.push(entry);
    if (collection === RFP_NSID) {
      pendingBids.set(entry.uri, [{
        did: BIDDER_DID,
        uri: `at://${BIDDER_DID}/com.publicdomainrelay.temp.market.bid/0`,
        cid: "bafybid0",
        record: {
          rfp: { uri: entry.uri, cid: entry.cid },
          payload: bidPayload,
          submitAccept: BIDDER_ENDPOINT,
        },
      }]);
    }
    return Promise.resolve({ uri: entry.uri, cid: entry.cid });
  };

  const pds = {
    did: REQUESTER_DID,
    privateKeyHex: "",
    relay: {
      ingressRef: "fake-ref",
      ingressUrl: "",
      ingressHost: "",
      close() {},
      onServe: () => Promise.resolve(),
    },
    relaySubdomain: "sub.relay.test",
    pendingBids,
    attestationKp: { did: () => "did:key:zfake", privateKey: { bytes: new Uint8Array() } },
    api: { listRecords: () => Promise.resolve({ records: [] }) },
    createRepoRecord: write,
    createSignedRepoRecord: (collection: string, record: Record<string, unknown>) => write(collection, record),
    resolveBidderEndpoint: (endpoint: string) =>
      Promise.resolve(
        endpoint === BIDDER_ENDPOINT
          ? { targetUrl: `${BIDDER_ENDPOINT}/xrpc`, audDid: "did:web:bidder.test" }
          : null,
      ),
    callBidder: (_base: string, nsid: string) =>
      Promise.resolve(
        nsid === SUBMIT_ACCEPT_NSID
          ? { status: 200, ok: true, body: ACCEPT_BODY }
          : { status: 200, ok: true, body: {} },
      ),
    setOnNetworkResolved: (key: string, fn: (address: string) => void) => {
      onNetwork.set(key, fn);
    },
    clearOnNetworkResolved: (key: string) => {
      onNetwork.delete(key);
    },
  };

  return {
    pds: pds as unknown as RequesterPDS,
    records,
    fireNetwork(address: string) {
      for (const fn of onNetwork.values()) fn(address);
    },
    resolveRecord: (ref: { uri: string; cid: string }) => {
      const found = records.find((r) => r.uri === ref.uri);
      if (!found) return Promise.reject(new Error(`no record ${ref.uri}`));
      return Promise.resolve(found.record);
    },
  };
}

function fakeEventStreams() {
  const registrations: Array<{ wantedCollections: string[]; onEvent: (e: never) => void }> = [];
  const client = {
    relays: [],
    jetstreams: [],
    watch: (wo: { wantedCollections: string[]; onEvent: (e: never) => void }) => {
      registrations.push(wo);
      return { close() {} };
    },
    close() {},
  };
  return {
    client: client as unknown as NonNullable<ComputeRequestOptions["eventStreams"]>,
    registrations,
  };
}

function opts(
  fake: ReturnType<typeof fakePds>,
  events: LoggedEvent[],
  extra: Partial<ComputeRequestOptions>,
): ComputeRequestOptions {
  return {
    vmName: "compute-test",
    logger: recordingLogger(events),
    policy: { name: POLICY_NAME, args: POLICY_ARGS },
    extraBidderDids: [BIDDER_DID],
    vmReadyTimeoutSec: 10,
    sshPublicKey: "ssh-ed25519 AAAAfake test",
    recordResolver: fake.resolveRecord,
    receiptVerifier: () => Promise.resolve({ ok: true, sigOk: true, bindOk: true }),
    ...extra,
  };
}

function handlers(fake: ReturnType<typeof fakePds>) {
  return {
    onReceipt: () => {
      setTimeout(() => fake.fireNetwork(TICKET), 0);
      return "continue" as const;
    },
  };
}

Deno.test("a warmed scope cache answers a bid in under 200ms", async () => {
  let scopeCalls = 0;
  const executor: PolicyEngineExecutor = {
    kind: POLICY_GHA_LITE_NSID,
    execute: () => Promise.resolve({ allow: true, violations: [] }),
    scope: async () => {
      scopeCalls++;
      await delay(1000);
      return { allow: true, violations: [] };
    },
  };
  const cache = createScopeCache();
  const evaluator = createPolicyEvaluator({
    registry: { get: () => executor, kinds: () => [POLICY_GHA_LITE_NSID] },
    resolve: () => Promise.resolve({ $type: POLICY_GHA_LITE_NSID, workflow: "name: fake" }),
    scopeCache: cache,
  });
  const gate = createCandidateScopeGate({
    evaluator,
    selfDid: REQUESTER_DID,
    args: POLICY_ARGS,
    policyRecord: {
      uri: SCOPE_URI,
      cid: POLICY_NAME,
      value: { $type: POLICY_GHA_LITE_NSID, workflow: "name: fake" },
    },
    log: () => {},
  });

  const prewarm = gate(BIDDER_DID);
  await delay(1500);
  await prewarm;

  const startedAt = Date.now();
  const verdict = await gate(BIDDER_DID);
  const elapsedMs = Date.now() - startedAt;

  assertEquals(verdict.allow, true);
  assert(elapsedMs < 200, `warmed gate answered in ${elapsedMs}ms`);
  assertEquals(scopeCalls, 1, "the pre-warm ran the executor exactly once");
});
