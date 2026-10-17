import type {
  AtprotoRecordWatch,
  MarketIdentity,
  MarketIdentityPendingAction,
  MarketIdentitySpec,
  MarketIdentityStatus,
} from "@publicdomainrelay/socialweb-computer-k8s-api";

export interface IdentityPrompt {
  get(name: string): Promise<MarketIdentity | null>;
  writeStatus(name: string, status: MarketIdentityStatus): Promise<void>;
  answerAssociation?(name: string, callerDid: string, answer: AssociationAnswer): Promise<void>;
  associationWatch?(name: string): Promise<AtprotoRecordWatch | null>;
}

export type AssociationAnswer = "approve" | "reject" | "revoke";

export interface IdentityAnswers {
  answerAssociation(name: string, callerDid: string, answer: AssociationAnswer): Promise<void>;
}

export interface PendingActionView {
  title: string;
  url?: string;
  question?: string;
}

const SECRET_KEY = /(token|jwt|password|passwd|secret|private|jwk|dpop|^code$|authorization_?code|keyhex|credential)/i;
const JWT = /eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\./;
const LONG_HEX = /(^|[^0-9a-f])[0-9a-f]{64,}([^0-9a-f]|$)/i;
const SECRET_PARAM = /[?&#](code|access_token|refresh_token|id_token|password|client_secret)=/i;
const PEM_KEY = /-----BEGIN [A-Z ]*PRIVATE KEY-----/;

export function findSecretMaterial(value: unknown, path = "$"): string[] {
  if (typeof value === "string") {
    if (JWT.test(value) || LONG_HEX.test(value) || SECRET_PARAM.test(value) || PEM_KEY.test(value)) return [path];
    return [];
  }
  if (Array.isArray(value)) return value.flatMap((v, i) => findSecretMaterial(v, `${path}[${i}]`));
  if (value && typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const out: string[] = [];
    if ("kty" in obj && "d" in obj) out.push(`${path}.d`);
    for (const [k, v] of Object.entries(obj)) {
      const p = `${path}.${k}`;
      if (!k.endsWith("Ref") && SECRET_KEY.test(k)) out.push(p);
      else out.push(...findSecretMaterial(v, p));
    }
    return out;
  }
  return [];
}

export function refuseSecretMaterial(name: string, what: "status" | "spec", value: unknown): void {
  const secrets = findSecretMaterial(value);
  if (secrets.length) {
    throw new Error(
      `marketidentity ${name}: refusing a ${what} that carries secret material at ${secrets.join(", ")}; keys, tokens and codes belong in the vault`,
    );
  }
}

export function pendingActionKey(status: MarketIdentityStatus | undefined): string {
  const a = status?.pendingAction;
  if (!a) return `${status?.phase ?? ""}|`;
  return [status?.phase ?? "", a.kind, a.url ?? "", a.callerDid ?? ""].join("|");
}

export function specWithAnswer(spec: MarketIdentitySpec, callerDid: string, answer: AssociationAnswer): MarketIdentitySpec {
  const approved = (spec.approvedAssociations ?? []).filter((d) => d !== callerDid);
  const rejected = (spec.rejectedAssociations ?? []).filter((d) => d !== callerDid);
  if (answer === "approve") approved.push(callerDid);
  else if (answer === "reject") rejected.push(callerDid);
  return {
    ...spec,
    approvedAssociations: approved.length ? approved : undefined,
    rejectedAssociations: rejected.length ? rejected : undefined,
  };
}

export function nextGeneration(obj: MarketIdentity): number {
  const g = Number(obj.metadata?.generation ?? 0);
  return (Number.isFinite(g) ? g : 0) + 1;
}

export function pendingActionView(status: MarketIdentityStatus | undefined): PendingActionView | null {
  const a: MarketIdentityPendingAction | undefined = status?.pendingAction;
  if (!a) return null;
  switch (a.kind) {
    case "scanQr":
      return { title: "Scan this QR code with your phone to sign in", url: a.url };
    case "signInAgain":
      return { title: "The session expired; scan this QR code to sign in again", url: a.url };
    case "visitUrl":
      return { title: "Open this URL in a browser to authorize the sign-in", url: a.url };
    case "approveAssociation":
      return {
        title: `${a.handle ?? a.callerDid ?? "an account"} asks to associate`,
        question: `Associate with ${a.handle ?? a.callerDid}? [y/N] `,
      };
    default:
      return { title: `pending action ${String(a.kind)}`, url: a.url };
  }
}

export function isYes(answer: string | null | undefined): boolean {
  return !!answer && answer.trim().toLowerCase().startsWith("y");
}
