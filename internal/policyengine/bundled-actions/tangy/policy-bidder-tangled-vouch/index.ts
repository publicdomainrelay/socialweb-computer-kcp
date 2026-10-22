// bidder-tangled-vouch policy action — source for dist/index.js (rebuild with `deno task build`).
import { createBidderTangledVouchPolicy } from "@publicdomainrelay/policy-deno-typescript";
import { evaluatePolicyAction } from "../../../action-common.ts";

await evaluatePolicyAction(createBidderTangledVouchPolicy());
