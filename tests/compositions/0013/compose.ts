// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0013-mixed-prod.md; entries run = A, c, b)
import { build } from "../build.ts";
await build("acceptance 0013", "Mixed prod: run requester + mixed-prod.yaml by discovery (A), the gateway in the requester kcp (C), install requester (B, staging) (docs/acceptance/0013-mixed-prod.md).", "acc-0013");
