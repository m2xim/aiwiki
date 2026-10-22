# Interactive artifacts

Status: contract for implementation (2026-10-22). Owner decisions: artifacts are
arbitrary HTML/JS run in a sandbox plus a built-in kit of ready blocks; a public
link opens without sign-in and a system administrator can disable public links
organisation-wide; an artifact is a versioned object of a space or project.

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
history checks the container current for the artifact (artifacts are not moved
in this stage).

## Rights

| Action | inherit | private |
| --- | --- | --- |
| list, view, read files, history | reader+ on container | owner |
| create | editor+ on container | editor+ on container |
| update files/title/description | editor+ | owner |
| change visibility | owner or manager+ | owner |
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
   (≤ 15 minutes), kind (`user`|`public`). Secret: `AIWIKI_ARTIFACT_SECRET`
   (required for several instances) or a random per-process value. The frame
   path is the authorisation; cookies are neither needed nor used.
2. `GET /artifact-frame/<cap>/<path>` verifies the HMAC and expiry, re-checks that
   the artifact is not deleted (and for `public` that the link is not revoked and
   public links are enabled), and serves the file of that revision with:
   `Content-Security-Policy: sandbox allow-scripts allow-modals; default-src 'none';
   script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src
   'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:;
   connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'`,
   `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
   `Cross-Origin-Resource-Policy: same-origin`, `Permissions-Policy` denying camera,
   microphone, geolocation, payment, usb and clipboard-read, and
   `Cache-Control: private, max-age=<seconds left>`.
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
page, call the wiki API, or open windows.

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
