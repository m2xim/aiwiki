# Instructions for AI agents

## Knowledge base (optional)

If an MCP server named `aiwiki` is configured in your client, use it as the knowledge store:

1. At the start of a task call `wiki_bootstrap` (pass the project's `space_id`) and read the pinned general and project rules.
2. Before researching, use `wiki_search` and `wiki_read` to find existing knowledge.
3. Record useful findings, sources, decisions and negative results with `wiki_write` / `wiki_edit`. Before changing an article, read its current revision and pass it as `base_revision`. On a revision conflict, re-read and reconcile; do not overwrite someone else's work.
4. If the wiki is unavailable, say so. Do not claim knowledge was read or saved when it was not.

Building, testing and contributing never require a running wiki, a private server or any specific endpoint. Do not add private addresses or credentials to the repository. Tool details: [docs/AI-CONNECTION.md](docs/AI-CONNECTION.md).

## Working on this repository

- Stack: Go (`cmd/aiwiki`, `internal/server`), SQLite via `modernc.org/sqlite` (trusted mode), PostgreSQL via pgx (corporate mode), React + TypeScript frontend in `web/`.
- Verify with: `go vet ./...`, `go test ./... -count=1`, then in `web/` `npm ci` and `npm run build`, then `node scripts/qa.mjs` from the repository root. Report only what you actually ran and its real output.
- Trusted SQLite mode is anonymous. Corporate PostgreSQL mode uses OIDC and RLS, requires S3 and separate owner/runtime database roles; see docs/CORPORATE.md. Native persistent MCP keys require an optional current-admission checker; see docs/MCP-PERSISTENT-ACCESS.md. Do not imply a tested production AD connection exists. PostgreSQL integration tests reset the configured disposable public schema.
- Use documentation addresses (`192.0.2.x`, `example.org`) in examples and tests.
- Do not commit data, databases, backups, `.env` files or secrets. Do not publish, push or tag without explicit approval from the maintainer.
