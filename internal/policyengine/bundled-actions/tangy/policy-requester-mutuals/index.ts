// requester-mutuals policy action — source for dist/index.js (rebuild with `deno task build`).
import { createRequesterMutualPolicy } from "@publicdomainrelay/policy-deno-typescript";
import { evaluatePolicyAction } from "../../../action-common.ts";

await evaluatePolicyAction(createRequesterMutualPolicy());
