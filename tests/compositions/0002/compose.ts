// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0002-ssh-gateway.md)
import { build } from "../build.ts";
await build("acceptance 0002", "ssh gateway as the user: a transient command; atirc agent run, transient and held, answering an observer (docs/acceptance/0002-ssh-gateway.md).", "acc-0002");
