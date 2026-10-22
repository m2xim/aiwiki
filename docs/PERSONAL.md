# Personal mode: PostgreSQL and S3 without an identity provider

Personal mode runs the corporate data model (PostgreSQL with forced row-level
security, S3 objects, projects, artifacts, quotas, semantic search) for one
owner, without OIDC, AD or Keycloak. Access control at the network edge stays as
in trusted mode: the Host and Origin checks (loopback or the single
`-allowed-origin`), no login. It is meant for a home network or VPN.

## Settings

| Variable | Meaning |
| --- | --- |
| `AIWIKI_MODE=personal` | Selects personal mode. Required; personal mode is never inferred. |
| `AIWIKI_DATABASE_URL` | Runtime role (non-owner, `NOBYPASSRLS`), as in corporate mode. |
| `AIWIKI_MIGRATION_DATABASE_URL`, `AIWIKI_DATABASE_ROLE` | Owner role that runs migrations, and the runtime role name, as in corporate mode. |
| `AIWIKI_S3_*` | Object storage, as in corporate mode. Required. |
| `AIWIKI_PERSONAL_NAME` | Display name of the owner. Default `Владелец`. |
| other corporate settings | Quotas, metrics, access log, embeddings work unchanged. |

Setting any `AIWIKI_OIDC_*`, `AIWIKI_KEYCLOAK_*` or `AIWIKI_MCP_RESOURCE_URL` /
`AIWIKI_MCP_INTROSPECTION_*` variable together with `AIWIKI_MODE=personal` is a
startup error: two identity models must not be mixed by accident.

## Owner identity (shared by the server and the import)

Exactly these rows, created idempotently at every start and by the import:

* `users`: `id='owner'`, `issuer='aiwiki:personal'`, `subject='owner'`,
  `name=<AIWIKI_PERSONAL_NAME>`, `admin=1`, `disabled=0`. A changed name is
  updated; nothing else about the row may be changed by the server.
* For every space: `memberships(resource_type='space', resource_id=<space>,
  subject_type='user', subject_id='owner', role='owner')`, and
  `spaces.owner_id='owner'`. Spaces stay `personal=0`.
* Favorites belong to `user_id='owner'`.
* Authors of migrated revisions, comments and articles keep their original text
  (`author` is historical data, not an identity).

## Requests

* Every request that passes the Host/Origin checks is served as the principal
  `{ID:"owner", Name:<name>, Admin:true}` on a connection scoped to `owner`
  (`scoped`/`scopedMCP` with no project scope), exactly like an authenticated
  corporate request. Row-level security therefore applies to every query.
* `/mcp` works without a key, as in trusted mode. A request with an
  `Authorization` header is refused (401) in personal mode v1: there are no
  keys, and a header that is silently ignored would suggest protection that does
  not exist.
* `GET /api/me` returns `{"mode":"personal","user":{"id":"owner","name":...,"admin":true}}`.
  The frontend treats `personal` like `corporate` for features (projects,
  spaces directory, administration, artifacts, quotas) and hides everything
  about login, logout, account switching, MCP keys and Keycloak.
* Routes of the OIDC login flow and the MCP OAuth metadata answer 404.
* `GET /api/health` and `GET /api/ready` as everywhere.

## Import of a trusted-mode SQLite installation

`aiwiki migrate-from-sqlite -from <data-dir> [-dry-run]`

* **Source.** A trusted-mode data directory (`wiki.db`, `files/`) of any earlier
  version. The tool copies `wiki.db` to a temporary file and works on the copy;
  the source is never opened for writing. The copy is brought to the current
  trusted schema by the normal trusted migration before reading.
* **Target.** The personal-mode database and bucket from the same environment
  variables as the server, plus `AIWIKI_IMPORT_DATABASE_URL`: a role that may
  write past row-level security (superuser or `BYPASSRLS`; in the home compose
  the PostgreSQL superuser). It is used only by the import, never by the server.
  The tool first runs the server's migrations (owner role), then imports.
* **Refusals (exit code 2, nothing written):** the target already contains any
  article, revision, comment or attachment row; the source has a schema the
  trusted migration cannot read; an attachment file listed in the database is
  missing in `files/`.
* **Copied:** spaces (ids, names, descriptions, archive state), articles with
  their ids, every revision with its original number, author, message, time and
  snapshot, comments, attachments (metadata and the file put into the bucket
  under the same object key scheme the server uses), favorites, full-text rows
  rebuilt by the server's own indexing function, artifacts if the source has
  them. Then the owner identity rows above. One database transaction; files are
  uploaded before the commit and an upload failure aborts it (objects already
  uploaded are reported as orphans for `check-storage -delete-orphans`).
* **Verification printed at the end and exit code 1 if any check fails:** row
  counts per table source vs target, SHA-256 of every attachment read back from
  the bucket vs the source file, and a comparison of every current article
  (title, summary, body, tags, properties, revision, deleted state) and of every
  revision snapshot, read back through the runtime role as the owner (so the
  check also proves that row-level security lets the owner see everything).
* `-dry-run` performs everything in a transaction that is rolled back and
  uploads nothing; it reports what would be copied.

## Deployment

`compose.personal.yaml`: PostgreSQL 16, SeaweedFS (S3), the wiki in personal
mode. The database and object data live in directories next to the compose file;
only the wiki port is published. Roles and passwords come from an `.env` file
that is not part of the repository. The existing trusted-mode data directory is
left untouched, so switching back is starting the old container again.
