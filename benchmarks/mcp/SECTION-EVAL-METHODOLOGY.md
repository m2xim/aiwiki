# Paired section-search evaluator

`section-search-eval.mjs` compares the frozen published section-search binary with a locally built candidate on exactly the same persisted corpus. It evaluates the frozen seven `readTasks` as a development set and the independently authored `section-queries-v2.mjs` as a separate query set. It makes direct scripted MCP calls; it does not call a model, tune queries, inspect labels to choose a result, or change ranking behavior.

## Run

From the repository root, with Node.js, Go, and the pinned benchmark tokenizer installed:

```powershell
node benchmarks/mcp/section-search-eval.mjs `
  --baseline-bin .cache/mcp-bench-20261021194432-c4b56a/aiwiki-bench.exe `
  --repetitions 20
```

`--baseline-bin` is required and must resolve to a file under this repository's `.cache`. The runner requires its SHA-256 to equal the confirmed published baseline `7e5e70b2f5f11d59366ead32f16e025319bb0605929fccbb79d5ff08269edfc4`; it refuses any other binary. `--repetitions` defaults to 20 and accepts integers from 5 through 1000. There is no external-server option: the runner binds child servers to loopback only.

`--baseline-sha256` optionally supplies the expected 64-digit hash; by default it uses the known Windows baseline hash above. The summary says whether that expected hash came from the audited default or was caller-asserted. `--baseline-source-ref` optionally supplies a 7–40 digit hexadecimal commit reference; its default is the declared short ref `3803cb0`. The source reference is metadata only and is explicitly marked unverified from the binary. A matching binary hash does not establish Git provenance.

Every run creates a fresh `.cache/mcp-section-eval-<UUID>` directory. The full raw `summary.json`, `traces.jsonl`, server logs, candidate binary, and data snapshots remain local in that directory. Standard output is a compact result without raw filesystem paths. The lead should review and sanitize any material selected for publication; raw traces can contain full queries, article contents, and local paths.

## Corpus and replica setup

The runner imports `corpus.mjs` and `section-queries-v2.mjs` dynamically and freezes their hashes at startup, before any section-search call. It requires exactly seven frozen `readTasks` and pins the v2 manifest's `SHA256(JSON.stringify(manifest))` to `15f56b747a42cd7f82ec3b124b52b6cee0ac4837125fe698504a0d1aef62e3c7`. The query source-file hash is recorded separately. For the corpus, both `SHA256(JSON.stringify(corpus))` and sorted-key canonical JSON SHA-256 are recorded. The query-set sorted-canonical hash is also recorded separately. The query module must export `makeSectionQueries()` returning `{version, queries}`; every query has a unique key, category, query text, relevant section anchors, and rationale. An empty `relevantSections` array labels a true no-hit query.

The baseline binary starts with a fresh data directory. The runner confirms the built-in spaces/articles, seeds all corpus spaces and documents through REST, resolves fixture links after article IDs exist, and checks `/api/export` for 33 articles with no unresolved fixture links. It then stops the seed process and copies the entire stopped data directory, including SQLite files, into independent baseline and candidate data directories. Replica fixture maps reuse the seed's key-to-ID map and verify those IDs against each export. Thus article IDs, content, links, revisions, and timestamps begin from one snapshot.

The candidate is built from this checkout with `GOTOOLCHAIN=local`, `GOPROXY=off`, and `GOFLAGS=-mod=readonly` (`go build -trimpath -mod=readonly -o <run-dir> ./cmd/aiwiki`). Go's build and temporary caches also live under the fresh run directory. Both replicas start from their copies. Each export must be the same fresh 33-document snapshot; canonical export hashes and fixture article/space IDs must agree. Each child receives only a loopback address and its own data directory. Setup, initialization, tool schema listing, and bootstrap traffic use the `setup` trace phase.

## Gold anchors and execution checks

Expected anchors come from exact title and one-based occurrence matches in `wiki_read(outline=true)` on the baseline replica. Ranking results are never used to construct gold labels. The authored query file defines section gold; the evaluator does not infer additional relevant sections from collision results. Old seven read tasks use their frozen article, heading, occurrence, and answer literal.

Before warmup or measurement, every distinct query is searched once on each replica in phase `verify`. Every returned top-five `read_args` is executed. The evaluator checks that its ID and revision match the hit and exported snapshot, then checks that the returned bounded text equals the exact expected character window from a full exported article body or a revision-pinned legacy anchored read. This includes built-in articles that happen to rank in the top five. Warmup and measured search payloads must have the same SHA-256 of canonical parsed JSON as the verification payload; a mismatch aborts the run. This also ensures measured read addresses were covered by the one verification pass. A mismatch or tool/protocol error aborts the run; it is not counted as a quality miss. Verification calls do not enter task cost or quality samples.

One outline verification pass resolves all expected headings before search. For every fixture article used by a gold label, the runner compares its local ATX heading sequence (title and level) with the baseline `wiki_read(outline=true)` result. It fails clearly on mismatches and on fenced code, setext underlines, or C/C# preprocessor directives, because the local section-boundary helper intentionally supports only this fixture corpus's ATX headings. Gold hits match article ID and exact heading; repeated headings additionally match the expected section's full-body offset when the search result uses an empty anchor. For new queries, authored labels remain authoritative, and a plausible collision alone does not create another gold label.

For each query, each replica receives one warmup search, followed by the requested number of paired searches at limit 5. Pair order alternates: baseline then candidate on odd repetitions, candidate then baseline on even repetitions. Warmups are retained in raw traces but excluded from result summaries. Search quality is scored from ranked search hits only; expected labels do not filter or reorder results. After scoring, both replicas are exported again; canonical exports must still equal the original seed export, and the summary records their hashes and `finalExportsUnchanged`.

## Measures

- **Top-1 coverage:** whether any authored relevant section is ranked first.
- **Top-5 coverage:** whether any authored relevant section appears in the first five.
- **Recall@5:** fraction of all authored relevant sections present in the first five. Multiple authored gold sections are all counted; any one is sufficient for coverage.
- **MRR@5:** reciprocal rank of the first authored relevant section, or zero when none appears.
- **No-hit:** empty-gold queries are reported separately as true-negative and false-positive counts. They do not enter recall, coverage, or MRR denominators.
- **Latency and payload:** per-search local HTTP round-trip milliseconds, raw JSON-RPC request/response bytes, and `cl100k_base` proxy tokens. The invocation proxy adds the token count of the tool name to the token count of `JSON.stringify(arguments)`; returned text is tokenized separately. This harness's values should not be compared as absolute token counts with the older runner's different accounting convention. Per-query samples and medians/p95 are retained; no token ratios are pooled across questions.
- **Old7 bounded fact answer:** after each old7 search, the rank-1 hit's executable bounded `wiki_read` is checked for both the frozen answer literal and the expected article/heading/occurrence. `found` requires both literal and source correctness; `literalFound` and `literalFoundFromWrongSource` expose literal-only matches separately. The report also flags cases where the bounded offset starts after the answer's known section position. These extra reads have their own trace phase and costs, outside search latency and payload figures.

Percentiles use nearest rank. Local timings include loopback HTTP and server work, with ordinary operating-system scheduling and SQLite cache effects; they are not model or production latency. Token values are a fixed proxy, not provider tokenization or billing. The synthetic corpus and small authored query sets measure only these fixtures. Keep old7 results labeled as development-set results and report the v2 query set separately. The full raw report stays local until the lead reviews and sanitizes it for publication.

## Output fields

The local summary records the expected and observed baseline hashes, whether the expected hash was the audited default or caller-asserted, the declared but unverified source reference, candidate binary hash and offline build settings, corpus and query-manifest versions/hashes, runner and methodology hashes, tokenizer/runtime environment, export snapshot hashes, resolved source keys and anchors, per-query verified payload hashes and stability, all per-query samples, paired medians, `read_args` verification outcomes, bounded fact-answer outcomes, child cleanup status, and request counts by trace phase. `traces.jsonl` records every REST and MCP call from setup, verification, warmup, measurement, and old7 fact reads, including raw request/response JSON, HTTP status, byte counts, token proxies where applicable, and local round-trip time. Network failures and response-body read failures are recorded when a request was attempted. Treat these files as raw local artifacts.
