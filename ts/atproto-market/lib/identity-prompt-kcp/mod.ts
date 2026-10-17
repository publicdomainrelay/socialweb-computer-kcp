import type { MarketIdentityStatus } from "@publicdomainrelay/socialweb-computer-k8s-api";
import type { AtprotoRecordWatch } from "@publicdomainrelay/socialweb-computer-k8s-api";
import { assertMarketIdentityStatus, ATPROTO_RECORD_WATCH_RESOURCE, MARKET_IDENTITY_RESOURCE, parseMarketIdentity } from "@publicdomainrelay/socialweb-computer-k8s-api";
import type { IdentityAnswers, IdentityPrompt } from "@publicdomainrelay/identity-prompt-abc";
import { refuseSecretMaterial, specWithAnswer } from "@publicdomainrelay/identity-prompt-abc";
import { type KcpClient, kcpResourceClient } from "@publicdomainrelay/kcp-client-abc";
import { isConflict } from "@publicdomainrelay/kcp-client-common";
import { createKcpRestClient } from "@publicdomainrelay/kcp-client-rest";

export interface KcpIdentityPromptOptions {
  kcp?: KcpClient;
  server: string;
  workspace: string;
  token?: string;
  tokenFile?: string;
  caCert?: string;
  fetch?: typeof fetch;
  timeoutMs?: number;
  conflictRetries?: number;
}

const DEFAULT_CONFLICT_RETRIES = 5;

export function createKcpIdentityPrompt(opts: KcpIdentityPromptOptions): IdentityPrompt & Required<Pick<IdentityPrompt, "associationWatch">> & IdentityAnswers {
  const kcp = opts.kcp ??
    createKcpRestClient(
      { server: opts.server, token: opts.token, tokenFile: opts.tokenFile, caCert: opts.caCert },
      { fetch: opts.fetch, timeoutMs: opts.timeoutMs },
    );
  const identities = kcpResourceClient(kcp, { resource: MARKET_IDENTITY_RESOURCE, workspace: opts.workspace }, parseMarketIdentity);

  return {
    get: (name) => identities.get(name),
    associationWatch: async (name) => await kcp.get({ resource: ATPROTO_RECORD_WATCH_RESOURCE, workspace: opts.workspace }, name) as AtprotoRecordWatch | null,
    async writeStatus(name, status: MarketIdentityStatus) {
      refuseSecretMaterial(name, "status", status);
      assertMarketIdentityStatus(status);
      await identities.replaceStatus(name, status);
    },
    async answerAssociation(name, callerDid, answer) {
      const retries = opts.conflictRetries ?? DEFAULT_CONFLICT_RETRIES;
      for (let attempt = 0;; attempt++) {
        const obj = await identities.get(name);
        if (!obj?.spec) throw new Error(`marketidentity ${name} not found`);
        const spec = specWithAnswer(obj.spec, callerDid, answer);
        refuseSecretMaterial(name, "spec", spec);
        try {
          await identities.patch(name, {
            type: "merge",
            body: {
              metadata: { resourceVersion: obj.metadata?.resourceVersion },
              spec: {
                approvedAssociations: spec.approvedAssociations ?? null,
                rejectedAssociations: spec.rejectedAssociations ?? null,
              },
            },
          });
          return;
        } catch (err) {
          if (isConflict(err) && attempt < retries) continue;
          throw err;
        }
      }
    },
  };
}
