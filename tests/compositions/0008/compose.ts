// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0008-host-observer.md)
import { build } from "../build.ts";
await build("acceptance 0008", "Acceptance 0008 host observer: the VM on its host while Held, vm.terminated pinned, the orphan check. (docs/acceptance/0008-host-observer.md).", "acc-0008");
