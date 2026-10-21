# Corporate core validation — 2026-10-22

This is a development core for a pilot, not evidence of a production AD/S3 deployment. The existing home checkout and deployed instance were not migrated or deployed.

## Checks performed

- `go vet ./...` passed.
- `go test ./... -count=1` passed with PostgreSQL integration enabled against a disposable PostgreSQL 16.15 database, separate non-superuser owner/runtime roles and FORCE RLS.
- The focused child-access/S3 test additionally verifies that catalog counts and tags exclude inaccessible parent documents.
- `npm ci` reported zero vulnerabilities; `npm run build` passed. Vite still reports chunks above 500 kB; this is not a bundle-size optimization change.
- `node scripts/qa.mjs`: 15 integration checks passed, including SQLite compatibility, original attachment bytes, concurrent writes and MCP.
- `node scripts/qa-diff.mjs`: 200 reproducible reconstruction cases and boundary checks passed.
- MCP benchmark graders: 17 tests passed, without new paid model calls. This stage does not claim improved search relevance or end-to-end token savings.
- Browser verification against the isolated PostgreSQL/signed OIDC mock/S3 mock preview: separate management page, URL section selection, refresh, browser Back, creation of a personal space/article, return to the exact article URL after refreshing management, and restored sidebar resize handle.
- Three Opus architecture/code review passes informed integration fixes; the final focused pass reported no new blocking findings. The lead inspected changes and ran checks independently of implementer reports.

## Security cases covered

Tests exercise signed OIDC validation and introspection, browser-bound login state/nonce/PKCE, CSRF, disabled users and removed admission roles, per-user favorites, separately granted child projects with hidden ancestors, inherited and closed project roles, manager escalation rejection, atomic concurrent grant updates, historical container checks, concurrent article revisions, connection identity cleanup including forgotten transactions, rejection of unsafe runtime database roles, real AWS SDK signing against a mock endpoint, unauthorized uploads and revoked downloads before object-store access.

Review fixes include removing stale JWT role fallback, preventing manager-to-owner promotion when creating child projects, replacing full ACL replacement with atomic grant operations, checking affected rows after RLS writes, rechecking privileges under the ACL lock, preventing inherited runtime table ownership, separating authentication connections from content scopes, releasing database scopes before slow object transfers, and correcting MIME sniffing after file-size probing.

## Limits for pilot acceptance

- Configure and test the real Keycloak realm, AD federation, token audience/role mappers, token lifetime and directory revocation/cache behavior.
- Verify the chosen S3-compatible service, TLS, IAM/bucket policy, backups, retention and orphan reconciliation. Local tests use an SDK-backed signed mock, not AWS or MinIO.
- Sessions are short-lived and in memory; refresh/multi-instance session persistence is not implemented.
- Native scoped personal MCP tokens, service accounts and OAuth/client discovery are the next access stage. Current corporate MCP uses signed/introspected OIDC bearer tokens.
- Article container moves, project reparenting and legacy bulk export are blocked in corporate mode pending permission-safe workflows. Rich artifacts and planned import/export features are separate work.
- System administrators govern teams and can explicitly add themselves to a team; self-addition is audited. Direct project creator grants persist until separately revoked.
- Search still ranks authorized rows using the existing application algorithm. PostgreSQL full-text/vector indexes and large-corpus performance are not evaluated here.
- The generated dependency inventory reports upstream packages for which no local standalone license file was found; see `THIRD_PARTY_NOTICES.md`. It does not claim a complete inventory of absent optional platform packages.

Setup and runtime configuration: [CORPORATE.md](CORPORATE.md).

## Temporary MCP access follow-up — 2026-10-22

The next slice adds [temporary exact-project read-only keys](MCP-TEMPORARY-ACCESS.md).
Sonnet implemented the session/key lifecycle; gpt-6-luna implemented the scope
migration, UI and optional S3 smoke harness. The lead integrated and inspected
the changes and ran the full Go suite with PostgreSQL, vet, frontend build and
the existing 15 HTTP/MCP regressions. Opus reviewed the architecture and final
code; the final code pass found no P0/P1 issues. Its remaining startup-grant and
Unicode-audit recommendations were incorporated.

New checks cover key expiry/revocation/logout/admission loss/identity-provider
outage, per-user/global caps, rate limiting before introspection, no secret in
metadata/audit, CSRF, live owner ACL revocation, exact-project search/history/
section isolation, content and metadata RLS, read-only RPC, forbidden direct
owner-role execution, missing-protection startup refusal and scope cleanup.

After explicit owner confirmation, the browser flow passed on the isolated
PostgreSQL/signed mock OIDC fixture: select one project, issue a key, verify the
secret field without logging its value, clear it, refresh, confirm that only
key metadata returns, and revoke. The active list was empty after revocation.
The earlier automatic-review block was resolved by that explicit confirmation,
not bypassed. Automated Go/HTTP lifecycle checks had already passed separately.

The production container built successfully; its base image contains trusted CA
certificates. An optional real-service S3 test was added, but actual S3 service
coverage remains pending: the official MinIO Quay pull returned unauthorized and
the official Windows download listing returned HTTP 410. No substitute mirror
was used, and a skipped smoke test is not counted as a storage-service pass.

Long-lived PATs, service-account sponsorship/lifecycle, OAuth discovery and
production Keycloak/AD/S3 acceptance remain open. The existing home installation
is untouched.
