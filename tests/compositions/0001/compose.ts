// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0001-auth-modes.md sections 3-6)
import { build } from "../build.ts";
await build("acceptance 0001 s3-s6", "Tenant cluster, kubeconfigs and their revoke (xrpc, page), 1-2-3-2-1-0 one replica per node (docs/acceptance/0001-auth-modes.md).", "acc-0001");
