// The compositions' shared builder: instance `test` of the runbook's module, `assert` beside it, every assert hook of
// the test routed to it. Each <runbook>/compose.ts calls build(); `deno run -A --no-lock compose.ts > composition.json`.
import { composition } from "npm:@publicdomainrelay/atlibs@0.5.1/compose/build.js";

type Hook = { nsid: string };
const binding = async (key: string) => {
  const m = JSON.parse(await Deno.readTextFile(new URL(`../modules/${key}/module.json`, import.meta.url)));
  return { key, calls: m.calls ?? [], hooks: m.hooks ?? [], implements: m.implements ?? [], config: m.config, entries: m.actions.map((a: { entry: string }) => a.entry) };
};

export async function build(name: string, description: string, testKey: string) {
  const c = composition({ name, description });
  const testMod = await binding(testKey);
  const test = c.instance("test", testMod);
  const asserts = testMod.hooks.filter((h: Hook) => h.nsid.startsWith("computer.socialweb.test.assert."));
  if (asserts.length) {
    const assert = c.instance("assert", await binding("assert"));
    for (const h of asserts) c.route(test, h.nsid, assert);
  }
  console.log(await c.json());
}
