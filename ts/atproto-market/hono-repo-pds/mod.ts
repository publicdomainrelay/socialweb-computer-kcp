import { Command } from "@publicdomainrelay/cli-args-env";
import { createLogger } from "@publicdomainrelay/logger";
import { marketNetwork } from "@publicdomainrelay/market-network-common";
import { createRepoPdsApp } from "@publicdomainrelay/hono-factory-repo-pds";
import cliArgsEnv from "./cli-args-env.json" with { type: "json" };

const { options } = await new Command("CONFIG_PATH_HONO_REPO_PDS", cliArgsEnv, null).resolve();
const o: Record<string, any> = options;

for (const required of ["kcpServer", "kcpWorkspace", "vaultOpenbaoServer", "vaultKcpServer", "storagePath", "plcDirectoryUrl", "ingressProxyHost"]) {
  if (!o[required]) {
    console.error(`hono-repo-pds needs --${required.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)}; it runs as the pds DenoPod of an application`);
    Deno.exit(2);
  }
}

const logger = createLogger({ serviceName: "pds" });
const app = createRepoPdsApp({
  network: marketNetwork({ plcDirectoryUrl: o.plcDirectoryUrl, ingressProxyHost: o.ingressProxyHost, hosts: o.networkHosts }),
  identityName: o.marketIdentityName,
  associateServiceId: o.associateServiceId,
  kcp: { server: o.kcpServer, workspace: o.kcpWorkspace, tokenFile: o.kcpTokenFile, caFile: o.kcpCaCertFile },
  vault: { server: o.vaultOpenbaoServer, client: o.vaultOpenbaoClient, clientDir: o.vaultOpenbaoClientDir, kcpServer: o.vaultKcpServer, mount: o.vaultMount, kvMountClient: o.vaultKvMountClient },
  storagePath: o.storagePath,
  serve: { addr: o.serveAddr, port: o.servePort },
  reports: { portFile: o.portFile, listenersFile: o.listenersFile, statusFile: o.statusFile },
}, { logger });

const shutdown = () => void app.shutdown().finally(() => Deno.exit());
Deno.addSignalListener("SIGINT", shutdown);
Deno.addSignalListener("SIGTERM", shutdown);

try {
  await app.start();
} catch (err) {
  logger.error("repo_pds_start_failed", { error: err instanceof Error ? err.message : String(err) });
  Deno.exit(1);
}
setInterval(() => logger.info("pds_memory", { ...Deno.memoryUsage() }), o.memoryLogMs as number);
