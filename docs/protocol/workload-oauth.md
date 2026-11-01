# Workload OAuth for atproto PDSes (draft protocol note)

Status: draft for discussion (IETF atproto working group, implementers). One implementation:
socialweb-computer's PDS. NSIDs are `com.fedproxy.temp.*` until a standard names them.

## Problem

Software running without a person (a CI job, an agent in a VM, a service) often needs an atproto
OAuth session at a third-party app for an account it acts for. Today that means a person in a
browser, or a stored password or app password inside the workload. Workloads usually already
hold a short-lived OIDC identity token from their platform (GitHub Actions, a cloud's metadata
service, an OpenBao/Vault issuer). This note lets the account, by writing one record in its own
repo, let one such workload approve OAuth authorizations of named apps, with named scopes, on the
account's PDS.

## Actors

- **Account**: an atproto account; its PDS implements this note.
- **Workload**: holds JWTs from an OIDC issuer with a stable `sub`.
- **App**: any atproto OAuth client (PAR, PKCE, DPoP; RFC 9126, RFC 7636, RFC 9449).
- **PDS**: the account's authorization server and resource server.

## Grant record

Collection `com.fedproxy.rbac`, record key = role name. Exactly one role per record:

```
protects: {<role>: {service: <PDS service DID>, scope: "com.fedproxy.temp.oauth"}}
roles:    {<role>: {role_name: <role>, definition: {iss, aud: "<account DID>#role/<role>", sub, policies: [...]}}}
policies: {<name>: {meta, schemas: {<XRPC path or * glob>: <JSON Schema>}}}
```

A path schema's `properties.capability.enum` lists the allowed capabilities (HTTP method mapped:
GET read, POST create, PUT/PATCH update, DELETE delete); its optional `properties.body` is a JSON
Schema the request document must satisfy. Implementations must evaluate `body` and must refuse
any keyword outside the subset they support. The format is the one the fedproxy reverse proxy and
the compute-contract reference implementation use; this note adds only the rkey rule, the one-role
rule and the body evaluation.

Lookups are `getRecord(account, com.fedproxy.rbac, role)`: a PDS never enumerates the collection.
Deleting or changing the record revokes what it granted.

## Token exchange

`com.fedproxy.temp.oauth.exchangeToken` (procedure, DPoP proof required), input named as RFC 8693:
`subject_token` (the workload JWT), `subject_token_type`
`urn:ietf:params:oauth:token-type:jwt`, `audience` `<account DID>#role/<role>`,
`requested_token_type` `urn:ietf:params:oauth:token-type:access_token`.

The PDS:
1. parses the audience; the account must be active on this PDS;
2. reads the record; it must protect this PDS's service DID and scope;
3. fetches OIDC discovery and keys for the role's `iss` only (https, public address); verifies the
   signature, `iss`, `aud` = the role's `aud`, `exp`, `iat`, and `sub` = the role's `sub`; refuses a
   reused `jti`;
4. applies the role's policy for the exchangeToken path (capability create);
5. answers `{access_token, issued_token_type, token_type: "DPoP", expires_in}`: a JWT of typ
   `wid+jwt` with `iss` the PDS service DID, `sub` the account DID, `aud` the requested audience,
   `lxm` the approve NSID, `act {iss, sub}` (RFC 8693 actor) of the workload, `cnf.jkt` (RFC 9449)
   of the proof key, at most 5 minutes and never past the subject token.

A later version can be the RFC 8693 grant at the token endpoint with the same fields.

## Approval

`com.fedproxy.temp.oauth.approveAuthorization` (procedure), `Authorization: DPoP <wid+jwt>` with a
proof bound to it, input `{request_uri}` (a PAR request of this PDS). The PDS builds
`{client_id, redirect_uri, scope: [...]}` from the stored request, applies the role's policy for the
approve path (capability create, body = that document), consumes the request, issues the
authorization code exactly as its authorize page would, and answers `{redirect_uri}` with `code`,
`state` and `iss`. The `wid+jwt` is single use. A refused approval leaves the request pending.

## Session lifecycle

The session the code starts remembers its role. A refresh re-applies the approve policy; a change
or deletion of the record revokes the sessions it admitted. The PDS enforces the session's
granted scopes (atproto permission scopes) on every write.

## Security considerations

- Trust anchor: the account's own record names the issuer; the PDS fetches issuer metadata only
  for accounts it hosts and roles they wrote.
- The policy input is built by the PDS from its own state; a caller cannot widen client, redirect
  or scope.
- An app scope that grants full access (`transition:*`) or write access to `com.fedproxy.rbac`
  should not be grantable to a workload.
- Workload tokens may be minted for any audience; the subject is fixed by the issuer. Relying
  parties check `iss`, `sub` and `aud`.
- Exchanged tokens: short, DPoP bound, `lxm` limited, single use.

## Open questions for discussion

- An `exp` field on the record versus deletion as the only expiry.
- A standard collection NSID and a standard scope string in `protects`.
- Whether `approveAuthorization` should be an extension of the PAR/authorize endpoints instead of
  an XRPC.
- Discovery: advertising support in the authorization server metadata.
