import { CopyObjectCommand, HeadObjectCommand, ListObjectsV2Command, S3Client } from "npm:@aws-sdk/client-s3@3.700.0";
import { parseArgs } from "jsr:@std/cli@1/parse-args";

const args = parseArgs(Deno.args, { string: ["from", "to"], collect: ["to"] });
const need = (name: string): string => {
  const v = Deno.env.get(name);
  if (!v) {
    console.error(`copy-spaces: ${name} is not set`);
    Deno.exit(2);
  }
  return v;
};
const to = (args.to as string[]) ?? [];
if (!args.from || to.length === 0) {
  console.error("usage: copy-spaces.ts --from <bucket prefix> --to <bucket prefix> [--to <bucket prefix> ...]");
  Deno.exit(2);
}
const bucket = need("SPACES_BUCKET");
const s3 = new S3Client({
  region: "us-east-1",
  endpoint: `https://${need("SPACES_REGION")}.digitaloceanspaces.com`,
  credentials: { accessKeyId: need("SPACES_ACCESS_KEY_ID"), secretAccessKey: need("SPACES_SECRET_ACCESS_KEY") },
});

const attempt = async <T>(key: string, fn: () => Promise<T>): Promise<T> => {
  for (let n = 1; ; n++) {
    try {
      return await fn();
    } catch (err) {
      console.error(`copy-spaces: ${key} attempt ${n} failed: ${err instanceof Error ? err.message : err}`);
      if (n >= 4) throw err;
      await new Promise((r) => setTimeout(r, 2000 * n));
    }
  }
};

const head = async (key: string): Promise<{ sha256?: string; size?: number } | undefined> => {
  try {
    const h = await s3.send(new HeadObjectCommand({ Bucket: bucket, Key: key }));
    return { sha256: h.Metadata?.sha256, size: h.ContentLength };
  } catch (err) {
    if ((err as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode === 404) return undefined;
    throw err;
  }
};

const from = args.from.replace(/\/+$/, "");
const keys: Array<{ rel: string; size: number }> = [];
for (let token: string | undefined;;) {
  const page = await attempt(`${from} list`, () =>
    s3.send(new ListObjectsV2Command({ Bucket: bucket, Prefix: `${from}/`, ContinuationToken: token })));
  for (const o of page.Contents ?? []) keys.push({ rel: o.Key!.slice(from.length + 1), size: o.Size ?? 0 });
  if (!page.IsTruncated) break;
  token = page.NextContinuationToken;
}
if (keys.length === 0) {
  console.error(`copy-spaces: nothing under ${from}/`);
  Deno.exit(1);
}

const queue = keys.flatMap((k) => to.map((t) => ({ ...k, dest: `${t.replace(/\/+$/, "")}/${k.rel}` })));
const workers = Array.from({ length: Math.min(4, queue.length) }, async () => {
  for (let c = queue.shift(); c; c = queue.shift()) {
    const started = Date.now();
    const source = await attempt(`${from}/${c.rel} head`, () => head(`${from}/${c.rel}`));
    const held = await attempt(`${c.dest} head`, () => head(c.dest));
    if (source?.sha256 && held?.sha256 === source.sha256 && held.size === source.size) {
      console.log(`${c.dest} ${c.size} bytes sha256 ${held.sha256} already there, not copied`);
      continue;
    }
    await attempt(c.dest, () =>
      s3.send(new CopyObjectCommand({
        Bucket: bucket,
        Key: c.dest,
        CopySource: encodeURI(`${bucket}/${from}/${c.rel}`),
        MetadataDirective: "COPY",
        ACL: "public-read",
      })));
    console.log(`${c.dest} ${c.size} bytes copied from ${from}/${c.rel} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
  }
});
await Promise.all(workers);
