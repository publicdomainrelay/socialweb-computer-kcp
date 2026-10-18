import { findVmContract, listOwnContractRecords, releaseCompute } from "@publicdomainrelay/compute-request-xrpc";
import { isTerminated } from "@publicdomainrelay/compute-request-abc";
import type { ContractState } from "@publicdomainrelay/compute-request-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import type { AccountPdsProvider } from "@publicdomainrelay/requester-controller-abc";
import type { ComputeContract } from "@publicdomainrelay/socialweb-computer-k8s-api";
import { removeOwnRecords } from "./retention.ts";

export const DEFAULT_ORPHAN_SCAN_INTERVAL_MS = 5 * 60_000;

export const DEFAULT_UNCLAIMED_GRACE_MS = 10 * 60_000;

export const ORPHAN_LOOKUPS_PER_PASS = 20;

export const ORPHAN_REMOVAL_PASS_MS = 60_000;

const RELEASE_REASON_UNCLAIMED = "unclaimed_after_join_grace";

export interface OrphanScannerOptions {
  logger: StructuredLoggerInterface;
  pds: AccountPdsProvider;
  plcUrl: string;
  scanOwners: () => string[];
  graceMs: number;
  intervalMs: number;
  retentionMs?: number;
  now?: () => number;
}

export interface OrphanScanner {
  due(): boolean;
  scan(contracts: ComputeContract[], stateOf: (c: ComputeContract) => ContractState | null): Promise<void>;
  stats(): Record<string, number>;
}

export function createOrphanScanner(opts: OrphanScannerOptions): OrphanScanner {
  const now = opts.now ?? Date.now;
  const startedAt = now();
  const settledVms = new Map<string, string>();
  const knownOwners = new Set<string>();
  const unclaimedSince = new Map<string, number>();
  let lastScan = Number.NEGATIVE_INFINITY;
  let scanning = false;

  function blocking(contracts: ComputeContract[], owner: string, vmName: string, stateOf: (c: ComputeContract) => ContractState | null): boolean {
    return contracts.some((c) => {
      if ((opts.pds.accounts && c.spec?.ownerDid !== owner) || c.spec?.vmName !== vmName) return false;
      const phase = c.status?.phase;
      const settled = phase === "Held" || phase === "Released" || phase === "Failed";
      return !settled || !stateOf(c)?.payload;
    });
  }

  return {
    stats() {
      return { settledVms: settledVms.size, unclaimedVms: unclaimedSince.size, owners: knownOwners.size };
    },
    due() {
      const elapsed = now() - lastScan;
      return !scanning && (elapsed >= opts.intervalMs || (unclaimedSince.size > 0 && elapsed >= 30_000));
    },
    async scan(contracts, stateOf) {
      if (scanning) return;
      scanning = true;
      lastScan = now();
      try {
        const claimed = new Set<string>();
        for (const owner of opts.scanOwners()) knownOwners.add(owner);
        for (const c of contracts) {
          if (c.spec?.ownerDid) knownOwners.add(c.spec.ownerDid);
          const state = stateOf(c);
          for (const uri of [state?.payload?.uri, state?.rfp?.uri]) if (uri) claimed.add(uri);
        }
        const retentionMs = opts.retentionMs ?? 0;
        const removalDeadline = now() + ORPHAN_REMOVAL_PASS_MS;
        let lookups = 0;
        let waiting = 0;
        let removed = 0;
        let bounded = false;
        for (const owner of opts.pds.accounts ? knownOwners : [""]) {
          const pds = await opts.pds.pdsFor(owner);
          const { sets, eventsByReceipt } = await listOwnContractRecords(pds as never, { plcUrl: opts.plcUrl });
          const listed = new Set(sets.map((s) => s.vm.uri));
          for (const uri of settledVms.keys()) if (!listed.has(uri)) settledVms.delete(uri);
          for (const uri of unclaimedSince.keys()) if (!listed.has(uri)) unclaimedSince.delete(uri);
          for (const { vm, rfps, accepts } of sets) {
            if (claimed.has(vm.uri) || rfps.some((r) => claimed.has(r))) continue;
            const created = Date.parse(vm.createdAt);
            const due = retentionMs > 0 && pds.deleteRepoRecord !== undefined && Number.isFinite(created) && now() - created >= retentionMs;
            if (accepts.length === 0) {
              if (!due || now() >= removalDeadline) continue;
              await removeOwnRecords(pds, [vm.uri, ...rfps]);
              removed++;
              opts.logger.info("requester_orphan_records_removed", { owner, vmUri: vm.uri, rfps, accepted: false, createdAt: vm.createdAt });
              continue;
            }
            if (!settledVms.has(vm.uri)) {
              if (blocking(contracts, owner, vm.vmName, stateOf)) continue;
              if (lookups >= ORPHAN_LOOKUPS_PER_PASS) {
                bounded = true;
                continue;
              }
              lookups++;
              const found = await findVmContract(pds as never, vm.vmName, { plcUrl: opts.plcUrl, payloadUri: vm.uri });
              if (!found || found.deleteSubmitted || isTerminated(found.state) || found.state.release) {
                settledVms.set(vm.uri, found?.state.receipt?.uri ?? "");
                unclaimedSince.delete(vm.uri);
              } else {
                const first = unclaimedSince.get(vm.uri) ?? now();
                unclaimedSince.set(vm.uri, first);
                const since = Math.max(first, startedAt);
                if (now() - since < opts.graceMs) {
                  waiting++;
                  opts.logger.info("requester_receipt_unclaimed_waiting", {
                    owner,
                    vmName: vm.vmName,
                    receiptUri: found.state.receipt?.uri ?? null,
                    waitedMs: now() - since,
                    graceMs: opts.graceMs,
                  });
                  continue;
                }
                const next = await releaseCompute(pds, found.state, { logger: opts.logger, reason: RELEASE_REASON_UNCLAIMED });
                settledVms.set(vm.uri, found.state.receipt?.uri ?? "");
                unclaimedSince.delete(vm.uri);
                opts.logger.info("requester_receipt_unclaimed_released", {
                  owner,
                  vmName: vm.vmName,
                  receiptUri: found.state.receipt?.uri ?? null,
                  ok: next.release?.ok ?? false,
                  error: next.error ?? null,
                });
                continue;
              }
            }
            if (!due || now() >= removalDeadline) continue;
            const events = eventsByReceipt.get(settledVms.get(vm.uri) ?? "") ?? [];
            await removeOwnRecords(pds, [vm.uri, ...rfps, ...accepts, ...events]);
            settledVms.delete(vm.uri);
            removed++;
            opts.logger.info("requester_orphan_records_removed", { owner, vmUri: vm.uri, rfps, accepts, events: events.length, accepted: true, createdAt: vm.createdAt });
          }
        }
        if (bounded) opts.logger.info("requester_orphan_scan_bounded", { lookups, next: "the next scan continues" });
        opts.logger.info("requester_orphan_scan", { owners: knownOwners.size, contracts: contracts.length, lookups, waiting, settled: settledVms.size, removed, retentionMs });
      } finally {
        scanning = false;
      }
    },
  };
}
