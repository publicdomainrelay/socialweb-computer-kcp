import { Secp256k1Keypair } from "@atproto/crypto";
import { parsePolicyArgs } from "@publicdomainrelay/policy-engine-cli-options";
import { bidCost, bidPayloadNsid } from "@publicdomainrelay/requester-abc";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import {
  awaitComputeHostKeys,
  awaitComputeTermination,
  ComputeRequestFailed,
  type ComputeRequestHandle,
  type ComputeRequestOptions,
  findVmContract,
  releaseCompute,
  requestCompute,
  resubmitRelease,
} from "@publicdomainrelay/compute-request-xrpc";
import type { ComputeRequestHandlers, ContractState } from "@publicdomainrelay/compute-request-abc";
import { isTerminated } from "@publicdomainrelay/compute-request-abc";
import type { GuestCapability } from "@publicdomainrelay/guest-capability-abc";
import { createSecretsCapability } from "@publicdomainrelay/guest-capability-secrets";
import { parseSecretsFile, type SecretEntry } from "@publicdomainrelay/secrets-common";
import type { UserDataPatch } from "@publicdomainrelay/cloud-init-common";
import type { IdentityVault, VaultRef } from "@publicdomainrelay/market-identity-abc";
import type { MarketLoopRecorder } from "@publicdomainrelay/market-abc";
import {
  type AccountPdsProvider,
  CONTRACT_TTL_ANNOTATION,
  type ContractDisposition,
  type ContractProfile,
  type ProfilePreparation,
  releaseAcknowledged,
  releaseRetryDeadline,
  releaseRetryDelayMs,
  withContractConditions,
} from "@publicdomainrelay/requester-controller-abc";
import { ExecutionState, type KcpClient, kcpResourceClient, type KcpCondition } from "@publicdomainrelay/kcp-client-abc";
import { isNotFound } from "@publicdomainrelay/kcp-client-common";
import { createCoalescedStatusWriter } from "@publicdomainrelay/kcp-client-rest";
import {
  assertComputeContractStatus,
  COMPUTE_CONTRACT_RESOURCE,
  type ComputeContract,
  type ComputeContractPhase,
  type ComputeContractStatus,
  type ContractBid,
  type ContractChosen,
  type NodeClaimContract,
  type NodeClaimGrant,
  parseComputeContract,
} from "@publicdomainrelay/socialweb-computer-k8s-api";
import { removeContractRecords } from "./retention.ts";

export interface RunnerEnvironment {
  kcp: KcpClient;
  workspace?: string;
  logger: StructuredLoggerInterface;
  vault: IdentityVault;
  vaultMount: string;
  pds: AccountPdsProvider;
  profiles: ContractProfile[];
  plcUrl: string;
  ingressProxyHost: string;
  relayUrls: string[];
  eventStreams: unknown;
  scopeCache?: ComputeRequestOptions["scopeCache"];
  scopeRefresher?: ComputeRequestOptions["scopeRefresher"];
  loop?: MarketLoopRecorder;
  guestNetwork: UserDataPatch;
  guestHostAliases: string[];
  extraBidderDids: string[];
  defaultVmReadyTimeoutSec: number;
  skipRbac: boolean;
  policyEngine?: string;
  policyEngineUrl?: string;
  onlyRemotePolicyExec?: boolean;
  allowUntrustedPolicyExec?: boolean;
  acceptCopies?: number;
  offeringWatcherDids: () => string[];
}

export interface ContractRunner {
  readonly name: string;
  update(contract: ComputeContract | null): void;
  stop(): void;
  readonly done: Promise<void>;
}

const STATUS_WRITE_ATTEMPTS = 3;

const MAX_RECORDED_BIDS = 20;

export const AT_CAPACITY_RETRY_MS = 60_000;

export const RFP_ONLY_ANNOTATION = "socialweb.computer/rfp-only";

const FETCH_BOUND_SLACK_MS = 0;

function prefixed(logger: StructuredLoggerInterface, extra: Record<string, unknown>): StructuredLoggerInterface {
  return {
    debug: () => {},
    info: (event, meta) => logger.info(event, { ...extra, ...meta }),
    warn: (event, meta) => logger.warn(event, { ...extra, ...meta }),
    error: (event, meta) => logger.error(event, { ...extra, ...meta }),
  };
}

function hexOf(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function untilAny(signals: AbortSignal[], timeoutMs: number, also: Promise<unknown>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const bounded = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, timeoutMs);
    for (const signal of signals) {
      if (signal.aborted) resolve();
      else signal.addEventListener("abort", () => resolve(), { once: true });
    }
  });
  return Promise.race([also.then(() => {}), bounded]).finally(() => clearTimeout(timer));
}

export function startContractRunner(
  env: RunnerEnvironment,
  initial: ComputeContract,
  disposition: Exclude<ContractDisposition, { kind: "done" }>,
): ContractRunner {
  const name = initial.metadata?.name ?? "";
  const logger = prefixed(env.logger, { contract: name });
  const contracts = kcpResourceClient<ComputeContract>(
    env.kcp,
    { resource: COMPUTE_CONTRACT_RESOURCE, workspace: env.workspace },
    parseComputeContract,
  );
  const rfpOnly = initial.metadata?.annotations?.[RFP_ONLY_ANNOTATION] === "true";
  const stopping = new AbortController();
  const releaseAsked = Promise.withResolvers<void>();
  const ended = new AbortController();
  const releaseEarly = new AbortController();
  let latest: ComputeContract | null = initial;
  let released = initial.spec?.release === true;
  let deleted = false;

  function update(contract: ComputeContract | null): void {
    latest = contract;
    if (contract === null || contract.metadata?.deletionTimestamp) {
      deleted = true;
      released = true;
    } else if (contract.spec?.release === true) {
      released = true;
    }
    if (released) {
      releaseEarly.abort();
      releaseAsked.resolve();
    }
  }

  const spec = initial.spec!;
  const status0 = initial.status;
  let state: ContractState | undefined = disposition.kind === "resume" ? disposition.state : undefined;
  let secretsUrl = status0?.secretsUrl;
  let secretsFetchedAt = status0?.secretsFetchedAt;
  let grant: NodeClaimGrant | undefined = status0?.grant;
  const bids: ContractBid[] = [...(status0?.bids ?? [])];
  let chosen: ContractChosen | undefined = status0?.chosen;
  let phaseNow: ComputeContractPhase = status0?.phase ?? "Pending";
  let atCapacity: string | undefined;
  let retryAtCapacity = false;
  let rfpAnswers = 0;
  let capacityAnswers = 0;
  let messageNow = status0?.message ?? "";
  const execution = new ExecutionState({
    phase: status0?.phase,
    message: status0?.message,
    conditions: status0?.conditions as KcpCondition[] | undefined,
    execution: status0?.execution ?? {},
  });
  execution.observe(initial.metadata?.generation);

  function contractStatus(): ComputeContractStatus {
    const view = execution.view();
    return withContractConditions({
      ...view,
      phase: phaseNow,
      message: messageNow,
      ...(secretsUrl ? { secretsUrl } : {}),
      ...(secretsFetchedAt ? { secretsFetchedAt } : {}),
      ...(grant ? { grant } : {}),
      ...(bids.length > 0 ? { bids: bids.map((b) => ({ ...b })) } : {}),
      ...(chosen ? { chosen } : {}),
      ...(state ? { contract: state as unknown as NodeClaimContract } : {}),
    } as ComputeContractStatus, new Date(), atCapacity);
  }

  let statusWriteError: unknown;
  const statusWriter = createCoalescedStatusWriter<null>({
    write: async () => {
      if (deleted) return;
      const status = contractStatus();
      assertComputeContractStatus(status);
      for (let attempt = 1;; attempt++) {
        try {
          await contracts.replaceStatus(name, status);
          statusWriteError = undefined;
          return;
        } catch (err) {
          if (isNotFound(err)) {
            deleted = true;
            released = true;
            releaseEarly.abort();
            releaseAsked.resolve();
            return;
          }
          if (attempt >= STATUS_WRITE_ATTEMPTS) throw err;
          logger.warn("requester_contract_status_retry", { attempt, error: String(err) });
          await new Promise((r) => setTimeout(r, 500));
        }
      }
    },
    onError: (err) => {
      statusWriteError = err;
      logger.warn("requester_contract_status_failed", { error: String(err) });
    },
  });

  function stepTo(step: string): void {
    if (execution.step(step, new Date())) logger.info("requester_contract_step", { step });
    statusWriter.set(null);
  }

  function failedStep(err: unknown): void {
    execution.error(err, new Date());
    statusWriter.set(null);
  }

  async function writeStatus(phase: ComputeContractPhase, message: string): Promise<void> {
    phaseNow = phase;
    messageNow = message;
    execution.setPhase(phase, message, new Date());
    statusWriter.set(null, true);
    await statusWriter.flush();
    if (statusWriteError && !deleted) throw statusWriteError;
    logger.info("requester_contract_status", { phase, message, contractPhase: state?.phase, step: execution.view().execution.step });
  }

  async function heldKeyHex(ref: VaultRef, field: string): Promise<string> {
    const existing = await env.vault.read(ref);
    const held = typeof existing?.[field] === "string" ? existing[field] as string : undefined;
    if (held) return held;
    const kp = await Secp256k1Keypair.create({ exportable: true });
    const hex = hexOf(await kp.export());
    await env.vault.write(ref, { ...(existing ?? {}), [field]: hex });
    return hex;
  }

  async function holdUntilEnded(pds: Awaited<ReturnType<AccountPdsProvider["pdsFor"]>>, capabilities: GuestCapability[], handle: ComputeRequestHandle | null): Promise<void> {
    if (!state) return;
    const stopWatch = new AbortController();
    const watched = handle?.terminated
      ? handle.terminated.then((s) => s)
      : awaitComputeTermination(pds, state, {
        signal: stopWatch.signal,
        eventStreams: env.eventStreams as never,
        plcUrl: env.plcUrl,
        logger,
      });
    const termination = watched.then((s) => {
      if (s && isTerminated(s)) {
        state = s;
        ended.abort();
      }
      return s;
    });
    await Promise.race([releaseAsked.promise, termination.then(() => {}), new Promise<void>((resolve) => stopping.signal.addEventListener("abort", () => resolve(), { once: true }))]);
    stopWatch.abort();
    if (stopping.signal.aborted && !released) return;
    if (state.termination) {
      stepTo("terminated");
      await releaseCompute(pds, state, { logger, capabilities }).catch(() => undefined);
      await handle?.dispose().catch(() => undefined);
      await writeStatus("Released", `the bidder ended the contract: ${state.termination.reason}`);
      return;
    }
    stepTo("releasing");
    await writeStatus("Releasing", state.release?.event ? "resending the vm.delete event to the winner" : "submitting vm.delete to the winner");
    state = await releaseCompute(pds, state, { logger, capabilities, plcUrl: env.plcUrl });
    await handle?.dispose().catch(() => undefined);
    await retryRelease(pds);
  }

  async function retryRelease(pds: Awaited<ReturnType<AccountPdsProvider["pdsFor"]>>): Promise<void> {
    if (!state) return;
    const deadline = releaseRetryDeadline(latest?.metadata?.creationTimestamp ?? initial.metadata?.creationTimestamp, initial.metadata?.annotations?.[CONTRACT_TTL_ANNOTATION], Date.now());
    const stopWatch = new AbortController();
    const terminated = awaitComputeTermination(pds, state, {
      signal: stopWatch.signal,
      eventStreams: env.eventStreams as never,
      plcUrl: env.plcUrl,
      logger,
    }).then((s) => (s && isTerminated(s) ? s : new Promise<never>(() => {}))).catch(() => new Promise<never>(() => {}));
    try {
      for (let attempt = 1;; attempt++) {
        if (releaseAcknowledged(state.release?.status)) {
          stepTo("released");
          await writeStatus("Released", `vm.delete accepted (${state.release?.status}) after ${attempt} attempt(s)`);
          return;
        }
        const reason = state.error ?? `vm.delete answered ${state.release?.status ?? "nothing"}`;
        if (!state.release?.event) {
          failedStep(reason);
          stepTo("releaseFailed");
          await writeStatus("Failed", `vm.delete failed before its event was written: ${reason}`);
          return;
        }
        const waitMs = Math.min(releaseRetryDelayMs(attempt), Math.max(0, deadline - Date.now()));
        logger.warn("requester_release_retry", { attempt, reason, nextInMs: waitMs, deadline: new Date(deadline).toISOString(), eventUri: state.release.event.uri });
        await writeStatus("Releasing", `attempt ${attempt}: ${reason}; the same vm.delete event is sent again in ${Math.round(waitMs / 1000)}s, until ${new Date(deadline).toISOString()} or a terminated ack`);
        const ack = await Promise.race([
          terminated,
          untilAny([stopping.signal], waitMs, new Promise<void>(() => {})).then(() => null),
        ]);
        if (ack) {
          state = ack;
          stepTo("released");
          await writeStatus("Released", `the bidder reported the guest terminated (${ack.termination?.reason}) after ${attempt} vm.delete attempt(s)`);
          return;
        }
        if (stopping.signal.aborted) return;
        if (Date.now() >= deadline) {
          failedStep(reason);
          stepTo("releaseFailed");
          await writeStatus("Failed", `vm.delete was not acknowledged by ${new Date(deadline).toISOString()} (the contract TTL) after ${attempt} attempt(s): ${reason}`);
          return;
        }
        state = await resubmitRelease(pds, state, { logger, plcUrl: env.plcUrl });
      }
    } finally {
      stopWatch.abort();
    }
  }

  async function adoptFlow(): Promise<void> {
    stepTo("adopting");
    const pds = await env.pds.pdsFor(spec.ownerDid);
    await writeStatus("Requesting", "a joined Node with no claim: matching it to its receipt and vm.hostKeys record");
    const found = await findVmContract(pds as never, spec.vmName ?? "", { plcUrl: env.plcUrl, logger });
    if (released) {
      await writeStatus("Released", "the contract was released before it was adopted");
      return;
    }
    const why = !found
      ? "no receipt in the owner's records names this node"
      : found.deleteSubmitted
      ? "its receipt was already released with a vm.delete"
      : isTerminated(found.state)
      ? `its bidder reported the guest terminated (${found.state.termination?.reason})`
      : !found.state.hostKeys
      ? "its receipt has no bidder-signed vm.hostKeys record, the guest's signed identity"
      : null;
    if (found) state = found.deleteSubmitted ? { ...found.state, phase: "released" } : found.state;
    if (why) {
      failedStep(why);
      await writeStatus("Failed", `the joined Node could not be adopted: ${why}`);
      return;
    }
    stepTo("held");
    await writeStatus("Held", "adopted: the joined Node, its vault role and its receipt's vm.hostKeys record agree; no RFP and no vm.delete");
    await holdUntilEnded(pds, [], null);
  }

  async function flow(): Promise<void> {
    if (spec.adopt) return await adoptFlow();
    stepTo("preparing");
    const pds = await env.pds.pdsFor(spec.ownerDid);
    const profile = env.profiles.find((p) => p.appliesTo(spec));
    const heldAlready = state?.receipt !== undefined &&
      (phaseNow === "Failed" || (phaseNow === "Releasing" && state.release?.event !== undefined) || (phaseNow === "Held" && state.vmAddress !== undefined && (!spec.secretsRef || secretsFetchedAt !== undefined)));
    if (heldAlready && state) {
      logger.info("requester_contract_held_resume", { phase: phaseNow, receiptUri: state.receipt?.uri });
      await holdUntilEnded(pds, [], null);
      return;
    }

    const capabilities: GuestCapability[] = [];
    const secrets: SecretEntry[] = [];
    let preparation: ProfilePreparation | undefined;
    let fetched = secretsFetchedAt !== undefined;
    const secretsFetched = Promise.withResolvers<void>();
    if (fetched) secretsFetched.resolve();
    if (spec.secretsRef) {
      stepTo("readingSecrets");
      const bundle = await env.vault.read(spec.secretsRef);
      const text = typeof bundle?.bundle === "string" ? bundle.bundle : undefined;
      if (!text) {
        failedStep(`the secrets bundle ${spec.secretsRef.mount}/${spec.secretsRef.path} is not in the vault`);
        await writeStatus("Failed", `the secrets bundle ${spec.secretsRef.mount}/${spec.secretsRef.path} is not in the vault`);
        return;
      }
      secrets.push(...parseSecretsFile(text));
    }
    if (profile) {
      preparation = await profile.prepare({
        name,
        spec,
        secrets,
        logger,
        writeGrant: async (g) => {
          grant = g;
          statusWriter.set(null, true);
          await statusWriter.flush();
          if (statusWriteError && !deleted) {
            grant = undefined;
            return false;
          }
          return true;
        },
      });
    }
    if (spec.secretsRef) {
      const keyHex = await heldKeyHex({ mount: env.vaultMount, path: `contracts/${name}/secrets-key` }, "privateKeyHex");
      const inner = createSecretsCapability({ secrets, logger, privateKeyHex: keyHex });
      const prepare = inner.prepare!.bind(inner);
      inner.prepare = async (ctx) => {
        const prepared = await prepare(ctx);
        secretsUrl = (prepared.ctx as { secretsUrl?: string } | undefined)?.secretsUrl;
        statusWriter.set(null);
        return prepared;
      };
      capabilities.push(preparation?.wrapCapability ? preparation.wrapCapability(inner) : inner);
      logger.info("secrets_capability_enabled", { count: secrets.length, paths: secrets.map((s) => s.path) });
    }

    const policy = spec.policy ? { name: spec.policy, args: parsePolicyArgs(spec.policyArgs) } : undefined;
    const vmReadyTimeoutSec = spec.vmReadyTimeoutSec ?? env.defaultVmReadyTimeoutSec;
    let acceptedAt = Date.now();

    const track = (phase: ComputeContractPhase, what: string) => async (_event: unknown, s: ContractState) => {
      state = s;
      if (s.vmAddress && s.hostKeys) await writeStatus(phaseNow, messageNow);
      else await writeStatus(phase, what);
      return "continue" as const;
    };
    const aborted = () => preparation?.abortReason?.();
    const decide = () => (aborted() || released || stopping.signal.aborted ? "return" as const : "continue" as const);
    const handlers: ComputeRequestHandlers = {
      onRfpSubmitted: async (event, s) => {
        stepTo("collectingBids");
        await track("Requesting", "the RFP is submitted")(event, s);
        return rfpOnly ? "return" as const : decide();
      },
      onBid: (event) => {
        env.loop?.bid();
        execution.event(new Date());
        if (bids.length < MAX_RECORDED_BIDS && !bids.some((b) => b.uri === event.bid.uri)) {
          const cost = bidCost(event.bid);
          const offer = bidPayloadNsid(event.bid);
          bids.push({
            did: event.bid.did,
            uri: event.bid.uri,
            ...(offer ? { offer } : {}),
            ...(Number.isFinite(cost) ? { cost } : {}),
            verdict: "considered",
            arrivedAt: event.arrivedAt,
          });
        }
        statusWriter.set(null);
        return "accept" as const;
      },
      onWinner: (event) => {
        chosen = { uri: event.bid.uri, did: event.did, reason: event.reason, considered: event.considered };
        for (const b of bids) b.verdict = b.uri === event.bid.uri ? "chosen" : "notChosen";
        statusWriter.set(null, true);
      },
      onAccepting: () => {
        stepTo("accepting");
        return decide();
      },
      onAccepted: async (event, s) => {
        env.loop?.win();
        acceptedAt = Date.now();
        stepTo("awaitingReceipt");
        await track("Requesting", "a bid is accepted")(event, s);
        return "continue" as const;
      },
      onReceipt: async (event, s) => {
        stepTo("awaitingGuestOnNetwork");
        await track("Requesting", "the winner's receipt is in")(event, s);
        return decide();
      },
      onNetwork: async (event, s) => {
        stepTo(preparation?.requireSecretsFetched ? "awaitingSecretsFetch" : "awaitingHostKeys");
        state = s;
        if (s.hostKeys) await writeStatus(phaseNow, messageNow);
        else await writeStatus("Requesting", `the guest reported onNetwork at ${event.address}; waiting for its host keys`);
        return decide();
      },
      onHostKeys: async (_event, s) => {
        if (s.vmAddress && !preparation?.requireSecretsFetched) stepTo("held");
        state = s;
        if (!preparation?.requireSecretsFetched) await writeStatus(s.vmAddress ? "Held" : "Requesting", "the guest reported its ssh host keys");
        else statusWriter.set(null);
      },
      onTerminated: async (event, s) => {
        stepTo("terminated");
        state = s;
        await writeStatus("Released", `the bidder ended the contract: ${event.reason}`);
      },
      onSecretsFetched: async (event) => {
        execution.event(new Date());
        logger.info("requester_secrets_fetched", { ...event });
        secretsFetchedAt = new Date().toISOString();
        fetched = true;
        secretsFetched.resolve();
        statusWriter.set(null, true);
        return "continue" as const;
      },
    };

    logger.info("requester_contract_start", { resume: state?.phase ?? null, vmName: spec.vmName });
    const profileModules = preparation?.userDataModules ?? [];
    let handle: ComputeRequestHandle;
    stepTo(state?.phase ? "resuming" : "postingRfp");
    try {
      handle = await requestCompute(pds, {
        logger,
        ingressProxyHost: env.ingressProxyHost,
        plcUrl: env.plcUrl,
        guestHostAliases: env.guestHostAliases,
        vmName: spec.vmName,
        sshAuthorizedKeys: spec.sshAuthorizedKeys ?? [],
        vmReadyTimeoutSec,
        extraBidderDids: [...env.extraBidderDids, ...(spec.bidderDids ?? [])],
        denyBidderDids: spec.denyBidderDids ?? [],
        relayUrls: env.relayUrls,
        userData: { base: spec.userData, modules: [...profileModules, () => env.guestNetwork] },
        rbac: !env.skipRbac,
        capabilities,
        policy,
        policyEngine: env.policyEngine,
        policyEngineUrl: env.policyEngineUrl,
        onlyRemotePolicyExec: env.onlyRemotePolicyExec,
        allowUntrustedPolicyExec: env.allowUntrustedPolicyExec,
        offeringWatcherDids: env.offeringWatcherDids,
        eventStreams: env.eventStreams,
        ...(env.scopeCache ? { scopeCache: env.scopeCache } : {}),
        ...(env.scopeRefresher ? { scopeRefresher: env.scopeRefresher } : {}),
        resume: state,
        returnSignal: releaseEarly.signal,
        acceptCopies: env.acceptCopies,
        onRfpAnswer: (answer: { error?: string }) => {
          rfpAnswers++;
          if (answer.error === "capacity") capacityAnswers++;
        },
      } as unknown as ComputeRequestOptions, handlers);
    } catch (err) {
      if (err instanceof ComputeRequestFailed) state = err.state;
      failedStep(err);
      await writeStatus("Failed", `the contract flow threw: ${err instanceof Error ? err.message : String(err)}`);
      return;
    }
    state = handle.state;

    if (!state.receipt) {
      if (released) {
        await handle.dispose();
        await writeStatus("Released", "the contract was released before a VM was held");
        return;
      }
      if (state.outcome === "no_bids" && rfpAnswers > 0 && capacityAnswers === rfpAnswers && aborted() === undefined) {
        atCapacity = `every bidder (${rfpAnswers}) declined the RFP with reason capacity; the contract asks again in ${AT_CAPACITY_RETRY_MS / 1000}s`;
        retryAtCapacity = true;
        await handle.dispose();
        stepTo("atCapacity");
        await writeStatus("Pending", atCapacity);
        await removeContractRecords(pds, state).catch((err) => logger.warn("requester_round_records_removal_failed", { error: String(err) }));
        return;
      }
      const why = preparation?.explainEnd(state, { error: state.error, grantFailed: aborted() !== undefined }) ??
        `no contract: ${state.outcome ?? state.error ?? state.phase}`;
      failedStep(state.outcome ?? state.error ?? state.phase);
      stepTo("noContract");
      await handle.dispose();
      await writeStatus("Failed", preparation ? why : `no contract: ${state.outcome ?? state.error ?? state.phase}`);
      return;
    }

    if (released) {
      await holdUntilEnded(pds, handle.capabilities, handle);
      return;
    }

    const bailed = aborted();
    if (bailed) {
      failedStep(bailed);
      await writeStatus("Failed", preparation?.explainEnd(state, { grantFailed: true }) ?? bailed);
      await holdUntilEnded(pds, handle.capabilities, handle);
      return;
    }

    if (preparation?.requireSecretsFetched) {
      const guestEnded = handle.terminated?.then((s) => {
        state = s;
        ended.abort();
        return s;
      });
      if (!fetched && state.outcome === "complete" && state.receiptOk === true && state.vmAddress !== undefined && !isTerminated(state) && !released) {
        await untilAny(
          [releaseEarly.signal, stopping.signal, ended.signal],
          Math.max(0, acceptedAt + vmReadyTimeoutSec * 1000 + FETCH_BOUND_SLACK_MS - Date.now()),
          guestEnded ? Promise.race([secretsFetched.promise, guestEnded]) : secretsFetched.promise,
        );
      }
      if (stopping.signal.aborted && !released) {
        logger.info("requester_contract_left_for_restart", { phase: phaseNow });
        return;
      }
      if (!fetched && !released && !state.termination) {
        const why = preparation.explainEnd(state, { grantFailed: false });
        failedStep(why);
        await writeStatus("Failed", why);
        await holdUntilEnded(pds, handle.capabilities, handle);
        return;
      }
      if (fetched && !state.termination && !released) {
        stepTo("held");
        await writeStatus("Held", "held: the node fetched its join secrets and is joining its cluster; set spec.release to release it");
      }
    } else {
      if (state.vmAddress && !state.hostKeys) stepTo("awaitingHostKeys");
      if (state.vmAddress && !state.hostKeys && handle.hostKeys) {
        const hostKeys = await Promise.race([
          handle.hostKeys,
          new Promise<null>((r) => setTimeout(() => r(null), vmReadyTimeoutSec * 1000)),
        ]);
        if (hostKeys) state = { ...state, hostKeys };
      }
      if (state.vmAddress && !state.hostKeys) {
        state = await awaitComputeHostKeys(pds, state, { logger, eventStreams: env.eventStreams as never, plcUrl: env.plcUrl, timeoutMs: 120_000 });
      }
      if (!state.termination) {
        const held = state.vmAddress && state.hostKeys;
        stepTo(held ? "held" : "awaitingRelease");
        await writeStatus(
          held ? "Held" : "Failed",
          held
            ? `held: the guest is on the network at ${state.vmAddress}; set spec.release to release it`
            : `the guest did not report ${state.vmAddress ? "its ssh host keys" : "onNetwork"} (${state.outcome ?? state.error ?? state.phase}); set spec.release to release the contract`,
        );
      }
    }
    await holdUntilEnded(pds, handle.capabilities, handle);
  }

  const done = (async () => {
    try {
      for (;;) {
        retryAtCapacity = false;
        atCapacity = undefined;
        rfpAnswers = 0;
        capacityAnswers = 0;
        await flow();
        if (!retryAtCapacity || released || stopping.signal.aborted) break;
        await untilAny([stopping.signal, releaseEarly.signal], AT_CAPACITY_RETRY_MS, new Promise<void>(() => {}));
        if (released || stopping.signal.aborted) break;
        state = undefined;
        bids.length = 0;
        chosen = undefined;
      }
    } catch (err) {
      logger.error("requester_contract_failed", { error: String(err) });
      failedStep(err);
      await writeStatus("Failed", `the contract flow threw: ${err instanceof Error ? err.message : String(err)}`).catch(() => undefined);
    } finally {
      await statusWriter.close().catch(() => undefined);
    }
  })();

  if (released) update(initial);

  return {
    name,
    update,
    stop() {
      stopping.abort();
    },
    done,
  };
}
