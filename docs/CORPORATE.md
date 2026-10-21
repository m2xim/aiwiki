# Corporate edition

The target product has one shared core for home and corporate use: PostgreSQL, S3 and the same features. Authentication is the intended difference. This stage implements Keycloak OIDC; a home sign-in strategy and migration of the existing SQLite installation are separate later work. The current trusted-network SQLite/local-files mode remains a compatibility path, not a separate long-term feature fork. It does not import or change the existing home installation. AD federation and directory group mapping remain in Keycloak; project membership remains in AI Wiki.

## Database roles

Use a dedicated database and two non-superuser, non-BYPASSRLS login roles: a migration owner and a runtime application role. The runtime role must not own content tables. FORCE ROW LEVEL SECURITY protects spaces, articles, revisions, comments, attachments, favorites and the compatibility search table. No authenticated identity means no content access. Do not run the service using PostgreSQL's `postgres` superuser.

Configure `AIWIKI_DATABASE_URL` for runtime, `AIWIKI_MIGRATION_DATABASE_URL` for initial schema setup and `AIWIKI_DATABASE_ROLE` for the runtime role name. Migration credentials need not remain in the running service after provisioning. Use TLS for remote PostgreSQL connections. Migration changes are versioned and transactional. Never point integration tests at a database containing real data: they reset the public schema.

## Keycloak configuration

Use a confidential OIDC client with authorization-code flow, PKCE S256, an exact callback URL ending in `/auth/callback`, and an audience mapper that includes the client ID in access tokens. The discovery document must provide authorization, token, JWKS and introspection endpoints. Enable the configured admission/admin role claims in both ID/access tokens and introspection responses. Password grant is not used.

Environment variables:

| Variable | Purpose |
| --- | --- |
| `AIWIKI_OIDC_ISSUER` | Exact realm issuer, for example `https://identity.example.org/realms/company` |
| `AIWIKI_OIDC_CLIENT_ID` | Confidential client and expected token audience |
| `AIWIKI_OIDC_CLIENT_SECRET` | Server-only client secret |
| `AIWIKI_OIDC_REDIRECT_URL` | Exact public callback URL, e.g. `https://wiki.example.org/auth/callback` |
| `AIWIKI_OIDC_ROLES_CLAIM` | Dot path, default `realm_access.roles` |
| `AIWIKI_OIDC_ACCESS_ROLE` | Admission role, default `aiwiki-access` |
| `AIWIKI_OIDC_ADMIN_ROLE` | System administrator role, default `aiwiki-admin` |
| `AIWIKI_ALLOW_INSECURE_LOOPBACK` | `1` permits HTTP only on loopback for local integration tests |

Missing admission claims deny access, including for administrators. Identities use `(issuer, subject)`; email changes never merge accounts. Setting corporate configuration incompletely fails startup instead of opening an anonymous SQLite service. TLS terminates at the trusted deployment proxy; publish the application only behind that proxy and configure the exact browser origin.

Browser sessions use opaque HttpOnly, SameSite cookies, Secure in normal deployments; random secrets are hashed as in-memory session-map keys. Login validates state, a browser-bound cookie, PKCE, nonce, issuer, audience, signature and expiry. Sessions default to five minutes, never exceed fifteen minutes or the access-token expiry, and have no refresh. A restart logs users out. Unsafe browser operations require `X-CSRF-Token` from `/api/me`. Logout revokes the local session; it does not terminate the entire Keycloak SSO session.

Every authenticated request introspects the access token; outages deny access. Locally disabling a user or revoking a content grant takes effect on the next statement/request. Directory/role changes cannot be promised instantaneously: an already issued Keycloak token may retain claims until it expires or Keycloak reports it inactive. Set a short access-token lifetime in the pilot realm and test the actual AD federation/cache settings. No production AD connection is bundled or implied by the local tests.

## Content rights

Optional persistent MCP credentials use a separate current-admission checker and
can survive browser logout/restarts. Configuration, exact project scopes, service
accounts and revocation boundaries: [MCP-PERSISTENT-ACCESS.md](MCP-PERSISTENT-ACCESS.md).

| Role | Permission |
| --- | --- |
| Reader | Read accessible content/history; own favorites |
| Commenter | Reader plus adding comments and editing/resolving own comments |
| Editor | Commenter plus content edits, attachments, lifecycle operations and comment moderation |
| Manager | Editor plus project/container management and membership management below owner |
| Owner | Manager plus ownership grants/transfer; the final owner cannot be removed |

System administration grants no automatic content read. Any admitted user can create a personal space; administrators can create team spaces and manage internal teams. Projects have one space and one parent, and articles can exist directly in a space. Grants target users or internal teams. Effective rights take the strongest grant along the inheritance chain; disabling inheritance makes a project independent. A project can be granted independently of its ancestors: hidden names and descriptions are not returned. The catalog and space selector use a generic limited navigation group for an accessible child whose space is hidden; this group grants no access to the space itself. Team archival removes its grants from effective access. Archived containers remain readable to authorized users when explicitly selected and reject content writes.

Creating a child requires management rights on its immediate parent (or the space for a root project). A manager receives a direct manager grant on the new project; an owner receives an owner grant. These explicit grants survive later removal of inherited parent access until separately revoked. Changing inheritance requires ownership of both the project and its parent container. Administrators govern team membership and can explicitly add themselves to a team; such self-additions are audited separately and can confer that team's content rights. Administration itself never bypasses content RLS.

Search and section ranking operate only on rows PostgreSQL permits the current identity to read, before snippets, document-frequency scoring, counts or pagination. History also checks the historical container, so a move cannot expose an old snapshot merely because its current article became accessible. Authors come from the server identity. Favorites are keyed by user and article. Corporate drafts are isolated by user and browser session.

The first core deliberately rejects article container/parent moves and project reparenting until complete subtree permission previews are implemented. It also disables the legacy whole-database export in corporate mode. Scoped Markdown/HTML/PDF import/export and interactive artifacts remain separate planned work.

## MCP

In corporate mode `/mcp` accepts an OIDC access token using `Authorization: Bearer ...`, with signature/issuer/audience/expiry verification, introspection and local eligibility checks. It also supports [temporary project-scoped read-only keys](MCP-TEMPORARY-ACCESS.md), tied to a live browser session with per-request introspection. Browser cookies do not authorize MCP. Existing trusted-network mode remains compatible. Long-lived personal tokens, service-account lifecycle, OAuth authorization-server discovery and the client compatibility matrix remain subsequent MCP work.

## Verification

The management interface is a separate page at `/?view=admin&tab=catalog` (or `projects`, `teams`, `users`). Its URL survives refresh and supports browser Back/Forward; the return button opens the previous wiki view. Team and user management are restricted to system administrators, while space/project owners and managers use the same page for their own resources. Server authorization is enforced independently of the visible navigation.

Normal SQLite regression tests run without any server. PostgreSQL integration tests require an isolated disposable database and `AIWIKI_TEST_POSTGRES_OWNER` / `AIWIKI_TEST_POSTGRES`; the latter must connect as the runtime role. The integration suite deletes and recreates `public`. It checks unauthorized HTTP/MCP access, personal favorites, grant revocation, disabled users, inheritance and concurrent revisions. OIDC tests use a local signed mock issuer; they do not prove a specific AD deployment is configured correctly.

Primary references, consulted 2026-10-22: [PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html), [Keycloak OIDC endpoints](https://www.keycloak.org/securing-apps/oidc-layers), [go-oidc verification and caller nonce responsibilities](https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc), [OAuth security BCP](https://www.rfc-editor.org/rfc/rfc9700.html).
# S3 storage requirement

Corporate deployments require a private S3 bucket. Supported configuration: `AIWIKI_S3_BUCKET`, `AIWIKI_S3_REGION` (or the standard AWS region configuration), optional `AIWIKI_S3_ENDPOINT`, `AIWIKI_S3_PATH_STYLE=1` for compatible services, and `AIWIKI_S3_PREFIX` for object isolation. Credentials come from the AWS SDK default chain (environment/IAM/workload identity); do not put credentials into the wiki database or committed configuration. Endpoint examples must use documentation hosts such as `https://objects.example.org`.

The application checks the existing bucket and never creates it, changes its policy or makes objects public. Give its service identity only the required bucket/object permissions for its prefix, enable encryption and backup/versioning according to company policy, and block public access. Runtime requests proxy the object through `/files/{id}` after current article authorization; there are no public or presigned download URLs. PostgreSQL stores metadata and object keys; the binary content lives in S3. SQLite trusted mode keeps its original local-file behavior.

Soft deletion retains the object for the existing recovery/retention behavior. If upload succeeds but database insertion fails, the application attempts to remove the new orphan object; database and object storage are not one distributed transaction. Operators need a retention/orphan-reconciliation policy before the production pilot. Database backups alone do not back up attachment bytes.

Authorized recovery views can still download attachments of a soft-deleted article; new uploads to deleted articles are rejected. Local storage tests exercise the real AWS SDK against a signed mock endpoint, including revocation before any object request. They do not replace a pilot against the selected S3/MinIO service.

Local integration tests may explicitly permit HTTP endpoints on loopback; production endpoints require TLS. Primary reference: [AWS SDK v2 configuration and credential providers](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html), consulted 2026-10-22.
