package gatewayhttp

import "net/http"

func Page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(page))
}

const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>socialweb.computer k8s</title>
<style>body{font-family:system-ui,sans-serif;max-width:44rem;margin:2rem auto;padding:0 1rem}input,select,textarea,button{font:inherit}textarea{width:100%;height:10rem;box-sizing:border-box}li{margin:.6rem 0}code,pre{overflow-wrap:anywhere;white-space:pre-wrap}.row{display:flex;flex-wrap:wrap;gap:.4rem;align-items:center;margin:.3rem 0}.muted{color:#666}#out{color:#b00}</style>
</head><body>
<h1>socialweb.computer k8s</h1>
<p id="out" role="alert"></p>
<form id="signin" hidden><p>Sign in with your atproto handle.</p>
<input id="handle" placeholder="you.bsky.social" autocapitalize="none" spellcheck="false" required> <button>Sign in</button></form>
<div id="app" hidden>
<p>Signed in as <b id="who"></b> <button id="signout">Sign out</button></p>
<h2>Clusters</h2>
<form id="create" class="row"><input id="name" placeholder="my-cluster" maxlength="63" pattern="[a-z0-9]([\-a-z0-9]*[a-z0-9])?" autocapitalize="none" spellcheck="false" required><button>Create</button></form>
<ul id="list"></ul>
<div id="kc" hidden><h2>Kubeconfig <span id="kcname"></span></h2>
<p class="muted">Shown once. Keep it mode 0600; anyone with it has your access until it expires or is revoked.</p>
<textarea id="kctext" readonly></textarea>
<div class="row"><button id="dl">Download</button><button id="cp">Copy</button><button id="hide">Close</button></div>
<pre><code id="use"></code></pre></div>
</div>
<p><a href="https://github.com/johnandersen777/socialweb-computer-kcp">Source</a></p>
<script>
const $ = (id) => document.getElementById(id);
const say = (t) => { $("out").textContent = t || ""; };
const el = (tag, text, attrs) => { const e = document.createElement(tag); if (text) e.textContent = text; Object.assign(e, attrs || {}); return e; };
const call = async (method, path, body) => {
  const r = await fetch(path, {method, credentials: "same-origin", headers: body ? {"Content-Type": "application/json"} : {}, body: body ? JSON.stringify(body) : undefined});
  const j = await r.json().catch(() => ({}));
  if (r.status === 401 && path !== "/session") { $("app").hidden = true; $("signin").hidden = false; }
  if (!r.ok) throw new Error(j.message || j.error || r.status);
  return j;
};
const act = (fn) => async () => { say(""); try { await fn(); } catch (e) { say(e.message); } await clusters(); };
const label = (s) => s % 86400 === 0 ? (s / 86400) * 24 + " hours" : s % 3600 === 0 ? (s / 3600) + (s === 3600 ? " hour" : " hours") : s + " s";
let config = {ttlChoicesSec: [28800]}, timer;
const ttlPicker = () => {
  const s = el("select", "", {ariaLabel: "kubeconfig lifetime"});
  for (const t of config.ttlChoicesSec || []) s.append(el("option", label(t), {value: String(t), selected: t === 28800}));
  if (config.noExpiry) s.append(el("option", "No expiry (until revoked)", {value: "0"}));
  return s;
};
const mint = async (name, ttl) => {
  if (ttl === 0 && !confirm("This kubeconfig for " + name + " never expires. It works until you revoke it, delete the cluster or sign out here. Issue it?")) return;
  const j = await call("POST", "/api/clusters/kubeconfig", {name, ttlSeconds: ttl});
  $("kcname").textContent = name;
  $("kctext").value = j.kubeconfig;
  $("kctext").dataset.file = j.filename;
  $("use").textContent = "chmod 600 " + j.filename + "\nKUBECONFIG=./" + j.filename + " kubectl get ns";
  $("kc").hidden = false;
};
const issued = async (name, ul) => {
  const j = await call("POST", "/api/clusters/kubeconfigs", {name});
  const now = Date.now();
  const live = (j.kubeconfigs || []).filter((k) => !k.revoked && (k.noExpiry || Date.parse(k.notAfter) > now));
  live.sort((a, b) => (b.createdAt || "").localeCompare(a.createdAt || ""));
  ul.replaceChildren(...live.map((k) => {
    const li = el("li", "", {className: "row"});
    li.append(el("code", k.serial.slice(0, 16)), el("span", (k.createdAt || "") + " " + (k.noExpiry ? "no expiry" : "expires " + k.notAfter), {className: "muted"}),
      el("button", "Revoke", {onclick: act(() => call("POST", "/api/clusters/kubeconfigs/revoke", {name, serial: k.serial}))}));
    return li;
  }));
  if (!live.length) ul.append(el("li", "No live kubeconfigs.", {className: "muted"}));
};
const clusters = async () => {
  clearTimeout(timer);
  let j;
  try { j = await call("POST", "/api/clusters/list"); } catch (e) { say(e.message); return; }
  const cs = (j.clusters || []).sort((a, b) => a.name.localeCompare(b.name));
  $("list").replaceChildren(...cs.map((c) => {
    const li = el("li");
    const head = el("div", "", {className: "row"});
    head.append(el("b", c.name), el("span", c.phase || "Unknown", {className: "muted"}));
    const ops = el("div", "", {className: "row"});
    const draining = c.phase === "Draining";
    if (c.running && !draining) {
      const ttl = ttlPicker();
      ops.append(ttl, el("button", "Get kubeconfig", {onclick: async () => { say(""); try { await mint(c.name, Number(ttl.value)); } catch (e) { say(e.message); } await clusters(); }}),
        el("button", "Stop", {onclick: act(() => call("POST", "/api/clusters/down", {name: c.name}))}));
    } else if (!draining) {
      ops.append(el("button", "Start", {onclick: act(() => call("POST", "/api/clusters/up", {name: c.name}))}));
    }
    if (!draining) ops.append(el("button", "Delete", {onclick: async () => { if (confirm("Delete cluster " + c.name + "? Its objects and every kubeconfig for it stop working.")) await act(() => call("POST", "/api/clusters/delete", {name: c.name}))(); }}));
    li.append(head, ops);
    if (!draining) { const ul = el("ul"); li.append(ul); issued(c.name, ul).catch((e) => ul.replaceChildren(el("li", e.message, {className: "muted"}))); }
    return li;
  }));
  if (!cs.length) $("list").append(el("li", "No clusters yet.", {className: "muted"}));
  if (cs.some((c) => c.phase === "Initializing" || c.phase === "Draining")) timer = setTimeout(clusters, 5000);
};
const load = async () => {
  const q = new URLSearchParams(location.search);
  if (q.get("error")) { say(q.get("error")); history.replaceState(null, "", location.pathname); }
  let s;
  try { s = await call("GET", "/session"); } catch (e) { $("signin").hidden = false; return; }
  $("who").textContent = s.handle || s.did;
  $("app").hidden = false;
  config = await call("GET", "/api/config").catch(() => config);
  await clusters();
};
$("signin").onsubmit = async (e) => { e.preventDefault(); try { const j = await call("POST", "/oauth/start", {handle: $("handle").value.trim()}); location.href = j.authorizationUrl; } catch (x) { say(x.message); } };
$("create").onsubmit = (e) => { e.preventDefault(); const name = $("name").value.trim(); act(async () => { await call("POST", "/api/clusters/create", {name}); $("name").value = ""; })(); };
$("signout").onclick = async () => { if (!confirm("Signing out revokes every kubeconfig issued for your clusters. Sign out?")) return; try { await call("DELETE", "/session"); } catch (x) {} location.reload(); };
$("dl").onclick = () => { const a = el("a", "", {href: URL.createObjectURL(new Blob([$("kctext").value], {type: "application/yaml"})), download: $("kctext").dataset.file}); a.click(); URL.revokeObjectURL(a.href); };
$("cp").onclick = async () => { try { await navigator.clipboard.writeText($("kctext").value); say(""); } catch (e) { $("kctext").select(); say("select the text and copy it"); } };
$("hide").onclick = () => { $("kctext").value = ""; $("kc").hidden = true; };
load();
</script></body></html>
`
