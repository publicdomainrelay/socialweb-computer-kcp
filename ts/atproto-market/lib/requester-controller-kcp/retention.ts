import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import type { ContractState } from "@publicdomainrelay/compute-request-abc";
import type { AccountPdsProvider } from "@publicdomainrelay/requester-controller-abc";
import type { RequesterPDS } from "@publicdomainrelay/requester-abc";
import type { ComputeContract } from "@publicdomainrelay/socialweb-computer-k8s-api";

export const DEFAULT_RECORD_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;
export const DEFAULT_RECORD_RETENTION_SCAN_MS = 10 * 60 * 1000;
export const RECORDS_REMOVED_ANNOTATION = "socialweb.computer/records-removed";
export const SWEEP_CONTRACTS_PER_PASS = 500;

export interface RecordRetentionOptions {
  logger: StructuredLoggerInterface;
  pds: AccountPdsProvider;
  retentionMs: number;
  intervalMs?: number;
  annotate(name: string, annotations: Record<string, string>): Promise<void>;
  running?(name: string): boolean;
  now?: () => number;
}

export interface RecordRetention {
  due(): boolean;
  sweep(contracts: ComputeContract[], stateOf: (c: ComputeContract) => ContractState | null): Promise<number>;
}

function finishedAtOf(contract: ComputeContract): number | undefined {
  let at: number | undefined;
  for (const c of contract.status?.conditions ?? []) {
    if ((c.type !== "Complete" && c.type !== "Failed") || c.status !== "True") continue;
    const t = Date.parse(String(c.lastTransitionTime ?? ""));
    if (Number.isFinite(t) && (at === undefined || t > at)) at = t;
  }
  return at;
}

function atUriParts(uri: string | undefined): { did: string; collection: string; rkey: string } | undefined {
  if (!uri) return undefined;
  const m = /^at:\/\/([^/]+)\/([^/]+)\/([^/]+)$/.exec(uri);
  return m ? { did: m[1], collection: m[2], rkey: m[3] } : undefined;
}

export function removeContractRecords(pds: RequesterPDS, state: ContractState | null | undefined): Promise<void> {
  return removeOwnRecords(pds, [state?.rfp?.uri, state?.payload?.uri, state?.accept?.uri, state?.release?.payload.uri, state?.release?.event.uri, state?.submitEventRef]);
}

export async function removeOwnRecords(pds: RequesterPDS, uris: Array<string | undefined>): Promise<void> {
  const repoDid = (pds as unknown as { oauthSession?: { userDid?: string } }).oauthSession?.userDid ?? pds.did;
  for (const uri of uris) {
    const at = atUriParts(uri);
    if (!at || at.did !== repoDid || !pds.deleteRepoRecord) continue;
    try {
      await pds.deleteRepoRecord(at.collection, at.rkey);
    } catch (err) {
      if (!/404|not ?found|RecordNotFound/i.test(String(err))) throw err;
    }
  }
}

export function createRecordRetention(opts: RecordRetentionOptions): RecordRetention {
  const now = opts.now ?? Date.now;
  const intervalMs = opts.intervalMs ?? DEFAULT_RECORD_RETENTION_SCAN_MS;
  const warned = new Set<string>();
  let lastAt = 0;

  async function removeFor(contract: ComputeContract, state: ContractState | null): Promise<boolean> {
    const name = contract.metadata?.name ?? "";
    const owner = contract.spec?.ownerDid;
    const pds = await opts.pds.pdsFor(owner);
    if (!pds.deleteRepoRecord) {
      if (!warned.has(name)) {
        warned.add(name);
        opts.logger.warn("requester_records_not_removable", { contract: name, why: "this requester's repo has no deleteRepoRecord" });
      }
      return false;
    }
    await removeContractRecords(pds, state);
    return true;
  }

  return {
    due() {
      return now() - lastAt >= intervalMs;
    },
    async sweep(contracts, stateOf) {
      lastAt = now();
      let removed = 0;
      for (const contract of contracts) {
        if (removed >= SWEEP_CONTRACTS_PER_PASS) break;
        const name = contract.metadata?.name;
        if (!name || contract.metadata?.annotations?.[RECORDS_REMOVED_ANNOTATION]) continue;
        if (contract.status?.phase !== "Released" && contract.status?.phase !== "Failed") continue;
        if (opts.running?.(name)) continue;
        const finished = finishedAtOf(contract);
        if (finished === undefined || now() - finished < opts.retentionMs) continue;
        try {
          if (!(await removeFor(contract, stateOf(contract)))) continue;
          await opts.annotate(name, { [RECORDS_REMOVED_ANNOTATION]: new Date(now()).toISOString() });
          removed++;
          opts.logger.info("requester_records_removed", { contract: name, retentionMs: opts.retentionMs });
        } catch (err) {
          opts.logger.warn("requester_records_removal_failed", { contract: name, error: String(err) });
        }
      }
      return removed;
    },
  };
}
