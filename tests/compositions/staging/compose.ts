// deno run -A --no-lock compose.ts > composition.json   (from tests/compositions/staging)
// README.md: the staging stand-up, one module on the host's handlers.
import { composition } from "npm:@publicdomainrelay/atlibs@0.5.1/compose/build.js";

const binding = async (key: string) => {
  const m = JSON.parse(await Deno.readTextFile(new URL(`../../modules/${key}/module.json`, import.meta.url)));
  return { key, calls: m.calls ?? [], hooks: m.hooks ?? [], implements: m.implements ?? [], config: m.config, entries: m.actions.map((a: { entry: string }) => a.entry) };
};

const c = composition({
  name: "staging",
  description: "A full socialweb.computer on rented VMs for one suite run, torn down after (tests/compositions/staging/README.md).",
});
c.instance("test", await binding("staging"));

console.log(await c.json());
