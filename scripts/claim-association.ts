import { parseArgs } from "jsr:@std/cli@1/parse-args";

const args = parseArgs(Deno.args, {
  string: ["creds", "account", "sessions", "pds", "url", "ca", "did", "service"],
  boolean: ["delete"],
});
const fail = (msg: string): never => {
  console.error(`claim-association: ${msg}`);
  Deno.exit(2);
};
const COLLECTION = "com.publicdomainrelay.temp.badgeBlueKeys";

let identifier: string;
let password: string;
let pds = args.pds ?? "https://bsky.social";
if (args.sessions) {
  const sessions = JSON.parse(await Deno.readTextFile(args.sessions)) as Record<string, { service: string; identifier: string; password: string }>;
  const first = Object.keys(sessions).sort()[0] ?? fail(`${args.sessions} holds no session`);
  ({ identifier, password } = sessions[first]);
  pds = args.pds ?? sessions[first].service;
} else {
  if (!args.creds || !args.account) fail("--creds and --account, or --sessions, are required");
  const vars: Record<string, string> = {};
  for (const line of (await Deno.readTextFile(args.creds!)).split("\n")) {
    const m = line.match(/^\s*(?:export\s+)?([A-Z0-9_]+)=(.*)$/);
    if (m) vars[m[1]] = m[2].replace(/^['"]|['"]$/g, "");
  }
  identifier = vars[`${args.account}_ATPROTO_HANDLE`] ?? fail(`${args.account}_ATPROTO_HANDLE is not in ${args.creds}`);
  password = vars[`${args.account}_ATPROTO_PASSWORD`] ?? fail(`${args.account}_ATPROTO_PASSWORD is not in ${args.creds}`);
}

const claim = new URLSearchParams((args.url ?? "").split("#")[1] ?? "");
const did = args.did ?? claim.get("bdr") ?? claim.get("plc") ?? fail("--url (the MarketIdentity status.pendingAction.url) or --did is required");
const service = args.service ?? claim.get("s") ?? fail("the url names no s=<service>; pass --service");
const nonce = claim.get("n");
if (!args.delete && !nonce) fail("the url names no n=<nonce>");

const client = args.ca ? Deno.createHttpClient({ caCerts: [await Deno.readTextFile(args.ca)] }) : undefined;
async function xrpc(method: string, nsid: string, body?: unknown, token?: string): Promise<Record<string, unknown>> {
  const res = await fetch(`${pds.replace(/\/+$/, "")}/xrpc/${nsid}`, {
    method,
    headers: { ...(body ? { "content-type": "application/json" } : {}), ...(token ? { authorization: `Bearer ${token}` } : {}) },
    body: body ? JSON.stringify(body) : undefined,
    client,
  } as RequestInit);
  const out = await res.json().catch(() => ({}));
  if (!res.ok) fail(`${nsid}: ${res.status} ${out.error ?? ""} ${out.message ?? ""}`);
  return out;
}

const session = await xrpc("POST", "com.atproto.server.createSession", { identifier, password });
const operator = session.did as string;
const token = session.accessJwt as string;
const rkey = `${service}-${did.replaceAll(":", "-")}`;
if (args.delete) {
  await xrpc("POST", "com.atproto.repo.deleteRecord", { repo: operator, collection: COLLECTION, rkey }, token);
  console.log(JSON.stringify({ deleted: `at://${operator}/${COLLECTION}/${rkey}` }));
} else {
  const record = { $type: COLLECTION, keyId: did, challenge: operator, service, nonce, name: `${service.split("_")[0]}-${did.slice(-8)}`, createdAt: new Date().toISOString() };
  const out = await xrpc("POST", "com.atproto.repo.putRecord", { repo: operator, collection: COLLECTION, rkey, record }, token);
  console.log(JSON.stringify({ claimed: did, operator, uri: out.uri, cid: out.cid }));
}
