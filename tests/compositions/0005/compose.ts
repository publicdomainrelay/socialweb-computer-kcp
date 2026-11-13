// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0005-guest-requester-association.md; entries run = A, b)
import { build } from "../build.ts";
await build("acceptance 0005", "Guest requester R2: the nested VM through the association, the cascade (A); co-located mesh1 and the out-of-band loss (B) (docs/acceptance/0005-guest-requester-association.md).", "acc-0005");
