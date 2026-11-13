// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0016-vm-image.md)
import { build } from "../build.ts";
await build("acceptance 0016", "Acceptance 0016: a VM rented with a requester-chosen image boots it and reports its marker; a wrong sha256 is refused before boot. (docs/acceptance/0016-vm-image.md).", "acc-0016");
