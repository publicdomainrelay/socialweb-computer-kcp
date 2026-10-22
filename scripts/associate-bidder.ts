const args = new Map<string, string>();
for (let i = 0; i < Deno.args.length; i += 2) args.set(Deno.args[i].replace(/^--/, ""), Deno.args[i + 1]);
const credsPath = args.get("creds");
const account = args.get("account");
const bidderDid = args.get("bidder-did");
if (!credsPath || !account || !bidderDid) {
  console.error("usage: associate-bidder.ts --creds FILE --account BOB --bidder-did did:plc:...");
  Deno.exit(2);
}

const text = await Deno.readTextFile(credsPath);
const env: Record<string, string> = {};
for (const line of text.split("\n")) {
  const m = line.match(/^\s*(?:export\s+)?([A-Z0-9_]+)\s*=\s*(.*)\s*$/);
  if (m) env[m[1]] = m[2].replace(/^["']|["']$/g, "");
}
const handle = env[`${account}_ATPROTO_HANDLE`];
const password = env[`${account}_ATPROTO_PASSWORD`];
if (!handle || !password) {
  console.error(`missing ${account}_ATPROTO_HANDLE or ${account}_ATPROTO_PASSWORD in ${credsPath}`);
  Deno.exit(2);
}

async function json(url: string, init?: RequestInit): Promise<any> {
  const res = await fetch(url, init);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}: ${JSON.stringify(body)}`);
  return body;
}

const resolved = await json(`https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle?handle=${encodeURIComponent(handle)}`);
const operatorDid: string = resolved.did;
const doc = await json(`https://plc.directory/${operatorDid}`);
const pds = doc.service?.find((s: any) => s.id === "#atproto_pds")?.serviceEndpoint;
if (!pds) throw new Error(`no atproto_pds service in the DID document of ${operatorDid}`);

const session = await json(`${pds}/xrpc/com.atproto.server.createSession`, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ identifier: handle, password }),
});

const nonce = crypto.randomUUID().replace(/-/g, "");
const rkey = `bidder_associate-${bidderDid.replace(/:/g, "-")}`;
const record = {
  $type: "com.publicdomainrelay.temp.badgeBlueKeys",
  keyId: bidderDid,
  challenge: operatorDid,
  service: "bidder_associate",
  nonce,
  name: `bidder-${bidderDid.slice(-8)}`,
  createdAt: new Date().toISOString(),
};
const written = await json(`${pds}/xrpc/com.atproto.repo.putRecord`, {
  method: "POST",
  headers: { "Content-Type": "application/json", Authorization: `Bearer ${session.accessJwt}` },
  body: JSON.stringify({
    repo: operatorDid,
    collection: "com.publicdomainrelay.temp.badgeBlueKeys",
    rkey,
    record,
  }),
});
console.log(JSON.stringify({ operatorDid, handle, rkey, uri: written.uri, cid: written.cid }));
