import type { Storage, StateStore, Cid, Did, Tid, Bytes, BlockSweepReport } from "@publicdomainrelay/atproto-repo-abc";

interface BlockIndexEntry {
  at: number;
  size: number;
}

const BLOCK_INDEX_READY = ["blockidx-ready"];

export class DenoKvStorage implements Storage, StateStore {
  private kv: Deno.Kv;
  private path?: string;

  private constructor(kv: Deno.Kv, path?: string) {
    this.kv = kv;
    this.path = path;
  }

  static async create(path?: string): Promise<DenoKvStorage> {
    if (typeof Deno === "undefined" || typeof Deno.openKv !== "function") {
      throw new Error(
        "DenoKvStorage: Deno.openKv is not available in this environment",
      );
    }
    if (path) {
      const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : ".";
      await Deno.mkdir(parent, { recursive: true });
    }
    const kv = await Deno.openKv(path);
    return new DenoKvStorage(kv, path);
  }

  async get(cid: Cid): Promise<Bytes | null> {
    const result = await this.kv.get<Bytes>(["blocks", cid]);
    return result.value ?? null;
  }

  async put(cid: Cid, bytes: Bytes): Promise<void> {
    await this.kv.atomic()
      .set(["blocks", cid], bytes)
      .set(["blockidx", cid], { at: Date.now(), size: bytes.byteLength } satisfies BlockIndexEntry)
      .commit();
  }

  async has(cid: Cid): Promise<boolean> {
    const result = await this.kv.get<Bytes>(["blocks", cid]);
    return result.value !== null;
  }

  async getHead(did: Did): Promise<{ commit: Cid; rev: Tid } | null> {
    const result = await this.kv.get<{ commit: Cid; rev: Tid }>(["heads", did]);
    return result.value ?? null;
  }

  async setHead(did: Did, head: { commit: Cid; rev: Tid }): Promise<void> {
    await this.kv.set(["heads", did], head);
  }

  async listHeads(): Promise<{ commit: Cid; rev: Tid }[]> {
    const out: { commit: Cid; rev: Tid }[] = [];
    for await (const entry of this.kv.list<{ commit: Cid; rev: Tid }>({ prefix: ["heads"] })) out.push(entry.value);
    return out;
  }

  async #indexUnindexedBlocks(): Promise<void> {
    if ((await this.kv.get(BLOCK_INDEX_READY)).value) return;
    const at = Date.now();
    for await (const entry of this.kv.list<Bytes>({ prefix: ["blocks"] }, { batchSize: 100 })) {
      const key = ["blockidx", entry.key[1]];
      await this.kv.atomic()
        .check({ key, versionstamp: null })
        .set(key, { at, size: entry.value.byteLength } satisfies BlockIndexEntry)
        .commit();
    }
    await this.kv.set(BLOCK_INDEX_READY, at);
  }

  async #fileBytes(): Promise<number | undefined> {
    if (!this.path) return undefined;
    let total = 0;
    for (const suffix of ["", "-wal"]) {
      try {
        total += (await Deno.stat(this.path + suffix)).size;
      } catch {
        continue;
      }
    }
    return total;
  }

  async sweepBlocks(live: Set<Cid>, writtenBefore: number): Promise<BlockSweepReport> {
    await this.#indexUnindexedBlocks();
    const report: BlockSweepReport = { blocks: 0, bytes: 0, removedBlocks: 0, removedBytes: 0, fileBytesBefore: await this.#fileBytes() };
    for await (const entry of this.kv.list<BlockIndexEntry>({ prefix: ["blockidx"] }, { batchSize: 500 })) {
      const cid = entry.key[1] as Cid;
      const removable = !live.has(cid) && entry.value.at < writtenBefore;
      const removed = removable && (await this.kv.atomic().check(entry).delete(["blocks", cid]).delete(entry.key).commit()).ok;
      if (removed) {
        report.removedBlocks++;
        report.removedBytes += entry.value.size;
      } else {
        report.blocks++;
        report.bytes += entry.value.size;
      }
    }
    report.fileBytesAfter = await this.#fileBytes();
    return report;
  }

  async getState<T>(key: string[]): Promise<T | null> {
    const result = await this.kv.get<T>(["state", ...key]);
    return result.value ?? null;
  }

  async setState(key: string[], value: unknown, expireInMs?: number): Promise<void> {
    await this.kv.set(["state", ...key], value, expireInMs ? { expireIn: expireInMs } : undefined);
  }

  async deleteState(key: string[]): Promise<void> {
    await this.kv.delete(["state", ...key]);
  }

  async listState<T>(prefix: string[]): Promise<T[]> {
    const out: T[] = [];
    for await (const entry of this.kv.list<T>({ prefix: ["state", ...prefix] })) out.push(entry.value);
    return out;
  }

  close(): void {
    this.kv.close();
  }
}
