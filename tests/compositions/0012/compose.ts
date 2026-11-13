// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0012-infra-quickstart.md)
import { build } from "../build.ts";
await build("acceptance 0012", "Acceptance 0012 Infra quickstart: dry runs, vm.yaml ssh, deno stdout, restart keeps Held, deletes release. (docs/acceptance/0012-infra-quickstart.md).", "acc-0012");
