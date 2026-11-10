# lexicons-market: one row per lexicon

- reference rows for every lexicon under `lexicons/com/publicdomainrelay/temp/` except `cluster.*` ([infra.md](infra.md#compublicdomainrelaytempcluster)) and `badgeBlueKeys` ([identity.md](identity.md#badgebluekeys)), plus the two NSIDs here that have no lexicon file
- the flows: [market.md](market.md) (rfp to release), [guest-events.md](guest-events.md) (events under a receipt); steps S3 to S9: [README.md](README.md#end-to-end-sequence)
- headings drop the prefix `com.publicdomainrelay.temp.`; file paths are under `lexicons/com/publicdomainrelay/temp/`
- the Go types are generated from these files into `internal/atproto/market/zz_generated.lexicons.go` (`go generate` in `internal/atproto/market/nsid.go`)
- "defined, not written": the lexicon and its NSID constant exist, but no Go code in this repo writes the record

## Columns

| column | meaning |
|---|---|
| type | lexicon `defs.main.type`: record, procedure, query, subscription |
| key fields | required fields first (`*`), then the fields the code reads |
| rkey | what the code writes, not only the lexicon `key` |
| written by / read by | role and the Go function |
| step | the README step it belongs to |

## Coverage

| file | section | done |
|---|---|---|
| `market/rfp.json` | [market.rfp](#marketrfp) | x |
| `market/bid.json` | [market.bid](#marketbid) | x |
| `market/bids/free.json` | [market.bids.free](#marketbidsfree) | x |
| `market/bids/x402.json` | [market.bids.x402](#marketbidsx402) | x |
| `market/accept.json` | [market.accept](#marketaccept) | x |
| `market/accepts/free.json` | [market.accepts.free](#marketacceptsfree) | x |
| `market/accepts/x402.json` | [market.accepts.x402](#marketacceptsx402) | x |
| `market/receipt.json` | [market.receipt](#marketreceipt) | x |
| `market/receipts/free.json` | [market.receipts.free](#marketreceiptsfree) | x |
| `market/receipts/x402.json` | [market.receipts.x402](#marketreceiptsx402) | x |
| `market/event.json` | [market.event](#marketevent) | x |
| `market/offering.json` | [market.offering](#marketoffering) | x |
| `market/heartbeat.json` | [market.heartbeat](#marketheartbeat) | x |
| `market/relays.json` | [market.relays](#marketrelays) | x |
| `market/association.json` | [market.association](#marketassociation) | x |
| `market/associationRequest.json` | [market.associationRequest](#marketassociationrequest) | x |
| `computer/socialweb/peer.json` | [computer.socialweb.peer](#computersocialwebpeer) | x |
| `market/submitRfp.json` | [market.submitRfp](#marketsubmitrfp) | x |
| `market/submitBid.json` | [market.submitBid](#marketsubmitbid) | x |
| `market/submitAccept.json` | [market.submitAccept](#marketsubmitaccept) | x |
| `market/submitEvent.json` | [market.submitEvent](#marketsubmitevent) | x |
| `compute/vm.json` | [compute.vm](#computevm) | x |
| `compute/events/vm/delete.json` | [compute.events.vm.delete](#computeeventsvmdelete) | x |
| `compute/events/vm/hostKeys.json` | [compute.events.vm.hostKeys](#computeeventsvmhostkeys) | x |
| `compute/events/vm/onNetwork.json` | [compute.events.vm.onNetwork](#computeeventsvmonnetwork) | x |
| `compute/events/vm/registerIdentity.json` | [compute.events.vm.registerIdentity](#computeeventsvmregisteridentity) | x |
| `compute/events/vm/sandbox.json` | [compute.events.vm.sandbox](#computeeventsvmsandbox) | x |
| `compute/events/vm/started.json` | [compute.events.vm.started](#computeeventsvmstarted) | x |
| `compute/events/vm/terminated.json` | [compute.events.vm.terminated](#computeeventsvmterminated) | x |
| `compute/events/guest/publicKey.json` | [compute.events.guest.publicKey](#computeeventsguestpublickey) | x |
| `compute/events/guest/signalWebRTC.json` | [compute.events.guest.signalWebRTC](#computeeventsguestsignalwebrtc) | x |
| `compute/events/guest/secrets.json` | [compute.events.guest.secrets](#computeeventsguestsecrets) | x |
| `compute/events/guest/secretsRequested.json` | [compute.events.guest.secretsRequested](#computeeventsguestsecretsrequested) | x |
| `compute/events/guest/secretsApplied.json` | [compute.events.guest.secretsApplied](#computeeventsguestsecretsapplied) | x |
| `compute/events/guest/secretsUpdated.json` | [compute.events.guest.secretsUpdated](#computeeventsguestsecretsupdated) | x |
| `compute/events/guest/agentStart.json` | [compute.events.guest.agentStart](#computeeventsguestagentstart) | x |
| `compute/events/guest/requesterDid.json` | [compute.events.guest.requesterDid](#computeeventsguestrequesterdid) | x |
| `compute/events/guest/r2Start.json` | [compute.events.guest.r2Start](#computeeventsguestr2start) | x |
| `compute/events/guest/r2Release.json` | [compute.events.guest.r2Release](#computeeventsguestr2release) | x |
| `compute/events/guest/r2Released.json` | [compute.events.guest.r2Released](#computeeventsguestr2released) | x |
| `compute/guest/subscribeEvents.json` | [compute.guest.subscribeEvents](#computeguestsubscribeevents) | x |
| `ingress/webRTC.json` | [ingress.webRTC](#ingresswebrtc) | x |
| no file (`SecretsNSID`) | [compute.secrets.getSecrets](#computesecretsgetsecrets) | x |
| no file (`WIFSimpleNSID`) | [compute.config.wif.simple](#computeconfigwifsimple) | x |

## Summary

| NSID tail | type | rkey written | written by | step |
|---|---|---|---|---|
| `market.rfp` | record | TID | requester | S3 |
| `market.bid` | record | `market.BidRKey` | bidder | S4 |
| `market.bids.free` | record | `<bid rkey>-payload` | bidder | S4 |
| `market.bids.x402` | record | none | defined, not written | S4 |
| `market.accept` | record | `SessionRKey("accept", ...)` | requester | S5 |
| `market.accepts.free`, `market.accepts.x402` | record | none | defined, not written | S5 |
| `market.receipt` | record | `market.ReceiptRKey` | bidder | S6 |
| `market.receipts.free`, `market.receipts.x402` | record | none | defined, not written | S6 |
| `market.event` | record | per kind | both | S7-S9 |
| `market.offering` | record | `self` | bidder | S3 (discovery) |
| `market.heartbeat` | record | `self` (literal) | bidder | S3 (discovery) |
| `market.relays` | record | `self` | no Go writer here | S3 (discovery) |
| `market.association` | record | `market.ContractScopedRKey` | requester | S5, S8 |
| `market.associationRequest` | record | `market.ContractScopedRKey` | guest requester | S8 |
| `computer.socialweb.peer` | record | the subhandle | guest requester (its own and each co-located account's) | S8 |
| `market.submitRfp` | procedure | - | served by bidder | S3 |
| `market.submitBid` | procedure | - | served by requester | S4 |
| `market.submitAccept` | procedure | - | served by bidder | S5 |
| `market.submitEvent` | procedure | - | served by both | S7-S9 |
| `compute.vm` | record | TID | requester | S3 |
| `compute.events.vm.*` | record | per kind | bidder (vm.delete: requester) | S7, S9 |
| `compute.events.guest.*` | record | per kind | requester or bidder for the guest | S8 |
| `compute.guest.subscribeEvents` | subscription | - | served by bidder GuestControlPlane | S8 |
| `ingress.webRTC` | record | `webrtc` (listen), TID (offer, answer) | requester, bidder | S3-S6 |
| `compute.secrets.getSecrets` | query (route, no file) | - | served by requester | S8 |
| `compute.config.wif.simple` | record (no file) | `<bid rkey>-config` | bidder | S4 |

## market.rfp

| item | value |
|---|---|
| file | `market/rfp.json` |
| type | record, key any |
| key fields | `payload`* (strongRef -> `compute.vm`), `signatures`*, `domain` (`compute`), `submitBid`, `ingress[]`, `policies[]`, `createdAt` |
| rkey | TID (`market.CreateRFP` passes no rkey) |
| written by | requester: `contractRunner` in `internal/market/requester/runner.go` via `market.CreateRFP` (`internal/atproto/market/records.go`) |
| read by | bidder: stream or `submitRfp`, then `getRecord` (`internal/market/bidder/submit.go` `SubmitRFP`, `acceptContract` in `internal/market/bidder/adapters.go`); bid window from `createdAt` (`internal/atproto/market/rfpwindow.go`) |
| step | [S3](README.md#step-s3) |

## market.bid

| item | value |
|---|---|
| file | `market/bid.json` |
| type | record, key any |
| key fields | `rfp`*, `payload`* (-> `market.bids.free`), `signatures`*, `config` (-> `compute.config.wif.simple`), `submitAccept`, `ingress[]`, `createdAt` |
| rkey | `market.BidRKey(bidderDID, rfpURI, rfpCID)` = `bid-<20 hex>`; also the Bid object name |
| written by | bidder: `writeBidRecord` in `internal/market/bidder/bids.go` (`market.BuildBid`, `market.SignRecord`) |
| read by | requester: `submitBid` (`Requester.SubmitBid` in `internal/market/requester/callbacks.go`), stream, catch-up `getRecord` at `BidRKey` (`internal/market/requester/bidcatchup.go`); bidder at accept time |
| step | [S4](README.md#step-s4) |

## market.bids.free

| item | value |
|---|---|
| file | `market/bids/free.json` |
| type | record, key any |
| key fields | `reason`* (`free`), `signatures`*, `cost` (0), `createdAt` |
| rkey | `<bid rkey>-payload` |
| written by | bidder: `writeBidRecord` |
| read by | requester: free detection for `firstFree` (`internal/market/requester/accept.go`, `internal/market/requester/inbox.go`) |
| step | [S4](README.md#step-s4) |

## market.bids.x402

| item | value |
|---|---|
| file | `market/bids/x402.json` |
| type | record, key any |
| key fields | `cost`*, `currency`*, `frequency`*, `prepay`*, `url`*, `signatures`* |
| rkey | none: defined, not written |
| written by | no Go writer |
| read by | requester reads `cost` when a bid names one (`internal/market/requester/accept.go`, `internal/market/requester/inbox.go`) |
| step | [S4](README.md#step-s4) |

## market.accept

| item | value |
|---|---|
| file | `market/accept.json` |
| type | record, key any |
| key fields | `rfp`*, `bid`*, `signatures`*, `payload` (unset by the code), `submitEvent`, `ingress[]`, `createdAt` |
| rkey | `acceptRKeyFor` -> `market.SessionRKey("accept", repoDID, contract, rfpURI, rfpCID, bidURI, bidCID)` (`internal/market/requester/secrets.go`) |
| written by | requester: `market.CreateAcceptAt` from `runner.go` |
| read by | bidder: `submitAccept` or stream -> provisioning StateSync (`SubmitAccept` in `internal/market/bidder/submit.go`, `acceptContract`); the receipt `cid` binds it |
| step | [S5](README.md#step-s5) |

## market.accepts.free

| item | value |
|---|---|
| file | `market/accepts/free.json` |
| type | record, key any |
| key fields | `bid`*, `signatures`*, `payload`, `createdAt` |
| rkey | none: defined, not written |
| written by | no Go writer (`AcceptsFreeNSID` constant only) |
| read by | none |
| step | [S5](README.md#step-s5) |

## market.accepts.x402

| item | value |
|---|---|
| file | `market/accepts/x402.json` |
| type | record, key any |
| key fields | `bid`*, `signatures`*, `payload` (the payment), `createdAt` |
| rkey | none: defined, not written |
| written by | no Go writer (`AcceptsX402NSID` constant only) |
| read by | none |
| step | [S5](README.md#step-s5) |

## market.receipt

| item | value |
|---|---|
| file | `market/receipt.json` |
| type | record, key any |
| key fields | `rfp`*, `bid`*, `accept`*, `cid`*, `signatures`*, `payload` (-> the RFP's `compute.vm`; `null` when none), `submitEvent` (copied from the accept when the bidder serves inbound XRPC), `ingress[]` |
| rkey | `market.ReceiptRKey(acceptURI, acceptCID)` = `receipt-<32 hex>` |
| written by | bidder: `createReceipt` in `internal/market/bidder/submit.go` -> `market.CreateReceiptRecordAt` (`internal/atproto/market/attest.go`) |
| read by | requester: `FindReceipt` (one `getRecord`, `internal/market/requester/market.go`), `market.VerifyReceipt` in `acceptReceipt`; policy trust (`internal/policyengine/trust/association.go`) |
| step | [S6](README.md#step-s6) |

## market.receipts.free

| item | value |
|---|---|
| file | `market/receipts/free.json` |
| type | record, key any |
| key fields | `accept`*, `cid`*, `signatures`*, `createdAt` |
| rkey | none: defined, not written |
| written by | no Go writer (`ReceiptsFreeNSID` constant; named in OAuth scopes, `internal/atproto/oauth/scope.go`) |
| read by | none |
| step | [S6](README.md#step-s6) |

## market.receipts.x402

| item | value |
|---|---|
| file | `market/receipts/x402.json` |
| type | record, key any |
| key fields | `accept`*, `cid`*, `signatures`*, `createdAt` |
| rkey | none: defined, not written |
| written by | no Go writer (`ReceiptsX402NSID` constant; named in OAuth scopes) |
| read by | none |
| step | [S6](README.md#step-s6) |

## market.event

| item | value |
|---|---|
| file | `market/event.json` |
| type | record, key any |
| key fields | `receipt`*, `payload`* (-> a `compute.events.*` record in the same repo), `signatures`*, `createdAt` |
| rkey | bidder: `market.EventRKeys(key)` second value `<prefix>-event-<32 hex>`; requester: `SessionRKey("event", receiptURI)` for vm.delete, `SessionRKey("contract-event-envelope", uid)` for ContractEvents |
| written by | bidder event adapter (`writeMarketEvent` in `internal/market/bidder/adapters.go`); requester `destroyEnvelope` (`runner.go`), `publishContractEvent` (`internal/market/requester/contractevents.go`); both via `market.PutEvent` |
| read by | the other party: `submitEvent` or stream; requester `FindTermination` |
| step | [S7](README.md#step-s7), [S8](README.md#step-s8), [S9](README.md#step-s9) |

## market.offering

| item | value |
|---|---|
| file | `market/offering.json` |
| type | record, key any (writers use `self`) |
| key fields | `appliesTo`* (holds `compute.vm`), `createdAt`*, `endpointUrl`, `ingress[]`, `refreshedAt`, `withdrawnAt` |
| rkey | `self` (`market.OfferingRKey`) |
| written by | bidder: `putOffering`, `withdraw` in `internal/market/bidder/offering.go` |
| read by | requester: `market.ReadOffering` (`self`, else the rkey the heartbeat names), `market.OfferingApplies`; relays list its repos by `listReposByCollection` |
| step | [S3](README.md#step-s3) (discovery) |

## market.heartbeat

| item | value |
|---|---|
| file | `market/heartbeat.json` |
| type | record, key `literal:self` |
| key fields | `offering`* (at-uri of the offering in the same repo), `createdAt`* |
| rkey | `self` |
| written by | bidder: `beat` in `internal/market/bidder/offering.go`, every `HeartbeatInterval` (60 s) |
| read by | requester: `market.HeartbeatCache.FreshDIDs` (fresh = at most `HeartbeatTTL` 300 s old); `internal/marketpresence` |
| step | [S3](README.md#step-s3) (discovery) |

## market.relays

| item | value |
|---|---|
| file | `market/relays.json` |
| type | record, key any (writers use `self`, readers read only `self`) |
| key fields | `relays`* (HTTP(S) relay base URLs), `createdAt`* |
| rkey | `self` (`market.RelaysRKey`) |
| written by | no Go writer in this repo |
| read by | requester discovery: `market.RelayURLsOf` from `relaySources` in `internal/market/requester/discovery.go` |
| step | [S3](README.md#step-s3) (discovery) |

## market.association

| item | value |
|---|---|
| file | `market/association.json` |
| type | record, key any |
| key fields | `subject`*, `service`* (`requester_associate`, `bidder_associate`), `createdAt`*, `contract` (-> `market.accept`, needs `expiresAt`), `expiresAt`, `parent`, `note` |
| rkey | standing: `<service>-<subject with : replaced by ->`; contract-scoped: `market.ContractScopedRKey(service, subject, acceptURI)` = `<service>-<32 hex of sha256(subject)>-<accept rkey>` (`ContractAssociationRKey` in `internal/market/requester/association.go`) |
| written by | requester (contract-scoped, `association.go`); standing ones: [identity.md](identity.md#associations-plan-0036); the guest requester for each co-located account (`internal/app/compose/peer.go`: subject the co-located DID, `contract` and `expiresAt` copied from its own association, `parent` its own association) |
| read by | bidder (`internal/market/bidder/association.go`, ComputeVM `spec.associationExpiresAt`: the earliest `expiresAt` of the chain, the contract an accept of a repo of the chain), policy trust (`internal/policyengine/trust/association.go`) |
| chain | a verifier follows `parent` (a strongRef to the market.association that names this record's repo) up to `maxDepth` links (AssociationPolicy `maxDepth`, 1 to 100, default 100); every link of a chain is contract-scoped, unexpired, names the next link's repo as its subject, and its contract is a Held `market.accept` of its own repo or of a repo above it; the operator is the top repo. Nothing relies on a cleanup: the owner's release (vm.delete), the bidder's vm.terminated or the first `expiresAt` ends the whole chain. `socialweb-computer internal association <did>` prints the verdict |
| step | [S5](README.md#step-s5), [S8](README.md#step-s8); release deletes only its own |

## market.associationRequest

| item | value |
|---|---|
| file | `market/associationRequest.json` |
| type | record, key any |
| key fields | `authority`*, `service`*, `contract`* (the authority's accept uri), `nonce`*, `createdAt`* |
| rkey | `market.ContractScopedRKey(service, authority, acceptURI)` |
| written by | the guest requester: an AtprotoRecord object from `internal/app/compose/records.go` |
| read by | the authority requester: one `getRecord` in the DID the guest reported (`internal/market/requester/association.go`); `cmd/socialweb-computer/requestassociation.go` |
| step | [S8](README.md#step-s8); flow: [identity.md](identity.md#associations-plan-0036) |

## computer.socialweb.peer

| item | value |
|---|---|
| file | `computer/socialweb/peer.json` |
| type | record, key any |
| key fields | `createdAt`*, `iroh` (short ticket), `webrtc` (the DID answering ingress offers), `channels`, `lease` (-> the market.association naming the account) |
| rkey | the subhandle: the guest requester's atirc subhandle (`r2-<accept rkey>.<owner handle>`, its handle without one); `<name>.<that>` for a co-located account |
| written by | the guest requester (`run request --guest-requester [--colocated a,b]`): AtprotoRecord objects of `internal/app/compose/peer.go` in its own repo and each co-located account's, present only while its association ref verifies |
| read by | mesh peers: one `getRecord(DID, computer.socialweb.peer, subhandle)`, never listRecords; trusted only while `lease` verifies as a chain |
| release | the guest requester's `run request release` sets `spec.peer.retire`: the peer, association and keys-mirror records are deleted, each local account deactivated and its did:plc tombstoned (best effort; expiry covers a skipped cleanup) |

## market.submitRfp

| item | value |
|---|---|
| file | `market/submitRfp.json` |
| type | procedure, service auth `lxm` = the NSID, issuer = requester |
| input | `{rfpUri, rfpCid, ingress?}` (ingress = `ingress.webRTC` offers) |
| output | `{ok, name, phase, ingress?}`; errors `InvalidRequest`, `Unauthorized`, `Forbidden`, `Unavailable`; the code also answers 409 `RFPClosed` |
| served by | bidder: `handleSubmitRFP` in `internal/services/bidder/routes.go` -> `Bidder.SubmitRFP` |
| called by | requester: `solicitBids` -> `HTTPMarket.SubmitRFP` (`internal/market/requester/runner.go`, `market.go`) |
| step | [S3](README.md#step-s3) |

## market.submitBid

| item | value |
|---|---|
| file | `market/submitBid.json` |
| type | procedure, issuer = bidder |
| input | `{uri, cid, record}` of the `market.bid`, `ingress?` |
| output | `{ok, ingress?}`; errors `InvalidRequest`, `Unauthorized`, `Forbidden`, `Unavailable` |
| served by | requester ingress: `internal/services/requester/routes.go` -> `Requester.SubmitBid`; the bidder route only parses it |
| called by | bidder: `submitBid` in `internal/market/bidder/submit.go` to the RFP's `submitBid` |
| step | [S4](README.md#step-s4) |

## market.submitAccept

| item | value |
|---|---|
| file | `market/submitAccept.json` |
| type | procedure, issuer = requester |
| input | `{acceptUri, acceptCid, ingress?}` |
| output | 202 `{ok, id, acceptUri, acceptCid}`; lexicon also has `uri`, `cid`, `submitEvent`, `ingress`; errors add `InternalError` |
| served by | bidder: `handleSubmitAccept` -> `Bidder.SubmitAccept` (provisioning StateSync) |
| called by | requester: `submitAcceptAndWait` (`internal/market/requester/accept.go`) |
| step | [S5](README.md#step-s5) |

## market.submitEvent

| item | value |
|---|---|
| file | `market/submitEvent.json` |
| type | procedure, issuer = the accept author (to a bidder) or the receipt author (to a requester) |
| input | `{uri, cid, record}` of the `market.event`, `ingress?` |
| output | `{ok, ingress?}`; 403 `Forbidden` "not the accept author" |
| served by | bidder `handleSubmitEvent` -> `Bidder.SubmitEvent`; requester `Requester.SubmitEvent` (`callbacks.go`) |
| called by | requester `releaseContract`, `publishContractEvent`; bidder event adapter (`internal/market/bidder/adapters.go`) |
| step | [S7](README.md#step-s7), [S8](README.md#step-s8), [S9](README.md#step-s9) |

## compute.vm

| item | value |
|---|---|
| file | `compute/vm.json` |
| type | record, key any |
| key fields | `role`* (VM name in the contract), `kind` (`vm` default, `deno`), `cpus` (2), `mem` (`4G`), `disk` (`50G`), `network`, `location`, `user_data`, `createdAt` |
| rkey | TID (`CreateRecord`) |
| written by | requester: `runner.go` with `DefaultComputeCPUs`, `DefaultComputeMem`, `DefaultComputeDisk` (`internal/market/requester/requester.go`); `user_data` from `cloudinit.BuildUserData` (`internal/atproto/cloudinit`) |
| read by | bidder through `rfp.payload`: kind check (`rfpKind` in `submit.go`), boot (`acceptContract`, `internal/guestseed` adds the binaries unit) |
| step | [S3](README.md#step-s3) |

## compute.events.vm.delete

| item | value |
|---|---|
| file | `compute/events/vm/delete.json` |
| type | record, key any |
| key fields | `reason`* (`released`), `createdAt`*, `signatures` |
| rkey | `market.ReleasePayloadRKey(receiptURI)` = `SessionRKey("delete", receiptURI)` |
| written by | requester: `destroyEnvelope` in `internal/market/requester/runner.go` |
| read by | bidder: `verifyInboundDelete`, `applyRelease` (`internal/market/bidder/adapters.go`); policy trust |
| step | [S9](README.md#step-s9) |

## compute.events.vm.hostKeys

| item | value |
|---|---|
| file | `compute/events/vm/hostKeys.json` |
| type | record, key any; def `hostKey {type*, publicKey*, fingerprint*}` |
| key fields | `keys[]`*, `observedAt`*, `createdAt`* |
| rkey | `EventRKeys("hostKeys:<receipt>")` -> `hostkeys-payload-<32 hex>` |
| written by | bidder: `report` in `internal/market/bidder/report.go` from the host key bound at login (`RecordBoundHostKey` in `internal/guestidentity/service.go`) |
| read by | requester: `waitGuest` -> `status.contract.hostKeys` |
| step | [S7](README.md#step-s7) |

## compute.events.vm.onNetwork

| item | value |
|---|---|
| file | `compute/events/vm/onNetwork.json` |
| type | record, key any |
| key fields | `createdAt`*, `kind` (`iroh`, `https`, `webrtc`, `sandbox`), `address`, `tag` |
| rkey | `EventRKeys("onNetwork:<receipt>")` or `EventRKeys("onNetwork:<tag>:<receipt>")` |
| written by | bidder: `report` (`internal/market/bidder/report.go`) from the guest POST (`Guest.onNetwork` in `internal/guestidentity/service.go`); sandbox: `internal/market/bidder/sandbox.go` |
| read by | requester: `waitGuest` -> `status.contract.vmAddress`, `status.contract.networks` |
| step | [S7](README.md#step-s7) |

## compute.events.vm.registerIdentity

| item | value |
|---|---|
| file | `compute/events/vm/registerIdentity.json` |
| type | record, key any |
| key fields | `createdAt`* |
| rkey | none: defined, not written |
| written by | no Go writer (`ComputeEventsVMRegisterIdentityNSID` constant only) |
| read by | none |
| step | [S7](README.md#step-s7) |

## compute.events.vm.sandbox

| item | value |
|---|---|
| file | `compute/events/vm/sandbox.json` |
| type | record, key any |
| key fields | `exitCode`* (-1 = killed), `createdAt`*, `timedOut`, `stdout`, `stderr` (capped) |
| rkey | `EventRKeys("<nsid>:<receipt>")` |
| written by | bidder: `internal/market/bidder/sandbox.go` (`compute.vm.kind: deno`) |
| read by | requester: `status.contract.sandbox` (`internal/market/requester/events.go`, `runner.go`) |
| step | [S7](README.md#step-s7) |

## compute.events.vm.started

| item | value |
|---|---|
| file | `compute/events/vm/started.json` |
| type | record, key any |
| key fields | `createdAt`*, `observedAt` |
| rkey | none: defined, not written |
| written by | no Go writer (`ComputeEventsVMStartedNSID` constant only) |
| read by | none |
| step | [S7](README.md#step-s7) |

## compute.events.vm.terminated

| item | value |
|---|---|
| file | `compute/events/vm/terminated.json` |
| type | record, key any |
| key fields | `reason`* (`market.VMTerminatedReasons`), `observedAt`*, `createdAt`*, `exitCode`, `request` (-> the `vm.delete`) |
| rkey | `EventRKeys(TerminationKey(receiptURI))` -> `termination-payload-<32 hex>` |
| written by | bidder: termination StateSync (`internal/computevm/termination.go`, `internal/controllers/computevm/controller.go`), adapter `atproto.market.event` |
| read by | requester `FindTermination` (`market.go`); atirc via the token lease (`contractLease` in `internal/market/requester/atirc.go`); policy trust |
| step | [S9](README.md#step-s9) |

## compute.events.guest.publicKey

| item | value |
|---|---|
| file | `compute/events/guest/publicKey.json` |
| type | record, key any |
| key fields | `kind`* (`age-x25519`), `tag`*, `key`* (age1...), `createdAt`* |
| rkey | requester: `SessionRKey("contract-event", uid)`; bidder: `EventRKeys("guest:...")` |
| written by | requester (outbound ContractEvent, `contractevents.go`); bidder for the guest (`PublishGuestEvent` in `internal/market/bidder/guestevents.go`) |
| read by | guest agent (`internal/guestagent/subscribe.go`); requester (inbound ContractEvent) |
| step | [S8](README.md#step-s8) |

## compute.events.guest.signalWebRTC

| item | value |
|---|---|
| file | `compute/events/guest/signalWebRTC.json` |
| type | record, key any |
| key fields | `kind`* (`offer`, `answer`), `tag`*, `ciphertext`* (age armored `{sdp, replyTo}`), `createdAt`* |
| rkey | as `guest.publicKey` |
| written by | requester (CLI / browser ContractEvent, `internal/stack/request/webrtc.go`); bidder for the guest |
| read by | guest agent (`internal/guestagent/session.go`, `subscribe.go`); requester (inbound ContractEvent) |
| step | [S8](README.md#step-s8) |

## compute.events.guest.secrets

| item | value |
|---|---|
| file | `compute/events/guest/secrets.json` |
| type | record, key any |
| key fields | `ciphertext`* (age armored bundle, at most 65536 bytes), `createdAt`* |
| rkey | `SessionRKey("contract-event", uid)` of ContractEvent `secrets-<contract>` |
| written by | requester only, on demand: `sealGuestSecrets` in `internal/market/requester/guestsecrets.go` |
| read by | guest agent (`internal/guestagent/secrets.go`); the bidder forwards it unopened |
| step | [S8](README.md#step-s8) |

## compute.events.guest.secretsRequested

| item | value |
|---|---|
| file | `compute/events/guest/secretsRequested.json` |
| type | record, key any |
| key fields | `createdAt`* |
| rkey | bidder `EventRKeys("guest:...")` |
| written by | bidder for the guest agent (`secretsFallback: Sealed` only) |
| read by | requester: `status.contract.secretsRequested`, then `sealGuestSecrets` |
| step | [S8](README.md#step-s8) |

## compute.events.guest.secretsApplied

| item | value |
|---|---|
| file | `compute/events/guest/secretsApplied.json` |
| type | record, key any |
| key fields | `via`* (`getSecrets`, `sealed`), `createdAt`*, `count` |
| rkey | bidder `EventRKeys("guest:...")` |
| written by | bidder for the guest agent |
| read by | requester: `status.contract.secretsApplied`, condition `SecretsApplied` |
| step | [S8](README.md#step-s8) |

## compute.events.guest.secretsUpdated

| item | value |
|---|---|
| file | `compute/events/guest/secretsUpdated.json` |
| type | record, key any |
| key fields | `createdAt`* |
| rkey | `SessionRKey("contract-event", uid)` of ContractEvent `secrets-updated-<SessionRKey("secretsUpdated", contract, ref)>` |
| written by | requester only: `notifySecretsUpdated` (`internal/market/requester/association.go`), every fresh atirc mint |
| read by | guest agent: fetch the bundle again |
| step | [S8](README.md#step-s8) |

## compute.events.guest.agentStart

| item | value |
|---|---|
| file | `compute/events/guest/agentStart.json` |
| type | record, key any |
| key fields | `createdAt`* |
| rkey | `SessionRKey("contract-event", uid)` |
| written by | requester only (`run request agent <name>` -> ContractEvent; needs `spec.agent`) |
| read by | guest agent: `systemctl start --no-block atirc-agent.service` (`internal/guestagent/agentstart.go`) |
| step | [S8](README.md#step-s8) |

## compute.events.guest.requesterDid

| item | value |
|---|---|
| file | `compute/events/guest/requesterDid.json` |
| type | record, key any |
| key fields | `did`* (did:plc only), `createdAt`* |
| rkey | bidder `EventRKeys("guest:...")` |
| written by | bidder for the guest agent (`internal/guestagent/association.go`) |
| read by | requester: `status.contract.guestRequester` (`internal/market/requester/association.go`, `market.ValidGuestRequesterDID`) |
| step | [S8](README.md#step-s8) |

## compute.events.guest.r2Start

| item | value |
|---|---|
| file | `compute/events/guest/r2Start.json` |
| type | record, key any |
| key fields | `name`* (DNS label), `createdAt`*, `agent`, `hold` |
| rkey | `SessionRKey("contract-event", uid)` |
| written by | requester only (`run request r2 <name>`; needs `spec.association.fromGuest`) |
| read by | guest agent: starts `socialweb-r2.service` (`internal/guestagent/association.go`) |
| step | [S8](README.md#step-s8) |

## compute.events.guest.r2Release

| item | value |
|---|---|
| file | `compute/events/guest/r2Release.json` |
| type | record, key any |
| key fields | `createdAt`* |
| rkey | `SessionRKey("contract-event", uid)` |
| written by | requester only: `releaseGuestRequester` (`internal/market/requester/guestrequester.go`) |
| read by | guest agent: `socialweb-r2-release.service` |
| step | [S9](README.md#step-s9) |

## compute.events.guest.r2Released

| item | value |
|---|---|
| file | `compute/events/guest/r2Released.json` |
| type | record, key any |
| key fields | `ok`*, `createdAt`*, `message` |
| rkey | bidder `EventRKeys("guest:...")` |
| written by | bidder for the guest agent |
| read by | requester: `status.contract.r2Release`, condition GuestRequesterReleased |
| step | [S9](README.md#step-s9) |

## compute.guest.subscribeEvents

| item | value |
|---|---|
| file | `compute/guest/subscribeEvents.json` |
| type | subscription; param `cursor`; message `#event {seq*, uri*, cid*, record*}`; error `FutureCursor` |
| served by | bidder GuestControlPlane: `handleSubscribeEvents` in `internal/services/guestcontrolplane/subscribe.go`, queue `internal/guestcp/queue` |
| called by | guest agent: `internal/guestagent/subscribe.go` (bearer = workload identity token) |
| carries | requester-authored `compute.events.guest.*` payloads, in order; keepalive every 25 s |
| step | [S8](README.md#step-s8) |

## ingress.webRTC

| item | value |
|---|---|
| file | `ingress/webRTC.json` |
| type | record, key any |
| key fields | `kind`* (`webrtc`), `mode`* (`listen`, `offer`, `answer`), `id`* (hex sha256 of the requester repo DID), `createdAt`*, `peer`, `session`, `descriptor` (-> the listen record), `sdp`, `stunUrls` |
| rkey | requester listen: `webrtc` (`ingressDescriptor` in `internal/market/requester/requester.go`); offer: TID (`createIngressOffer` in `internal/market/requester/market.go`); bidder answer: TID (`AnswerIngressRecords` in `internal/market/bidder/submit.go`) |
| written by | requester (listen, offer), bidder (answer) |
| read by | the other side through `ingress[]` on rfp, bid, accept, receipt, offering and the submit calls; only refs to this NSID are allowed (`validateIngressList` in `internal/atproto/market/records.go`) |
| step | [S3](README.md#step-s3) to [S6](README.md#step-s6) |

## compute.secrets.getSecrets

| item | value |
|---|---|
| file | none. `SecretsNSID` = `com.publicdomainrelay.temp.compute.secrets`, `SecretsRoute` in `internal/market/requester/secrets.go` |
| type | query-shaped HTTP route: `GET /xrpc/com.publicdomainrelay.temp.compute.secrets.getSecrets/<contract>` |
| auth | `Authorization: Bearer <workload identity token>`; audience `api://ATProto?actx=<requester DID>`; subject claim must match the contract's grant |
| output | JSON array `[{path, value, mode?}]`; 401 `AuthenticationRequired`, `InvalidToken`; 503 `NotReady` |
| served by | requester ingress: `internal/services/requester/routes.go` -> `Requester.Secrets` |
| called by | guest `setup-secrets.service` (`internal/atproto/cloudinit`) |
| step | [S8](README.md#step-s8); fallback: [guest-events.md](guest-events.md#secrets-getsecrets-or-sealed-fallback) |

## compute.config.wif.simple

| item | value |
|---|---|
| file | none. `WIFSimpleNSID` in `internal/guestidentity/bidconfig.go` |
| type | record (bid config) |
| key fields | `accept_path`, `issuer_uri`, `to_issue` (`openbao-identity-oidc`), `actx`, `token_path`, `url_path`, `url_route` (`/v1/identity/oidc/token`), `subject` (`actx:{actx}:plc:{did-plc-key}:role:{role}`), `createdAt` |
| rkey | `<bid rkey>-config` |
| written by | bidder: `writeBidRecord` via `guests.BidConfig` (`internal/services/bidder/guests.go`, `guestidentity.BidConfigFor`) |
| read by | requester: `resolveBidConfig` and `prepareWorkloadGrant` (`internal/market/requester/secrets.go`) to build the workload identity grant; bidder at accept time |
| step | [S4](README.md#step-s4) |
