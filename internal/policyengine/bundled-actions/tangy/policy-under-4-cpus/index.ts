// under-4-cpus policy action — source for dist/index.js (rebuild with `deno task build`).
import { createUnderFourCpusPolicy } from "@publicdomainrelay/policy-deno-typescript";
import { evaluatePolicyAction } from "../../../action-common.ts";

await evaluatePolicyAction(createUnderFourCpusPolicy());
