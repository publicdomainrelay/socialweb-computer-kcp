// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0017-owner-release.md)
import { build } from "../build.ts";
await build("acceptance 0017", "Acceptance 0017: a release asked by another process while the owner holds an interactive ssh session settles in seconds, with the owner stopping after Released (or taken over when the owner dies). (docs/acceptance/0017-owner-release.md).", "acc-0017");
