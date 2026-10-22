# Load test results

Measured with `scripts/load-test.mjs` and `TestLoadCorporatePostgres` (contract:
[OPERATIONS.md](OPERATIONS.md) section 4). All numbers below come from the runs
described here; nothing is estimated. Date of the runs: 2026-10-22.

## Machine and versions

| | |
|---|---|
| CPU | AMD Ryzen 9 7900X, 12 cores / 24 threads |
| RAM | 63.7 GB |
| OS | Windows 11 Pro 10.0.26200 |
| Go | go1.26.0 windows/amd64 |
| Node | v24.14.1 |
| PostgreSQL | 16.15 (Alpine, x86_64-pc-linux-musl; shared server on 127.0.0.1:55433, database `aiwiki_load`) |

The machine was a shared developer workstation: other agents ran builds and
tests against the same PostgreSQL server at times, and the load generator runs on
the same host as the server. Treat absolute numbers as indicative.

## Workload

Per iteration of every virtual user (VU): 60 % `wiki_search`, 25 % read
(`wiki_read` with `outline=true`, then one section by `anchor`; both are timed
separately, so `read_outline` and `read_section` have equal counts), 10 %
`wiki_edit` op=`append` with `base_revision`, 5 % `wiki_write` (create). Corpus:
500 articles of roughly 2-8 KB with Russian and English text, in a space named
`load-test-<timestamp>`. A VU issues the next request as soon as the previous one
returns (no think time). Percentiles are nearest-rank over successful and
conflicting requests; failed requests are counted as errors and not timed.

## (a) Trusted mode, SQLite, one binary, 127.0.0.1:19421

```
go build -o .cache/load/aiwiki.exe ./cmd/aiwiki
# per run: fresh data directory, then
.cache/load/aiwiki.exe -addr 127.0.0.1:19421 -data .cache/load/data-<VUS>
node scripts/load-test.mjs --target http://127.0.0.1:19421 --vus <VUS> --duration 60 --articles 500 --out .cache/load/trusted-<VUS>.json
```

No token (trusted mode). 60 s per run, 0 errors in all three runs.

### 1 VU: 3767 requests, 62.8 req/s, 0 errors, 0 conflicts

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search | 1763 | 31.5 | 40.8 | 44.8 | 54.2 |
| read_outline | 776 | 0.6 | 0.8 | 1.1 | 2.5 |
| read_section | 776 | 0.4 | 0.6 | 0.8 | 3.0 |
| edit_append | 299 | 4.9 | 6.7 | 13.9 | 16.1 |
| create | 153 | 5.0 | 7.1 | 13.3 | 14.2 |

### 10 VUs: 3865 requests, 64.2 req/s, 0 errors, 0 conflicts

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search | 1854 | 130.5 | 437.2 | 631.0 | 1267.2 |
| read_outline | 788 | 96.3 | 390.3 | 590.5 | 793.4 |
| read_section | 788 | 99.3 | 406.6 | 659.5 | 913.3 |
| edit_append | 287 | 103.3 | 373.2 | 700.8 | 868.7 |
| create | 148 | 123.8 | 480.7 | 627.1 | 713.7 |

### 50 VUs: 4272 requests, 70.3 req/s, 0 errors, 1 revision conflict

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search | 2067 | 521.8 | 2020.8 | 3229.0 | 5507.0 |
| read_outline | 851 | 465.7 | 2067.8 | 2975.7 | 4010.6 |
| read_section | 851 | 462.3 | 2094.6 | 3493.0 | 4761.8 |
| edit_append | 324 (323 ok, 1 conflict) | 506.9 | 2005.3 | 2984.5 | 4070.3 |
| create | 179 | 462.6 | 2162.1 | 2887.3 | 3263.6 |

## (b) Corporate mode, PostgreSQL with RLS, `TestLoadCorporatePostgres`

In-process server (real `httptest` HTTP listener, corporate handler, OIDC mock
issuer for the login), PostgreSQL 16 through the restricted `aiwiki_app` role with
row level security. Five users, each with an own personal space, project and a
persistent MCP write key; each VU uses the key of user `vu mod 5`. 500 articles
are seeded (100 per user), so a search sees about 100 articles, not 500.

```
export AIWIKI_LOAD_POSTGRES=1
export AIWIKI_TEST_POSTGRES_OWNER='postgres://aiwiki_owner:...@127.0.0.1:55433/aiwiki_load?sslmode=disable'
export AIWIKI_TEST_POSTGRES='postgres://aiwiki_app:...@127.0.0.1:55433/aiwiki_load?sslmode=disable'
AIWIKI_LOAD_DURATION=60s AIWIKI_LOAD_ARTICLES=500 AIWIKI_LOAD_USERS=5 \
AIWIKI_LOAD_SKIP_EDITS=1 AIWIKI_LOAD_VUS=<10|50> \
go test ./internal/server -run TestLoadCorporatePostgres -count=1 -v
```

Two deviations from the contract mix, both forced by the product:

1. `AIWIKI_LOAD_SKIP_EDITS=1`: the 10 % edit share is executed as search instead,
   because `wiki_edit` through a persistent write key currently always fails (see
   "Defect found" below). Without the variable the test fails on those errors.
   The edit and conflict path is therefore not measured in (b); (a) covers it.
2. A persistent key is limited to 120 requests per minute
   (`mcpPersistentRateLimit`), i.e. 2 req/s per key; without a workaround the
   test gets HTTP 429 for almost everything. The limiter stays in the request path
   (one atomic `UPDATE mcp_credentials` per request), but the test zeroes
   `rate_requests` from the owner connection every 25 ms. The extra UPDATE on the
   same rows adds some contention that real use would not have.

### 10 VUs, 60 s: 11717 requests, 194.9 req/s, 0 errors

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search (incl. replaced edit share) | 6512 | 79.3 | 109.1 | 130.0 | 201.7 |
| read_outline | 2374 | 11.4 | 17.9 | 32.5 | 84.6 |
| read_section | 2374 | 11.3 | 17.3 | 28.6 | 43.8 |
| edit_append | 0 | - | - | - | - |
| create | 457 | 23.2 | 34.9 | 52.4 | 96.1 |

### 50 VUs, 60 s: 14867 requests, 246.5 req/s, 0 errors

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search (incl. replaced edit share) | 8366 | 240.5 | 395.8 | 490.9 | 657.3 |
| read_outline | 2961 | 125.4 | 269.0 | 354.3 | 693.0 |
| read_section | 2961 | 124.2 | 260.1 | 340.3 | 480.6 |
| edit_append | 0 | - | - | - | - |
| create | 579 | 142.0 | 277.7 | 351.3 | 421.6 |

## Defect found: wiki_edit fails with a scoped persistent write key

With `AIWIKI_LOAD_SKIP_EDITS` unset (10 VUs, 10 s), every `wiki_edit` returned
`database error` (208 of 208); the run fails. Cause, found by temporarily logging
the underlying error in `commitUpdate` (reverted afterwards):
`ERROR: new row violates row-level security policy "mcp_scope_article_fts_update"
(USING expression) for table "article_fts" (SQLSTATE 42501)`. `indexArticle`
(model.go) writes the full-text row with `INSERT ... ON CONFLICT (article_id) DO
UPDATE`; `mcp_scope.sql` creates a restrictive UPDATE policy on `article_fts`
(`NOT corporate_mcp_scope_active()`), while `mcp_persistent.sql` relaxes only the
`article_fts` insert and delete policies, not the update one. Creating articles
works (no conflict), updating any existing article through a scoped key does not.
Not fixed here; server code was left untouched.

## Interpretation

* **Trusted mode is bounded by a single database connection and by search.** The
  server opens SQLite with `SetMaxOpenConns(1)` (`internal/server/server.go`), so
  every request is serialized. Throughput is flat at 63-70 req/s from 1 to 50 VUs
  and extra VUs only add queueing: median latency grows roughly linearly with the
  number of VUs (read_outline p50 0.6 ms at 1 VU, 96 ms at 10, 466 ms at 50, while
  reading itself takes well under a millisecond). The cost is search, about 31 ms
  per query over 500 articles: 1763 searches x 31 ms is about 55 of the 60 s at 1
  VU, so the one connection is busy with search nearly all the time. Cheap
  operations (reads) pay for searches queued ahead of them. The tail is long (max
  5.5 s at 50 VUs) but no request failed or timed out.
* **Edit conflicts are rare in this mix.** One revision conflict in 4272 requests
  at 50 VUs over 500 articles; the client re-reads the revision after a conflict.
* **Corporate PostgreSQL scales further** (195 req/s at 10 VUs, 247 at 50 VUs,
  with a smaller per-user corpus), with a connection pool of 20 and 5 idle.
  Reads stay about 11 ms at 10 VUs. Search is again the most expensive operation
  (p50 79 ms at 10 VUs, 240 ms at 50) and is where throughput saturates between
  10 and 50 VUs; request counts grow only 27 % for 5x the VUs, so beyond about 10
  VUs the server, the database and the load generator share one machine and
  compete for CPU. The `mcp_credentials` counter UPDATE per request and the audit
  insert per MCP call are on the path of every request; their share was not
  profiled separately, so no claim is made about it.
* The numbers cannot be compared directly between (a) and (b): different
  databases, corpus per search (500 vs about 100), edit share replaced in (b), and
  the generator in the same process family as the server.
