// deno run -A --no-lock compose.ts > composition.json   (from tests/compositions/0004)
// docs/acceptance/0004-vm-atirc-agent.md: the test module, its assertions routed to the assert module.
import { composition } from "npm:@publicdomainrelay/atlibs@0.5.1/compose/build.js";

const binding = async (key: string) => {
  const m = JSON.parse(await Deno.readTextFile(new URL(`../../modules/${key}/module.json`, import.meta.url)));
  return { key, calls: m.calls ?? [], hooks: m.hooks ?? [], implements: m.implements ?? [], config: m.config, entries: m.actions.map((a: { entry: string }) => a.entry) };
};

const c = composition({
  name: "acceptance 0004",
  description: "An atirc agent in a market VM: chain, owner mesh node, answer, fast release (docs/acceptance/0004-vm-atirc-agent.md).",
});
const test = c.instance("test", await binding("acc-0004"));
const assert = c.instance("assert", await binding("assert"));
for (const n of ["recordsGone", "tombstoned", "agentChain"]) c.route(test, `computer.socialweb.test.assert.${n}`, assert);

console.log(await c.json());
