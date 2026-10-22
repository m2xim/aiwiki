# Safe moves with access-impact preview (corporate mode)

Status: contract for implementation (2026-10-22). Trusted SQLite mode keeps its
current move behaviour and is not changed by this work.

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
`users_losing` count distinct active users after expanding teams. Names come
only from containers the actor manages, which the rules above guarantee.

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
space and a revision `Перенос вместе с проектом`. One `security_audit` event
`move.article` / `move.project` per move with source, target, counts and the
access diff summary (subject ids and roles, no content).

## Database safeguards

The `corporate_articles_no_move` trigger stays. A move is permitted only inside
the transaction that performs it: the server sets a transaction-local marker,
and the trigger additionally requires that the row's old and new containers both
grant the current user manager+ (or editor+ for a same-container parent change).
RLS update policies remain in force for every row. The project PATCH keeps
rejecting `parent_id`/`space_id`; project moves go through `/api/moves/apply`.
