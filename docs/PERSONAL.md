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
  mode). Only the wiki port is published.
* The role passwords are used once, when PostgreSQL initialises an empty
  `./personal-data/pg`. Changing them later in `.env.personal` does not change the
  roles: run `ALTER ROLE ... PASSWORD` in the database as well. The `db` service
  keeps them in its environment; whoever can run `docker inspect` on the host can
  read them, as with every other secret of the compose file.
* Passwords and keys go into URLs and a JSON file unescaped: use letters and
  digits only.
* Data stays in `./personal-data/pg` and `./personal-data/s3`; back them up with
  `scripts/personal-backup.sh` (see Backups below; BACKUP.md has the background).
* To serve a home network or VPN set both `AIWIKI_BIND_ADDR` (this machine's
  address) and `AIWIKI_ALLOWED_ORIGIN` (the address browsers use). There is no
  login: do not expose the port outside a trusted network.
* `db` and `s3` are reachable only on the compose network. A plain-HTTP S3
  endpoint is accepted for loopback with `AIWIKI_ALLOW_INSECURE_LOOPBACK=1` or for
  a host name listed in `AIWIKI_S3_ALLOW_HTTP_HOSTS` (here `s3`); every other
  endpoint must use HTTPS.
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

## Backups

What is backed up:

* a PostgreSQL dump (`pg_dump -Fc`, taken as the `postgres` superuser inside the `db` container), one file per run in `<backup dir>/db/`;
* the S3 objects (attachments and artifact files), copied to `<backup dir>/objects/`. Objects are immutable, so each run only adds the new ones and all dumps share the same copy; nothing is ever deleted or overwritten there (a size mismatch is reported as a conflict);
* **not** included: `.env.personal` (passwords, S3 keys, `AIWIKI_ARTIFACT_SECRET`). Keep it in a secret store of your own; without the same values a restored system cannot sign artifact links.

Run it on the host from the install directory:

```sh
sh scripts/personal-backup.sh
```

Settings: `AIWIKI_BACKUP_DIR` (default `./backups/personal`), `AIWIKI_BACKUP_KEEP` (newest dumps kept, default 14; older dumps are removed only after a successful run), `AIWIKI_ENV_FILE` (default `.env.personal`), `AIWIKI_COMPOSE_PROJECT` (compose project name, default empty) and `AIWIKI_BACKUP_UID` / `AIWIKI_BACKUP_GID` (the user the backup container writes as; default is the invoking user, so the copy is not root-owned). The script takes `AIWIKI_BACKUP_DIR`, `AIWIKI_BACKUP_KEEP` and `AIWIKI_COMPOSE_PROJECT` from the shell environment, or else from lines of the env file with exactly those names. It reads only those lines and never sources the file; the shell environment wins. A second run while one is active exits with status 2 (lock `<backup dir>/.lock`).

The order is fixed: dump first, then objects (see [BACKUP.md](BACKUP.md)). The script checks that the dump is a complete archive (`pg_restore -f /dev/null` reads every data block), never leaves a half-written `.part` file, and then runs `aiwiki backup-objects`, which copies the missing objects (each is written to `objects/.incoming` and renamed into place once complete; `art-` objects are hash-checked on the way) and compares the copy with the database. The database references are read before the copy, so an upload during the run is not reported as missing. Exit status 1 means missing or damaged objects, 2 means the command could not run. Add `-verify-hashes` to the `backup` service command if you also want every artifact object re-read.

Example schedule (cron, nightly at 03:15; not installed by anything):

```cron
15 3 * * * cd /opt/aiwiki && sh scripts/personal-backup.sh >>/var/log/aiwiki-backup.log 2>&1
```

A backup directory on the same disk as `personal-data` protects against mistakes (a bad deletion, a bad upgrade), not against the loss of the disk. Copy `<backup dir>` to another machine or medium as well; the files contain all content of the wiki, so protect and encrypt them like the database.

### Restore

**Use the same compose project name as the installation does.** The commands below show it as `-p "$AIWIKI_COMPOSE_PROJECT"`; leave the option out only if the installation never used one. A different project name on the same `./personal-data` starts a second PostgreSQL on the same data directory, which can corrupt it.

On a fresh installation (empty `./personal-data`), with the same `.env.personal`:

```sh
# the backup container writes and reads as this user; it must own the backup files
export AIWIKI_BACKUP_UID=$(id -u) AIWIKI_BACKUP_GID=$(id -g) AIWIKI_BACKUP_DIR=/path/to/backups/personal
dc="docker compose -p $AIWIKI_COMPOSE_PROJECT --env-file .env.personal -f compose.personal.yaml"

$dc up -d db s3 s3-init
# wait until db is healthy; the init script has created the roles and an empty database "aiwiki"
$dc exec -T db pg_restore -U postgres -d aiwiki --no-owner --role=aiwiki_owner --exit-on-error \
  < "$AIWIKI_BACKUP_DIR/db/aiwiki-<timestamp>.dump"
$dc --profile backup run --rm backup restore-objects -dir /backup/objects
$dc up -d wiki
```

Why `pg_restore` looks like this: `scripts/personal-pg-init.sh` created the database owned by `aiwiki_owner`, which is the owner the server expects (it runs migrations as that role and grants the runtime role `aiwiki_app` its rights at every start). A plain superuser restore would make `postgres` the owner of every table. `--no-owner` ignores the owner recorded in the dump and `--role=aiwiki_owner` makes the restore run as `aiwiki_owner` after connecting, so all objects belong to it. The superuser is used because the roles themselves are not part of a dump and the content tables use FORCE ROW LEVEL SECURITY. `--exit-on-error` stops at the first problem instead of leaving a half-restored database. If it fails, drop and recreate the database (or empty `personal-data/pg`) and start again. Rehearsed on 22.10.2026 on a real installation (289 articles, 12 spaces), restored next to it under another project name and port, from a backup made by the script. Row counts of all tables were identical, all 23 tables belonged to `aiwiki_owner`, randomly chosen articles and an artifact file were byte-identical through the API, and `check-storage -verify-hashes` reported `RESULT: OK`. Rehearse it on your own copy once anyway; restoring next to the running wiki is safe only with another project name, another directory and another port.

Finally check the result by comparing database and bucket, reading every artifact object:

```sh
$dc --profile backup run --rm backup check-storage -verify-hashes
```

Expect `RESULT: OK`. The `backup` service is used because it already carries the superuser database URL that `check-storage` needs (`AIWIKI_BACKUP_DATABASE_URL`); its volume is not used by this command.

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
