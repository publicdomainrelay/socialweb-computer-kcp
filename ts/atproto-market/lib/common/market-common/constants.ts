export { $nsid as BID_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/bid.ts";
export { $nsid as ACCEPT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/accept.ts";
export { $nsid as EVENT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/event.ts";
export { $nsid as RECEIPT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/receipt.ts";
export { $nsid as RFP_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/rfp.ts";
export { $nsid as OFFERING_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/offering.ts";
export { $nsid as HEARTBEAT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/heartbeat.ts";
export const HEARTBEAT_RKEY = "self";
export const HEARTBEAT_INTERVAL_MS = 60_000;
export const HEARTBEAT_TTL_MS = 300_000;

export { $nsid as COMPUTE_VM_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/vm.ts";
export { $nsid as COMPUTE_EVENTS_VM_DELETE_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/delete.ts";
export const VOUCH_NSID = "sh.tangled.graph.vouch";
export const RELAYS_NSID = "com.publicdomainrelay.temp.market.relays";

export { $nsid as BIDS_FREE_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/bids/free.ts";
export { $nsid as ACCEPTS_FREE_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/accepts/free.ts";
export { $nsid as RECEIPTS_FREE_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/receipts/free.ts";
export { $nsid as BIDS_X402_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/bids/x402.ts";
export { $nsid as ACCEPTS_X402_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/accepts/x402.ts";
export { $nsid as RECEIPTS_X402_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/receipts/x402.ts";

export { $nsid as SUBMIT_RFP_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/submitRfp.ts";
export { $nsid as SUBMIT_BID_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/submitBid.ts";
export { $nsid as SUBMIT_ACCEPT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/submitAccept.ts";
export { $nsid as SUBMIT_EVENT_NSID } from "../market-lexicons/com/publicdomainrelay/temp/market/submitEvent.ts";

export const SUBMIT_RFP_LXM = "com.publicdomainrelay.temp.market.submitRfp";
export const SUBMIT_BID_LXM = "com.publicdomainrelay.temp.market.submitBid";
export const SUBMIT_ACCEPT_LXM = "com.publicdomainrelay.temp.market.submitAccept";
export const SUBMIT_EVENT_LXM = "com.publicdomainrelay.temp.market.submitEvent";

export const DEFAULT_MARKET_SERVICE_ID = "pdr_temp_market";
export const DEFAULT_COMPUTE_EVENT_SERVICE_ID = "pdr_temp_compute_event";

export { $nsid as COMPUTE_EVENTS_VM_ONNETWORK_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/onNetwork.ts";
export { $nsid as COMPUTE_EVENTS_VM_STARTED_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/started.ts";
export const REGISTER_IDENTITY_NSID = "com.publicdomainrelay.temp.market.registerIdentity";
export { $nsid as COMPUTE_EVENTS_VM_REGISTER_IDENTITY_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/registerIdentity.ts";

export { $nsid as COMPUTE_EVENTS_VM_TERMINATED_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/terminated.ts";

export const VM_TERMINATED_REASONS = [
  "guest_poweroff",
  "vmm_exited",
  "oom",
  "host_reboot",
  "identity_mismatch",
  "deleted_on_request",
] as const;

export type VmTerminatedReason = typeof VM_TERMINATED_REASONS[number];

export { $nsid as COMPUTE_EVENTS_VM_HOST_KEYS_NSID } from "../market-lexicons/com/publicdomainrelay/temp/compute/events/vm/hostKeys.ts";

export const VM_HOST_KEY_TYPES = [
  "ssh-ed25519",
  "ecdsa-sha2-nistp256",
  "ssh-rsa",
] as const;

export type VmHostKeyType = typeof VM_HOST_KEY_TYPES[number];
