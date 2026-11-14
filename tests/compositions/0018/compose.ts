// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0018-downloads-integrity.md)
import { build } from "../build.ts";
await build("acceptance 0018", "Acceptance 0018: the shipped binary never uses a tampered downloads tree. (docs/acceptance/0018-downloads-integrity.md).", "acc-0018");
