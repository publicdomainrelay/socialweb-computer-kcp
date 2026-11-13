// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0006-rollout-compat.md)
import { build } from "../build.ts";
await build("acceptance 0006", "Acceptance 0006 rollout checks: every bidder signed in on bidder-tangled-vouch (run), the unchanged CLI rents and releases (compat). (docs/acceptance/0006-rollout-compat.md).", "acc-0006");
