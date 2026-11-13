# atirc on a market network

Goal: `dev up --profile market-network` serves its own atirc, so `network.atircUrl` is set and 0002, 0004, 0011
run against staging.

## Shape

| piece | how |
| --- | --- |
| process | a HostProcess of the pinned atirc asset (`internal/versions/versions.json` atirc, de9e74a or later): `atirc serve --xrpc-port <lease> --plc-directory-url <network PLC>` |
| identity | its config `service_endpoint` = the local Exposure URL; DID `did:web:<host>%3A<port>`, audience `<did>#atirc` (requesters derive it from `atircUrl`, 6096eb65) |
| network value | `networkValueFromExposures` sets `AtircURL`; `hosts` gains `atirc.default.global.svc.kcp.local` |
| guests | an ingress Exposure through the network's dispatcher TLS (`*.relay.localhost`, the network CA), as the PDS ingress, so nested guests reach it without host loopback |

## Steps

1. `desired.go`: lease, HostProcess, local + ingress Exposures (the Plc/Relay pattern).
2. `networkValueFromExposures`: `AtircURL`, the hosts entry.
3. Proof on a local network: 0011 (token lifecycle), then 0004 (mesh), then 0002, as compositions on the VM.

## Open

- atirc's own did:web on a loopback host with a port: check atirc accepts it as its service DID.
- The guest agent's `ATIRC_ENDPOINT`: the ingress host, not loopback.
