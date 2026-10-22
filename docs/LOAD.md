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
AIWIKI_LOAD_DURATION=60s AIWIKI_LOAD_ARTICLES=500 AIWIKI_LOAD_USERS=5 AIWIKI_LOAD_VUS=<10|50> go test ./internal/server -run TestLoadCorporatePostgres -count=1 -v
```

One deviation from a real deployment: a persistent key is limited to 120
requests per minute (`mcpPersistentRateLimit`), i.e. 2 req/s per key; without a
workaround the test gets HTTP 429 for almost everything. The limiter stays in the
request path (one atomic `UPDATE mcp_credentials` per request), but the test
zeroes `rate_requests` from the owner connection every 25 ms. The extra UPDATE
on the same rows adds some contention that real use would not have.

The runs below include the fix for editing through write keys (see "Defect
found"); the full mix, edits included, was executed.

### 10 VUs, 60 s: 12384 requests, 206.2 req/s, 0 errors, 1 revision conflict

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search | 6020 | 76.1 | 102.9 | 142.5 | 277.5 |
| read_outline | 2475 | 13.3 | 35.2 | 90.9 | 214.8 |
| read_section | 2475 | 13.0 | 29.2 | 61.8 | 215.9 |
| edit_append | 949 | 28.8 | 49.5 | 93.0 | 255.1 |
| create | 465 | 23.2 | 45.4 | 109.3 | 217.7 |

### 50 VUs, 60 s: 16331 requests, 270.6 req/s, 0 errors, 2 revision conflicts

| op | count | p50 ms | p95 ms | p99 ms | max ms |
|---|---:|---:|---:|---:|---:|
| search | 7812 | 223.0 | 385.8 | 486.8 | 723.4 |
| read_outline | 3284 | 119.4 | 252.0 | 368.7 | 629.3 |
| read_section | 3284 | 119.9 | 247.9 | 337.0 | 516.0 |
| edit_append | 1291 | 143.7 | 293.1 | 425.2 | 551.3 |
| create | 660 | 134.7 | 258.9 | 322.5 | 408.5 |

## Defect found and fixed: wiki_edit failed with a scoped persistent write key

The first runs failed: every `wiki_edit` through a persistent write key returned
`database error` (208 of 208 in 10 s). The underlying error was
`new row violates row-level security policy "mcp_scope_article_fts_update"`:
`indexArticle` wrote the full-text row with `INSERT ... ON CONFLICT DO UPDATE`,
and the MCP scope policies allow write keys to insert and delete `article_fts`
rows but not to update them. Creating articles worked, updating did not. Fixed by
replacing the row with delete and insert (as SQLite already did), with an
end-to-end regression test (m2xim/aiwiki#20). `AIWIKI_LOAD_SKIP_EDITS=1` remains
available to measure a build without the fix.

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
* **Corporate PostgreSQL scales further** (206 req/s at 10 VUs, 271 at 50 VUs,
  with a smaller per-user corpus), with a connection pool of 20 and 5 idle.
  Reads stay about 13 ms and edits about 29 ms at 10 VUs. Search is again the most expensive operation
  (p50 76 ms at 10 VUs, 223 ms at 50) and is where throughput saturates between
  10 and 50 VUs; request counts grow only 32 % for 5x the VUs, so beyond about 10
  VUs the server, the database and the load generator share one machine and
  compete for CPU. The `mcp_credentials` counter UPDATE per request and the audit
  insert per MCP call are on the path of every request; their share was not
  profiled separately, so no claim is made about it.
* The numbers cannot be compared directly between (a) and (b): different
  databases, corpus per search (500 vs about 100), and
  the generator in the same process family as the server.
