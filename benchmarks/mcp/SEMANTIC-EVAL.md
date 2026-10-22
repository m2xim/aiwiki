# Lexical, vector and hybrid section search

Question: does semantic (embedding) search find the right section where the
current lexical search does not, and what does it cost? Measured 2026-10-22 on
the frozen corpus of `corpus.mjs` (30 articles, 71 sections, 263 chunks).

## Method

* `semantic-eval.mjs` starts a fresh server, seeds the corpus and builds the
  section units from the server's own outline and anchors. Lexical results come
  from `wiki_search_sections` (limit 20, `semantic: false`). Vector results are
  exact cosine over all chunks (best chunk per section). The embedding endpoint
  must be local; nothing is sent to an external provider.
* **Query sets.** Development: `section-queries-v2` (17 positive, 5 negative)
  and the old seven read tasks. Held-out: `semantic-queries-v3` (20 positive:
  paraphrases, Russian/English crossings, exact identifiers; 6 near-domain
  negatives) and `semantic-queries-v4` (16 positive, 5 negative). Both held-out
  sets were authored from the static corpus and committed with their SHA-256
  before any run against them (commits `0050e0b`, `9423bfa`); the script refuses
  to run if they change.
* **Threshold.** The vector cut-off for "nothing relevant" is chosen on the
  development sets only (best top-5 coverage plus true no-hit count).
* **Metrics.** Over the positive queries: top-1 and top-5 coverage (a gold
  section at rank 1 / within 5), MRR@5, recall@5. Over the negative queries: true
  no-hit (empty result).
* **Order of decisions, disclosed.** v3 was run first with lexical, vector and
  plain RRF. After looking at v3, the variant "only full lexical matches take
  part" (`hybridAllOnly`) was added. Because that choice was informed by v3, v3
  is no longer a held-out set for it, and v4 was written and frozen afterwards to
  test the choice. v4 numbers are the unbiased ones for `hybridAllOnly`.

## Results

### Qwen3-Embedding-0.6B (multilingual, GGUF Q8_0, LM Studio)

Query prefix `Instruct: Given a search query in Russian or English, retrieve the
wiki section that answers it
Query: `, no document prefix, dev-chosen cut 0.358.

| Set | Variant | top-1 | top-5 | MRR@5 | recall@5 | true no-hit |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| v4 held-out (16 + 5) | lexical | 5 | 11 | 0.424 | 0.656 | 3/5 |
| | vector | 8 | 12 | 0.609 | 0.750 | 0/5 |
| | plain RRF | 9 | 14 | 0.677 | 0.844 | 0/5 |
| | **hybridAllOnly** | **10** | **14** | **0.734** | 0.875 | 0/5 |
| v3 (20 + 6, informed the choice) | lexical | 7 | 12 | 0.438 | 0.600 | 3/6 |
| | vector | 15 | 16 | 0.767 | 0.800 | 3/6 |
| | plain RRF | 11 | 16 | 0.667 | 0.775 | 2/6 |
| | hybridAllOnly | 16 | 17 | 0.817 | 0.850 | 3/6 |
| dev v2 (17 + 5) | lexical | 14 | 17 | 0.879 | 0.971 | 5/5 |
| | hybridAllOnly | 16 | 17 | 0.961 | 1.000 | 5/5 |
| dev old7 (7) | lexical | 6 | 6 | 0.857 | 0.857 | — |
| | hybridAllOnly | 5 | 7 | 0.857 | 1.000 | — |

**Server equals offline.** With `--server-semantic` the server embeds and fuses
itself, here with `AIWIKI_EMBEDDINGS_MIN_SIMILARITY=0.358`. Its rank of the
first gold section equals the offline `hybridAllOnly` rank for every positive
query of every set (per-query field `serverHybrid` in the result file). The only
difference is the negative query z03: no section passed the cut, and the server
then returns the plain lexical results, partial matches included, instead of an
empty list. A run without a cut (`-nocut` file) gives the same positive ranks
and results for every negative query.

On v4 the four exact identifiers (`OFR-7319`, `ledger-oncall`, `TIER-2-RU`,
`AUDIT-7Q4M-91ZX`) are rank 1 for lexical and hybridAllOnly, while vectors alone
miss two of them. The gain therefore rests on the 12 other v4 positives: top-1
1 of 12 lexical, 6 of 12 hybridAllOnly.

An earlier run had a defect in the script: an empty `--doc-prefix ""` was read
as `"1"`, so chunks were embedded with a leading "1". It was found in review and
fixed; all Qwen3 numbers above are from the corrected rerun (the defective run
had v4 hybridAllOnly 11/14/0.766).

### nomic-embed-text-v1.5 (English-only)

| Set | Variant | top-1 | top-5 | MRR@5 |
| --- | --- | ---: | ---: | ---: |
| v4 | lexical | 5 | 11 | 0.424 |
| | vector | 3 | 3 | 0.188 |
| | hybridAllOnly | 4 | 4 | 0.250 |
| v3 | lexical | 7 | 12 | 0.450 |
| | vector | 4 | 5 | 0.225 |
| | hybridAllOnly | 5 | 6 | 0.275 |

The English-only model does not understand Russian: "рецепт борща со сметаной"
had cosine 0.74 with operational sections. Hybrid search with it is worse than
lexical search.

### Cost

On a Ryzen 9 7900X with LM Studio: indexing 263 chunks took about 4 s (offline
3.9 s, server 4.3 s until coverage 1), query embedding p50 about 12 ms, max
about 13 ms; exact scoring over 263 chunks is negligible.

## Conclusions

* With a multilingual model, hybrid search doubles top-1 on the v4 held-out
  set (5 to 10 of 16), raises top-5 from 11 to 14 and MRR@5 from 0.42 to 0.73,
  while keeping exact identifiers first.
* Semantic search does not detect absence: near-domain negative queries got
  results in every variant that uses vectors, and a similarity cut did not
  separate them from true answers (some negatives scored higher than some
  correct sections). Agents must read a semantic hit before relying on it.
* The model matters more than the fusion: an English-only model makes things
  worse.

## Limits

The corpus is small and synthetic, the query sets are 21 to 26 queries each and
were written by the same author as the corpus. These are not production recall
figures. Lexical results contain ties resolved by update time, so a rerun on a
freshly seeded server can move a lexical hit by one rank (observed once: v3
lexical MRR 0.4375 vs 0.45). v4 was written by the same author shortly after seeing the v3 failure modes.
Result files: `results/2026-10-22-semantic-qwen3.json` (server with cut),
`results/2026-10-22-semantic-qwen3-nocut.json`, `results/2026-10-22-semantic-nomic.json`.

## Reproduce

```
go build -o .cache/aiwiki.exe ./cmd/aiwiki
lms load text-embedding-qwen3-embedding-0.6b
node benchmarks/mcp/semantic-eval.mjs --bin .cache/aiwiki.exe \
  --model text-embedding-qwen3-embedding-0.6b --doc-prefix "" \
  --query-prefix $'Instruct: Given a search query in Russian or English, retrieve the wiki section that answers it\nQuery: ' \
  [--server-semantic] --out .cache/semantic.json
# the server run takes the cut from AIWIKI_EMBEDDINGS_MIN_SIMILARITY in the environment
```
