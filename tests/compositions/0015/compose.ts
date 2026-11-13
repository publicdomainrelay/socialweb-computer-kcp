// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0015-vm-memory.md)
import { build } from "../build.ts";
await build("acceptance 0015", "Acceptance 0015: this run's VM fills ~95% of its RAM and writes with fsync for 5 min, and lives through it. (docs/acceptance/0015-vm-memory.md).", "acc-0015");
