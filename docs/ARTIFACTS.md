# Interactive artifacts

Status: implemented on the server (2026-10-22); the application interface and the
block kit are built separately. Owner decisions: artifacts are arbitrary HTML/JS run
in a sandbox plus a built-in kit of ready blocks; a public link opens without sign-in
and a system administrator can disable public links organisation-wide; an artifact is
a versioned object of a space or project. Deviations found while implementing are
listed in "Implementation notes" at the end.

## Model

An artifact belongs to one space and optionally one project (same container rules
as articles). It has a title, a description, a visibility and a linear history of
revisions. Each revision is an immutable manifest of files; `index.html` is the
entry point.

| Field | Meaning |
| --- | --- |
| `visibility = "inherit"` | readers of the container can view; editor+ can edit |
| `visibility = "private"` | only the owner (creator) can view and edit; container managers and system administrators get no automatic access |

Files: at most 50 per revision, 5 MB per file, 20 MB per revision. Paths match
`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`, no `..`, at most 4
levels and 200 bytes. Allowed extensions and the only MIME types served:
`html`, `css`, `js`, `mjs`, `json`, `csv`, `txt`, `md`, `svg`, `png`, `jpg`/`jpeg`,
`gif`, `webp`. File bytes are stored content-addressed (SHA-256) in the existing
object storage (S3 in corporate mode, local files in trusted mode); the database
keeps metadata and manifests.

Tables (both SQLite and PostgreSQL): `artifacts(id, space_id, project_id, title,
description, visibility, owner_id, revision, author, created_at, updated_at,
deleted_at)`, `artifact_revisions(artifact_id, revision, author, message,
created_at, manifest)`, `artifact_blobs(sha256, size, mime, stored_name,
created_at)`, `artifact_shares(id, artifact_id, token_hash, revision, created_by,
created_at, revoked_at)`, `app_settings(key, value)` (corporate: admin-only
writes). PostgreSQL uses FORCE RLS on artifacts, revisions and shares with the
same `corporate_effective_role` chain as articles plus the visibility/owner rule;
history checks the container current for the artifact (artifacts move together
with their project, see "Moving a project" below).

## Rights

| Action | inherit | private |
| --- | --- | --- |
| list, view, read files, history | reader+ on container | owner |
| create | editor+ on container | editor+ on container |
| update files/title/description | editor+ | owner |
| change visibility | owner (see notes) | owner |
| delete / restore (soft) | editor+ | owner |
| create / revoke public links | owner or manager+ | owner |

MCP credentials follow their existing ceilings (read-only keys read only; scoped
keys see only their projects). MCP never creates or revokes public links.

## HTTP API (authenticated, CSRF for unsafe methods)

- `GET /api/artifacts?space_id=&project_id=&q=` — list visible, not deleted (title,
  description, visibility, owner name, revision, updated_at, file count); `q`
  searches title, description and the text projection.
- `POST /api/artifacts` `{space_id, project_id, title, description, visibility,
  files:[{path, content, encoding:"utf8"|"base64"}], message}` → artifact, revision 1.
  `index.html` is required.
- `GET /api/artifacts/{id}` → metadata + manifest of the current revision
  (`[{path, size, sha256, mime}]`) + caller's effective permissions
  (`can_edit`, `can_share`, `can_manage`).
- `GET /api/artifacts/{id}/files/{path}?revision=` → raw bytes for editing, always
  `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`.
- `PUT /api/artifacts/{id}` `{base_revision, title?, description?, visibility?,
  files?:[{path, content, encoding} | {path, delete:true}], message}` → new revision
  with only the listed changes applied (partial update for agents). 409 on stale
  `base_revision`.
- `DELETE /api/artifacts/{id}` `{base_revision}` → soft delete; revokes all links.
  `POST /api/artifacts/{id}/restore` `{base_revision}`.
- `GET /api/artifacts/{id}/history` → revisions (author, message, date, file count).
- `POST /api/artifacts/{id}/view` `{revision?}` → `{frame_url, expires_at}`.
- `GET /api/artifacts/{id}/shares`, `POST …/shares` `{revision: null|n}` →
  `{id, url, revision, created_at}` (the token appears only in this response),
  `DELETE …/shares/{share_id}`.
- `GET /api/admin/settings`, `PUT /api/admin/settings` `{public_artifact_links:
  bool}` — system administrators only (corporate); default `true`. When `false`,
  every public link answers as disabled without being deleted. Trusted mode: links
  always allowed, no admin endpoint.

All writes produce `security_audit` events in corporate mode
(`artifact.create|update|delete|restore|visibility|share.create|share.revoke`,
`settings.update`) without file content.

## Rendering and isolation

The artifact never runs on the application's origin with application privileges.

1. `POST /view` (or a valid public token) yields a capability: a URL
   `/artifact-frame/<cap>/index.html`, where `<cap>` = base64url(payload) + "." +
   base64url(HMAC-SHA256(secret, payload)), payload = artifact id, revision, expiry
   (≤ 15 minutes), kind (`user`|`public`); a `public` payload also carries the id of
   its link, so that revoking exactly that link ends the capability at once. Secret: `AIWIKI_ARTIFACT_SECRET`
   (at least 32 bytes, required for several instances; a shorter value stops the
   start) or, when unset, a random per-process value. The frame
   path is the authorisation; cookies are neither needed nor used.
2. `GET /artifact-frame/<cap>/<path>` verifies the HMAC and expiry, re-checks that
   the artifact is not deleted (and for `public` that the link is not revoked and
   public links are enabled), and serves the file of that revision with:
   `Content-Security-Policy: sandbox allow-scripts allow-modals; default-src 'none';
   script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src
   'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:;
   connect-src 'none'; form-action 'none'; base-uri 'none'; webrtc 'block'; frame-ancestors 'self'`,
   `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
   `Cross-Origin-Resource-Policy: cross-origin`, `Permissions-Policy` denying camera,
   microphone, geolocation, payment, usb and clipboard-read, and
   `Cache-Control: private, max-age=<seconds left>`. The policy is `cross-origin`,
   not `same-origin`, because the sandboxed document has the opaque origin `null`:
   for the browser every request it makes is cross-origin, so a `same-origin` policy
   blocks its own scripts, styles and images (verified in Edge/Chromium 155: the
   artifact rendered as bare HTML). The files of a frame are protected by the
   capability in the path; `/artifact-runtime/*` is public code.
   Module scripts and fonts of the frame send `Origin: null`; for `GET`/`HEAD` of
   `/artifact-frame/` and `/artifact-runtime/` the server answers
   `Access-Control-Allow-Origin: *` to that origin (all other paths still refuse it).
   Query strings are ignored: `frame_url` may carry `?theme=light|dark`, which the
   kit reads; relative paths inside the artifact do not inherit it.
   For the served `index.html` (text/html, that file only) the server inserts
   `<script src="/artifact-runtime/resize.js"></script>` before the last `</body>`
   (at the end if there is none) so artifacts built without the kit report their
   height. The stored file, its size and sha256 are unchanged; `resize.js` is
   embedded in the binary, does nothing when `window.AIWiki` exists at
   `DOMContentLoaded`, and sends `aiwiki:resize` only inside a frame.
   The server also inserts `<script src="/artifact-runtime/theme.js"></script>`
   before the first element: after a leading UTF-8 BOM, whitespace, comments and
   doctype (never before the doctype, which would switch to quirks mode); tags are
   not parsed. It runs before the first paint, reads `?theme=light|dark` from the
   frame URL and sets `data-theme` on `<html>`; the parser merges the attributes of
   the author's `<html>` later and keeps that value, so a static `data-theme` in
   the markup is overridden (a script can still change it). An artifact written
   without the kit follows the wiki theme with ordinary `:root[data-theme="dark"]`
   CSS. Both scripts are embedded and served with an ETag. A theme switch in the
   wiki reloads the frame, which discards the artifact's in-memory state.
3. The application embeds it as `<iframe sandbox="allow-scripts allow-modals"
   referrerpolicy="no-referrer" src="<frame_url>">` (never `allow-same-origin`,
   `allow-top-navigation`, `allow-popups`, `allow-forms`). Verified on 2026-10-22
   in Chromium (Edge headless): such a frame has origin `null`, cannot read the
   wiki's cookies, `fetch` is blocked by `connect-src 'none'`, scripts from the
   same server load through `'self'`, and the same CSP header also sandboxes the
   frame URL opened directly in a tab.
4. The kit is served at `/artifact-runtime/aiwiki-kit.js` and
   `/artifact-runtime/aiwiki-kit.css` (same origin, `'self'`).
5. Optional hardening: `AIWIKI_ARTIFACT_ORIGIN` serves frames from a separate host.

Residual risks, documented, not prevented: an artifact can navigate its own frame
to an external URL (leaking what the author or viewer typed into it and the
viewer's IP); it can consume CPU. It cannot read wiki data, cookies or the parent
page, call the wiki API, or open windows. The `webrtc 'block'` directive keeps the
frame from opening peer connections, but it is not supported by every browser; where
it is not, an artifact could use WebRTC to reach other hosts (a data exfiltration
channel besides navigation). Secrets travel in paths: `/s/<token>` and
`/artifact-frame/<cap>/…` appear in reverse-proxy and server access logs, browser
history and `Referer`-less navigations; mask or drop those path segments in logs
(for example log `/s/*` and `/artifact-frame/*` without the secret part). A user
capability is a bearer URL valid for 15 minutes: it is not revoked when the user's
rights change before it expires (it is revoked at once by deleting the artifact).
Public links are different: each request re-checks the link and its creator.

The frame may ask its parent to resize: `parent.postMessage({type:
"aiwiki:resize", height}, "*")`. The parent accepts it only from that iframe's
`contentWindow`, clamps the height to 120–4000 px, and ignores everything else.

## Public links

`GET /s/<token>` returns a minimal server-rendered page (no application bundle,
no cookies set, `Cache-Control: no-store`, `noindex`) with the title and the
sandboxed iframe for a `public` capability. The token is 32 random bytes,
stored as SHA-256. Pinned links show a fixed revision; unpinned show the latest.
Deleted artifacts, revoked links and disabled public links show the same neutral
"Ссылка недоступна" page with status 404. Requests are rate-limited per IP and
per token (in-process, documented).

## Articles

A fenced block embeds an artifact in an article:

````markdown
```artifact
<artifact id>
```
````

Readers without access see "Артефакт недоступен"; the article itself is unchanged.

## Text projection and MCP

The text projection (title, description, visible text of HTML and Markdown,
headers and first rows of CSV/JSON; at most 100 KB) feeds `q` search. MCP tools:
`wiki_artifact_list`, `wiki_artifact_read` (manifest, or one file with
`max_chars`/`offset` continuation pinned to a revision) and `wiki_artifact_write`
(create, or partial file changes with `base_revision`). MCP cannot share,
change visibility or delete.

## Implementation notes

Code: `internal/server/artifacts*.go`, SQL in `artifacts.sql`, `artifacts_2.sql` and `artifacts_3.sql` (component `artifacts`
v1, v2 and v3, after `corporate_moves`), tests in `artifacts*_test.go` (SQLite) and
`artifacts_postgres_test.go` (PostgreSQL).

Additions to the tables above. `artifacts` also has `file_count`, `projection`
(JSON: text of each file of the current revision) and `search_text` (lower-cased
title + description + projection, at most 100 KB, searched by `q` as AND of
substrings; lower-casing happens in Go so Cyrillic works in both databases);
`artifact_revisions` has `file_count`. A delete or restore is a revision too
(same manifest, `base_revision` required, message "Удаление артефакта" /
"Восстановление артефакта"). An update that changes nothing records no revision and
returns the current state. Request bodies are decoded strictly (unknown fields are
`400`), up to 28 MB; a stale `base_revision` answers `409` with
`{"error", "current_revision"}`. Paths are stricter than the pattern: any `..` is
refused, names are unique case-insensitively, a path cannot be both file and
directory. Bytes are stored under the key `art-<sha256>`; they are written before the
database transaction and read back with a size and sha256 check; unreferenced
objects (failed commits, deleted artifacts) are not collected in this stage.
`artifact_blobs` is written by the server but cannot be read or changed by the
runtime role at all (no SELECT policy; duplicates are detected through a savepoint).
MCP requests are limited to about 3 MB by the existing MCP body limit.

Rights, as enforced (RLS policies, a guard trigger, and the same rules in the API):

- `private` is the owner's alone, and only while the owner keeps reader (view) or
  editor (write) access to the container; managers, container owners and system
  administrators see nothing, not even in lists or search.
- **Deviation:** only the owner changes visibility. The table said "owner or
  manager+", but a manager who made an artifact private would lose sight of it, and
  PostgreSQL requires the updated row to stay visible to the updating session, so
  that cannot work with the "managers get no access" rule. `can_manage` equals
  "owner and editor+"; `can_share` = owner, or manager+ for `inherit` artifacts.
- Creation needs editor+ on the container; the creator becomes the owner
  (`owner_id`, in trusted mode the empty string). Owner and creation
  time are immutable, and so is the container, except for the project-move path
  below (guard trigger).
- Soft deletion revokes all links in the same transaction (a trigger in PostgreSQL,
  so an editor who cannot manage links still revokes them); restore does not revive
  them. New links are refused (`409`) for deleted artifacts and while public links
  are disabled; listing and revoking still work. A link works only while its creator
  is an active user who still may share the artifact (private: the owner with read
  access to the container; inherit: the owner with editor+, or manager+), checked
  by `artifact_link_valid` on every `/s/` request and every public-capability
  frame request, with the same `404` when false. Listing and revoking links
  (`artifact_can_manage_links`) need no writable container: the owner while the
  owner can read the container, and managers for inherit artifacts, also in an
  archived container; creating a link still needs a writable container. Delete and
  restore stay forbidden in an archived container (archive is read-only). The token appears only in the
  `201` response as `"url": "/s/<token>"` (relative); the UI composes the address.
- `GET /api/artifacts/{id}`, `/history` and `/files/…` of a deleted artifact are
  answered only to those who may restore it (editor+ for inherit, the owner for
  private); everybody else gets `404`, enforced by the row policy so also direct
  SQL, revisions and links see nothing. `?revision=` is accepted. The list
  excludes deleted ones.
- Request bodies are examined after rights: `PUT` loads the artifact and checks
  the right to edit before the body is read; `POST` reads the body, looks at
  `space_id`/`project_id` only and checks the container before any file is parsed.
  A caller without rights gets `403` (or `404`) without file validation.
- `q` uses at most 10 terms of at most 100 characters; the rest is dropped.

PostgreSQL: `artifacts`, `artifact_revisions`, `artifact_blobs`, `artifact_shares`,
`app_settings` have ENABLE + FORCE RLS and no DELETE policy (hard deletes and
`TRUNCATE` are impossible for the runtime role). Restrictive `mcp_scope_*` policies
keep MCP-scoped sessions from sharing, changing settings or writing in read mode; a
trigger refuses MCP visibility and delete changes. `app_settings` is readable by all
and writable only through a policy that calls `artifact_is_admin(...)` (a
SECURITY DEFINER lookup of `users.admin`), on top of the API check. Unauthenticated
requests (frame, public page) run on a connection whose session carries
`app.artifact_view` / `app.share_id` / `app.share_token_hash`, set only after the
capability was verified or the token hashed; policies grant exactly that live
artifact, link and its revisions. Narrow SECURITY DEFINER functions
(`artifact_revoke_shares`, `artifact_guard_project_move`, `artifact_follow_project_move`) read and update rows
through a probe predicate (`current_user <> session_user` and
`app.artifact_probe`), which only definer code can satisfy. Startup
(`validateRuntimeSchema` → `validateArtifactSchema`) refuses to run when the tables
lack FORCE RLS or a separate owner, expected policies or triggers are missing, any
delete/ALL policy exists, `app_settings` writes are not admin-only, or the helper
functions are executable by PUBLIC. Audit events (`artifact.create|update|delete|
restore|visibility|share.create|share.revoke`, `settings.update`) are written in
the same transaction without file content or titles; scoped MCP requests are not
written there (the table is closed to them) but are audited per request as
before. Trusted mode has no audit table.

Rendering: the capability payload is JSON `{a, r, e, k[, s]}`; it is verified in
constant time and must expire within 15 minutes (+30 s). `AIWIKI_ARTIFACT_SECRET`
(at least 32 bytes; shorter stops the start) is read at start, otherwise a random per-process secret is used; there is no built-in constant.
The frame CSP comes from one function (`artifactFrameCSP`) and the whole string is
tested. Every frame request re-checks deletion, link revocation and the global
switch; the user capability does not depend on the switch. `/s/<token>` is a
server-rendered page with a per-request CSP nonce, no cookies, `no-store`, `noindex`,
and one identical `404` page for malformed, unknown, revoked, deleted and disabled
cases. Rate limits are in memory per process and answered `429`: `/s/` 300
requests/min per client address and 600/min per link; `/artifact-frame/` 1200/min
per client address (public and user capabilities alike, a separate budget). The
client address is `RemoteAddr`, so without configuration all clients behind a
reverse proxy share one address and one limit. `AIWIKI_TRUSTED_PROXIES` (comma or
space separated CIDRs or IPs) names proxies whose `X-Forwarded-For` is believed:
only when `RemoteAddr` is in the list, the client is the rightmost
`X-Forwarded-For` address that is not itself a trusted proxy (a malformed chain
falls back to `RemoteAddr`). The tables are bounded (expired windows are dropped,
then arbitrary ones are evicted). A public page open longer than 15
minutes cannot load further sub-files of its frame (reload the page).
With `AIWIKI_ARTIFACT_ORIGIN`, `/artifact-frame/` and `/artifact-runtime/` are
served on that host only (on the application host they answer `404`, and the frame
host answers `404` to everything else), `Access-Control-Allow-Origin: *` is sent to
`Origin: null` on `GET`/`HEAD` there, `frame_url` is absolute, and
`frame-ancestors` is exactly the application origin (`-allowed-origin`; `'none'`
until it is known). Tested with two `Host` values, not against a real second host.

Known limitation: in trusted (SQLite, local files) mode the deduplication of file
contents is observable by timing, since a file that already exists is not written
again; with S3 every upload is written and there is no such difference.


Settings: `GET|PUT /api/admin/settings` exist in corporate mode only and need a
system administrator (the other users get `403`); trusted mode has no endpoint and
links are always allowed. Not implemented: artifact content in `GET /api/export`,
the `artifact` block rendering in articles (interface), object garbage collection.

## Moving a project

Moving a project (with its sub-projects) to another space (`/api/moves/apply`,
see `MOVES.md`) moves **all** artifacts of the moved projects with it, in the same
transaction: `inherit` and `private` ones, deleted ones too, including those the
person moving cannot see. Same-space moves change nothing for artifacts.

- Only `space_id` changes. Content, files, title, `revision`, `updated_at` and the
  revision history stay as they are; no new artifact revision is created (nothing
  of the artifact changed, and `artifact_revisions` does not depend on the space).
  This is the one exception to "the revision advances by one on every change" and
  is allowed by `artifact_guard` only inside the move function.
- The container of an artifact can be changed by nothing but the SECURITY DEFINER
  function `artifact_follow_project_move(root)` (v3). It runs only with the
  transaction marker `app.move='project'` and `app.move_root=<root>` set by the
  server, takes the target space from `projects.space_id` (the project rows are
  already moved and checked by `corporate_projects_no_move`), builds the subtree
  itself and updates artifacts through the probe predicate
  (`current_user <> session_user AND app.artifact_probe='1'`) and the policy
  `artifacts_probe_update`. A direct `UPDATE artifacts SET space_id` by the runtime
  role (or by the table owner) is refused or affects no rows.
- Bypass: v1 refused a move of a project with artifacts. v3 replaces that trigger
  by the deferred constraint trigger `artifact_projects_follow_check` on
  `projects(space_id)`: at commit, a project whose `space_id` changed must have no
  artifact in another space, otherwise the transaction is rolled back. So a project
  row updated by hand (markers set, roles satisfied) cannot leave artifacts behind.
- Access after the move is ordinary: the `inherit` artifacts follow the roles of
  the new container; a private artifact stays readable only by its owner and only
  while the owner has at least reader access in the new place (it becomes
  readable again when access is granted). A public link works while its creator is
  still allowed to share the artifact in the new container (`artifact_link_valid`),
  otherwise it answers 404 and works again if the right is restored.
- The preview reports `artifacts` (only those visible to the person moving, so
  private artifacts of others are not counted or revealed) and, for any move to
  another space, the warning about artifacts. The fingerprint covers the ids and
  revisions of the visible artifacts, so a change between preview and apply gives
  `409`. The `move.project` audit event carries `artifacts` (the real number moved,
  no ids).

### Moving a project: races and manual bypasses (schema v4)

There is no foreign key `artifacts.project_id -> projects`, so v4 adds database checks
instead (all deferred constraint triggers, judged at commit; violation = `42501`):

- `artifact_rows_container_check` on `artifacts` (insert, change of `space_id` or
  `project_id`): the artifact must live in the space of its project. This closes the
  race "an artifact is created in a project while the project moves": creation by
  MCP sessions does not share-lock the project, so the second of the two transactions
  to commit is rolled back (the move answers `409 concurrent change, retry`, a
  creation fails). The ordinary API takes the share lock and is unaffected.
- `artifact_projects_insert_check` (the same check after a project row is inserted)
  and `artifact_projects_no_delete`: the runtime role cannot delete projects (the
  application never does), so "delete and re-insert the project row with another
  space" is not possible for it. The database owner role is trusted and is not
  covered by these probes.
- `artifact_guard` allows the container change only if the new space equals the space
  of the artifact's project, in addition to the probe predicate and `app.move`.
- Startup: `validateArtifactSchema` verifies trigger types, events, columns and
  deferral, the expressions of `artifacts_probe_update`, key fragments of the guard,
  and that the runtime role is not a member of any role (the probe predicate relies
  on `current_user = session_user` for it). The server also logs a non-fatal warning
  with the number of artifacts outside their project's space
  (`artifact_stray_count()`).
- The subtree walk is limited to 64 levels both in the server and in the SQL function.
