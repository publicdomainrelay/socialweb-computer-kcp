import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { parseContractState } from "@publicdomainrelay/compute-request-abc";
import type { ContractState } from "@publicdomainrelay/compute-request-abc";
import {
  accountOfSpec,
  AdmissionState,
  type AdmissionLimits,
  type AccountPdsProvider,
  dispositionOf,
  FINISHED_PHASES,
  withContractConditions,
} from "@publicdomainrelay/requester-controller-abc";
import type { KcpClient } from "@publicdomainrelay/kcp-client-abc";
import { kcpResourceClient } from "@publicdomainrelay/kcp-client-abc";
import { isNotFound } from "@publicdomainrelay/kcp-client-common";
import type { MarketLoopRecorder } from "@publicdomainrelay/market-abc";
import {
  COMPUTE_CONTRACT_RESOURCE,
  type ComputeContract,
  parseComputeContract,
} from "@publicdomainrelay/socialweb-computer-k8s-api";
import { type ContractRunner, type RunnerEnvironment, startContractRunner } from "./runner.ts";
import { createOrphanScanner, DEFAULT_ORPHAN_SCAN_INTERVAL_MS, DEFAULT_UNCLAIMED_GRACE_MS } from "./orphans.ts";
import { createRecordRetention, DEFAULT_RECORD_RETENTION_MS, DEFAULT_RECORD_RETENTION_SCAN_MS } from "./retention.ts";

export interface RequesterControllerOptions extends Omit<RunnerEnvironment, "logger" | "kcp" | "workspace"> {
  kcp: KcpClient;
  workspace?: string;
  logger: StructuredLoggerInterface;
  limits: AdmissionLimits;
  scanOwners?: () => string[];
  orphanScan?: boolean;
  unclaimedGraceMs?: number;
  orphanScanIntervalMs?: number;
  recordRetentionMs?: number;
  recordRetentionScanMs?: number;
  resyncMs?: number;
}

export interface RequesterController {
  start(): void;
  stop(): Promise<void>;
  active(): string[];
}

const DEFAULT_RESYNC_MS = 30_000;

export const CONTRACT_KEY_SWEEP_MS = 5 * 60_000;

const WATCH_RETRY_MS = 2_000;

function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done);
  });
}

function stateOf(contract: ComputeContract): ContractState | null {
  const raw = contract.status?.contract;
  if (!raw || Object.keys(raw).length === 0) return null;
  try {
    return parseContractState(raw);
  } catch {
    return null;
  }
}

export function createRequesterController(opts: RequesterControllerOptions): RequesterController {
  const logger = opts.logger;
  const target = { resource: COMPUTE_CONTRACT_RESOURCE, workspace: opts.workspace };
  const contracts = kcpResourceClient<ComputeContract>(opts.kcp, target, parseComputeContract);
  const admission = new AdmissionState(opts.limits);
  const runners = new Map<string, ContractRunner>();
  const waiting = new Map<string, ComputeContract>();
  const finished = new Set<string>();
  const stopping = new AbortController();
  const scanner = opts.orphanScan
    ? createOrphanScanner({
      logger,
      pds: opts.pds,
      plcUrl: opts.plcUrl,
      scanOwners: opts.scanOwners ?? (() => []),
      graceMs: opts.unclaimedGraceMs ?? DEFAULT_UNCLAIMED_GRACE_MS,
      intervalMs: opts.orphanScanIntervalMs ?? DEFAULT_ORPHAN_SCAN_INTERVAL_MS,
    })
    : null;
  const retentionMs = opts.recordRetentionMs ?? DEFAULT_RECORD_RETENTION_MS;
  const retention = retentionMs > 0
    ? createRecordRetention({
      logger,
      pds: opts.pds,
      retentionMs,
      intervalMs: opts.recordRetentionScanMs,
      annotate: async (name, annotations) => {
        await contracts.patch(name, { type: "merge", body: { metadata: { annotations } } });
      },
      running: (name) => runners.has(name),
    })
    : null;
  let known: ComputeContract[] = [];
  let retentionTimer: ReturnType<typeof setInterval> | undefined;
  let reconciling: Promise<void> = Promise.resolve();
  let started = false;
  let lastKeySweep = Number.NEGATIVE_INFINITY;

  async function sweepContractKeys(present: Set<string>): Promise<void> {
    const dirs = await opts.vault.list({ mount: opts.vaultMount, path: "contracts" });
    for (const dir of dirs) {
      const name = dir.replace(/\/+$/, "");
      if (!name || present.has(name) || runners.has(name) || waiting.has(name)) continue;
      await opts.vault.remove({ mount: opts.vaultMount, path: `contracts/${name}/secrets-key` });
      logger.info("requester_contract_key_removed", { contract: name, reason: "the ComputeContract is gone" });
    }
  }

  function report(): void {
    opts.loop?.contractsActive(admission.running);
  }

  async function settle(contract: ComputeContract, reason: string): Promise<void> {
    const name = contract.metadata?.name ?? "";
    finished.add(name);
    waiting.delete(name);
    if (contract.spec?.release === true && contract.status?.phase && !FINISHED_PHASES.has(contract.status.phase)) {
      await contracts.replaceStatus(name, withContractConditions({
        ...(contract.status ?? {}),
        phase: "Released",
        message: reason,
      }, new Date())).catch((err) => {
        if (!isNotFound(err)) logger.warn("requester_contract_settle_failed", { contract: name, error: String(err) });
      });
    } else if (contract.spec?.release === true && !contract.status?.phase) {
      await contracts.replaceStatus(name, withContractConditions({ phase: "Released", message: reason }, new Date())).catch((err) => {
        if (!isNotFound(err)) logger.warn("requester_contract_settle_failed", { contract: name, error: String(err) });
      });
    }
  }

  function launch(contract: ComputeContract): void {
    const name = contract.metadata?.name ?? "";
    const account = accountOfSpec(contract.spec ?? {});
    const disposition = dispositionOf({
      phase: contract.status?.phase,
      state: stateOf(contract),
      release: contract.spec?.release === true,
    });
    if (disposition.kind === "done") {
      void settle(contract, disposition.reason);
      return;
    }
    const decision = admission.decide(name, account);
    if (!decision.admit) {
      if (!waiting.has(name)) {
        logger.info("requester_contract_waiting", { contract: name, reason: decision.reason });
        void contracts.replaceStatus(name, withContractConditions({
          ...(contract.status ?? {}),
          phase: "Pending",
          message: decision.reason,
        }, new Date(), decision.reason)).catch(() => undefined);
      }
      waiting.set(name, contract);
      return;
    }
    waiting.delete(name);
    admission.admit(name, account);
    report();
    const runner = startContractRunner(opts, contract, disposition);
    runners.set(name, runner);
    logger.info("requester_contract_admitted", { contract: name, account, resume: disposition.kind === "resume", running: admission.running });
    void runner.done.finally(() => {
      runners.delete(name);
      admission.finish(name);
      finished.add(name);
      report();
      void reconcileWaiting();
    });
  }

  async function reconcileWaiting(): Promise<void> {
    for (const contract of [...waiting.values()]) {
      if (stopping.signal.aborted) return;
      launch(contract);
    }
  }

  function observe(contract: ComputeContract): void {
    const name = contract.metadata?.name;
    if (!name || !contract.spec) return;
    const runner = runners.get(name);
    if (runner) {
      runner.update(contract);
      return;
    }
    if (finished.has(name)) {
      if (waiting.has(name)) waiting.delete(name);
      return;
    }
    launch(contract);
  }

  function forget(name: string): void {
    waiting.delete(name);
    runners.get(name)?.update(null);
  }

  async function resync(): Promise<string> {
    const out = await contracts.list();
    known = out.items;
    const present = new Set<string>();
    for (const contract of out.items) {
      const name = contract.metadata?.name;
      if (name) present.add(name);
      observe(contract);
    }
    for (const name of [...runners.keys()]) if (!present.has(name)) forget(name);
    for (const name of [...finished]) if (!present.has(name)) finished.delete(name);
    if (Date.now() - lastKeySweep >= CONTRACT_KEY_SWEEP_MS) {
      lastKeySweep = Date.now();
      void sweepContractKeys(present).catch((err) => logger.warn("requester_contract_key_sweep_failed", { error: String(err) }));
    }
    if (scanner?.due()) {
      void scanner.scan(known, stateOf).catch((err) => logger.warn("requester_orphan_scan_failed", { error: String(err) }));
    }
    return out.resourceVersion;
  }

  function enqueue(work: () => Promise<unknown>): void {
    reconciling = reconciling.then(work).then(() => {}, (err) => logger.warn("requester_reconcile_failed", { error: String(err) }));
  }

  async function follow(): Promise<void> {
    while (!stopping.signal.aborted) {
      try {
        const resourceVersion = await resync();
        const watchStop = new AbortController();
        stopping.signal.addEventListener("abort", () => watchStop.abort(), { once: true });
        const resyncTimer = setInterval(() => enqueue(resync), opts.resyncMs ?? DEFAULT_RESYNC_MS);
        try {
          for await (const event of opts.kcp.watch(target, { signal: watchStop.signal, resourceVersion })) {
            if (event.type === "BOOKMARK") continue;
            if (event.type === "ERROR") break;
            const raw = event.object;
            let contract: ComputeContract;
            try {
              contract = parseComputeContract(raw);
            } catch (err) {
              logger.warn("requester_contract_unparsable", { error: String(err) });
              continue;
            }
            if (event.type === "DELETED") {
              const name = contract.metadata?.name;
              if (name) {
                known = known.filter((c) => c.metadata?.name !== name);
                forget(name);
              }
              continue;
            }
            const name = contract.metadata?.name;
            if (name) known = [...known.filter((c) => c.metadata?.name !== name), contract];
            observe(contract);
          }
        } finally {
          clearInterval(resyncTimer);
          watchStop.abort();
        }
      } catch (err) {
        if (stopping.signal.aborted) return;
        logger.warn("requester_watch_failed", { error: String(err) });
      }
      await pause(WATCH_RETRY_MS, stopping.signal);
    }
  }

  let following: Promise<void> | null = null;

  return {
    start() {
      if (started) return;
      started = true;
      following = follow();
      if (retention) {
        retentionTimer = setInterval(() => {
          void retention.sweep(known, stateOf).catch((err) => logger.warn("requester_retention_sweep_failed", { error: String(err) }));
        }, opts.recordRetentionScanMs ?? DEFAULT_RECORD_RETENTION_SCAN_MS);
      }
    },
    async stop() {
      if (retentionTimer !== undefined) clearInterval(retentionTimer);
      stopping.abort();
      for (const runner of runners.values()) runner.stop();
      await following;
      await reconciling;
      await Promise.allSettled([...runners.values()].map((r) => r.done));
    },
    active() {
      return [...runners.keys()];
    },
  };
}

export type { AccountPdsProvider };
