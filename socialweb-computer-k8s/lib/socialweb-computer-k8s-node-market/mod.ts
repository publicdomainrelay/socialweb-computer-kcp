import type { ContractState } from "@publicdomainrelay/compute-request-abc";
import { parseContractState } from "@publicdomainrelay/compute-request-abc";
import type { SecretEntry } from "@publicdomainrelay/secrets-common";
import type { IdentityVault } from "@publicdomainrelay/market-identity-abc";
import type { KcpResourceClient } from "@publicdomainrelay/kcp-client-abc";
import { isNotFound, isQuotaExceeded } from "@publicdomainrelay/kcp-client-common";
import type {
  ClusterLinkReader,
  NodeClaim,
  NodeClaimStore,
  PinnedDownload,
} from "@publicdomainrelay/socialweb-computer-k8s-abc";
import { nodeClaimKey } from "@publicdomainrelay/socialweb-computer-k8s-abc";
import { MESHD_ARCHES, type MeshdPin } from "@publicdomainrelay/cloud-init-plugin-k3s-join";
import {
  clusterLinkName,
  NODE_LINK_ENV_PATH,
  nodeLinkEnv,
  OPENBAO_CA_PATH,
  OPENBAO_TICKET_PATH,
} from "@publicdomainrelay/socialweb-computer-k8s-common";
import {
  type ComputeContract,
  type ComputeContractSpec,
  FINALIZER_NODE_CLAIM,
  type NodeClaimContract,
  type NodeClaimPhase,
  type NodeClaimSpec,
  type NodeClaimStatus,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export type Log = (event: string, data?: Record<string, unknown>) => void;

export interface NodeClaimMapperOptions {
  store: NodeClaimStore;
  links: ClusterLinkReader;
  contracts: KcpResourceClient<ComputeContract>;
  vault: IdentityVault;
  vaultMount: string;
  vmReadyTimeoutSec?: number;
  policy?: { name: string; args: Record<string, unknown> };
  extraBidderDids?: string[];
  userDataBase?: string;
  pollIntervalMs?: number;
  openBaoCAFileOf?: (spec: NodeClaimSpec) => string;
  readText?: (path: string) => Promise<string>;
  meshd?: MeshdPin;
  log?: Log;
}

export interface NodeClaimMapper {
  pass(): Promise<void>;
  start(): void;
  stop(): Promise<void>;
}

const IN_FLIGHT_PHASES: ReadonlySet<NodeClaimPhase> = new Set(["Bidding", "Provisioning"]);

const ENDED_PHASES: ReadonlySet<NodeClaimPhase> = new Set(["Failed", "Released", "Lost"]);

const CONTRACT_NAME_PREFIX = "nodeclaim-";

const NODE_CLAIM_ANNOTATION = "socialweb.computer/nodeclaim";

const CONTRACT_TTL_ANNOTATION = "socialweb.computer/ttl-after-finished";

const CONTRACT_TTL = "8d";

const NODE_CLAIM_UID_LABEL = "socialweb.computer/nodeclaim-uid";

export const OPENBAO_CA_RELATIVE_TO_CLUSTER_DIR = "openbao/tls/ca.crt";

export const MESHD_DOWNLOAD_PREFIX = "socialweb-meshd-";

export const DEFAULT_NODE_VM_READY_TIMEOUT_SEC = 120;

export function meshdPinOf(downloads: PinnedDownload[], downloadBase: string | undefined): MeshdPin | null {
  if (!downloadBase) return null;
  const sha256: Partial<MeshdPin["sha256"]> = {};
  for (const arch of MESHD_ARCHES) {
    const found = downloads.find((d) => d.name === `${MESHD_DOWNLOAD_PREFIX}${arch}`);
    if (!found) return null;
    sha256[arch] = found.sha256;
  }
  return { downloadBase, sha256: sha256 as MeshdPin["sha256"] };
}

export function defaultOpenBaoCAFile(spec: NodeClaimSpec): string {
  const dir = spec.tokenFile.replace(/\/+[^/]*$/, "");
  return `${dir}/${OPENBAO_CA_RELATIVE_TO_CLUSTER_DIR}`;
}

export function publishedOpenBaoCAFile(caDir: string): (spec: NodeClaimSpec) => string {
  return (spec) => {
    const dir = spec.tokenFile.replace(/\/+[^/]*$/, "");
    const name = dir.replace(/^.*\//, "");
    return `${caDir.replace(/\/+$/, "")}/${name}.crt`;
  };
}

function definedOnly<T extends object>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, v]) => v !== undefined)) as T;
}

export function claimContractOf(state: ContractState): NodeClaimContract {
  return definedOnly({
    ...state,
    vmUri: state.payload?.uri,
    rfpUri: state.rfp?.uri,
    acceptUri: state.accept?.uri,
    receiptUri: state.receipt?.uri,
    receiptCid: state.receipt?.cid,
  });
}

export function lostMessage(state: ContractState): string {
  const t = state.termination;
  if (!t) return "the node's guest is gone";
  const exit = t.exitCode !== undefined ? `, exit code ${t.exitCode}` : "";
  return `the bidder reported the guest terminated (${t.reason}${exit}) at ${t.observedAt}; the node is ` +
    "released without a vm.delete and replaced through a new claim and RFP";
}

async function sha256Hex(text: string): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
  return Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("");
}

export async function contractNameOfClaim(claim: Pick<NodeClaim, "workspace" | "name" | "uid">): Promise<string> {
  const basis = claim.uid ?? `${claim.workspace}/${claim.name}`;
  return `${CONTRACT_NAME_PREFIX}${(await sha256Hex(basis)).slice(0, 20)}`;
}

function stateOfContract(contract: ComputeContract): ContractState | null {
  const raw = contract.status?.contract;
  if (!raw || Object.keys(raw).length === 0) return null;
  try {
    return parseContractState(raw);
  } catch {
    return null;
  }
}

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .filter(([, v]) => v !== undefined)
        .sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)
        .map(([k, v]) => [k, canonical(v)]),
    );
  }
  return value;
}

function sameStatus(a: NodeClaimStatus, b: NodeClaimStatus): boolean {
  return JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
}

export function createNodeClaimMapper(opts: NodeClaimMapperOptions): NodeClaimMapper {
  const log = opts.log ?? (() => {});
  const readText = opts.readText ?? ((p: string) => Deno.readTextFile(p));
  const openBaoCAFileOf = opts.openBaoCAFileOf ?? defaultOpenBaoCAFile;
  const vmReadyTimeoutSec = opts.vmReadyTimeoutSec ?? DEFAULT_NODE_VM_READY_TIMEOUT_SEC;
  const stopping = new AbortController();
  let timer: ReturnType<typeof setInterval> | undefined;
  let passing: Promise<void> | null = null;
  let again = false;
  const creating = new Set<string>();

  const bundleRef = (contractName: string) => ({ mount: opts.vaultMount, path: `contracts/${contractName}/bundle` });

  async function replaceStatus(claim: NodeClaim, status: NodeClaimStatus): Promise<void> {
    if (sameStatus(claim.status, status)) return;
    try {
      await opts.store.replaceStatus(claim, status);
      claim.status = status;
    } catch (err) {
      log("node_claim_status_write_failed", { claim: nodeClaimKey(claim), error: String(err) });
    }
  }

  async function buildBundle(claim: NodeClaim): Promise<SecretEntry[]> {
    const spec = claim.spec;
    const linkName = clusterLinkName(spec.cluster, "openbao");
    const link = await opts.links.get(claim.workspace, linkName);
    const ticket = link?.status.ticket;
    if (!ticket) {
      throw new Error(
        `the cluster's OpenBao link ${linkName} carries no ticket yet${link?.status.message ? ` (${link.status.message})` : ""}`,
      );
    }
    const openBaoCA = await readText(openBaoCAFileOf(spec));
    const bundle: SecretEntry[] = [
      { path: OPENBAO_TICKET_PATH, value: ticket },
      { path: OPENBAO_CA_PATH, value: openBaoCA },
    ];
    const apiserverName = clusterLinkName(spec.cluster, "apiserver");
    let apiserverLink: Awaited<ReturnType<ClusterLinkReader["get"]>> = null;
    let apiserverWhy: string | null = null;
    try {
      apiserverLink = await opts.links.get(claim.workspace, apiserverName);
    } catch (err) {
      apiserverWhy = String(err);
    }
    const linkEnv = apiserverLink?.status.phase === "Listening" && apiserverLink.status.ticket
      ? nodeLinkEnv(
        { node: spec.nodeName, advertiseAddress: spec.advertiseAddress, apiserverPort: spec.apiserverPort },
        apiserverLink.status.ticket,
      )
      : null;
    if (linkEnv) {
      bundle.push({ path: NODE_LINK_ENV_PATH, value: linkEnv });
      log("node_claim_apiserver_link_delivered", { claim: nodeClaimKey(claim), link: apiserverName });
    } else {
      log("node_claim_apiserver_link_withheld", {
        claim: nodeClaimKey(claim),
        link: apiserverName,
        phase: apiserverLink?.status.phase ?? null,
        ticketBytes: apiserverLink?.status.ticket?.length ?? 0,
        error: apiserverWhy,
        next: "the guest reads its apiserver ticket out of the cluster OpenBao instead, once its own link " +
          "to that OpenBao is up, so this node's join is serial rather than parallel",
      });
    }
    return bundle;
  }

  function specOf(claim: NodeClaim, name: string, adopt: boolean): ComputeContractSpec {
    const spec = claim.spec;
    return {
      vmName: spec.nodeName,
      ownerDid: spec.ownerDid,
      ...(spec.denyBidderDids?.length ? { denyBidderDids: spec.denyBidderDids } : {}),
      ...(opts.extraBidderDids?.length ? { bidderDids: opts.extraBidderDids } : {}),
      ...(opts.userDataBase ? { userData: opts.userDataBase } : {}),
      ...(opts.policy ? { policy: opts.policy.name, policyArgs: JSON.stringify(opts.policy.args) } : {}),
      vmReadyTimeoutSec,
      ...(adopt ? { adopt: true } : {
        secretsRef: bundleRef(name),
        nodeJoin: {
          cluster: spec.cluster,
          ...(opts.meshd
            ? {
              meshd: {
                downloadBase: opts.meshd.downloadBase,
                pins: MESHD_ARCHES.map((arch) => ({ arch, sha256: opts.meshd!.sha256[arch] })),
              },
            }
            : {}),
        },
      }),
    };
  }

  async function ensureContract(claim: NodeClaim, adopt: boolean): Promise<void> {
    const key = nodeClaimKey(claim);
    const name = await contractNameOfClaim(claim);
    if (creating.has(name)) return;
    creating.add(name);
    try {
      if (!claim.finalizers.includes(FINALIZER_NODE_CLAIM)) await opts.store.addFinalizer(claim);
      if (await opts.contracts.get(name)) return;
      if (!adopt) {
        const bundle = await buildBundle(claim);
        await opts.vault.write(bundleRef(name), { bundle: JSON.stringify(bundle) });
      }
      const { created } = await opts.contracts.createIfAbsent({
        apiVersion: "socialweb.computer/v1alpha1",
        kind: "ComputeContract",
        metadata: {
          name,
          labels: claim.uid ? { [NODE_CLAIM_UID_LABEL]: claim.uid } : {},
          annotations: { [NODE_CLAIM_ANNOTATION]: key, [CONTRACT_TTL_ANNOTATION]: CONTRACT_TTL },
        },
        spec: specOf(claim, name, adopt),
      } as ComputeContract);
      log("node_claim_contract_created", { claim: key, contract: name, adopt, created });
      await replaceStatus(claim, {
        ...claim.status,
        phase: adopt ? "Adopting" : "Bidding",
        message: adopt
          ? "a joined Node with no claim: matching it to its receipt and vm.hostKeys record"
          : "requesting a node from the compute market",
        contract: claim.status.contract ?? {},
      });
    } catch (err) {
      if (isQuotaExceeded(err)) {
        const message = "AtCapacity: the quota of ComputeContracts of this gateway is used up; the claim waits";
        log("node_claim_contract_at_capacity", { claim: key, error: String(err) });
        if (claim.status.message !== message) await replaceStatus(claim, { ...claim.status, phase: "Pending", message });
        return;
      }
      log("node_claim_contract_failed", { claim: key, error: String(err) });
      await replaceStatus(claim, {
        ...claim.status,
        phase: "Failed",
        message: `the contract failed: ${String(err)}`,
      });
    } finally {
      creating.delete(name);
    }
  }

  function desiredStatus(claim: NodeClaim, contract: ComputeContract): NodeClaimStatus | null {
    const phase = claim.status.phase;
    if (phase && ENDED_PHASES.has(phase)) return null;
    const cs = contract.status;
    if (!cs?.phase) return null;
    if ((phase === "Joining" || phase === "Running") && cs.phase !== "Released" && cs.phase !== "Failed") return null;
    const state = stateOfContract(contract);
    const adopt = contract.spec?.adopt === true;
    const next: NodeClaimStatus = { ...claim.status };
    if (state) {
      next.contract = claimContractOf(state);
      if (state.vmAddress) next.fqdn = state.vmAddress;
    }
    if (cs.grant) next.grant = cs.grant;
    const inFlight = !phase || phase === "Pending" || IN_FLIGHT_PHASES.has(phase) || phase === "Adopting";
    switch (cs.phase) {
      case "Pending":
        break;
      case "Requesting":
        if (inFlight && !adopt) {
          const accepted = state?.accept !== undefined;
          next.phase = accepted ? "Provisioning" : "Bidding";
          next.message = accepted ? "a bid was accepted; the VM is booting" : "requesting a node from the compute market";
        }
        break;
      case "Held":
        if (adopt) {
          if (phase === "Adopting" || inFlight) {
            next.phase = "Running";
            next.message = cs.message ?? "adopted";
          }
        } else if (cs.secretsFetchedAt && inFlight) {
          next.phase = "Joining";
          next.message = "the node fetched its join secrets and is joining its cluster";
        } else if (!cs.secretsFetchedAt && inFlight) {
          next.phase = "Provisioning";
          next.message = "a bid was accepted; the VM is booting";
        }
        break;
      case "Releasing":
        break;
      case "Released":
        if (state?.termination) {
          next.phase = "Lost";
          next.message = lostMessage(state);
        } else {
          next.phase = "Released";
          next.message = cs.message ?? "the contract was released";
        }
        break;
      case "Failed":
        next.phase = "Failed";
        next.message = cs.message ?? "the contract failed";
        break;
    }
    return sameStatus(claim.status, next) ? null : next;
  }

  async function settleDeleted(claim: NodeClaim): Promise<void> {
    const key = nodeClaimKey(claim);
    if (!claim.finalizers.includes(FINALIZER_NODE_CLAIM)) return;
    const name = await contractNameOfClaim(claim);
    let contract: ComputeContract | null;
    try {
      contract = await opts.contracts.get(name);
    } catch (err) {
      log("node_claim_release_deferred", { claim: key, error: String(err) });
      return;
    }
    if (contract && contract.spec?.release !== true) {
      await opts.contracts.patch(name, { type: "merge", body: { spec: { release: true } } }).catch((err) => {
        if (!isNotFound(err)) log("node_claim_release_patch_failed", { claim: key, error: String(err) });
      });
      log("node_claim_release_requested", { claim: key, contract: name });
      return;
    }
    const finished = !contract || contract.status?.phase === "Released" || contract.status?.phase === "Failed";
    if (!finished && contract?.status?.phase === "Releasing") {
      await replaceStatus(claim, {
        ...claim.status,
        phase: "Failed",
        message: `the claim was deleted; the release of its VM is still retrying on ComputeContract ${name}`,
      });
      await opts.store.removeFinalizer(claim).catch((err) => log("node_claim_finalizer_release_failed", { claim: key, error: String(err) }));
      log("node_claim_released_release_retrying", { claim: key, contract: name });
      return;
    }
    if (!finished) return;
    const state = contract ? stateOfContract(contract) : null;
    const confirmed = !contract || contract.status?.phase === "Released" || state?.receipt === undefined;
    await replaceStatus(claim, {
      ...claim.status,
      phase: confirmed ? "Released" : "Failed",
      message: confirmed
        ? "the claim was deleted and the node's VM released"
        : "the claim was deleted but the node's VM could not be confirmed released",
      ...(state ? { contract: claimContractOf(state) } : {}),
    });
    if (contract) await opts.contracts.delete(name).catch((err) => {
      if (!isNotFound(err)) log("node_claim_contract_delete_failed", { claim: key, error: String(err) });
    });
    await opts.vault.remove(bundleRef(name)).catch((err) => log("node_claim_bundle_remove_failed", { claim: key, error: String(err) }));
    await opts.store.removeFinalizer(claim).catch((err) => log("node_claim_finalizer_release_failed", { claim: key, error: String(err) }));
    log("node_claim_released", { claim: key, contract: name, confirmed });
  }

  async function pass(): Promise<void> {
    const claims = await opts.store.list();
    const byName = new Map<string, ComputeContract>();
    for (const c of (await opts.contracts.list()).items) if (c.metadata?.name) byName.set(c.metadata.name, c);
    for (const claim of claims) {
      const key = nodeClaimKey(claim);
      try {
        if (claim.deletionTimestamp) {
          await settleDeleted(claim);
          continue;
        }
        if (stopping.signal.aborted) return;
        const name = await contractNameOfClaim(claim);
        const contract = byName.get(name);
        const phase = claim.status.phase;
        if (!contract) {
          if (phase === "Adopting" || ((!phase || phase === "Pending") && claim.adoptJoinedNode)) {
            await ensureContract(claim, true);
          } else if (!phase || phase === "Pending" || IN_FLIGHT_PHASES.has(phase)) {
            await ensureContract(claim, false);
          }
          continue;
        }
        const next = desiredStatus(claim, contract);
        if (next) {
          await replaceStatus(claim, next);
          log("node_claim_mapped", { claim: key, contract: name, phase: next.phase ?? null });
        }
      } catch (err) {
        log("node_claim_pass_failed", { claim: key, error: String(err) });
      }
    }
    const live = new Set(claims.map(nodeClaimKey));
    for (const [name, c] of byName) {
      const owner = c.metadata?.annotations?.[NODE_CLAIM_ANNOTATION];
      if (!owner || live.has(owner)) continue;
      if (c.status?.phase !== "Released" && c.status?.phase !== "Failed") continue;
      await opts.contracts.delete(name).catch((err) => {
        if (!isNotFound(err)) log("node_claim_contract_delete_failed", { claim: owner, error: String(err) });
      });
      await opts.vault.remove(bundleRef(name)).catch((err) => log("node_claim_bundle_remove_failed", { claim: owner, error: String(err) }));
      log("node_claim_orphan_contract_removed", { claim: owner, contract: name, phase: c.status?.phase });
    }
  }

  function tick(): void {
    if (passing) {
      again = true;
      return;
    }
    passing = pass().catch((err) => log("node_claim_list_failed", { error: String(err) })).finally(() => {
      passing = null;
      if (again && !stopping.signal.aborted) {
        again = false;
        tick();
      }
    });
  }

  async function followContracts(): Promise<void> {
    while (!stopping.signal.aborted) {
      try {
        for await (const event of opts.contracts.watch({ signal: stopping.signal })) {
          if (event.type === "BOOKMARK") continue;
          tick();
          if (event.type === "ERROR") break;
        }
      } catch {
        if (stopping.signal.aborted) return;
      }
      await new Promise((r) => setTimeout(r, 2_000));
    }
  }

  return {
    pass,
    start() {
      if (timer !== undefined) return;
      tick();
      timer = setInterval(tick, opts.pollIntervalMs ?? 5_000);
      opts.store.watch?.(stopping.signal, tick);
      void followContracts();
    },
    async stop() {
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
      stopping.abort();
      await passing;
    },
  };
}
