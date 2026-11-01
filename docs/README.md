# docs: index

Start with SYSTEM, then KCP. Agents: read [../AGENTS.md](../AGENTS.md) first.

## Overview

| doc | what |
|---|---|
| [SYSTEM.md](SYSTEM.md) | the whole system: repos, hosts, apps, market protocol, guests, identity, ssh gateway, atirc, tenant clusters, records policy, deploy |
| [QUICKSTART.md](QUICKSTART.md) | index of the two quickstarts; examples in ../examples/infra |
| [quickstart-local.md](quickstart-local.md) | own market network: clone -> dev up -> kubectl apply Infra / EphemeralInfra: VM, tenant k8s, Deno sandbox, agent VM up to the mint |
| [quickstart-prod.md](quickstart-prod.md) | prod bidders: vouches, browser page kubectl, CLI per contract, atirc agent join, prod gateway clusters, the long-lived requester (`run requester`, `install requester`, `--with-gateway` tenant clusters; plan 0039) |
| [protocol/README.md](protocol/README.md) | the atproto protocol: every lexicon (NSID families), how and when each is used, the end-to-end sequence sign-in -> trust -> RFP -> guest -> agent -> release |
| [KCP.md](KCP.md) | how every app uses kcp: profiles, slots/front/kine, workspaces, exports, bindings, component runtime, controllers, full API catalog, schema revisions, apigen |
| [ARCHITECTURE.md](ARCHITECTURE.md) | decisions, measurements, invariants. open a section by heading; do not read end to end |

## Operate

| doc | what |
|---|---|
| [deploy.md](deploy.md) | release build, gateway deploy, bidder upgrade, rollback, canary |
| [gateway.md](gateway.md) | gateway deploy env, API front, caddy sites, post-deploy sign-in |
| [bidder.md](bidder.md) | bidder install, guest image, identity + association, upgrade, terminate a VM |
| [oauth.md](oauth.md) | scripted OAuth sign-in |
| [host-contract.md](host-contract.md), [host-setup.md](host-setup.md) | what a host must provide |
| [inspect.md](inspect.md) | reading live state |
| [infra/inventory.md](infra/inventory.md) | what runs where on the non-gateway hosts |
| [../deploy/market-network/README.md](../deploy/market-network/README.md) | the one-kcp market network for dev, CI, soak |

## Verify And Coordinate

| doc | what |
|---|---|
| [acceptance/](acceptance/) | acceptance runbooks (auth modes, ssh gateway, webrtc ssh, VM atirc agent, guest requester, ...) |
| [ATIRC-WORKFLOW.md](ATIRC-WORKFLOW.md) | how agents coordinate over atirc |
| [plans/](plans/) | plans and item status |
| [spec/](spec/) | integration catalog |
| [research/](research/) | research notes and measurements |
