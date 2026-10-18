import { findVmContract, listVmRecords, releaseCompute } from "@publicdomainrelay/compute-request-xrpc";
import { isTerminated } from "@publicdomainrelay/compute-request-abc";
import type { ContractState } from "@publicdomainrelay/compute-request-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import type { AccountPdsProvider } from "@publicdomainrelay/requester-controller-abc";
import type { ComputeContract } from "@publicdomainrelay/socialweb-computer-k8s-api";

export const DEFAULT_ORPHAN_SCAN_INTERVAL_MS = 5 * 60_000;

export const DEFAULT_UNCLAIMED_GRACE_MS = 10 * 60_000;

export const ORPHAN_LOOKUPS_PER_PASS = 20;

const RELEASE_REASON_UNCLAIMED = "unclaimed_after_join_grace";

export interface OrphanScannerOptions {
  logger: StructuredLoggerInterface;
  pds: AccountPdsProvider;
  plcUrl: string;
  scanOwners: () => string[];
  graceMs: number;
  intervalMs: number;
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
  const settledVms = new Set<string>();
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
        const claimedPayloads = new Set<string>();
        for (const owner of opts.scanOwners()) knownOwners.add(owner);
        for (const c of contracts) {
          if (c.spec?.ownerDid) knownOwners.add(c.spec.ownerDid);
          const payload = stateOf(c)?.payload?.uri;
          if (payload) claimedPayloads.add(payload);
        }
        let lookups = 0;
        let waiting = 0;
        for (const owner of opts.pds.accounts ? knownOwners : [""]) {
          const pds = await opts.pds.pdsFor(owner);
          const vms = await listVmRecords(pds as never, { plcUrl: opts.plcUrl });
          for (const vm of vms) {
            if (settledVms.has(vm.uri) || claimedPayloads.has(vm.uri) || blocking(contracts, owner, vm.vmName, stateOf)) continue;
            if (lookups >= ORPHAN_LOOKUPS_PER_PASS) {
              opts.logger.info("requester_orphan_scan_bounded", { lookups, next: "the next scan continues" });
              return;
            }
            lookups++;
            const found = await findVmContract(pds as never, vm.vmName, { plcUrl: opts.plcUrl, payloadUri: vm.uri });
            if (!found || found.deleteSubmitted || isTerminated(found.state) || found.state.release) {
              settledVms.add(vm.uri);
              unclaimedSince.delete(vm.uri);
              continue;
            }
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
            settledVms.add(vm.uri);
            unclaimedSince.delete(vm.uri);
            opts.logger.info("requester_receipt_unclaimed_released", {
              owner,
              vmName: vm.vmName,
              receiptUri: found.state.receipt?.uri ?? null,
              ok: next.release?.ok ?? false,
              error: next.error ?? null,
            });
          }
        }
        opts.logger.info("requester_orphan_scan", { owners: knownOwners.size, contracts: contracts.length, lookups, waiting, settled: settledVms.size });
      } finally {
        scanning = false;
      }
    },
  };
}
