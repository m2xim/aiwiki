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
  space, article, revision, comment, attachment, favorite, search, artifact or
  setting row, or a membership of someone other than the owner (the owner's
  `users` row left by an earlier personal-mode start is accepted); the source has a schema the
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
  uploads nothing; it reports what would be copied. The server migrations
  (schema only) are applied in a dry run too, as the server would at start.

Details of the import:

* After the commit, `-verify-only` runs just the verification against an imported
  target (for a commit whose outcome was unknown). If the import fails before the
  commit, the files uploaded so far are deleted again; those that cannot be
  deleted are listed (`check-storage -delete-orphans -yes -allow-empty-database`
  removes orphans older than 24 hours). An open `wiki.db-journal` is a refusal.
* Stop the old server first. `wiki.db` (and a pending `wiki.db-wal`) is copied
  byte for byte; if the files change while they are copied, the import refuses.
* No trigger or policy is disabled. Articles are inserted parents first (the
  insert guard requires a parent in the same space); a missing parent, a parent
  in another space or a cycle is a refusal, as is any project reference (trusted
  mode has none), a revision snapshot of an unknown space, or an attachment file
  whose size differs from the database. Deleted attachments count: their files
  must exist too. Search rows exist for live articles only, as in the server.
* The revision ids are kept and the `revisions` sequence is advanced past the
  largest id just before the commit (a sequence is not transactional, so a dry
  run or an aborted run never moves it).
* Artifacts get `owner_id='owner'` and links `created_by='owner'` (trusted mode
  has no owner, and a private artifact or a link would otherwise be invisible);
  everything else of an artifact is copied unchanged, blobs under `art-<sha256>`.
  Embedding vectors are derived data and are not copied; the server rebuilds them.
* Space usage is not charged against a quota while importing; it counts
  afterwards like any other stored data.

## Deployment

`compose.personal.yaml`: PostgreSQL 16, SeaweedFS (S3), the wiki in personal
mode. The database and object data live in directories next to the compose file;
only the wiki port is published. Roles and passwords come from an `.env` file
that is not part of the repository. The existing trusted-mode data directory is
left untouched, so switching back is starting the old container again.

## Run

```sh
cp .env.personal.example .env.personal      # fill in every value; the file is git-ignored
docker compose --env-file .env.personal -f compose.personal.yaml up -d --build
# open the address from AIWIKI_ALLOWED_ORIGIN, e.g. http://127.0.0.1:8090
```

* Services: `db` (PostgreSQL 16.15, roles `aiwiki_owner` and `aiwiki_app` created
  once by `scripts/personal-pg-init.sh`), `s3` (SeaweedFS 4.48, private bucket
  `wiki` created by `s3-init`), `wiki` (this repository's Dockerfile, personal
  mode). `edge` only holds the network namespace and the single published port.
* The role passwords are used once, when PostgreSQL initialises an empty
  `./personal-data/pg`. Changing them later in `.env.personal` does not change the
  roles: run `ALTER ROLE ... PASSWORD` in the database as well. The `db` service
  keeps them in its environment; whoever can run `docker inspect` on the host can
  read them, as with every other secret of the compose file.
* Passwords and keys go into URLs and a JSON file unescaped: use letters and
  digits only.
* Data stays in `./personal-data/pg` and `./personal-data/s3`; back them up as
  described in BACKUP.md (a database dump plus the bucket).
* To serve a home network or VPN set both `AIWIKI_BIND_ADDR` (this machine's
  address) and `AIWIKI_ALLOWED_ORIGIN` (the address browsers use). There is no
  login: do not expose the port outside a trusted network.
* The wiki accepts a plain-HTTP S3 endpoint only on loopback, therefore `wiki`,
  `s3` and `s3-init` share one network namespace and S3 is `127.0.0.1:8333`
  there; it is not reachable from the host or the network.
* Import of an existing trusted-mode installation (the superuser password is
  used by this one-off service only):
  `docker compose --env-file .env.personal -f compose.personal.yaml --profile import run --rm import`
  with `AIWIKI_IMPORT_SOURCE` pointing at the old data directory (read-only).
* Compose files were checked on Linux-style bind mounts. On a Windows host with
  Podman a bind-mounted PostgreSQL directory fails with "could not change
  permissions"; use a named volume for `db` there.
* Without Docker: `AIWIKI_MODE=personal` with the same `AIWIKI_DATABASE_URL`,
  `AIWIKI_MIGRATION_DATABASE_URL`, `AIWIKI_DATABASE_ROLE` and `AIWIKI_S3_*`
  variables as corporate mode, and the usual `-allowed-origin` flag.

## How the owner rows are written

The server does it at every start through the runtime role, in one transaction on
a connection scoped to `owner`, with no new privilege and no new migration:
`users` and `memberships` have row-level security enabled without FORCE and a
single policy ("no persistent MCP scope"), the runtime role already holds
INSERT/UPDATE on them; `spaces` (FORCE RLS) is updated by the ordinary
manager-or-better update policy, which holds because the owner membership was
inserted just before in the same transaction. Spaces are enumerated from
`corporate_space_state`, which a trigger keeps equal to `spaces`. A pre-existing
`owner` user with another issuer/subject, `admin=0` or `disabled=1` stops the
start instead of being taken over. An audit row `personal.owner.ensure` is
written only when something changed.
