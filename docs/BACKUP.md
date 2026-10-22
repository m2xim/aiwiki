# Backup and restore

This page covers what to back up, in which order, how to restore, and how to check the result. It is written for the corporate edition (PostgreSQL + S3). The trusted-network SQLite edition is covered at the end.

Everything here was exercised by `scripts/backup-drill.mjs` (see [Restore drill](#restore-drill)) against PostgreSQL 16.15 and SeaweedFS 4.48. That is evidence for this procedure, not for your own infrastructure: repeat the drill against your real S3 service and your real backup tooling before relying on it.

## What a backup consists of

| Part | Holds | How |
| --- | --- | --- |
| PostgreSQL database | articles, revisions, comments, users, teams, grants, artifact manifests and share-link hashes, attachment and blob metadata, audit log, MCP access records | `pg_dump -Fc` |
| S3 objects | attachment bytes and artifact file bytes | bucket versioning and/or a copy of the configured prefix |
| Secrets, outside both | `AIWIKI_ARTIFACT_SECRET`, `AIWIKI_OIDC_CLIENT_SECRET`, S3 credentials, database passwords | your secret store |

Database dumps alone do **not** contain attachment or artifact bytes ([CORPORATE.md](CORPORATE.md)), and the bucket alone cannot be read without the database (keys are opaque). Both are needed. The dump contains the whole content of the wiki; protect and encrypt it like the database itself.

### The backup role (important)

The content tables use `FORCE ROW LEVEL SECURITY`. That applies to the table owner too, so the migration-owner role (`aiwiki_owner`) **cannot read the rows**. Measured on the drill database:

* `pg_dump` as the owner fails with `query would be affected by row-level security policy`;
* `pg_dump --enable-row-security` as the owner succeeds but silently leaves rows out. Never use it for backups.

Use a dedicated read-only role that bypasses row security, created once by a PostgreSQL administrator:

```sql
CREATE ROLE aiwiki_backup LOGIN PASSWORD '<from your secret store>' NOSUPERUSER BYPASSRLS;
GRANT pg_read_all_data TO aiwiki_backup;
```

(`pg_read_all_data` exists since PostgreSQL 14.) This role must never be the runtime role: the application refuses to start with a role that is a superuser or has `BYPASSRLS`. Keep its credentials with the backup job only.

`aiwiki check-storage` (below) needs the same property and checks it before reading anything. It would otherwise see zero rows and report a healthy storage.

## What is safe to copy while the service runs

Read from the code:

* **Attachments** get a random id as object key. The object is written once, before its database row, and never overwritten. The server deletes an attachment object only when the database insert that follows the upload fails. Deleting an attachment (or its article) is a soft delete: the row and the object stay.
* **Artifact files** are content-addressed: the key is `art-<sha256 of the bytes>`. Equal bytes in several artifacts or revisions are one object. Writing the same key again stores the same bytes, so objects are effectively immutable. The server never deletes artifact objects. The object is written before the database row (a failed transaction leaves an unreferenced object).

So after an object exists it does not change, and the database can only ever point at objects that were written earlier.

## Order: database first, then objects

1. Take the database dump (`pg_dump -Fc`).
2. Then copy the objects (or note the bucket state/version).

Why: a row is always created after its object. Anything in the dump therefore has its object in a copy taken afterwards. The reverse order can produce a database that refers to objects uploaded after the object copy: those rows would be restored without bytes. An object copy that is newer than the dump contains a few objects with no row (orphans). They are harmless and show up in `check-storage` as information. The drill covers this: an attachment is uploaded between dump and object copy, and the restored system reports it as exactly one orphan and no missing objects.

Consequences:

* Bucket **versioning** or replication protects against deletion and overwrite, but it is not a consistent snapshot by itself. To restore from versions, take the bucket state at or after the dump time, never before.
* Do not delete objects from the bucket (lifecycle rules, "cleanup" scripts) based on age or on guesses. Only `check-storage -delete-orphans` knows which objects are unreferenced.
* Copy tools must preserve the object bytes and keys exactly. Content types are not needed by the server (it serves the MIME stored in the database), but keeping them is cleaner. The drill copies through the S3 API without content types.

## Restore, step by step

Prepare a new, empty PostgreSQL database and the bucket (or prefix) first.

1. **Roles.** Create the migration owner and runtime roles as in [CORPORATE.md](CORPORATE.md) (non-superuser, `NOBYPASSRLS`, the database owned by the owner role, `GRANT ALL ON SCHEMA public` to the owner), plus `aiwiki_backup` for later checks. Roles belong to the PostgreSQL cluster, not to the database: `pg_dump -Fc` does not contain them.
2. **Database.** Restore as the owner role:

   ```sh
   pg_restore -h <host> -U aiwiki_owner -d aiwiki --no-owner --exit-on-error aiwiki.dump
   ```

   `--no-owner` makes everything belong to the connecting owner, which matches the normal setup. Access grants for the runtime role are part of the dump and are re-applied by the server at start. In the drill the owner-role restore reproduced the row counts of all 22 tables exactly. If your PostgreSQL service forbids this, a superuser restore without `--no-owner` also worked in a manual trial.
3. **Objects.** Put the objects back under the same bucket, the same `AIWIKI_S3_PREFIX` and the same keys.
4. **Secrets.** Provide the same environment as before; see the next section.
5. **Start the server** with the usual corporate configuration, including `AIWIKI_MIGRATION_DATABASE_URL` and `AIWIKI_DATABASE_ROLE`. At start it applies migrations that the dump predates (bringing a dump from an older release forward is expected to work but was not part of the drill), re-applies the runtime grants, verifies the schema and the bucket (`HEAD`), and refuses to run if any check fails. Start a release not older than the one that made the dump.
6. **Check storage:**

   ```sh
   AIWIKI_BACKUP_DATABASE_URL=postgres://aiwiki_backup:...@db.example.org/aiwiki \
   AIWIKI_S3_BUCKET=... AIWIKI_S3_REGION=... aiwiki check-storage -verify-hashes
   ```

   Expect `MISSING: 0` and `RESULT: OK`. Orphans are information only.
7. Log in and open a few articles, an attachment and an artifact.

## `aiwiki check-storage`

Compares object references in the database with the objects in the bucket (under `AIWIKI_S3_PREFIX`). It reads only, prints only keys and sizes (never content) and deletes nothing by default.

| Variable | Meaning |
| --- | --- |
| `AIWIKI_BACKUP_DATABASE_URL` | database URL of the backup role (superuser or `BYPASSRLS`). There is deliberately no fallback to `AIWIKI_MIGRATION_DATABASE_URL`: the owner role must stay `NOBYPASSRLS` (the server refuses to run otherwise), so it can never read the rows |
| `AIWIKI_S3_*`, `AIWIKI_ALLOW_INSECURE_LOOPBACK` | same as the server; standard AWS credential chain |

It collects references from `attachments.stored_name` (including soft-deleted rows), `artifact_blobs.stored_name` and the `sha256` values in `artifact_revisions` manifests (the only places where objects are referenced). The bucket needs list permission for the prefix for this command, which the server itself does not use.

Report:

* **MISSING**: referenced by the database but absent from the bucket. This is data loss. Exit code 1.
* **SIZE MISMATCH**: the object has a different size than the database records. Exit code 1.
* **HASH MISMATCH** (`-verify-hashes`): the bytes of an artifact object do not hash to the key/recorded value. Exit code 1. This reads every artifact object; run it after restores, not every minute. An unreadable object during this pass is reported as UNVERIFIED and also fails.
* **Orphans**: in the bucket, not in the database. Information. Typical causes: an upload whose database insert failed and whose cleanup failed, or an object copy newer than the dump.
* Exit code 2: the check could not run or was refused (wrong role, unreachable database or bucket, or an unsafe `-delete-orphans`, see below). The command never prints `OK` in that case.

Orphan clean-up is explicit: `-delete-orphans -yes`. Without `-yes` it only reports. Orphans younger than 24 hours are never deleted (they may belong to an upload in progress; `-orphan-min-age` can only raise this limit). Safeguards, all of which end with exit code 2 and no deletion at all:

* the report has missing, size-mismatched or corrupted objects (fix them first);
* the database references no objects while the bucket has some: the typical signature of an empty or wrong database. Only `-allow-empty-database` overrides this, for the case that you really want to empty the bucket;
* every object in the bucket is an orphan.

Because artifact objects are content-addressed, an upload can recreate an orphan's key between listing and deleting. Deletion therefore works in batches of 100: the references are re-read before each batch, and every object is checked with HEAD right before its DELETE and skipped if it changed since listing or is younger than the minimum age. Keys that the server cannot have written (for example containing `/`) are listed as "skipped" and never touched. Do not delete orphans right after a restore that you have not yet verified.

## Secrets and in-memory state

* `AIWIKI_ARTIFACT_SECRET` signs the short-lived capability URLs of the artifact frame (`/artifact-frame/...`). It is not stored in the database. If it is unset, a random per-process secret is used. After a restore with a different (or no) secret, frame URLs issued earlier stop working; a reload of the page issues new ones. The drill restored with a new random secret: signed-in views and public links worked. For several instances behind a balancer the secret must be set and identical everywhere anyway.
* **Public links (`/s/<token>`)** survive a restore: the database stores only the hash of the token and the page issues a fresh frame capability per request. A link keeps working only while its creator is still an active user with the right to share the artifact. Links created after the dump do not exist after the restore.
* `AIWIKI_OIDC_CLIENT_SECRET`, S3 credentials and database passwords live outside the dump. Keep them in your secret store, with the same lifecycle as the backup.
* The database does hold the users table (issuer + subject), grants and MCP access records. Restore it together with the same Keycloak realm (same issuer and subjects), otherwise users map to new accounts without their grants.
* **Sessions** are in server memory. Nothing from before the disaster survives: everybody signs in again (the drill checks that a cookie from before the restore is rejected).

## RPO and RTO: how to estimate

No numbers are promised here; they depend on your setup. Measure them:

* **RPO** (data you can lose) is the age of your newest consistent pair. For the database it is the interval between dumps (or your WAL/PITR setup if you add one). Objects need no tighter interval than the database: an object newer than the newest dump is an orphan. Write down the dump time and the time the object copy finished; the pair is usable only if the objects were copied after the dump.
* **RTO** (time to be back) is the sum of what you measure in a drill: provisioning the database and roles, `pg_restore` of your real dump size, copying your real object volume back, server start (migrations), and `check-storage -verify-hashes` duration. Run the drill with production-sized data at least once and after every change of infrastructure, and keep the timings.
* Test restores regularly, on a separate database and bucket prefix. A backup that was never restored is not known to work.

## Restore drill

`node scripts/backup-drill.mjs` starts its own disposable PostgreSQL and SeaweedFS containers (`aiwiki-drill-*`, ports 55440 and 39100 by default), then runs the Go test `TestBackupDrillPostgresS3` against them and removes the containers. It needs podman and Go. The test skips itself unless its environment variables are set, so ordinary `go test` is unaffected.

The scenario: create a space, an article with two revisions, two attachments (one soft-deleted), two artifacts with byte-identical files (one stored object), a second artifact revision and a public link; `pg_dump -Fc` with the backup role, then an upload (to demonstrate the ordering), then an object copy through the S3 API; drop the database and wipe the bucket prefix; restore as described above; start the server with `OpenCorporate` on the restored data; verify through HTTP: article and history, attachment bytes, artifact files of current and old revisions, the artifact frame, the public link and its frame, and that old sessions are rejected; run `check-storage` (also as the built binary, checking exit codes). Two negative cases follow: a deleted object is reported as MISSING with exit code 1, and a same-size corrupted artifact object is only found with `-verify-hashes`.
Before the disaster step the drill also points `check-storage -delete-orphans -yes` at a freshly migrated, empty schema next to the full bucket and checks that it is refused (exit code 2) and nothing is deleted. The drill refuses to start unless the container name and the bucket name start with `aiwiki-drill` and every database URL reports the same PostgreSQL `system_identifier` as the container. It reads only its own `AIWIKI_DRILL_*` variables, never the ones used by the ordinary CI tests.

## Trusted-network (SQLite) edition

The data directory holds `wiki.db` (SQLite in WAL mode, so also `wiki.db-wal`/`wiki.db-shm` while running) and `files/` (attachments and artifact objects, same rules as above: written once, never overwritten). Back up consistently by stopping the service and copying the directory ([DEPLOYMENT.md](DEPLOYMENT.md)), or with the SQLite `.backup` command for the database followed by a copy of `files/` afterwards (database first, objects second, for the same reason as above). `aiwiki check-storage` supports only the PostgreSQL edition.

## Personal mode

`scripts/personal-backup.sh` runs this procedure for `compose.personal.yaml` (dump first, then objects, rotation of dumps). The object copy is `aiwiki backup-objects -dir <path>` (bucket to a directory, then a check of the directory against the database; needs the same BYPASSRLS/superuser role as `check-storage`), the way back is `aiwiki restore-objects -dir <path>`. Both only add missing objects and never overwrite or delete one. See the Backups section of [PERSONAL.md](PERSONAL.md).
