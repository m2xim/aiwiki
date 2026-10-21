# Temporary project access for MCP

This is the first bounded MCP access slice. It provides temporary, read-only
keys tied to a current browser session. Long-lived personal tokens, service
accounts, automatic refresh and OAuth client discovery are not implemented.

## Use

1. Sign in and open **Команды и доступ → MCP-доступ**
   (`/?view=admin&tab=tokens`).
2. Name the key and select 1–20 projects you can currently read. Selection is
   exact: a child project must be selected separately. Space-level articles,
   hidden ancestors and global instructions are outside the key's scope.
3. Create the key. Copy the one-time secret into the MCP client's secret
   configuration, using the ordinary `/mcp` HTTP URL and an
   `Authorization: Bearer <secret>` header. Never put the secret in a URL,
   article, prompt, committed file or public issue.
4. Check the actual expiry shown in the list. The UI requests five minutes;
   the server caps the key at the parent session's remaining lifetime.
5. Revoke the key from the same page. Logging out of the issuing session,
   session expiry or a server restart also invalidates it.

Closing/restarting the browser does not by itself revoke a still-valid server
session. Run one application instance: sessions and keys are held in memory,
not shared across replicas. A job that outlives the key needs a newly issued key;
there is no refresh or silent extension.

## Authorization and audit

Every MCP request checks the live parent session, current Keycloak token
introspection and local user eligibility. The effective content scope is the
intersection of the owner's current grants and the selected projects. The key
has no administrator privileges. An identity-provider outage denies access.
Directory changes retain the upstream token/cache limits described in
[CORPORATE.md](CORPORATE.md); local key and ACL revocations apply on the next
request, rather than cancelling reads already in progress.

The random 32-byte secret is retained only as a hash. Metadata listings never
return it again. Browser APIs require the normal session and CSRF protection;
keys authorize only MCP and cannot mint more keys. MCP tool discovery excludes
mutation tools, dispatch rejects mutation attempts, and PostgreSQL applies
project/read-only restrictions to content and metadata. Database connections
clear both identity and scope before reuse; SQLite refuses restricted scopes.

Explicit creation, revocation and authenticated MCP requests are audited.
Request audit includes method/tool names and a non-secret key identifier; it
does not include arguments, article contents, bearer values or secret hashes.
Session expiry/logout invalidates in-memory keys; there is no separate durable
expiry event for each key. Limits are 20 active keys per user, 2,048 per process,
and 120 requests per minute per key (429 with `Retry-After`).

The server's initialize response explains the key's scope and expiry so agents
can avoid probing unavailable global material. Search, section discovery and
revision-pinned bounded reading remain available inside the scope.

## Deployment and next stage

Apply the separate `mcp_access` migration as the migration owner before starting
the new binary with runtime credentials. Startup rejects a missing scope layer,
unsafe runtime execution of the unfiltered owner-role helper, and missing RLS
protection. Future ACL changes belong in `corporate_owner_effective_role`;
replacing the scoped wrapper with an unfiltered body must fail startup.

A long-lived local PAT cannot discover an owner's external role loss from its
hash alone. Long-running agents/service accounts require a defined provider
admission signal and sponsorship/revocation lifecycle. This temporary session
path preserves current per-request introspection without persisting upstream
refresh credentials or claiming that larger lifecycle is solved.

For an actual S3-compatible pilot check, see [S3-SMOKE.md](S3-SMOKE.md). An omitted
service configuration is a skipped test, not a successful storage-service test.
