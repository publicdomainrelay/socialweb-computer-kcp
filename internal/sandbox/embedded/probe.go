package embedded

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
	ProbeTimeout = 30 * time.Second

	ProbeName = "socialweb-fence-probe"

	ProbeEnv = "SOCIALWEB_FENCE_ENV"

	ProbeStderr = "socialweb-fence-probe-stderr"

	ProbeExitCode = 3
)

var ProbePlants = []string{"keychain", "file", "defaults", "cookies"}

type Planted struct {
	Engine string `json:"engine"`

	FileURL string `json:"fileURL"`

	Planted map[string]string `json:"planted"`
}

type facts struct {
	keys   []string
	values map[string]string
}

func (f *facts) set(key, value string) {
	if f.values == nil {
		f.values = map[string]string{}
	}
	if _, ok := f.values[key]; !ok {
		f.keys = append(f.keys, key)
	}
	f.values[key] = strings.Join(strings.Fields(value), "_")
}

func (f *facts) get(key string) string { return f.values[key] }

func (f *facts) String() string {
	parts := make([]string, 0, len(f.keys))
	for _, key := range f.keys {
		parts = append(parts, key+"="+f.values[key])
	}
	return strings.Join(parts, " ")
}

func (r *Runner) Probe(ctx context.Context) (sandbox.Fence, error) {
	r.fenced.Store(false)
	if r.host == nil {
		return sandbox.Fence{Reason: policyv1alpha1.PolicyEngineFencedNotProbed, Message: ErrNoHost.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	marker, err := token()
	if err != nil {
		return sandbox.Fence{}, err
	}
	envToken, err := token()
	if err != nil {
		return sandbox.Fence{}, err
	}
	stash, err := token()
	if err != nil {
		return sandbox.Fence{}, err
	}
	var f facts
	planted, err := r.plant(ctx, marker)
	if err != nil {
		r.clear(marker)
		return sandbox.Fence{}, fmt.Errorf("sandbox/embedded: probe: planting the markers: %w", err)
	}
	f.set("engine", strings.Join(strings.Fields(planted.Engine), "-"))
	for _, name := range ProbePlants {
		f.set("plant."+name, planted.Planted[name])
	}
	if !strings.HasPrefix(planted.FileURL, "file://") {
		f.set("plant.fileURL", "MISSING")
	}
	r.probeFence(ctx, &f, marker, envToken, stash, planted.FileURL)
	r.probeTimeout(ctx, &f)
	r.probeRerun(ctx, &f, stash)
	r.probeUncaught(ctx, &f)
	f.set("clear", r.clear(marker))
	failed := failedFacts(&f)
	fenced := len(failed) == 0
	r.fenced.Store(fenced)
	message := "passed: " + f.String()
	if !fenced {
		message = "failed: " + strings.Join(failed, ",") + "; " + f.String()
	}
	return sandbox.Fence{Fenced: fenced, Message: message}, nil
}

func (r *Runner) plant(ctx context.Context, marker string) (Planted, error) {
	select {
	case out := <-call(func() ([]byte, error) { return r.host.PlantProbe(marker) }):
		if out.err != nil {
			return Planted{}, out.err
		}
		var planted Planted
		if err := json.Unmarshal(out.raw, &planted); err != nil {
			return Planted{}, fmt.Errorf("the plant answer: %w", err)
		}
		return planted, nil
	case <-ctx.Done():
		return Planted{}, ctx.Err()
	}
}

func (r *Runner) clear(marker string) string {
	select {
	case out := <-call(func() ([]byte, error) { return nil, r.host.ClearProbe(marker) }):
		if out.err != nil {
			return "failed:" + oneWord(out.err.Error())
		}
		return "ok"
	case <-time.After(Grace):
		return "hung"
	}
}

func (r *Runner) probeFence(ctx context.Context, f *facts, marker, envToken, stash, fileURL string) {
	answer, hung, err := r.run(ctx, sandbox.Program{
		Source:  fenceProgram(marker, envToken, stash, fileURL),
		Net:     true,
		Env:     map[string]string{ProbeEnv: envToken},
		Timeout: 15 * time.Second,
	})
	if seen, ok := lastJSON(answer.Stdout); err == nil && !hung && answer.ExitCode == 0 && ok {
		for _, key := range []string{"env", "shim", "net", "dom", "bridges", "file", "root", "xhr", "import", "scan", "idb", "cache"} {
			f.set(key, seen[key])
		}
		return
	}
	f.set("fence", outcome(answer, hung, err))
}

func (r *Runner) probeTimeout(ctx context.Context, f *facts) {
	answer, hung, err := r.run(ctx, sandbox.Program{Source: "for (;;) {}\n", Net: true, Env: map[string]string{}, Timeout: time.Second})
	switch {
	case err != nil:
		f.set("timeout", "error:"+oneWord(err.Error()))
	case hung:
		f.set("timeout", "hung")
	case answer.TimedOut:
		f.set("timeout", "killed")
	default:
		f.set("timeout", fmt.Sprintf("exit=%d", answer.ExitCode))
	}
}

func (r *Runner) probeRerun(ctx context.Context, f *facts, stash string) {
	answer, hung, err := r.run(ctx, sandbox.Program{Source: rerunProgram(stash), Net: true, Env: map[string]string{}, Timeout: 10 * time.Second})
	seen, ok := lastJSON(answer.Stdout)
	switch {
	case err != nil || hung || !ok:
		f.set("rerun", outcome(answer, hung, err))
	case answer.ExitCode != ProbeExitCode:
		f.set("rerun", fmt.Sprintf("exit=%d:stderr=%s", answer.ExitCode, oneWord(answer.Stderr)))
	case !strings.Contains(answer.Stderr, ProbeStderr):
		f.set("rerun", "NO-STDERR")
	default:
		f.set("rerun", "ok")
	}
	f.set("stash", seen["stash"])
}

func (r *Runner) probeUncaught(ctx context.Context, f *facts) {
	answer, hung, err := r.run(ctx, sandbox.Program{Source: "throw new Error(" + jsString(ProbeName) + ");\n", Net: true, Env: map[string]string{}, Timeout: 10 * time.Second})
	switch {
	case err != nil || hung:
		f.set("uncaught", outcome(answer, hung, err))
	default:
		f.set("uncaught", fmt.Sprintf("exit=%d", answer.ExitCode))
	}
}

func failedFacts(f *facts) []string {
	var failed []string
	for _, name := range ProbePlants {
		if f.get("plant."+name) != "yes" {
			failed = append(failed, "plant."+name)
		}
	}
	for _, key := range []string{"plant.fileURL", "fence"} {
		if f.get(key) != "" {
			failed = append(failed, key)
		}
	}
	want := []struct{ key, value string }{
		{"env", "ok"}, {"shim", "ok"}, {"net", "fetch"}, {"dom", "none"}, {"bridges", "none"}, {"scan", "clean"},
		{"timeout", "killed"}, {"rerun", "ok"}, {"stash", "none"}, {"uncaught", "exit=1"},
	}
	for _, w := range want {
		if f.get(w.key) != w.value {
			failed = append(failed, w.key)
		}
	}
	for _, key := range []string{"file", "root", "xhr", "import"} {
		if v := f.get(key); v != "none" && !strings.HasPrefix(v, "denied") {
			failed = append(failed, key)
		}
	}
	for _, key := range []string{"idb", "cache"} {
		if v := f.get(key); v != "none" && v != "stored" && !strings.HasPrefix(v, "denied") {
			failed = append(failed, key)
		}
	}
	return failed
}

func outcome(answer Answer, hung bool, err error) string {
	switch {
	case err != nil:
		return "error:" + oneWord(err.Error())
	case hung:
		return "hung"
	case answer.TimedOut:
		return "timedOut"
	default:
		return fmt.Sprintf("exit=%d:stdout=%s:stderr=%s", answer.ExitCode, oneWord(tail(answer.Stdout)), oneWord(answer.Stderr))
	}
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[len(s)-120:]
	}
	return s
}

func oneWord(s string) string {
	s = strings.Join(strings.Fields(s), "_")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func lastJSON(stdout string) (map[string]string, bool) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &raw); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if text, ok := value.(string); ok {
			out[key] = text
		} else {
			out[key] = fmt.Sprint(value)
		}
	}
	return out, true
}

func token() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsSplit(s string) string {
	half := len(s) / 2
	return "[" + jsString(s[:half]) + ", " + jsString(s[half:]) + "].join(\"\")"
}

func fenceProgram(marker, envToken, stash, fileURL string) string {
	return `import { Buffer } from "node:buffer";
const marker = ` + jsSplit(marker) + `;
const envToken = ` + jsSplit(envToken) + `;
const stash = ` + jsSplit(stash) + `;
const fileURL = ` + jsString(fileURL) + `;
const name = ` + jsString(ProbeName) + `;
const out = {};
const err = (e) => "denied:" + String(e?.name ?? "error");
const within = (promise, ms) => Promise.race([promise, new Promise((resolve) => setTimeout(() => resolve("denied:timeout"), ms))]);
out.env = Deno.env.get(` + jsString(ProbeEnv) + `) === envToken ? "ok" : "MISSING";
try { out.shim = Buffer.from("ok").toString(); } catch (e) { out.shim = err(e); }
out.net = typeof fetch === "function" ? "fetch" : "NONE";
out.dom = ["document", "window", "parent", "top", "localStorage", "sessionStorage"].every((n) => typeof globalThis[n] === "undefined") ? "none" : "PRESENT";
const bridges = ["webkit", "require", "process", "Bun", "__socialwebSandbox", "AtircGo", "Atircgo", "Go"].filter((n) => typeof globalThis[n] !== "undefined");
out.bridges = bridges.length ? "PRESENT:" + bridges.join(",") : "none";
const read = (url) => within((async () => {
  try {
    const res = await fetch(url);
    const text = await res.text();
    return text.includes(marker) ? "VISIBLE" : res.ok ? "READABLE" : "denied:status" + res.status;
  } catch (e) { return err(e); }
})(), 3000);
out.file = typeof fetch === "function" ? await read(fileURL) : "none";
out.root = typeof fetch === "function" ? await read("file:///") : "none";
out.xhr = typeof XMLHttpRequest === "undefined" ? "none" : await within(new Promise((resolve) => {
  try {
    const x = new XMLHttpRequest();
    x.onload = () => resolve(String(x.responseText).includes(marker) ? "VISIBLE" : "READABLE");
    x.onerror = () => resolve("denied:error");
    x.open("GET", fileURL);
    x.send();
  } catch (e) { resolve(err(e)); }
}), 3000);
out.import = await within((async () => {
  try {
    const mod = await import(fileURL);
    return mod?.default === marker ? "VISIBLE" : "READABLE";
  } catch (e) { return err(e); }
})(), 3000);
const seen = new Set();
const hits = [];
let nodes = 0;
const walk = (value, path, depth) => {
  if (hits.length || nodes > 20000) return;
  if (typeof value === "string") {
    if (value.includes(marker)) hits.push(path);
    return;
  }
  if (value === null || (typeof value !== "object" && typeof value !== "function") || seen.has(value) || depth > 4) return;
  seen.add(value);
  nodes++;
  let names = [];
  try { names = Object.getOwnPropertyNames(value); } catch { return; }
  for (const key of names) {
    let child;
    try {
      const d = Object.getOwnPropertyDescriptor(value, key);
      child = d && "value" in d ? d.value : undefined;
    } catch { continue; }
    walk(child, path + "." + key, depth + 1);
  }
};
walk(globalThis, "globalThis", 0);
out.scan = hits.length ? "VISIBLE:" + hits[0] : "clean";
globalThis.__socialwebFenceStash = stash;
out.idb = typeof indexedDB === "undefined" ? "none" : await within(new Promise((resolve) => {
  try {
    const r = indexedDB.open(name, 1);
    r.onupgradeneeded = () => r.result.createObjectStore("s");
    r.onsuccess = () => {
      try {
        const tx = r.result.transaction("s", "readwrite");
        tx.objectStore("s").put(stash, "k");
        tx.oncomplete = () => resolve("stored");
        tx.onerror = () => resolve("denied:tx");
      } catch (e) { resolve(err(e)); }
    };
    r.onerror = () => resolve(err(r.error));
  } catch (e) { resolve(err(e)); }
}), 3000);
out.cache = typeof caches === "undefined" ? "none" : await within((async () => {
  try {
    await (await caches.open(name)).put("https://" + name + ".invalid/k", new Response(stash));
    return "stored";
  } catch (e) { return err(e); }
})(), 3000);
console.log(JSON.stringify(out));
`
}

func rerunProgram(stash string) string {
	return `const stash = ` + jsSplit(stash) + `;
const name = ` + jsString(ProbeName) + `;
const within = (promise, ms) => Promise.race([promise, new Promise((resolve) => setTimeout(() => resolve(undefined), ms))]);
const seen = [];
if (globalThis.__socialwebFenceStash !== undefined) seen.push("global");
if (typeof indexedDB !== "undefined") {
  const value = await within(new Promise((resolve) => {
    try {
      const r = indexedDB.open(name);
      r.onsuccess = () => {
        try {
          const db = r.result;
          if (!db.objectStoreNames.contains("s")) return resolve(undefined);
          const g = db.transaction("s").objectStore("s").get("k");
          g.onsuccess = () => resolve(g.result);
          g.onerror = () => resolve(undefined);
        } catch { resolve(undefined); }
      };
      r.onerror = () => resolve(undefined);
    } catch { resolve(undefined); }
  }), 3000);
  if (value === stash) seen.push("idb");
}
if (typeof caches !== "undefined") {
  const hit = await within((async () => {
    try {
      const res = await (await caches.open(name)).match("https://" + name + ".invalid/k");
      return res ? await res.text() : undefined;
    } catch { return undefined; }
  })(), 3000);
  if (hit === stash) seen.push("cache");
}
console.log(JSON.stringify({ stash: seen.length ? "VISIBLE:" + seen.join(",") : "none" }));
console.error(` + jsString(ProbeStderr) + `);
Deno.exit(` + fmt.Sprint(ProbeExitCode) + `);
`
}
