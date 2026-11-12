# Persistent MCP access

Corporate mode supports opaque personal credentials and sponsored service accounts,
in addition to OIDC bearer tokens. They are always available in corporate mode and
need no extra configuration. Temporary session-bound keys (`aiwiki_tmp_`) were
removed: use a persistent credential with a short expiry instead.

## Admission

Admission is decided locally, from the database, on every request. The identity
provider is not contacted per request and nothing is cached in process memory, so
restarts and replicas behave identically. A credential works only while all of
these hold:

- it is neither expired nor revoked;
- its owner is not disabled in aiwiki's user administration;
- its service account, if any, is not disabled and still belongs to the owner;
- the owner completed an interactive (browser) login within the last **14 days**.

The login window is how directory changes reach credentials. A user removed from
the directory, or who lost the access role, can no longer sign in; all of their
credentials stop working at most 14 days after their last login. A user who simply
has not used the browser for 14 days gets the same refusal, and their credentials
work again after the next login: nothing is revoked. Disable the user in the
administration page to cut access immediately. The login time is stored in
`users.last_login_at` (migration `mcp_access` v4); users who have not signed in
since the upgrade start without it, so their existing credentials resume after
their next login.

Database faults deny access with 503. A disabled owner or an expired login window
denies with 403; credential state problems (unknown, expired, revoked) with 401.
Responses and errors never expose the secret.

## Issuance, projects and read-only

Open the administration page's MCP access tab. Any admitted user can manage their
own credentials and service accounts. A credential has a name, a list of 1-20
**agent projects** (see below), a `read_only` flag and an expiry (default 30 days,
maximum 180 days). The per-element `scope` of earlier releases was replaced by
projects; existing keys were migrated to a personal project of their owner.

`POST /api/mcp/credentials` takes
`{name, projects:[id...], read_only, ttl_seconds, service_account_id?}`; `PATCH
/api/mcp/credentials/{id}` takes `{projects?, read_only?}` (the secret stays the same;
changes are audited as `mcp_credential.update`). Responses carry
`projects: [{id, name, visibility, available}]` and `read_only`, no `scope`.
Every project must be visible to the key owner (own private projects and any public
one). Input with `scope`, `project_ids` or `mode` is rejected with 400 `use projects`.
Errors: 400 `invalid project`, `space not visible`, `too many spaces`, `use projects`.
Name, owner, service account and expiry cannot be changed; revocation is final.

### Agent projects

A project is a named set of rows "space + mode (`read` or `write`)", with an owner, a
description and a visibility: **private** (only its owner sees and edits it) or
**public** (every user sees it; the owner and administrators edit it; only the owner
changes visibility). A project grants no rights, it only limits the agent. API:
`GET/POST /api/agent-projects`, `GET/PATCH/DELETE /api/agent-projects/{id}`. Rows
whose space the caller cannot see come back as `accessible: false` without a name
(the UI shows "Недоступное пространство") and are kept with `{row_id, mode}` on PATCH.
Each accessible row carries the caller's own role (`my_role`) and `effective`
(`write`, `comment`, `read` or empty): what the caller's agent really gets there.

The agent's right in a space is the **minimum** of the project mode and the key
owner's current role (directly or through teams): `write` gives Editor only if the
owner is Editor or higher (Commenter stays Commenter, a Reader only reads); `read`
gives Reader. `read_only` on the key forces `read` everywhere. The general space
(admin setting `general_space_id`) is always readable and need not be a row; to write
there, add it as a row with mode `write` (effective only for Editors of the general
space). Changes to a project apply to all running agents from their next request.
Deleting a project removes it from every key.

### Connecting to a project

The agent connects to `<origin>/mcp?project=<id>` with the key as bearer. The project
must be in the key's list and visible to the owner, otherwise the request is refused
with 403 and the JSON-RPC error `project is not available for this key`. Without
`project` the session is introductory: only `wiki_bootstrap` and `wiki_projects`
work, and the scope is read access to the general space. `wiki_projects` returns
`{current_project_id, projects, connect_url_template}` where the template is built
from the configured public origin (`<origin>/mcp?project={id}`). The key form shows
`<origin>/mcp?project=<id>` after creation.

The server returns the random 256-bit secret once, prefixed with `aiwiki_pat_`.
Copy it to the MCP client's secret configuration and clear the displayed value.
The UI keeps it only in component memory. PostgreSQL stores only the SHA-256 hash;
list endpoints and audit events never contain the secret. Logging out or restarting
aiwiki does not revoke these credentials. Expiry or explicit revocation does.

Read rows expose read tools only. Write rows additionally permit article
creation, editing, revision restore and adding comments within their coverage. Current owner permissions still
apply, capped at Editor by RLS. ACLs, users, teams, ownership, space/project structure
and credential management are unavailable through native MCP credentials. A key
cannot mint another key. Partial reads and revision conflict checks remain available.

Use the secret as `Authorization: Bearer <secret>` on `/mcp?project=<id>`. In project
mode `wiki_bootstrap` returns the project's pinned rules (general space first) and
its composition with modes and effective rights; pass a space of the project as
`space_id` when creating articles. Native credentials do
not authenticate browser REST endpoints, and browser cookies do not authenticate MCP.
Native credentials are an explicit bearer option, not a claim of MCP OAuth discovery
or compatibility with every client. See [AI-CONNECTION.md](AI-CONNECTION.md).

## Service accounts and revocation

The sponsor can rename an active service account:
`PATCH /api/mcp/service-accounts/{id}` with `{name}` returns the updated account
(same shape as in the list). Errors: 400 for an invalid name (same rules as create),
404 if not found, and an error for a disabled account. Authors are stored as ids
(`u_…` users, `svc_…` service accounts); article, revision, comment and artifact
revision responses add `author_name` (current display name, may be empty) and
`author_kind` (`user`, `service` or empty), so a rename shows immediately in old
history too. The UI marks service authors with an «агент» badge.

A service account has a stable actor ID and an immutable human sponsor. Only its
sponsor manages it. It receives no administrator status or independent ACL grants:
each request uses current sponsor ACLs intersected with the project rows (per-row mode) and `read_only`.
Article/comment authorship identifies the service account; request audit records
include both actor and sponsor. A sponsor becoming locally disabled or exceeding
the login window blocks all associated personal and service credentials.

If the sponsor is demoted below Editor, a service account can still read permitted
content but cannot add comments at Commenter rank: comment ownership is tied to the
human authorization subject. Restore Editor access or use a personal credential for
that comment-only workflow.

Disabling a service account also revokes all its credentials atomically. There is
no re-enable or sponsor-transfer endpoint in this version. Create a new account and
new credentials for a new sponsor. Rotation means issue a replacement, update the
client, then revoke the old credential. Each owner is limited to 20 active and 200
total credential records, and 20 service accounts. Each credential is limited to
120 requests per minute before provider lookup. Revoked/expired rows are retained
for audit; automated retention management is not implemented yet.

Requests admitted after a committed local revocation are denied; requests already
admitted may finish. Local state is rechecked after provider I/O. ACLs are evaluated
on every SQL statement, so removing space access constrains later statements too.
The two provider reads do not constitute a cross-system atomic snapshot.

## Database migration and validation

`mcp_access` migration v2 adds credential/service-account tables and a validated
`app.mcp_mode` session setting. Published v1 remains unchanged. Runtime must own none
of these tables. Migration v8 adds `agent_project_ids` and `read_only` to credentials. Their metadata RLS denies scoped native sessions; authentication
uses a separate unscoped pool. Browser scopes see only their owner's credential
records. Triggers prevent changing credential authority, extending expiry, clearing
revocation, transferring sponsorship, or re-enabling disabled accounts. Runtime
cannot delete and reinsert those records; retention requires the migration owner.
Content tables retain FORCE RLS and current ACL
checks. The connection wrapper applies, verifies, clears and restores mode along
with user and project scope, including around S3 transfers.

The normal `AIWIKI_MIGRATION_DATABASE_URL` path provisions runtime grants. With
external migrations, the owner must also grant runtime SELECT/INSERT/UPDATE on
`mcp_credentials` and `mcp_service_accounts`, and EXECUTE on
`corporate_mcp_credential_audit(text,text,text)` and
`corporate_mcp_content_write_allowed()`. Preserve the existing v1 grants, revocation
of EXECUTE on `corporate_owner_effective_role(text,text,text)`, and the prohibitions
on schema CREATE, table ownership, TRIGGER/TRUNCATE and RLS bypass. Startup validates
the policies and enabled immutability triggers; missing migration/grants stop startup.

Tests use a disposable PostgreSQL schema and HTTP provider mocks. Real Keycloak/AD
and S3 pilot acceptance remains separate; a mocked role response does not prove
directory federation works in production.

Sources: [Keycloak Admin REST](https://www.keycloak.org/docs-api/26.8.0/rest-api/index.html),
[administration guide](https://www.keycloak.org/docs/latest/server_admin/index.html),
[role mapper implementation](https://github.com/keycloak/keycloak/blob/26.4.0/services/src/main/java/org/keycloak/services/resources/admin/RoleMapperResource.java),
[user role implementation](https://github.com/keycloak/keycloak/blob/26.4.0/model/jpa/src/main/java/org/keycloak/models/jpa/UserAdapter.java).
