import { createRequesterTangledVouchPolicy } from "@publicdomainrelay/policy-deno-typescript";
import { evaluatePolicyAction } from "../../../action-common.ts";

await evaluatePolicyAction(createRequesterTangledVouchPolicy());
