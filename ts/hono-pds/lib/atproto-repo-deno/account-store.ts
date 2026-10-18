import type { Did, Signer, StateStore } from "@publicdomainrelay/atproto-repo-abc";
import { base64Encode, utf8Encode } from "@publicdomainrelay/atproto-repo-common";

function b64url(bytes: Uint8Array): string {
  return base64Encode(bytes).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64urlJson(value: unknown): string {
  return b64url(utf8Encode(JSON.stringify(value)));
}

function b64urlToStandard(b64urlStr: string): string {
  let s = b64urlStr.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4 !== 0) s += "=";
  return s;
}

function makeHeader(typ: string, kid?: string): { typ: string; alg: string; kid?: string } {
  return kid ? { typ, alg: "ES256K", kid } : { typ, alg: "ES256K" };
}

async function signJwt(signer: Signer, payload: Record<string, unknown>, typ: string, kid?: string): Promise<string> {
  const header = makeHeader(typ, kid);
  const signingInput = `${b64urlJson(header)}.${b64urlJson(payload)}`;
  const sig = await signer.sign(utf8Encode(signingInput));
  return `${signingInput}.${b64url(sig)}`;
}

export interface AccountRecord {
  did: Did;
  handle: string;
  passwordHash: string;
  passwordSalt: string;
  email?: string;
  createdAt: number;
}

export interface AccountStore {
  createAccount(opts: {
    handle?: string;
    email?: string;
    password?: string;
    didOverride?: Did;
    signerOverride?: Signer;
    signingKeyHex?: string;
  }): Promise<{ accessJwt: string; refreshJwt: string; handle: string; did: Did; signer: Signer }>;
  restore(): Promise<Array<{ did: Did; signer: Signer }>>;
  getAccount(handleOrDid: string): AccountRecord | undefined;
  validatePassword(handleOrDid: string, password: string): Promise<boolean>;
  createSessionTokens(did: Did, handle: string): Promise<{
    accessJwt: string;
    refreshJwt: string;
  }>;
  validateAccessJwt(token: string): Promise<{ did: Did; handle: string } | null>;
  validateRefreshJwt(token: string): Promise<{ did: Did; handle: string } | null>;
}

interface PersistedAccount {
  record: AccountRecord;
  signingKeyHex?: string;
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

async function signerFromHex(did: Did, hex: string): Promise<Signer> {
  const { Secp256k1Keypair } = await import("@atproto/crypto");
  const kp = await Secp256k1Keypair.import(hex, { exportable: true });
  return { did: () => did, sign: (bytes: Uint8Array) => kp.sign(bytes) };
}

function createAccountStore(
  pdsDid: Did,
  pdsSigner: Signer,
  pdsKeyDid: Did = pdsSigner.did(),
  state?: StateStore,
): AccountStore {
  const accounts = new Map<string, AccountRecord>();

  async function hashPassword(password: string): Promise<{ hash: string; salt: string }> {
    const salt = crypto.getRandomValues(new Uint8Array(16));
    const key = await crypto.subtle.importKey(
      "raw", utf8Encode(password).buffer as ArrayBuffer, "PBKDF2", false, ["deriveBits"],
    );
    const bits = await crypto.subtle.deriveBits(
      { name: "PBKDF2", salt, iterations: 100_000, hash: "SHA-256" },
      key, 256,
    );
    return { hash: b64url(new Uint8Array(bits)), salt: b64url(salt) };
  }

  async function verifyPassword(password: string, record: AccountRecord): Promise<boolean> {
    const saltStr = b64urlToStandard(record.passwordSalt);
    const saltBytes = Uint8Array.from(atob(saltStr), (c) => c.charCodeAt(0));
    const key = await crypto.subtle.importKey(
      "raw", utf8Encode(password).buffer as ArrayBuffer, "PBKDF2", false, ["deriveBits"],
    );
    const bits = await crypto.subtle.deriveBits(
      { name: "PBKDF2", salt: saltBytes, iterations: 100_000, hash: "SHA-256" },
      key, 256,
    );
    return b64url(new Uint8Array(bits)) === record.passwordHash;
  }

  async function validateToken(token: string, expectedTyp: string) {
    try {
      const parts = token.split(".");
      if (parts.length !== 3) return null;
      const headerJson = atob(b64urlToStandard(parts[0]));
      const header = JSON.parse(headerJson);
      if (header.typ !== expectedTyp) return null;
      const payloadJson = atob(b64urlToStandard(parts[1]));
      const payload = JSON.parse(payloadJson);
      // Required, not optional: a missing exp is an immortal session token. Our own
      // createSessionTokens (:151,:156) always sets one, so this rejects nothing real.
      if (typeof payload.exp !== "number") return null;
      if (Math.floor(Date.now() / 1000) > payload.exp) return null;
      if (!payload.sub || !payload.handle) return null;
      const { verifySignature } = await import("@atproto/crypto");
      const signingInput = utf8Encode(`${parts[0]}.${parts[1]}`);
      const sigBytes = Uint8Array.from(atob(b64urlToStandard(parts[2])), (c) => c.charCodeAt(0));
      const valid = await verifySignature(pdsKeyDid, signingInput, sigBytes as unknown as Uint8Array<ArrayBuffer>);
      if (!valid) return null;
      return { did: payload.sub, handle: payload.handle };
    } catch { return null; }
  }

  return {
    async createAccount(opts) {
      const handle = opts.handle ?? `user-${b64url(crypto.getRandomValues(new Uint8Array(6)))}`;
      const password = opts.password ?? b64url(crypto.getRandomValues(new Uint8Array(12)));
      const { hash: passwordHash, salt: passwordSalt } = await hashPassword(password);

      let did: Did;
      let signer: Signer;
      let signingKeyHex = opts.signingKeyHex;

      if (opts.signerOverride) {
        signer = opts.signerOverride;
        did = opts.didOverride ?? signer.did();
      } else {
        const { Secp256k1Keypair } = await import("@atproto/crypto");
        const kp = await Secp256k1Keypair.create({ exportable: true });
        did = kp.did();
        signingKeyHex = toHex(await kp.export());
        signer = {
          did: () => did,
          sign: (bytes: Uint8Array) => kp.sign(bytes),
        };
      }

      const record: AccountRecord = { did, handle, passwordHash, passwordSalt, email: opts.email, createdAt: Date.now() };
      if (state) await state.setState(["accounts", handle], { record, signingKeyHex } satisfies PersistedAccount);
      accounts.set(handle, record);
      const tokens = await this.createSessionTokens(did, handle);
      return { ...tokens, handle, did, signer };
    },

    async restore() {
      if (!state) return [];
      const out: Array<{ did: Did; signer: Signer }> = [];
      for (const p of await state.listState<PersistedAccount>(["accounts"])) {
        accounts.set(p.record.handle, p.record);
        if (p.signingKeyHex) out.push({ did: p.record.did, signer: await signerFromHex(p.record.did, p.signingKeyHex) });
      }
      return out;
    },

    getAccount(handleOrDid: string) {
      const byHandle = accounts.get(handleOrDid);
      if (byHandle) return byHandle;
      for (const a of accounts.values()) {
        if (a.did === handleOrDid) return a;
      }
      return undefined;
    },

    async validatePassword(handleOrDid: string, password: string) {
      const record = this.getAccount(handleOrDid);
      if (!record) return false;
      return verifyPassword(password, record);
    },

    async createSessionTokens(did: Did, handle: string) {
      const now = Math.floor(Date.now() / 1000);
      const kid = `${pdsDid}#atproto`;
      const accessPayload = {
        iss: pdsDid, sub: did, handle, aud: pdsDid,
        iat: now, exp: now + 900,
        jti: b64url(crypto.getRandomValues(new Uint8Array(16))),
      };
      const refreshPayload = {
        iss: pdsDid, sub: did, handle, aud: pdsDid,
        iat: now, exp: now + 604800,
        jti: b64url(crypto.getRandomValues(new Uint8Array(16))),
      };
      const [accessJwt, refreshJwt] = await Promise.all([
        signJwt(pdsSigner, accessPayload, "at+jwt", kid),
        signJwt(pdsSigner, refreshPayload, "refresh+jwt", kid),
      ]);
      return { accessJwt, refreshJwt };
    },

    async validateAccessJwt(token: string) {
      return validateToken(token, "at+jwt");
    },

    async validateRefreshJwt(token: string) {
      return validateToken(token, "refresh+jwt");
    },
  };
}

export { createAccountStore };
