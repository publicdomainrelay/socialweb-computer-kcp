// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0004-vm-atirc-agent.md)
import { build } from "../build.ts";
await build("acceptance 0004", "An atirc agent in a market VM: chain, owner mesh node, answer, fast release (docs/acceptance/0004-vm-atirc-agent.md).", "acc-0004");
