// deno run -A --no-lock compose.ts > composition.json   (docs/acceptance/0003-cli-webrtc-ssh.md, 0001 section 9)
import { build } from "../build.ts";
await build("acceptance 0003", "CLI ssh over WebRTC, fresh and restarted (<= 10 s), no plaintext signaling, records gone, host empty (docs/acceptance/0003-cli-webrtc-ssh.md).", "acc-0003");
