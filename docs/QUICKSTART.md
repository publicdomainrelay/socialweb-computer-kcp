# QUICKSTART: a kcp, an Infra, and every kind of compute in it

The quickstart is two pages. Both apply the same Infra / EphemeralInfra objects ([../examples/infra/](../examples/infra/)).

| page | for | what it covers |
|---|---|---|
| [quickstart-local.md](quickstart-local.md) | your own market network on one Linux host, nothing on prod | build, `dev up`, kubectl, the requester app, vouches as kcp objects, the agent bundle; VM over iroh, tenant k8s node + pod, Deno sandbox, agent VM up to the mint; release, `dev down` (all proven by acceptance [0012](acceptance/0012-infra-quickstart.md)) |
| [quickstart-prod.md](quickstart-prod.md) | prod bidders with a real atproto account | vouches as records in people's repos, the browser page kubectl terminal (page-rc142), the CLI per contract (`run request`), the atirc agent join, tenant clusters through the prod gateway, and the long-lived requester (`run requester`, `install requester`, tenant clusters with `--with-gateway`; plan [0039](plans/0039-long-lived-requester.yaml)) |

The gaps both pages list ("Not Supported ... Today") are the items of plan
[0039-long-lived-requester.yaml](plans/0039-long-lived-requester.yaml).
