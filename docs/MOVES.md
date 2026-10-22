# Safe moves with access-impact preview (corporate mode)

Status: server side implemented (2026-10-22), covered by PostgreSQL tests in
`internal/server/corporate_moves_postgres_test.go`. Trusted SQLite mode keeps its
current move behaviour and is not changed by this work; the endpoints are not
registered there. The web interface is a separate piece of work against this
contract.

## Rules

| Move | Who may do it | Preview required |
| --- | --- | --- |
| Article: new parent in the **same** space and project | editor+ on that container (as today for edits) | no |
| Article subtree to another space and/or project | manager+ on the source container **and** manager+ on the target container | yes |
| Project (with its sub-projects) to another parent project or space | owner of the moved project **and** manager+ on the target parent (target space for a root project) | yes |

An article subtree always lives in one container (`parentOK` requires the parent
in the same space and project), so the whole subtree moves together, including
soft-deleted descendants, and the root may get a new parent inside the target
container. Archived source or target containers reject moves. Projects cannot
be moved under themselves or their descendants. Limits per move: 500 articles,
100 projects; larger moves are refused with 400 `move too large`.

## API

`POST /api/moves/preview` and `POST /api/moves/apply`, corporate mode only, CSRF
as for other unsafe requests. Request body (both):

```json
{
  "kind": "article" | "project",
  "id": "<article or project id>",
  "base_revision": 7,
  "target": { "space_id": "…", "project_id": "…", "parent_id": "…" },
  "fingerprint": "…"
}
```

- `base_revision`: article moves only, revision of the root article.
- `target.project_id`: article moves — target project or `""` for the space
  itself; `target.parent_id`: article moves — target parent article (must be in
  the target container) or `""`; project moves — target parent project or `""`
  for a root project of `target.space_id`.
- `fingerprint`: required for `apply`, ignored by `preview`.

Preview response `200`:

```json
{
  "fingerprint": "sha256 hex",
  "articles": 12,
  "projects": 0,
  "artifacts": 0,
  "source": { "space_id": "…", "space_name": "…", "project_id": "…", "project_name": "…" },
  "target": { "space_id": "…", "space_name": "…", "project_id": "…", "project_name": "…" },
  "access": [
    { "subject_type": "team", "subject_id": "…", "name": "Backend",
      "before": "reader", "after": null, "users": 5 }
  ],
  "users_gaining": 3,
  "users_losing": 5,
  "warnings": ["…"]
}
```

`access` lists every user or team whose effective role on the moved content
changes (`before`/`after` = role or `null`; `users` = active members for a
team). For project moves the comparison covers each project in the moved
subtree and reports the strongest change per subject. `users_gaining` /
`users_losing` count distinct active users after expanding teams. A user or team is listed by id and name only if the actor is manager+ on at
least one space or project that carries a grant of that principal in the compared
positions (the old or the new chain). Changes of all other principals are folded into
rows `{"subject_type":"other","subject_id":"","name":"","before":…,"after":…,"users":N}`,
one per (before, after) pair, where N is the number of distinct active users behind
those hidden principals (the number of principals is not disclosed; pairs with N=0
are omitted). Container names (`source`/`target`, space and project) are filled in
only if the actor has at least reader on that very space or project, otherwise they
are empty strings (the ids are always present). A single answer is used for every
permission refusal of a project move.

Apply recomputes the preview inside the move transaction, after taking the
existing ACL advisory lock, and compares fingerprints. Mismatch → `409` with
`{"error":"access changed","preview":{…fresh preview…}}`. Success → `200`
`{"articles": n, "projects": m, "root": <article or project>}`.

The fingerprint is a SHA-256 over a canonical encoding of: kind, id, target,
actor, the moved ids with their revisions (articles) or parent/space (projects),
and the sorted before/after principal-role sets. It is not a secret and grants
nothing by itself; apply re-checks every rule.

## Effects

Article move: the root and every descendant get the target space and project;
the root gets the target parent; each moved article gets a new revision
(message `Перенос` for the root, `Перенос вместе с родительской статьёй` for
descendants), the search index is updated, comments and attachments stay with
their articles. Project move: `parent_id`/`space_id` change for the project and
`space_id` for its descendant projects; articles in those projects get the new
space and a revision `Перенос вместе с проектом`. Artifacts of those projects (inherit
and private, deleted ones too) get the new `space_id` in the same transaction through the
SECURITY DEFINER function `artifact_follow_project_move(root)`, without a new revision
(content and history do not change); see `ARTIFACTS.md`, "Moving a project". One `security_audit` event
`move.article` / `move.project` per move with source, target, counts (for a project also the number of moved artifacts, no ids) and the
access diff summary (subject ids and roles, no content).

## Database safeguards

The `corporate_articles_no_move` trigger stays. A move is permitted only inside
the transaction that performs it: the server sets a transaction-local marker,
and the trigger additionally requires that the row's old and new containers both
grant the current user manager+ (or editor+ for a same-container parent change).
RLS update policies remain in force for every row. The project PATCH keeps
rejecting `parent_id`/`space_id`; project moves go through `/api/moves/apply`.

## Implementation notes (server)

Details fixed while implementing; the rules above are unchanged unless said so.

- **Where**: `corporate_moves.go` (handlers, validation, apply), `corporate_moves.sql`
  (migration component `corporate_moves`, version 1, applied after `corporate_acl`
  and `mcp_access`; the older migrations are untouched).
- **Preview vs apply**: both run the same planning code. Apply first takes the ACL
  advisory lock (`corporateLockOwners`), then recomputes and compares the fingerprint
  in constant time. `409 access changed` covers any difference in what the preview
  covered: grants, team membership counts, moved ids, and the revisions of all moved
  articles. A stale `base_revision` of the root is reported earlier as
  `409 revision conflict`.
- **Fingerprint**: SHA-256 over canonical JSON of `{v, kind, id, target, actor,
  base_revision, articles[(id, revision, parent)] or projects[(id, parent, space)],
  grants[(type, id, before, after, users)], users_gaining, users_losing}`, all lists
  sorted. It is computed over the *access diff* (principals whose effective role
  changes), not over complete before/after grant sets, so grants that are identical
  before and after do not influence it.
- **Access impact** is computed by the SQL function `corporate_move_impact(kind, id,
  target_space, target_container)` (SECURITY DEFINER, `search_path=pg_catalog,public`,
  EXECUTE revoked from PUBLIC). It reuses the recursive chain of
  `corporate_effective_role` (projects while `inherit_acl=1`, then the space only if the
  chain ends at a root project with inheritance) for the old and the new position; for
  projects the new position is evaluated hypothetically, with the moved subtree
  re-parented in the query. It returns only changes, never full grant lists: `principal`
  rows (users and teams, strongest change over the moved containers; `users` is the
  number of active members for a team and 1 for a user) and `user` rows (after
  expanding teams; used for `users_gaining`, `users_losing` and the self-lockout check).
  The function checks that the caller is the current `app.user_id` and holds the roles
  the rules demand (article: manager+ on both containers; project: owner on the moved
  project, manager+ on the target and on every project of the subtree) and raises
  otherwise. MCP-scoped sessions get at most `reader` from `corporate_effective_role`
  and are therefore refused; the handler also rejects them explicitly.
- **Additional rules not spelled out in the table**: the caller needs manager+ on every
  project of a moved subtree (otherwise RLS would hide those articles from the move); a
  subtree containing an archived project is refused; a project move that would leave the
  caller below editor on the moved content is refused (`403`); the root article must not
  be soft-deleted; an article subtree spanning several containers is refused (`409`);
  moving to the current container or the current parent is `400`.
- **Status codes**: `404` when the actor cannot read the object or the target, `403`
  when readable but the role is insufficient, `409` for archived containers, stale
  revisions, concurrent changes and `access changed`, `400` for bad input, cycles,
  and `move too large`.
- **Preview of a project move**: `source`/`target` describe the old and the new parent
  container (`project_id` is the parent project or empty for the space); `articles`
  counts only articles whose `space_id` changes (none for a move within one space).
  `warnings` are Russian, user-facing strings.
- **Artifacts in a project move**: `artifacts` counts the artifacts of the moved subtree that
  the actor can see (ordinary RLS; deleted ones only for restorers, private ones of other
  users never), and is 0 for a move inside one space. For every move to another space
  the preview carries the warning "Артефакты проекта переедут вместе с ним. …", whether or
  not the actor sees any artifact, so the warning reveals nothing. The fingerprint includes
  the visible artifacts (id, revision). Apply returns the same visible count in `artifacts`;
  the audit event holds the real number moved. All artifacts move, visible or not.
- **Same-container parent change** goes through the ordinary article `PUT`; changing
  `space_id` or `project_id` there is `400` with a hint to use `/api/moves/preview`.
  Restoring a revision still cannot change container or parent.

### Database safeguards (as implemented)

- Transaction-local settings `app.move` (`article` | `project`) and `app.move_root` are
  set by the server with `set_config(..., true)`. They are a switch, not a credential:
  any runtime connection can set them, so every guard also checks roles.
- `corporate_articles_no_move` (function `corporate_reject_article_move`, replaced in
  place): a parent-only change requires editor+ on the container; a container change
  requires `app.move='article'` and manager+ on the old **and** the new container; with
  `app.move='project'` it only lets articles follow a project that already lives in the
  new space (same project, same parent).
- New trigger `corporate_projects_no_move` (function `corporate_guard_project_move`,
  SECURITY DEFINER): a change of `parent_id`/`space_id` requires the marker; for the
  moved root it requires the owner role on the old position, manager+ on the target,
  a consistent target space, writable (non-archived) containers and no cycle; a
  sub-project may only follow its parent into the parent's space. The projects table has
  no role-based RLS, so this trigger is the database-level guard for projects.
- Policies `corporate_articles_project_move_read` (SELECT) and
  `corporate_articles_project_move` (UPDATE), permissive, active only with
  `app.move='project'`: during a project move the articles briefly have a stale
  `space_id`, for which neither the old nor the new row passes the ordinary policies
  (an `UPDATE ... WHERE` needs SELECT visibility of the old row too). The policies
  admit exactly those rows for an editor+ of the project at its current location.
- `validateRuntimeSchema` additionally verifies `corporate_move_impact` (SECURITY
  DEFINER, fixed `search_path`, not executable by PUBLIC, executable by the runtime role)
  and that both triggers exist and are enabled.

### Locking

Apply takes the ACL advisory lock, then locks the moved rows before it recomputes the
fingerprint: the article subtree (or the project subtree and the affected articles)
with `FOR NO KEY UPDATE`, re-reading the subtree until it is stable, because a child
creation that was already in flight may commit while the lock waits. Creating or
reparenting a child article reads its parent `FOR SHARE` (not `FOR KEY SHARE`, which
does not conflict with `FOR NO KEY UPDATE`), creating an article inside a project
shares the project row (delegated MCP sessions cannot lock it, since they have no UPDATE policy on projects; for them the race with a concurrent project move is not closed by locks), and creating a sub-project shares the parent project row.
So a child is either rejected after the move commits (parent in another container)
or included in the move/rejected with 409. As a last line of defense the database
checks that a parent article is in the same space and project on insert, on a
parent change, and for the moved root (`corporate_check_article_parent`,
`corporate_reject_article_move`), and that a parent project is in the same space on
insert (`corporate_check_project_parent`) and on a move.

### Fingerprint and hidden principals

The fingerprint is computed over the visible form of the preview (the rows the actor
sees, including the aggregate `other` rows and the user counts), not over the hidden
grants. A hash of hidden data would let the actor test guessed grants offline; the
visible form reveals nothing new. A change among hidden principals that leaves every
visible row (including the aggregates) identical does not invalidate a preview, and
the actor could not have taken it into account anyway. The self-lockout check uses the
actor's minimum resulting role over every compared position (a separate `self` row of
`corporate_move_impact`), not the strongest-change row; given the other rules it can
only trigger on inconsistent data.

### Known limits

A runtime connection that sets the markers itself and is manager+ on both containers
can still move an article without a preview through direct SQL (the preview is
enforced by the application, the roles by the database). That is the contract's
intended trade-off; non-managers are stopped by the trigger.

The owner of a project can set the markers by hand and update only the project row
(`projects.space_id`). The articles of that project then keep the old `space_id`; the
read policy sees no role for the mismatching pair, so they become invisible to every
user until the database owner repairs them. At startup the server logs a non-fatal
warning with the number of such articles (`corporate_move_mismatch_count()`, a
SECURITY DEFINER count-only probe; the permissive policy
`corporate_articles_mismatch_probe` lets only the table owner's context see such
rows, so users cannot read them through it).

That manual bypass is closed for artifacts: the deferred constraint trigger
`artifact_projects_follow_check` refuses, at commit, a change of `projects.space_id`
that leaves an artifact in another space (the whole transaction is rolled back).
The owner of a project therefore cannot strand its artifacts; articles still can be
(see above).
