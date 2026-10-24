import { CopyObjectCommand, HeadObjectCommand, PutObjectCommand, S3Client } from "npm:@aws-sdk/client-s3@3.700.0";
import { parseArgs } from "jsr:@std/cli@1/parse-args";
import { walk } from "jsr:@std/fs@1/walk";
import { relative } from "jsr:@std/path@1";

const args = parseArgs(Deno.args, { string: ["dir", "prefix", "also"], boolean: ["content-addressed"] });
const contentAddressed = args["content-addressed"];
const need = (name: string): string => {
  const v = Deno.env.get(name);
  if (!v) {
    console.error(`publish-spaces: ${name} is not set`);
    Deno.exit(2);
  }
  return v;
};
if (!args.dir || !args.prefix) {
  console.error("usage: publish-spaces.ts --dir <local dir> --prefix <bucket prefix> [--also <second prefix>] [--content-addressed]");
  Deno.exit(2);
}
const bucket = need("SPACES_BUCKET");
const region = need("SPACES_REGION");
const s3 = new S3Client({
  region: "us-east-1",
  endpoint: `https://${region}.digitaloceanspaces.com`,
  credentials: { accessKeyId: need("SPACES_ACCESS_KEY_ID"), secretAccessKey: need("SPACES_SECRET_ACCESS_KEY") },
});

const sha256 = async (path: string): Promise<string> => {
  const p = new Deno.Command("sha256sum", { args: [path], stdout: "piped" });
  const out = await p.output();
  if (!out.success) throw new Error(`sha256sum ${path} failed`);
  return new TextDecoder().decode(out.stdout).split(" ")[0];
};
const typeOf = (name: string): string =>
  name.endsWith(".json") ? "application/json" : name.endsWith(".txt") || name.endsWith("SUMS") ? "text/plain" : "application/octet-stream";

const files: Array<{ rel: string; path: string; size: number; sha256: string }> = [];
for await (const e of walk(args.dir, { includeDirs: false })) {
  const st = await Deno.stat(e.path);
  files.push({ rel: relative(args.dir, e.path), path: e.path, size: st.size, sha256: await sha256(e.path) });
}
files.sort((a, b) => a.rel.localeCompare(b.rel));
const env = (first: string, second: string): string => Deno.env.get(first) ?? Deno.env.get(second) ?? "";
const manifest = {
  commit: env("GITHUB_SHA", "CI_COMMIT_SHA"),
  ref: env("GITHUB_REF_NAME", "CI_COMMIT_REF_NAME"),
  run: env("GITHUB_RUN_ID", "CI_PIPELINE_ID"),
  published: new Date().toISOString(),
  files: files.map(({ rel, size, sha256 }) => ({ path: rel, size, sha256 })),
};

const attempt = async <T>(key: string, fn: () => Promise<T>): Promise<T> => {
  for (let n = 1; ; n++) {
    try {
      return await fn();
    } catch (err) {
      console.error(`publish-spaces: ${key} attempt ${n} failed: ${err instanceof Error ? err.message : err}`);
      if (n >= 4) throw err;
      await new Promise((r) => setTimeout(r, 2000 * n));
    }
  }
};

const held = async (key: string): Promise<{ sha256?: string; size?: number } | undefined> => {
  try {
    const h = await s3.send(new HeadObjectCommand({ Bucket: bucket, Key: key }));
    return { sha256: h.Metadata?.sha256, size: h.ContentLength };
  } catch (err) {
    if ((err as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode === 404) return undefined;
    throw err;
  }
};

const put = async (prefix: string, copyFrom?: string) => {
  const queue = [...files];
  const workers = Array.from({ length: Math.min(4, queue.length) }, async () => {
    for (let f = queue.shift(); f; f = queue.shift()) {
      const started = Date.now();
      const key = `${prefix}/${f.rel}`;
      if (contentAddressed) {
        const h = await attempt(`${key} head`, () => held(key));
        if (h?.sha256 === f.sha256 && h.size === f.size) {
          console.log(`${key} ${f.size} bytes sha256 ${f.sha256} already published, not uploaded`);
          continue;
        }
        console.log(`${key} ${h ? `holds sha256 ${h.sha256} ${h.size} bytes` : "is absent"}, uploading sha256 ${f.sha256}`);
      }
      if (copyFrom) {
        await attempt(key, () =>
          s3.send(new CopyObjectCommand({
            Bucket: bucket,
            Key: key,
            CopySource: encodeURI(`${bucket}/${copyFrom}/${f.rel}`),
            MetadataDirective: "COPY",
            ACL: "public-read",
          })));
      } else {
        await attempt(key, async () =>
          s3.send(new PutObjectCommand({
            Bucket: bucket,
            Key: key,
            Body: await Deno.readFile(f.path),
            ContentType: typeOf(f.rel),
            ACL: "public-read",
            Metadata: { sha256: f.sha256 },
          })));
      }
      const how = copyFrom ? `copied from ${copyFrom}` : "uploaded";
      console.log(`${key} ${f.size} bytes sha256 ${f.sha256} ${how} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
    }
  });
  await Promise.all(workers);
  if (contentAddressed) return;
  await s3.send(new PutObjectCommand({
    Bucket: bucket,
    Key: `${prefix}/manifest.json`,
    Body: new TextEncoder().encode(JSON.stringify(manifest, null, 2) + "\n"),
    ContentType: "application/json",
    ACL: "public-read",
  }));
  console.log(`${prefix}/manifest.json (${files.length} files)`);
};

await put(args.prefix);
if (args.also) await put(args.also, args.prefix);
