import { Secp256k1Keypair } from "@atproto/crypto";
import { IdResolver } from "@atproto/identity";
import type { StructuredLoggerInterface } from "@publicdomainrelay/logger";
import { createOAuthAgentFromSession, createRemoteAgent, pollForOAuthSession } from "@publicdomainrelay/atproto-helpers";
import type { AtprotoAgentLike, OAuthSessionData } from "@publicdomainrelay/atproto-helpers";
import { createGenesisOp, createPlcDirectoryClient, PlcNotFoundError } from "@publicdomainrelay/did-plc";
import { loadOrGenerateKeypair } from "@publicdomainrelay/market-atproto";
import { BADGE_BLUE_KEYS_NSID } from "@publicdomainrelay/market-lexicons";
import type {
  AssociatingRepo,
  BrowserOAuthClient,
  IdentityVault,
  SessionStore,
  MarketIdentity,
  IdentityPrompt,
  MarketIdentityPendingAction,
  MarketIdentityPhase,
  MarketIdentityStatus,
  VaultRef,
  WatchedRecord,
} from "@publicdomainrelay/market-identity-abc";
import {
  associationQrUrl,
  bidderAssociationCaller,
  callbackRef,
  CONDITION_ASSOCIATED,
  CONDITION_SIGNED_IN,
  decideAssociation,
  oauthQrUrl,
  requesterAssociationCaller,
  setCondition,
  vaultRefs,
} from "@publicdomainrelay/market-identity-abc";

export interface MarketIdentityReconcilerOptions {
  name: string;
  prompt: IdentityPrompt;
  vault: IdentityVault;
  logger: StructuredLoggerInterface;
  plcDirectoryUrl: string;
  qrOrigin: string;
  vaultMount: string;
  watchIntervalMs?: number;
  retryIntervalMs?: number;
  scanTimeoutMs?: number;
  qrPollIntervalMs?: number;
  associationRecordTimeoutMs?: number;
  autoRefreshThresholdMs?: number;
  oauthClientId?: string;
  localRepo?: { open(repoKeyHex: string): Promise<AssociatingRepo>; qrParam: string; service?: string };
  oauthBrowser?: { open(): Promise<BrowserOAuthClient> };
  now?: () => Date;
}

export interface IdentityReady {
  did: string;
  handle?: string;
  pdsUrl?: string;
  agent?: AtprotoAgentLike;
  repo?: AssociatingRepo;
}

export interface MarketIdentityHandle {
  ready(): Promise<IdentityReady>;
  agent(): AtprotoAgentLike | null;
  repo(): AssociatingRepo | null;
  stop(): Promise<void>;
}

type QrAgent = AtprotoAgentLike & { sessionData: OAuthSessionData; dispose(): void; proactiveRefresh(): Promise<void> };

class TerminalIdentityError extends Error {}

const DEFAULT_WATCH_MS = 1_000;
const DEFAULT_RETRY_MS = 10_000;
const DEFAULT_SCAN_TIMEOUT_MS = 5 * 60_000;
const DEFAULT_QR_POLL_MS = 3_000;
const DEFAULT_ASSOCIATION_RECORD_TIMEOUT_MS = 30_000;
const KEY_FIELD = "repoKeyHex";

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function nonce(): string {
  return hex(crypto.getRandomValues(new Uint8Array(16)));
}

function isSession(v: Record<string, unknown> | null): v is Record<string, unknown> & OAuthSessionData {
  return !!v && typeof v.accessJwt === "string" && typeof v.refreshJwt === "string" && typeof v.userDid === "string" &&
    typeof v.pds === "string" && typeof v.dpopPrivateJwk === "object";
}

function stripUndefined<T extends object>(o: T): T {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as T;
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve(v: T): void;
  reject(e: unknown): void;
  settled: boolean;
}

function deferred<T>(): Deferred<T> {
  const d = Promise.withResolvers<T>();
  d.promise.catch(() => {});
  const out: Deferred<T> = {
    promise: d.promise,
    settled: false,
    resolve: (v) => {
      out.settled = true;
      d.resolve(v);
    },
    reject: (e) => {
      out.settled = true;
      d.reject(e);
    },
  };
  return out;
}

export function startMarketIdentityReconciler(opts: MarketIdentityReconcilerOptions): MarketIdentityHandle {
  const { name, prompt, vault, logger } = opts;
  const watchMs = opts.watchIntervalMs ?? DEFAULT_WATCH_MS;
  const retryMs = opts.retryIntervalMs ?? opts.watchIntervalMs ?? DEFAULT_RETRY_MS;
  const now = opts.now ?? (() => new Date());
  const ac = new AbortController();
  const signal = ac.signal;

  let ready = deferred<IdentityReady>();
  let current: IdentityReady | null = null;
  let status: MarketIdentityStatus = {};
  let generation: number | undefined;
  let sessionLost = false;
  let expiry = deferred<void>();

  function sleep(ms: number): Promise<void> {
    return new Promise((resolve, reject) => {
      signal.throwIfAborted();
      const timer = setTimeout(() => {
        signal.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
      const onAbort = () => {
        clearTimeout(timer);
        reject(signal.reason);
      };
      signal.addEventListener("abort", onAbort, { once: true });
    });
  }

  function untilAborted<T>(p: Promise<T>): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      signal.throwIfAborted();
      const onAbort = () => reject(signal.reason);
      signal.addEventListener("abort", onAbort, { once: true });
      p.then((v) => {
        signal.removeEventListener("abort", onAbort);
        resolve(v);
      }, (e) => {
        signal.removeEventListener("abort", onAbort);
        reject(e);
      });
    });
  }

  async function publish(
    fields: Omit<MarketIdentityStatus, "conditions" | "observedGeneration">,
    conditions: Array<{ type: string; status: "True" | "False" | "Unknown"; reason: string; message?: string }> = [],
  ): Promise<void> {
    let conds = status.conditions;
    for (const c of conditions) conds = setCondition(conds, { ...c, observedGeneration: generation }, now());
    status = stripUndefined({ ...fields, observedGeneration: generation, conditions: conds });
    await prompt.writeStatus(name, status);
  }

  function markReady(r: IdentityReady): void {
    current = r;
    if (ready.settled) ready = deferred<IdentityReady>();
    ready.resolve(r);
  }

  function markLost(): void {
    current = null;
    if (ready.settled) ready = deferred<IdentityReady>();
  }

  async function repoKeyHex(ref: VaultRef): Promise<string> {
    const existing = await vault.read(ref);
    if (typeof existing?.[KEY_FIELD] === "string") return existing[KEY_FIELD] as string;
    const kp = await Secp256k1Keypair.create({ exportable: true });
    await vault.write(ref, { ...(existing ?? {}), [KEY_FIELD]: hex(await kp.export()) });
    const back = await vault.read(ref);
    if (typeof back?.[KEY_FIELD] !== "string") throw new Error(`vault ${ref.mount}/${ref.path} did not keep the repo key`);
    return back[KEY_FIELD] as string;
  }

  async function cliIdentity(keysRef: VaultRef): Promise<{ did: string; signer: { did(): string; sign(b: Uint8Array): Promise<Uint8Array> } }> {
    const keyHex = await repoKeyHex(keysRef);
    const kp = await Secp256k1Keypair.import(keyHex, { exportable: true });
    const attestation = await loadOrGenerateKeypair(keyHex);
    const genesis = await createGenesisOp({
      rotationKeys: [kp.did()],
      verificationMethods: { atproto: kp.did(), attestation: attestation.did() },
      sign: (bytes: Uint8Array) => kp.sign(bytes),
    });
    const plc = createPlcDirectoryClient({ plcDirectoryUrl: opts.plcDirectoryUrl });
    try {
      await plc.resolve(genesis.did);
    } catch (err) {
      if (!(err instanceof PlcNotFoundError)) throw err;
      await plc.submitOp(genesis.did, genesis.op);
      logger.info("market_identity_cli_did_registered", { name, did: genesis.did });
    }
    return { did: genesis.did, signer: { did: () => genesis.did, sign: (b) => kp.sign(b) } };
  }

  async function openSession(session: OAuthSessionData, sessionRef: VaultRef): Promise<QrAgent> {
    let agent: QrAgent | null = null;
    agent = await createOAuthAgentFromSession(session, {
      logger,
      clientId: opts.oauthClientId,
      autoRefreshThresholdMs: opts.autoRefreshThresholdMs,
      saveSession: (s) => vault.write(sessionRef, s as unknown as Record<string, unknown>),
      onSessionExpired: () => {
        if (!agent || current?.agent !== agent) return;
        logger.warn("market_identity_session_expired", { name, did: session.userDid });
        sessionLost = true;
        agent.dispose();
        markLost();
        vault.remove(sessionRef).catch((err) => logger.error("market_identity_vault_remove_failed", { name, error: String(err) }))
          .finally(() => expiry.resolve());
      },
    });
    return agent;
  }

  async function sessionUsable(agent: QrAgent): Promise<boolean> {
    try {
      const out = await agent.listRecords(agent.sessionData.userDid, BADGE_BLUE_KEYS_NSID, { limit: 1 });
      if (out && "records" in out) return true;
      await agent.proactiveRefresh();
      const again = await agent.listRecords(agent.sessionData.userDid, BADGE_BLUE_KEYS_NSID, { limit: 1 });
      return !!again && "records" in again;
    } catch {
      return false;
    }
  }

  async function becomeReadyWithSession(agent: QrAgent, sessionRef: VaultRef, serviceDid?: string): Promise<void> {
    const s = agent.sessionData;
    expiry = deferred<void>();
    sessionLost = false;
    markReady({ did: s.userDid, handle: s.handle, pdsUrl: s.pds, agent });
    await publish(
      { phase: "Ready", did: s.userDid, handle: s.handle, pdsUrl: s.pds, serviceDid, sessionRef },
      [{ type: CONDITION_SIGNED_IN, status: "True", reason: "SignedIn", message: `signed in as ${s.handle}` }],
    );
    logger.info("market_identity_ready", { name, did: s.userDid, handle: s.handle });
    await untilAborted(expiry.promise);
  }

  async function restoreSession(sessionRef: VaultRef): Promise<QrAgent | null> {
    const stored = await vault.read(sessionRef);
    if (!isSession(stored)) return null;
    const agent = await openSession(stored, sessionRef);
    if (await sessionUsable(agent)) {
      logger.info("oauth_qr_session_restored", { name, userDid: stored.userDid, handle: stored.handle });
      return agent;
    }
    agent.dispose();
    await vault.remove(sessionRef);
    sessionLost = true;
    logger.warn("market_identity_stored_session_unusable", { name });
    return null;
  }

  async function oauthQrFlow(obj: MarketIdentity, refs: { keys: VaultRef; session: VaultRef }): Promise<void> {
    const cli = await cliIdentity(refs.keys);
    const restored = await restoreSession(refs.session);
    if (restored) return await becomeReadyWithSession(restored, refs.session, cli.did);
    while (true) {
      signal.throwIfAborted();
      const expired = sessionLost;
      const scanTimeoutMs = opts.scanTimeoutMs ?? DEFAULT_SCAN_TIMEOUT_MS;
      const action: MarketIdentityPendingAction = {
        kind: expired ? "signInAgain" : "scanQr",
        url: oauthQrUrl(opts.qrOrigin, cli.did, nonce()),
        expiresAt: new Date(now().getTime() + scanTimeoutMs).toISOString(),
      };
      await publish(
        { phase: expired ? "SessionExpired" : "AwaitingScan", serviceDid: cli.did, handle: obj.spec?.handle, pendingAction: action },
        [{ type: CONDITION_SIGNED_IN, status: "False", reason: expired ? "SessionExpired" : "AwaitingScan", message: "waiting for the QR sign-in" }],
      );
      logger.info("market_identity_pending_action", { name, kind: action.kind, url: action.url });
      let session: OAuthSessionData;
      try {
        session = await pollForOAuthSession({
          cliDid: cli.did,
          signer: cli.signer,
          qrFedforkOrigin: opts.qrOrigin,
          pollIntervalMs: opts.qrPollIntervalMs ?? DEFAULT_QR_POLL_MS,
          timeoutMs: scanTimeoutMs,
          logger,
          signal,
        });
      } catch (err) {
        signal.throwIfAborted();
        logger.warn("market_identity_scan_window_closed", { name, error: String(err) });
        continue;
      }
      await vault.write(refs.session, session as unknown as Record<string, unknown>);
      const agent = await openSession(session, refs.session);
      return await becomeReadyWithSession(agent, refs.session, cli.did);
    }
  }

  async function oauthDelegatedFlow(refs: { session: VaultRef }): Promise<void> {
    while (true) {
      const restored = await restoreSession(refs.session);
      if (restored) return await becomeReadyWithSession(restored, refs.session);
      await publish(
        { phase: sessionLost ? "SessionExpired" : "AwaitingAuthorization", message: `waiting for a delegated session at ${refs.session.mount}/${refs.session.path}` },
        [{ type: CONDITION_SIGNED_IN, status: "False", reason: sessionLost ? "SessionExpired" : "AwaitingDelegation" }],
      );
      await sleep(watchMs);
    }
  }

  async function oauthBrowserFlow(refs: { session: VaultRef }): Promise<void> {
    if (!opts.oauthBrowser) throw new TerminalIdentityError("mode oauthBrowser needs an oauthBrowser client in this component");
    const client = await opts.oauthBrowser.open();
    if (!(await client.restore())) {
      const url = await client.startFlow();
      const scanTimeoutMs = opts.scanTimeoutMs ?? DEFAULT_SCAN_TIMEOUT_MS;
      await publish(
        { phase: "AwaitingAuthorization", pendingAction: { kind: "visitUrl", url, expiresAt: new Date(now().getTime() + scanTimeoutMs).toISOString() } },
        [{ type: CONDITION_SIGNED_IN, status: "False", reason: "AwaitingAuthorization" }],
      );
      const cbRef = callbackRef(refs.session);
      let params: Record<string, unknown> | null = null;
      while (!(params = await vault.read(cbRef))) await sleep(watchMs);
      await vault.remove(cbRef);
      await client.completeFlow(Object.fromEntries(Object.entries(params).map(([k, v]) => [k, String(v)])));
    }
    const did = client.agent.did;
    markReady({ did, agent: client.agent as AtprotoAgentLike });
    await publish({ phase: "Ready", did, sessionRef: refs.session }, [{ type: CONDITION_SIGNED_IN, status: "True", reason: "SignedIn" }]);
    await untilAborted(new Promise<never>(() => {}));
  }

  async function pdsAccountFlow(obj: MarketIdentity, refs: { keys: VaultRef }): Promise<void> {
    const spec = obj.spec!;
    const secret = await vault.read(refs.keys);
    const password = typeof secret?.password === "string" ? secret.password : "";
    if (!spec.handle || !spec.pdsUrl || !password) {
      throw new Error(`mode pdsAccount needs spec.handle, spec.pdsUrl and a password in the vault at spec.keysRef ${refs.keys.mount}/${refs.keys.path}`);
    }
    const agent = await createRemoteAgent({ handle: spec.handle, password, pdsUrl: spec.pdsUrl });
    markReady({ did: agent.did, handle: spec.handle, pdsUrl: spec.pdsUrl, agent });
    await publish(
      { phase: "Ready", did: agent.did, handle: spec.handle, pdsUrl: spec.pdsUrl },
      [{ type: CONDITION_SIGNED_IN, status: "True", reason: "SignedIn", message: `signed in as ${spec.handle}` }],
    );
    await untilAborted(new Promise<never>(() => {}));
  }

  async function resolveHandle(did: string): Promise<string | undefined> {
    try {
      const doc = await new IdResolver({ plcUrl: opts.plcDirectoryUrl }).did.resolve(did) as { alsoKnownAs?: string[] } | null;
      return doc?.alsoKnownAs?.[0]?.replace(/^at:\/\//, "");
    } catch {
      return undefined;
    }
  }

  function claimRef(keys: VaultRef): VaultRef {
    return { mount: keys.mount, path: keys.path.replace(/\/keys$/, "") + "/claim" };
  }

  async function claimNonce(ref: VaultRef): Promise<string> {
    const held = await vault.read(ref);
    if (typeof held?.nonce === "string" && held.nonce) return held.nonce;
    const fresh = nonce();
    await vault.write(ref, { nonce: fresh, createdAt: now().toISOString() });
    return fresh;
  }

  async function watchedClaims(watchName: string): Promise<{ verified: boolean; records: WatchedRecord[] } | null> {
    const w = await opts.prompt.associationWatch?.(watchName).catch((err) => {
      logger.warn("market_identity_association_watch_read_failed", { name, watch: watchName, error: String(err) });
      return null;
    });
    if (!w) return null;
    const verified = (w.status?.conditions ?? []).some((c) => c.type === "Verified" && c.status === "True");
    const records = (w.status?.records ?? []).filter((r) => r.value?.challenge === r.did)
      .sort((a, b) => a.verifiedAt.localeCompare(b.verifiedAt));
    return { verified, records };
  }

  async function localRepoFlow(obj: MarketIdentity, refs: { keys: VaultRef }): Promise<void> {
    if (!opts.localRepo) throw new TerminalIdentityError("mode localRepo needs a local repo opener in this component");
    const localRepo = opts.localRepo;
    const repo = await localRepo.open(await repoKeyHex(refs.keys));
    const watchName = obj.spec?.associationWatch;
    const claim = claimRef(refs.keys);
    const settle = async (want: (recorded: string[]) => boolean, what: string): Promise<string[]> => {
      const deadline = Date.now() + (opts.associationRecordTimeoutMs ?? DEFAULT_ASSOCIATION_RECORD_TIMEOUT_MS);
      let recorded = await repo.associations();
      while (!want(recorded) && Date.now() < deadline) {
        await sleep(Math.min(watchMs, 250));
        recorded = await repo.associations();
      }
      if (!want(recorded)) throw new TerminalIdentityError(what);
      return recorded;
    };
    const hold = async (associations: string[]): Promise<void> => {
      markReady({ did: repo.did, repo });
      await publish(
        { phase: "Ready", did: repo.did, associations: associations.length ? associations : undefined },
        [{
          type: CONDITION_ASSOCIATED,
          status: associations.length ? "True" : "False",
          reason: associations.length ? "Associated" : "AssociationSkipped",
          message: associations.length ? `associated with ${associations.join(", ")}` : "",
        }],
      );
      if (!watchName || !associations.length || !opts.prompt.answerAssociation) return await untilAborted(new Promise<never>(() => {}));
      while (true) {
        await sleep(Math.max(watchMs, 5_000));
        const claims = await watchedClaims(watchName);
        if (!claims?.verified) continue;
        const live = new Set(claims.records.map((r) => r.did));
        const gone = associations.filter((d) => !live.has(d));
        if (!gone.length) continue;
        for (const d of gone) await opts.prompt.answerAssociation(name, d, "revoke");
        logger.info("market_identity_association_revoked", { name, revoked: gone, watch: watchName });
        await publish(
          { phase: "Ready", did: repo.did },
          [{ type: CONDITION_ASSOCIATED, status: "False", reason: "Revoked", message: `the association record of ${gone.join(", ")} is gone from its repo (AtprotoRecordWatch ${watchName})` }],
        );
        await settle((recorded) => !recorded.some((d) => gone.includes(d)), `the association with ${gone.join(", ")} was revoked but its record stayed in the repo`);
        return;
      }
    };
    if (obj.spec?.association === "Skip") return await hold([]);
    while (true) {
      const existing = await repo.associations();
      if (existing.length) {
        await hold(existing);
        continue;
      }
      const claimNonceValue = watchName ? await claimNonce(claim) : "";
      const url = associationQrUrl(opts.qrOrigin, localRepo.qrParam, repo.did) +
        (claimNonceValue ? `&n=${claimNonceValue}${localRepo.service ? `&s=${localRepo.service}` : ""}` : "");
      const revoked = (status.conditions ?? []).some((c) => c.type === CONDITION_ASSOCIATED && c.reason === "Revoked");
      await publish(
        { phase: "AwaitingScan", did: repo.did, pendingAction: { kind: "scanQr", url } },
        [{
          type: CONDITION_ASSOCIATED,
          status: "False",
          reason: revoked ? "Revoked" : "AwaitingScan",
          message: watchName
            ? `waiting for an operator's ${BADGE_BLUE_KEYS_NSID} record {keyId: ${repo.did}, challenge: <operator DID>${localRepo.service ? `, service: ${localRepo.service}` : ""}, nonce: <n of the pendingAction url>} (AtprotoRecordWatch ${watchName})`
            : "waiting for the association scan",
        }],
      );
      let polling = true;
      const recordedElsewhere = (async (): Promise<string[] | { claimedBy: string }> => {
        while (polling && !signal.aborted) {
          await sleep(Math.max(watchMs, 1_000)).catch(() => {});
          const recorded = await repo.associations().catch(() => [] as string[]);
          if (recorded.length) return recorded;
          if (!watchName || !opts.prompt.answerAssociation) continue;
          const fresh = await opts.prompt.get(name).catch(() => null);
          if (!fresh?.spec || fresh.spec.approvedAssociations?.length) continue;
          const claims = await watchedClaims(watchName);
          const hit = claims?.records.find((r) =>
            !fresh.spec!.rejectedAssociations?.includes(r.did) &&
            (r.value?.nonce === claimNonceValue || decideAssociation(fresh.spec!, r.did, r.handle) === "approve")
          );
          if (hit) return { claimedBy: hit.did };
        }
        return new Promise<never>(() => {});
      })();
      const first = await untilAborted(Promise.race([repo.associateCalled, recordedElsewhere])).finally(() => polling = false);
      if (Array.isArray(first)) {
        logger.info("market_identity_association_recorded", { name, associations: first });
        continue;
      }
      if (typeof first === "object") {
        await opts.prompt.answerAssociation!(name, first.claimedBy, "approve");
        await vault.remove(claim);
        logger.info("market_identity_association_claimed", { name, operator: first.claimedBy, watch: watchName });
        await settle((recorded) => recorded.includes(first.claimedBy), `the claim of ${first.claimedBy} was approved but no association record appeared in the repo`);
        continue;
      }
      const callerDid = first;
      const callerHandle = await resolveHandle(callerDid);
      let asked = false;
      while (true) {
        const fresh = await prompt.get(name);
        if (!fresh?.spec) throw new TerminalIdentityError(`marketidentity ${name} disappeared during an association`);
        generation = fresh.metadata?.generation as number | undefined;
        const decision = decideAssociation(fresh.spec, callerDid, callerHandle);
        if (decision === "approve") {
          repo.approveAssociation();
          logger.info("market_identity_association_approved", { name, callerDid });
          await settle((recorded) => recorded.includes(callerDid), `association with ${callerDid} was approved but no association record appeared`);
          break;
        }
        if (decision === "reject") {
          repo.rejectAssociation(new Error(`association with ${callerDid} rejected by spec.rejectedAssociations`));
          logger.info("market_identity_association_rejected", { name, callerDid });
          throw new TerminalIdentityError(`association with ${callerHandle ?? callerDid} (${callerDid}) was rejected`);
        }
        if (!asked) {
          asked = true;
          await publish(
            { phase: "AwaitingApproval", did: repo.did, pendingAction: { kind: "approveAssociation", callerDid, handle: callerHandle } },
            [{ type: CONDITION_ASSOCIATED, status: "False", reason: "AwaitingApproval", message: `${callerHandle ?? callerDid} asks to associate` }],
          );
        }
        await sleep(watchMs);
      }
    }
  }

  async function reconcileOnce(obj: MarketIdentity): Promise<void> {
    const spec = obj.spec!;
    const refs = vaultRefs(name, spec, opts.vaultMount);
    switch (spec.mode) {
      case "oauthQr":
        return await oauthQrFlow(obj, refs);
      case "oauthDelegated":
        return await oauthDelegatedFlow(refs);
      case "oauthBrowser":
        return await oauthBrowserFlow(refs);
      case "pdsAccount":
        return await pdsAccountFlow(obj, refs);
      case "localRepo":
        return await localRepoFlow(obj, refs);
      default:
        throw new TerminalIdentityError(`unknown mode ${spec.mode}`);
    }
  }

  async function waitForSpecChange(from: number | undefined): Promise<void> {
    while (true) {
      await sleep(watchMs);
      const obj = await prompt.get(name).catch(() => null);
      if (obj && obj.metadata?.generation !== from) return;
    }
  }

  async function run(): Promise<void> {
    while (!signal.aborted) {
      let obj: MarketIdentity | null = null;
      try {
        obj = await prompt.get(name);
        if (!obj?.spec) {
          await sleep(watchMs);
          continue;
        }
        generation = obj.metadata?.generation as number | undefined;
        await reconcileOnce(obj);
      } catch (err) {
        if (signal.aborted) return;
        const message = err instanceof Error ? err.message : String(err);
        logger.error("market_identity_reconcile_failed", { name, error: message });
        markLost();
        const phase: MarketIdentityPhase = "Failed";
        try {
          await publish({ phase, did: status.did ?? obj?.status?.did, message });
        } catch (writeErr) {
          logger.error("market_identity_status_write_failed", { name, error: String(writeErr) });
        }
        try {
          if (err instanceof TerminalIdentityError) await waitForSpecChange(generation);
          else await sleep(retryMs);
        } catch {
          return;
        }
      }
    }
  }

  const loop = run();

  return {
    ready: () => ready.promise,
    agent: () => current?.agent ?? null,
    repo: () => current?.repo ?? null,
    async stop() {
      ac.abort(new Error("market identity reconciler stopped"));
      await loop;
      const agent = current?.agent as Partial<QrAgent> | undefined;
      agent?.dispose?.();
      if (!ready.settled) ready.reject(new Error("market identity reconciler stopped"));
    },
  };
}

interface RecordLister {
  listRecords(repo: string, collection: string, opts?: { limit?: number; cursor?: string }): Promise<{ records: Array<{ value: unknown }>; cursor?: string }>;
}

async function listAssociations(
  lister: RecordLister,
  did: string,
  match: (value: Record<string, unknown>) => string | null,
): Promise<string[]> {
  const out: string[] = [];
  let cursor: string | undefined;
  do {
    const page = await lister.listRecords(did, BADGE_BLUE_KEYS_NSID, { limit: 100, cursor });
    for (const r of page.records) {
      const caller = match(r.value as Record<string, unknown>);
      if (caller && !out.includes(caller)) out.push(caller);
    }
    cursor = page.cursor;
  } while (cursor);
  return out;
}

interface AssociationSource {
  readonly did: string;
  readonly associateCalled: Promise<string>;
  approveAssociation(): void;
  rejectAssociation(err: Error): void;
}

export function bidderAssociatingRepo(agent: AssociationSource & { repoApi: RecordLister }, serviceId: string): AssociatingRepo {
  return {
    did: agent.did,
    associateCalled: agent.associateCalled,
    approveAssociation: () => agent.approveAssociation(),
    rejectAssociation: (err) => agent.rejectAssociation(err),
    associations: () => listAssociations(agent.repoApi, agent.did, (v) => bidderAssociationCaller(v, agent.did, serviceId)),
  };
}

export function requesterAssociatingRepo(pds: AssociationSource & { api: RecordLister }, serviceId = "requester_associate"): AssociatingRepo {
  return {
    did: pds.did,
    associateCalled: pds.associateCalled,
    approveAssociation: () => pds.approveAssociation(),
    rejectAssociation: (err) => pds.rejectAssociation(err),
    associations: () => listAssociations(pds.api, pds.did, (v) => requesterAssociationCaller(v, pds.did, serviceId)),
  };
}

export function vaultOAuthSessionStore(vault: IdentityVault, ref: VaultRef): SessionStore {
  return {
    async get(key) {
      return (await vault.read(ref))?.[key];
    },
    async set(key, value) {
      const entry = (await vault.read(ref)) ?? {};
      await vault.write(ref, { ...entry, [key]: JSON.parse(JSON.stringify(value)) });
    },
    async del(key) {
      const entry = (await vault.read(ref)) ?? {};
      delete entry[key];
      await vault.write(ref, entry);
    },
  };
}
