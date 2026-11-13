// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0014-accept-fallback.md)
import { build } from "../build.ts";
await build("acceptance 0014", "Acceptance 0014: a refused accept falls back to the next bid; concurrent rentals never overcommit a bidder. (docs/acceptance/0014-accept-fallback.md).", "acc-0014");
