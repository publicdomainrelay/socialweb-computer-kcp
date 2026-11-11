const args = new Map<string, string>();
for (let i = 0; i < Deno.args.length; i++) {
  const key = Deno.args[i].replace(/^--/, "");
  const next = Deno.args[i + 1];
  if (next === undefined || next.startsWith("--")) args.set(key, "true");
  else args.set(key, Deno.args[++i]);
}
const credsPath = args.get("creds");
const account = args.get("account");
const agentDid = (args.get("agent-did") ?? args.get("bidder-did") ?? args.get("requester-did") ?? args.get("did"))?.trim();
const remove = args.get("remove") === "true";
if (!credsPath || !account || !agentDid) {
  console.error("usage: associate-bidder.ts --creds FILE --account BOB --agent-did DID [--remove]");
  Deno.exit(2);
}
if (!/^did:(plc|web):[A-Za-z0-9._:%-]+$/.test(agentDid)) {
  console.error(`not an agent DID: ${agentDid}`);
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

const GRANT = "com.publicdomainrelay.temp.market.association";
const ENDORSEMENT = "link.ident.endorsement";
function call(method: string, body: Record<string, unknown>): Promise<any> {
  return json(`${pds}/xrpc/com.atproto.repo.${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${session.accessJwt}` },
    body: JSON.stringify({ repo: operatorDid, rkey: agentDid, ...body }),
  });
}

if (remove) {
  await call("deleteRecord", { collection: ENDORSEMENT });
  await call("deleteRecord", { collection: GRANT });
  console.log(JSON.stringify({ operatorDid, handle, agentDid, removed: [ENDORSEMENT, GRANT] }));
} else {
  const grant = await call("putRecord", {
    collection: GRANT,
    record: { $type: GRANT, agent: agentDid, createdAt: new Date().toISOString() },
  });
  const endorsement = await call("putRecord", {
    collection: ENDORSEMENT,
    record: { $type: ENDORSEMENT, agent: agentDid },
  });
  console.log(JSON.stringify({ operatorDid, handle, agentDid, grant: { uri: grant.uri, cid: grant.cid }, endorsement: { uri: endorsement.uri, cid: endorsement.cid } }));
}
