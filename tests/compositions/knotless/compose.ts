// deno run -A --no-lock compose.ts > composition.json   (tests/compositions/knotless/README.md)
import { build } from "../build.ts";
await build("knotless", "A staging network's knotless (dev up --with-knotless): push, after: [build] artifacts, per-workflow executor bindings, a pull gets no secrets, a VM its bidder terminated fails its step at once, a run cancelled while it rents ends there and one cancelled holding a VM gives it back (tests/compositions/knotless/README.md).", "knotless");
