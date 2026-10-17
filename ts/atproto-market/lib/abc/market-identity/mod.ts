import type {
  Condition,
  MarketIdentity,
  MarketIdentitySpec,
  VaultRef,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export type {
  AssociationMode,
  Condition,
  MarketIdentity,
  MarketIdentityMode,
  MarketIdentityPendingAction,
  MarketIdentityPhase,
  MarketIdentitySpec,
  MarketIdentityStatus,
  PendingActionKind,
  VaultRef,
  WatchedRecord,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export {
  CONDITION_ASSOCIATED,
  CONDITION_SIGNED_IN,
  MARKET_IDENTITY_MODES,
  MARKET_IDENTITY_RESOURCE,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export { findSecretMaterial } from "@publicdomainrelay/identity-prompt-abc";
export type { IdentityPrompt } from "@publicdomainrelay/identity-prompt-abc";

import { MARKET_IDENTITY_MODES, MARKET_IDENTITY_RESOURCE } from "@publicdomainrelay/socialweb-computer-k8s-api";
import type { MarketIdentityMode } from "@publicdomainrelay/socialweb-computer-k8s-api";

export interface CliIdentityFlags {
  mode?: string;
  oauth?: boolean;
  oauthQr?: boolean;
  handle?: string;
  password?: boolean;
  vault?: boolean;
}

export function cliIdentityMode(flags: CliIdentityFlags): MarketIdentityMode {
  if (flags.mode) {
    if (!(MARKET_IDENTITY_MODES as readonly string[]).includes(flags.mode)) {
      throw new Error(`unknown identity mode ${JSON.stringify(flags.mode)}; one of ${MARKET_IDENTITY_MODES.join(", ")}`);
    }
    return flags.mode as MarketIdentityMode;
  }
  if (flags.oauth) return "oauthBrowser";
  if (flags.oauthQr) return "oauthQr";
  if (flags.handle && (flags.password || flags.vault)) return "pdsAccount";
  return "localRepo";
}

export function cliMarketIdentity(
  name: string,
  spec: MarketIdentitySpec,
): MarketIdentity {
  return {
    apiVersion: `${MARKET_IDENTITY_RESOURCE.group}/${MARKET_IDENTITY_RESOURCE.version}`,
    kind: MARKET_IDENTITY_RESOURCE.kind,
    metadata: { name, generation: 1 },
    spec,
  };
}

export interface IdentityVault {
  read(ref: VaultRef): Promise<Record<string, unknown> | null>;
  write(ref: VaultRef, data: Record<string, unknown>): Promise<void>;
  remove(ref: VaultRef): Promise<void>;
  create(ref: VaultRef, data: Record<string, unknown>): Promise<Record<string, unknown>>;
  list(ref: VaultRef): Promise<string[]>;
}

export function childrenOf(keys: Iterable<string>, dir: string): string[] {
  const prefix = dir.replace(/\/+$/, "") + "/";
  const out = new Set<string>();
  for (const k of keys) {
    if (!k.startsWith(prefix)) continue;
    const rest = k.slice(prefix.length);
    const slash = rest.indexOf("/");
    if (rest) out.add(slash < 0 ? rest : rest.slice(0, slash + 1));
  }
  return [...out].sort();
}

export interface SessionStore {
  get(key: string): Promise<unknown>;
  set(key: string, value: unknown): Promise<void>;
  del(key: string): Promise<void>;
}

export interface AssociatingRepo {
  readonly did: string;
  readonly associateCalled: Promise<string>;
  approveAssociation(): void;
  rejectAssociation(err: Error): void;
  associations(): Promise<string[]>;
}

export interface BrowserOAuthClient {
  restore(): Promise<boolean>;
  startFlow(): Promise<string>;
  completeFlow(params: Record<string, string>): Promise<void>;
  readonly agent: { readonly did: string };
}

export type AssociationDecision = "approve" | "reject" | "ask";

export function decideAssociation(spec: MarketIdentitySpec, callerDid: string, callerHandle?: string): AssociationDecision {
  if (spec.rejectedAssociations?.includes(callerDid)) return "reject";
  if (spec.approvedAssociations?.includes(callerDid)) return "approve";
  if (spec.associateWith && (spec.associateWith === callerDid || spec.associateWith === callerHandle)) return "approve";
  return "ask";
}

export function oauthQrUrl(origin: string, cliDid: string, nonce: string): string {
  return `${origin.replace(/\/+$/, "")}/#oauth=${encodeURIComponent(cliDid)}&n=${nonce}`;
}

export function associationQrUrl(origin: string, param: string, did: string): string {
  return `${origin.replace(/\/+$/, "")}/#${param}=${did}`;
}

export function parseOAuthQrUrl(url: string): { cliDid: string; nonce: string } | null {
  const hash = url.includes("#") ? url.slice(url.indexOf("#") + 1) : "";
  const params = new URLSearchParams(hash);
  const cliDid = params.get("oauth");
  const nonce = params.get("n");
  return cliDid && nonce ? { cliDid, nonce } : null;
}

export function callbackRef(session: VaultRef): VaultRef {
  return { mount: session.mount, path: `${session.path}/callback` };
}

export function vaultRefs(name: string, spec: MarketIdentitySpec, mount: string): { keys: VaultRef; session: VaultRef } {
  return {
    keys: spec.keysRef ?? { mount, path: `identity/${name}/keys` },
    session: spec.sessionRef ?? { mount, path: `identity/${name}/session` },
  };
}

export function setCondition(
  conditions: Condition[] | undefined,
  next: { type: string; status: "True" | "False" | "Unknown"; reason: string; message?: string; observedGeneration?: number },
  now: Date,
): Condition[] {
  const list = [...(conditions ?? [])];
  const i = list.findIndex((c) => c.type === next.type);
  const prior = i >= 0 ? list[i] : undefined;
  const lastTransitionTime = prior && prior.status === next.status && typeof prior.lastTransitionTime === "string"
    ? prior.lastTransitionTime
    : now.toISOString().replace(/\.\d{3}Z$/, "Z");
  const cond: Condition = {
    type: next.type,
    status: next.status,
    reason: next.reason,
    message: next.message ?? "",
    lastTransitionTime,
    ...(next.observedGeneration !== undefined ? { observedGeneration: next.observedGeneration } : {}),
  };
  if (i >= 0) list[i] = cond;
  else list.push(cond);
  return list;
}

export function bidderAssociationCaller(value: Record<string, unknown>, did: string, serviceId: string): string | null {
  if (value.service !== serviceId) return null;
  if (value.keyId === did && typeof value.challenge === "string") return value.challenge;
  if (value.challenge === did && typeof value.keyId === "string") return value.keyId;
  return null;
}

export function requesterAssociationCaller(value: Record<string, unknown>, did: string, serviceId: string): string | null {
  if (value.service !== serviceId) return null;
  if (value.keyId === did && typeof value.challenge === "string") return value.challenge;
  if (value.challenge === did && typeof value.keyId === "string") return value.keyId;
  return null;
}
