# Scripted section-search candidates

This addendum defines the candidate runs in `run.mjs`. The frozen `corpus.mjs`
(`mcp-baseline-v1`) and the published baseline results remain unchanged. The
baseline methodology still describes A/B/C and write measurements. Its statement
that search is article-level describes the baseline search API and A/B/C; it does
not describe candidate D, which exercises the new section-search API.

These are offline scripted measurements against the runner's fresh child server.
They invoke no paid model and make no claim about model reasoning, billing, or
autonomous agent behavior. The candidate grading labels are used only after the
script has chosen and read a result.

## Strategies

For each frozen `readTask`, D sends its original `task.query` to
`wiki_search_sections` with `limit: 5`. It selects result zero exactly as ranked,
without inspecting the expected article or heading. If a hit exists, D executes
that hit's `read_args` with `max_chars: 4000`, then follows each returned
`next_read` object directly as the next `wiki_read` arguments. It stops once the
literal answer appears in collected read text, when the snapshot ends, or at the
128-page safety cap. Search snippets are recorded separately. D still performs
the read when a search snippet already contains the answer; the report calls
this `forcedReadAfterSnippetAnswer` to make that extra work visible.

The answer-literal stopping condition uses fixture ground truth. D's section
selection is un-oracled, but its read stopping rule is oracle-assisted and is
reported as such; it does not represent an autonomous agent deciding when it
has enough evidence.

E is an explicitly oracle-conditioned lower bound. It starts with the expected
article and anchor from fixture setup, pins the seeded revision, and reads in
4000-character pages using the same continuation rule and 128-page cap. It
stops as soon as the literal answer is found or the section ends. E does not
include a search request.

The expected article and anchor are used to grade D/E after selection. A
candidate run is verified only if it reads the literal answer from the expected
article and expected anchor without a tool error or page-cap exhaustion.
Quality misses are retained in the candidate records and do not by themselves
fail the benchmark process. Infrastructure, protocol, and separate correctness
check failures retain the runner's normal failure behavior.

## Measurements and interpretation

Candidate call traces use the normal JSON/HTTP byte, cl100k token-proxy, and
monotonic local request-duration definitions from `METHODOLOGY.md`. D's costs
include section search and all bounded read pages; E's include every bounded
read page. Shared initialize instructions, `tools/list` schemas, and bootstrap
are measured once under `setupOverhead`, including the
`wiki_search_sections` schema. They are reported separately from per-task
candidate costs and are not repeatedly charged to each candidate call.

The summary reports D/E proxy metrics per task, per-task verification counts,
answer/article/anchor grading, continuation page counts, and payload stability
across measured repetitions. It does not calculate pooled D/E cost ratios or
cross-task pooled cost summaries. D's `top5Coverage` is a separate article plus
anchor hit rate against the known target, evaluated over all measured task and
repetition pairs; it does not influence D's top-1 choice. Answer-in-snippet and
forced-read-after-snippet-answer counts are also reported separately.

The per-task rows are separated by both strategy and task. A returned bounded
page must match the requested article, anchor, and revision, and each
`next_read` must preserve those addresses. Missing top-1 results and wrong
sources are quality misses; malformed reads, tool errors for returned read
addresses, and address/revision mismatches are classified as protocol failures.
A `wiki_search_sections` tool error is stored in `searchError` and classified as
a section-search protocol failure, separately from bounded-read errors. Protocol
failures are sent through the runner's fatal failure accounting and make the
run finish with failures; ordinary ranking, source, answer, or page-cap misses
remain scored quality outcomes.

`selectedReadOffset` records the initial requested offset (zero when omitted).
`expectedAnswerOffset` is always the answer's code-point offset within the
fixture's expected article and anchor. When the selected article and anchor
match that target but the answer occurs before the initial read offset, the
quality miss is `answer_before_read_start` rather than `answer_not_found`.

Each read repetition receives the same candidate query and frozen seeded
content. Payload stability hashes normalized tool names, arguments, and result
objects, so request IDs and transport envelope changes do not masquerade as
content drift. Any unstable task is named in `payloadStability`.

## Correctness outside measurement

Correctness traffic is labeled `phase: correctness` and is excluded from D/E
task metrics:

* The large `r06` section is read in 4000-character pages and concatenated. Its
  text must exactly equal a full section read at the same revision.
* A bounded read of the large `r04` section is started before editing a clone.
  After the edit advances its revision, the saved `next_read` continuations must
  keep returning the original revision and reconstruct the original section
  exactly. The edit is deliberately between the first page and continuation;
  pages from different snapshots are never combined.

The output keeps these checks under `correctness.byCheck` and `correctness.runs`.
They are correctness evidence, not throughput or candidate-quality samples.
