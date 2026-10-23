const args = new Map<string, string>();
for (let i = 0; i < Deno.args.length; i += 2) args.set(Deno.args[i].replace(/^--/, ""), Deno.args[i + 1]);
const credsPath = args.get("creds");
const account = args.get("account");
const subjectDid = args.get("bidder-did") ?? args.get("requester-did") ?? args.get("did");
const roleArg = args.get("role")?.trim();
const serviceArg = args.get("service")?.trim();
const nonceArg = args.get("nonce")?.trim();
const deleteStale = args.get("delete-stale-service") === "true";
if (!credsPath || !account || !subjectDid) {
  console.error("usage: associate-bidder.ts --creds FILE --account BOB (--bidder-did DID | --requester-did DID) [--service bidder_associate|requester_associate] [--nonce N] [--delete-stale-service true]");
  Deno.exit(2);
}
const subjectFlag = args.has("requester-did") ? "requester" : args.has("bidder-did") ? "bidder" : "";
const role = roleArg ?? (subjectFlag || (serviceArg === "requester_associate" ? "requester" : "bidder"));
if (role !== "bidder" && role !== "requester") {
  console.error(`unknown role ${role}; want bidder or requester`);
  Deno.exit(2);
}
if (subjectFlag && roleArg && subjectFlag !== roleArg) {
  console.error(`--${subjectFlag}-did does not match role ${roleArg}`);
  Deno.exit(2);
}
const service = serviceArg ?? `${role}_associate`;
if (service !== "bidder_associate" && service !== "requester_associate") {
  console.error(`unknown service ${service}; want bidder_associate or requester_associate`);
  Deno.exit(2);
}
if (service !== `${role}_associate`) {
  console.error(`role ${role} does not match service ${service}`);
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

const nonce = nonceArg || crypto.randomUUID().replace(/-/g, "");
const rkey = `${service}-${subjectDid.replace(/:/g, "-")}`;
const record = {
  $type: "com.publicdomainrelay.temp.badgeBlueKeys",
  keyId: subjectDid,
  challenge: operatorDid,
  service,
  nonce,
  name: `${role}-${subjectDid.slice(-8)}`,
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
if (deleteStale) {
  const staleService = service === "bidder_associate" ? "requester_associate" : "bidder_associate";
  const staleRkey = `${staleService}-${subjectDid.replace(/:/g, "-")}`;
  try {
    await json(`${pds}/xrpc/com.atproto.repo.deleteRecord`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${session.accessJwt}` },
      body: JSON.stringify({ repo: operatorDid, collection: "com.publicdomainrelay.temp.badgeBlueKeys", rkey: staleRkey }),
    });
    console.error(JSON.stringify({ deletedStaleService: staleService, rkey: staleRkey }));
  } catch {
  }
}
console.log(JSON.stringify({ operatorDid, handle, service, rkey, uri: written.uri, cid: written.cid }));
