# Section ranking iteration

The previous section search ignored article identity (title/summary/tags/properties) and sorted partial matches by raw matched-term count before relevance. Common task language and repeated synthetic notes could beat a section in the relevant article. This iteration keeps the lexical, global filtered search and revision-pinned read addresses, but combines article identity with local section evidence.

## Design constraints

- Keep `wiki_search` ranking unchanged. Preserve filters, deleted/archive behavior, snippets, duplicate-slug fallback and Unicode read offsets.
- Consider article title, summary, tags and property values as context for every section. Do not use other sections' body text as inherited evidence. Report context and local fields separately in `matched_in`.
- Give local heading/text evidence more influence than inherited ancestor headings. Article identity can retrieve a document even when its section headings are in another language. An exact article identity may outrank an incidental mention in another article. Do not present context-only matches as proof of a fact in a section.
- Downweight common terms using query-time inverse document frequency and normalize repeated body matches/section length. Partial matching must rank by weighted relevance, not simply number of common matching words. Remove only conservative Russian/English function words and retrieval imperatives; do not strip domain nouns, negation, identifiers, historical intent, or words selected from failed fixture questions.
- Keep all-terms mode before partial fallback, with `mode` evaluated against effective lexical query terms after normalization. Do not introduce query-specific aliases, hard-coded fixture identifiers, corpus answer literals, or a blanket penalty on historical documents. A historical query must still retrieve historical material.
- Metadata-only matches and lexical overlap remain ambiguous; no semantic, multilingual or negation-understanding claim. A short deterministic tie-break can favor document order for context-only siblings. No guarantee of 7/7: the old r06 question is intrinsically ambiguous.
- Prepare and freeze constants before looking at newly authored query scores. The old seven read queries are a development regression set, not held-out evaluation.

## Evaluation boundary

The configuration is fixed before the new-query run: own heading 6, ancestor
heading 2, body 4; article title 8, tags 5, summary 4, property values 3 (best
context field per term, not a sum). Body saturation uses k1=1.2, b=0.75 and an
average-length floor of 40 words. For each term, use max(local evidence, best article-context field), not their sum. There is no blanket context-only penalty: it would incorrectly demote an exact article identity below a cross-reference.
IDF counts each filtered article once, even if the term appears in many of its
sections. Ties first prefer weighted local section evidence, then updated time, article ID and section order. These are lexical
heuristics, not learned weights. Before evaluation, generic regression tests
exposed insufficient title weight relative to an incidental body mention; the
context weights were corrected and the complete Go tests passed. No scores from
the independently authored new query set were used to select these constants.

An interim old7-only development run then scored 5/7. Its remaining current-vs-
historical error exposed double counting of article identity and a cross-reference
to that identity in local text. A separate generic cross-reference regression
reproduced the problem. Before the first new-query evaluation, the combination
was changed from a sum with a 0.7 context-only penalty to the per-term maximum
described above; the initial run and model trials are retained as intermediate
development evidence. These development changes must not be described as gains on
unseen queries.

`not` is retained by the stopword filter, but the existing prefix matcher may
also match words such as `notes`; retaining negation is not understanding it.
Metadata-only siblings can occupy several results from the same article; there
is no diversification stage. An empty/whitespace body has no searchable sections
and should be found with article search instead. Property keys are not scored,
only their values. IDF and word-count arrays are rebuilt per query over the
filtered corpus; no scalability or cancellation guarantee is added.

Preserve `corpus.mjs`, previous reports and all recorded runs. New query manifest is authored independently from static corpus, without runtime scores, and frozen before candidate evaluation. Compare the previously published binary and the new binary on the same seeded document IDs/content per process. Report old and new sets separately, top-1/top-5/reciprocal rank, true no-hit accuracy, executable bounded-read addresses, payload cost and local latency. Repetitions measure stability rather than independent task count. Any changes after new-query results require a new version/run and disclosure; do not silently tune the frozen evaluation.
