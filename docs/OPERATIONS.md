# Operations: metrics, readiness, access log, load test, storage quotas

This document is the contract for stage 6 (operations). Each section names the
settings, the observable behaviour and what must never happen. Everything here
is off by default: an existing deployment that sets no new variable behaves
exactly as before, apart from the new `GET /api/ready` route.

## 1. Readiness

`GET /api/ready` is served on the main listener in both modes, without login,
like `/api/health`.

* `/api/health` stays a liveness check: the process answers.
* `/api/ready` answers `200 {"ok":true}` when the database answers
  `PingContext` within 2 s and, in corporate mode with S3 configured, the bucket
  answers a HEAD/stat request within 2 s. Otherwise `503 {"ok":false}`.
* The response never names the failing dependency, a host, an error text or a
  version. The reason is written to the server log, at most once per 30 s per
  dependency.
* The result is cached for 5 s, so a load balancer polling every second does
  not add database load. Concurrent callers share one check (no thundering herd).

## 2. Metrics

`AIWIKI_METRICS_ADDR` (for example `127.0.0.1:9464`) starts a second HTTP
listener that serves only `GET /metrics` (Prometheus text format 0.0.4) and
`GET /api/ready`. Empty means no listener and no collection overhead beyond
atomic counters.

* A non-loopback address requires `AIWIKI_METRICS_TOKEN` of at least 32
  characters; the scrape must send `Authorization: Bearer <token>` (compared in
  constant time). A non-loopback address without the token is a startup error.
  On a literal loopback IP (127.0.0.0/8, ::1) the token is optional and enforced when set; the name `localhost` counts as non-loopback and needs the token.
* No new Go dependency: the exposition is written by hand.
* Labels never contain identifiers, titles, user names, e-mail addresses,
  search queries, tokens, paths with values, or any other unbounded value.

Metrics (names are part of the contract):

| Name | Type | Labels |
| --- | --- | --- |
| `aiwiki_build_info` | gauge = 1 | `version`, `go_version`, `mode` (`trusted`/`corporate`) |
| `aiwiki_http_requests_total` | counter | `route`, `method`, `code` (`2xx`, `3xx`, `4xx`, `5xx`) |
| `aiwiki_http_request_duration_seconds` | histogram | `route`, `method` |
| `aiwiki_http_in_flight_requests` | gauge | — |
| `aiwiki_mcp_tool_calls_total` | counter | `tool`, `outcome` (`ok`, `error`) |
| `aiwiki_mcp_tool_duration_seconds` | histogram | `tool` |
| `aiwiki_rate_limited_total` | counter | `limiter` (fixed names, for example `mcp_oauth`, `artifact_token`, `artifact_public`, `artifact_frame`) |
| `aiwiki_auth_logins_total` | counter | `outcome` (`ok`, `expired`, `failed`, `denied`, `unavailable`) |
| `aiwiki_db_open_connections`, `aiwiki_db_in_use_connections`, `aiwiki_db_idle_connections` | gauge | `pool` (`app`, `auth` when present) |
| `aiwiki_db_wait_count_total` | counter | `pool` |
| `aiwiki_db_wait_duration_seconds_total` | counter | `pool` |
| `aiwiki_ready` | gauge 0/1 | — (last readiness result) |
| `go_goroutines`, `go_memstats_heap_alloc_bytes`, `process_start_time_seconds` | gauge | — |

Histogram buckets: 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, +Inf.

**Route label.** `route` is the `net/http.ServeMux` pattern that served the
request (for example `GET /api/spaces/{id}/audit`). The catch-all pattern `/`
is labelled `static`, except for paths under `/api/`, `/files/` and `/mcp`,
where it only produces a 404 or an authentication refusal and is labelled
`other`. When no pattern matched, the label is `other`.
`ServeMux.ServeHTTP` writes `Pattern` into the `*Request` it received
(`net/http/server.go`, Go 1.26: `h, r.Pattern, r.pat, r.matches = mux.findHandler(r)`),
and every `WithContext` in between makes a copy, so the outer middleware cannot
read it. The outer middleware therefore puts a small holder into the context;
every mux is wrapped so that after its `ServeHTTP` returns it stores its
`r.Pattern` into the holder **only if the holder is still empty**. The innermost
mux returns first, so the most specific pattern wins; the corporate outer `"/"`
delegating to `routes()` does not overwrite it. `tool` is the MCP tool name only
when it is one of the registered tools, otherwise `unknown`.

## 3. Access log

`AIWIKI_ACCESS_LOG=json` writes one `log/slog` JSON line per request to stderr:
`time`, `request_id`, `method`, `route` (same rule as above), `status`,
`duration_ms`, `bytes`. Default `off`. Never logged: query string, path values,
headers, cookies, bodies, user identity, client IP (the IP is already in the
audit log where it is needed).

Every response gets `X-Request-Id`: a fresh random 16-byte hex value. An incoming
`X-Request-Id` that matches `^[A-Za-z0-9._-]{1,64}$` is logged as
`upstream_request_id`; it is never echoed back and never replaces the own id.

## 4. Load test

`scripts/load-test.mjs` (Node, no dependencies) and the gated Go test
`TestLoadCorporatePostgres` (runs only with `AIWIKI_LOAD_POSTGRES=1` and the
usual `AIWIKI_TEST_POSTGRES_OWNER` / `AIWIKI_TEST_POSTGRES`).

* Workload mix per virtual user: 60 % search, 25 % read (outline, then one
  section by anchor), 10 % small edit (`append`), 5 % create. Seeded corpus:
  configurable, default 500 articles of 2–8 KB with Russian and English text in
  a dedicated space named `load-test-<timestamp>`.
* The script talks to `/mcp` (JSON-RPC `tools/call`) with a bearer token from
  `AIWIKI_LOAD_MCP_TOKEN`, or to the trusted-mode HTTP API without a token.
  It refuses a non-loopback target unless `--allow-remote` is given, and never
  prints the token.
* Output: per operation count, errors by status, p50/p95/p99 and max latency,
  overall throughput; as a table on stdout and as JSON with `--out`.
* `docs/LOAD.md` records the measured numbers, the machine, the versions and the
  exact commands. Numbers are from real runs, never estimated.

## 5. Storage quotas (corporate mode)

`AIWIKI_SPACE_QUOTA_BYTES` sets the default storage limit per space; `0` or
empty means unlimited. A system administrator can override the limit for a
single space (`spaces.storage_quota_bytes`, `NULL` = default, `0` = unlimited)
through the existing administration API; nobody else can change it.

* **Counted:** the bytes of distinct stored objects referenced by the space —
  attachments of its articles and the files of all revisions of its artifacts,
  deleted (restorable) ones included, each content hash once per space.
* **Enforced** on attachment upload and on artifact create/update (HTTP and
  `wiki_artifact_write`). The check and the insert happen in one transaction
  under `pg_advisory_xact_lock` keyed on the space, so two parallel uploads
  cannot both pass the limit.
* Usage is computed by a `SECURITY DEFINER` function that sees rows hidden by
  RLS (private artifacts of other users count against the space). It returns
  one number and is executable only by the runtime role.
* **Errors:** `413 {"error":"space storage quota exceeded"}`; the response does
  not contain the usage or the limit, because the usage would reveal the size of
  content the caller cannot see.
* **Visibility:** managers and owners of a space and system administrators see
  `used` and `limit` for that space; nobody else does.
* Moves (`docs/MOVES.md`) are not blocked by quotas; a space may end up above
  its limit after a move and then only accepts deletions until it is below.
* Trusted (single-user SQLite) mode has no quotas.
