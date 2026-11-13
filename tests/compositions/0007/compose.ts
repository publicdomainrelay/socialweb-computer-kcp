// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0007-bidder-termination.md)
import { build } from "../build.ts";
await build("acceptance 0007", "Acceptance 0007: an operator delete of this run's own VM on its bidder kcp ends it cleanly. (docs/acceptance/0007-bidder-termination.md).", "acc-0007");
