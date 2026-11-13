// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0011-atirc.md)
import { build } from "../build.ts";
await build("acceptance 0011", "Acceptance 0011 atirc: deploy gates (run), a minted token posts and is refused after its revoke (token). (docs/acceptance/0011-atirc.md).", "acc-0011");
