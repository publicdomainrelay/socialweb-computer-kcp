// bidder-only-me policy action — source for dist/index.js (rebuild with `deno task build`).
import { createBidderOnlyMePolicy } from "@publicdomainrelay/policy-deno-typescript";
import { evaluatePolicyAction } from "../../../action-common.ts";

await evaluatePolicyAction(createBidderOnlyMePolicy());
