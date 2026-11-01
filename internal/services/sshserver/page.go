package sshserver

const signInPage = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>socialweb.computer ssh</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}input,textarea,button{font:inherit}textarea{width:100%;height:5rem}li{margin:.3rem 0}code{word-break:break-all}</style>
</head><body>
<h1>socialweb.computer ssh</h1>
<div id="out"></div>
<form id="signin" hidden><p>Sign in with your atproto handle, then add the ssh public key you connect with.</p>
<input id="handle" placeholder="you.bsky.social" required> <button>Sign in</button></form>
<div id="keys" hidden>
<p>Signed in as <b id="who"></b>. Connect with <code id="cmd"></code> <button id="signout">Sign out</button></p>
<h2>ssh keys</h2><ul id="list"></ul>
<form id="add"><textarea id="key" placeholder="ssh-ed25519 AAAA... comment" required></textarea><br><button>Add key</button></form>
</div>
<script>
const $ = (id) => document.getElementById(id);
const say = (t) => { $("out").textContent = t; };
const call = async (method, path, body) => {
  const r = await fetch(path, {method, credentials: "same-origin", headers: body ? {"Content-Type": "application/json"} : {}, body: body ? JSON.stringify(body) : undefined});
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.message || j.error || r.status);
  return j;
};
const keys = async () => {
  const j = await call("GET", "/keys");
  $("list").replaceChildren(...(j.keys || []).map((k) => {
    const li = document.createElement("li");
    const b = document.createElement("button");
    b.textContent = "Remove";
    b.onclick = async () => { try { await call("DELETE", "/keys/" + encodeURIComponent(k.rkey)); await keys(); } catch (e) { say(e.message); } };
    li.append(k.name + " " + k.keyId.slice(0, 40) + "... ", b);
    return li;
  }));
};
const load = async () => {
  const err = new URLSearchParams(location.search).get("error");
  if (err) say(err);
  try {
    const s = await call("GET", "/session");
    $("who").textContent = s.handle || s.did;
    $("cmd").textContent = "ssh " + (s.handle || s.did) + "@" + location.hostname;
    $("keys").hidden = false;
    await keys();
  } catch (e) { $("signin").hidden = false; }
};
$("signin").onsubmit = async (e) => { e.preventDefault(); try { const j = await call("POST", "/oauth/start", {handle: $("handle").value}); location.href = j.authorizationUrl; } catch (x) { say(x.message); } };
$("add").onsubmit = async (e) => { e.preventDefault(); try { await call("POST", "/keys", {publicKey: $("key").value}); $("key").value = ""; await keys(); } catch (x) { say(x.message); } };
$("signout").onclick = async () => { try { await call("DELETE", "/session"); } catch (x) {} location.reload(); };
load();
</script></body></html>
`
