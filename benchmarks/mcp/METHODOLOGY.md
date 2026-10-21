# MCP baseline measurement contract

This is an offline, synthetic, scripted baseline. It does not measure an LLM's
reasoning, billing, or success in autonomously navigating the wiki. No private
wiki, identity provider or external model is needed. Production behavior is not
changed by this benchmark.

## Frozen corpus and ground truth

`corpus.mjs` exports `makeCorpus()` returning `{version, spaces, documents,
searchTasks, readTasks}`. Spaces have `{key,name}`. Documents have
`{key,spaceKey,title,summary,body,tags,properties}`. Search tasks have
`{key,category,query,relevantArticleKeys}`. Read tasks have
`{key,query,articleKey,headingTitle,headingOccurrence,answer}`. All keys are
stable fixture identifiers; server IDs are assigned during seeding and mapped.
The answer is a literal, unique fact in the relevant section, not a generated
claim. `headingOccurrence` is 1-based among headings with the same title.

Include small, medium and large multi-section articles, a large single section,
Russian/English wording, exact service names, aliases, paraphrases, morphology,
body-only matches, irrelevant keyword distractors, explicit links, duplicate
headings and a missing-term query. Ground truth must be authored independently
of the measured search ranking. Do not tune fixtures after observing scores
without changing the corpus version and describing the change.

## Measurements

Use only a new child server with a fresh temporary data directory under
`.cache/mcp-bench-*` and loopback. Never accept an external URL, reuse a server,
or modify application data. Record corpus checksum, source commit and dirty
state, server binary checksum, platform, Node/Go/tokenizer versions and options.
Build, seed and warm-up are outside measured query time and reported separately.
Warm up once, then at least 5 repetitions; alternate order of compared strategies.
The recorded baseline uses 20 repetitions. With only 5, nearest-rank p95 is
the maximum and must be labeled as such. Counterbalance ABC/CBA orders.
Use a monotonic timer and report local HTTP end-to-end median/p95, not model latency.

Measure full UTF-8 request/response JSON bytes separately from the model-facing
tool arguments and tool content text. Tokenize the latter with js-tiktoken
`cl100k_base`; record exact encoding/version. These are a fixed tokenizer proxy,
not Claude token counts, billed usage, complete chat serialization or an estimate
from character count. Record initialize instructions and tools/list schemas as
separate shared setup overhead, including the token count of both. Do not add
setup repeatedly or silently omit it from end-to-end comparisons.

Search: evaluate global top-5/top-10 article ranking against expected fixture
keys: recall and reciprocal rank; distinguish empty ground truth (no-hit accuracy)
and report AND/ANY fallback mode. Search is article-level today, not section-level.
Each query is evaluated independently; scripted read selection below is an oracle
and cannot be used to claim autonomous search/task success. Record whether the
target appeared in the returned page.

Read: compare (A) search + full read, (B) search + outline + anchored read, and
(C) search + direct anchored read, with C explicitly labeled known-anchor lower
bound. B selects the heading using fixture ground truth, so it measures payload
cost conditional on selecting the right document/heading. Verify the expected
fact is present; never hide failures from aggregates. Count all calls, requests
and responses, including search and outline. Compare tiny vs large documents and
large single sections; do not extrapolate one favorable size to all documents.

Write: two independent identically seeded articles per comparison, measured
full read + wiki_write(body,base_revision) versus outline + section read +
wiki_edit(replace_text,base_revision). Both use observed revisions. Verify the
final full body is exactly the expected single replacement and metadata is
unchanged. Verification traffic is outside task metrics and labeled separately.
Measure conflicting writes (exactly one winner), stale base revision rejection,
and historical section read after a heading rename; these are correctness checks,
not throughput claims. No ACL claims are possible on today's unauthenticated app.

## Output and interpretation

Persist a machine-readable summary and raw per-call traces in ignored `.cache`.
Trace records include phase, scenario, strategy, repetition, method/name, args,
result, request/response bytes, model-visible tokens and duration. No silently
truncated traces or unchecked tool-level `isError`. Distinguish expected errors.
P95 uses nearest-rank; include sample counts and never present tiny samples as
capacity/load measurements. Report paired payload ratios as well as absolute
sizes. Markdown results must state limitations and retain the unfavorable cases.

## Review clarifications

All read strategies use the identical search query/page size and the same oracle
document; a missing target in the page remains an explicit independent failure
flag, never a reason to discard a payload measurement. B obtains its anchor from
the actual outline. C obtains its anchor from an unmeasured setup outline; its
advantage is only a known-anchor payload lower bound, not a latency lower bound.

Assert unique literal answers across document bodies and absence from titles,
summaries and heading names. Record whether a search snippet already exposed an
answer, since such a task need not read more. Do not claim those tasks establish
the necessity of any read strategy. Freeze corpus/expected relevance before the
first scored run. A no-hit case means the authored relevance set is empty;
report whether the search actually returned an empty page separately.

Run search/read phases before writes, with search index readiness checked after
seeding. Every write repetition uses new, independent clones; verify revision
increments by exactly one and preserve all non-body editable metadata. Conflict
checks issue both requests concurrently with the same revision.

Treat model-visible tokens as the encoded tool-name/arguments and returned content
text (with any error text), not HTTP JSON escaping, role framing or cumulative
conversation replay. Record schemas/instructions/bootstrap separately and define
whether structuredContent exists. The corpus is mixed Russian/English, so do not
extrapolate its token ratios to monolingual corpora or other tokenizers. Verify
payload stability across read repetitions, accounting for changing request IDs;
store all verification calls. Publish per-task paired ratios rather than a mean
of ratios across unlike tasks.

This baseline decides what to prototype next; it cannot establish the value of
embeddings or a graph without implementing and comparing those candidates, nor
prove end-to-end agent context savings. A later frozen-model experiment needs
repeated matched tasks, explicit tool/schema/instruction accounting, answer and
source grading, and separately measured provider usage/cost.
