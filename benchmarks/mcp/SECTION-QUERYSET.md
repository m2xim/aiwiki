# Section query set v2

This directory contains a new, query-only global section retrieval evaluation set. Its module exports `makeSectionQueries()`, which returns `section-queries-v2` with 22 queries and section-level judgments. It leaves the frozen fixtures at `mcp-baseline-v1` unchanged.

The set has 17 positive queries and five true no-hit negatives. Positive labels use only article keys, exact section heading titles, and 1-based heading occurrences present in the frozen `corpus.mjs`. Where the query can reasonably identify more than one section, every defensible section is listed. Negative queries name absent subject areas whose distinctive content terms do not occur in the corpus, including its generated filler; they do not depend on obscure aliases or guessed spelling variants.

Coverage includes article identity with heading clues, metadata-only clues, English and Russian phrasing, inflection, a code identifier, repeated headings, close distractors, natural questions, explicit historical intent, and short and verbose queries. No query reproduces a corpus body sentence verbatim. The unique literal answers used by the frozen read tasks are not included in queries.

The queries were authored by the evaluation-set author and reviewed against the static frozen corpus. They are not production-heldout judgments or evidence of production recall. No implementation worker received the query set before it was frozen; this statement records the set's handling in this task, not a claim that these judgments were independently generated or held out from every model.

The frozen corpus is the sole source of positive gold labels. No runtime search code, previous scores, traces, or result files were consulted, and no search, server, or model was run to produce or validate the judgments.

## Integrity

SHA-256 is calculated over the UTF-8 bytes of the exact `JSON.stringify(makeSectionQueries())` result, with no added whitespace or trailing newline:

`15f56b747a42cd7f82ec3b124b52b6cee0ac4837125fe698504a0d1aef62e3c7`

The preceding hash `4bb6136bec386796c94ab35caf9daf1582ee3c4f9c764ce1a59b77d0ac18d53e` is superseded solely by static taxonomy and rationale corrections made before evaluation. Query texts and gold section judgments were unchanged.
