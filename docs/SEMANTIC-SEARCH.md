# Semantic section search

`wiki_search_sections` can rank sections by meaning in addition to words. A
Russian question then finds an English section and the other way round, and a
paraphrase that shares no word stems with the text is found too. The measurement
behind the design is in
[benchmarks/mcp/SEMANTIC-EVAL.md](../benchmarks/mcp/SEMANTIC-EVAL.md).

Semantic search is off unless an embedding endpoint is configured. Without it,
the server behaves exactly as before.

## Configuration

| Variable | Meaning |
| --- | --- |
| `AIWIKI_EMBEDDINGS_URL` | OpenAI-compatible `/v1/embeddings` endpoint, for example LM Studio `http://127.0.0.1:1234/v1/embeddings` or Ollama `http://127.0.0.1:11434/v1/embeddings`. Empty means off. |
| `AIWIKI_EMBEDDINGS_MODEL` | Model name sent to the endpoint. Required with the URL. Changing it starts a new index; stored vectors of the old model are not used. |
| `AIWIKI_EMBEDDINGS_QUERY_PREFIX` | Text put before every query. Models trained with instructions need it; `\n` is a newline. |
| `AIWIKI_EMBEDDINGS_DOC_PREFIX` | Text put before every section chunk. |
| `AIWIKI_EMBEDDINGS_MIN_SIMILARITY` | Cosine below which a section is not a semantic hit. Default 0 (no cut). |
| `AIWIKI_EMBEDDINGS_ALLOW_REMOTE` | `1` allows a non-loopback endpoint. |
| `AIWIKI_EMBEDDINGS_API_KEY` | Optional bearer token for the endpoint. |

**Article text is sent to the endpoint.** The endpoint must therefore be a
literal loopback address (`127.0.0.1`, `::1`); the name `localhost` is not
accepted. A remote endpoint needs `AIWIKI_EMBEDDINGS_ALLOW_REMOTE=1` and should
be an internal service that is allowed to process all wiki content. Do not point
it at an external provider for closed content.

Measured configuration (multilingual, about 640 MB, runs on a CPU or a GPU):

```
AIWIKI_EMBEDDINGS_URL=http://127.0.0.1:1234/v1/embeddings
AIWIKI_EMBEDDINGS_MODEL=text-embedding-qwen3-embedding-0.6b
AIWIKI_EMBEDDINGS_QUERY_PREFIX=Instruct: Given a search query in Russian or English, retrieve the wiki section that answers it\nQuery: 
AIWIKI_EMBEDDINGS_MIN_SIMILARITY=0.358
```

The model `Qwen/Qwen3-Embedding-0.6B` (GGUF Q8_0, Apache-2.0) is the one
measured. An English-only model (`nomic-embed-text-v1.5`) made search worse on
Russian and mixed queries and is not recommended. No document prefix was used. The
0.358 cut was chosen on the development sets for this model; another model
needs its own value.

## How it works

* **Units.** The sections are the ones `wiki_search_sections` already uses: each
  heading's own text. A section is split into chunks of at most 1200 characters
  on paragraph boundaries. Each chunk is embedded as `title > heading path`,
  a newline and the chunk text. A section's similarity is that of its best chunk.
* **Access.** A search scores only the sections of the articles that the
  request's own query returned. That query runs under row-level security, MCP
  project scopes and the search filters, exactly as for the lexical search, so
  the semantic part can never return an article the caller cannot read.
* **Storage.** Vectors are stored in `article_embeddings`, one row per article,
  model and chunk text hash. In PostgreSQL the table has FORCE RLS: every policy
  delegates to the visibility of the article (`EXISTS` on `articles` under the
  caller's policies). There is no UPDATE policy. The startup check refuses to
  start if the policies are missing or changed. The server keeps the vectors in
  memory and reads them from the table the first time a search sees an article.
* **Indexing.** A search that sees a section without a vector schedules the
  article for embedding in the background and does not wait. The response then
  says `semantic: "indexing"` and gives `semantic_coverage` (the share of the
  visible chunks that have a vector). Once an article is embedded, its stored
  vectors are replaced by exactly those of its current text, so vectors of
  removed text do not outlive the text. Vectors of a soft-deleted article stay,
  like its full-text row, until the article is removed.
* **Fusion.** The result list is reciprocal rank fusion (k = 60) of the top 20
  lexical sections and the top 20 semantic sections. Only full lexical matches
  (mode `all`) take part. The partial lexical fallback (`any`) is left out,
  because next to the semantic ranking it adds noise; this choice was checked on
  a second held-out query set. Exact identifiers such as `OFR-7319` or
  `ledger-oncall` stay first through the lexical part, where vectors alone miss
  them.
* **Fallback.** If the endpoint does not answer a query within 2 seconds, the
  response says `semantic: "unavailable"` and contains the ordinary lexical
  results, the partial fallback included. The same lexical results are returned
  when no section passes the similarity cut. While sections are still being
  embedded (`indexing`), partial lexical matches stay in the fusion, so a search
  right after enabling the feature is never worse than the lexical one.
* **Dimension change.** A stored vector whose dimension differs from the current
  endpoint output counts as missing and is embedded again.

## Response

In addition to the existing fields:

* `semantic`: `on`, `indexing`, `unavailable` or `off` (`semantic: false`
  argument). Absent when the server has no embedding endpoint.
* `semantic_coverage`: 0 to 1, while semantic search took part.
* per hit `matched_by`: `["lexical"]`, `["semantic"]` or both; `similarity`: the
  cosine of the best chunk when the semantic part found the section.
* `score` is the fusion score (about 0.016 to 0.033) when semantic search took
  part, not comparable with lexical scores.
* `mode` is `semantic` when partial lexical matches were left out in favour of
  the semantic ranking. With fusion, `total` counts the fused list (at most 40),
  not every lexical candidate.

**A semantic hit is a candidate, not evidence.** Semantic search always finds
something similar. A query about a subject the wiki does not cover still returns
sections, so an agent must read a semantic-only hit before relying on it.
Lexical search had the same weakness through its partial fallback, but less
often.

## Limits

* The index lives in memory: 1024 dimensions take 4 KB per chunk, about 200 MB
  for 50 000 chunks. Scoring is exact (no approximate index), linear in the
  visible chunks.
* Indexing speed depends on the endpoint. On the measured machine (Ryzen 9
  7900X, LM Studio, Qwen3-Embedding-0.6B) 263 chunks took about 4 seconds.
* Several server instances each keep their own memory copy; the table is shared.
* Vectors are derived from article text. They are protected like the text, but
  they are an additional copy of it in another form.
* Any user who can read an article can write its derived rows: the indexing runs
  as the user whose search saw the article, readers included. A session that can
  read an article could therefore delete or replace its vectors with its own;
  the server never does, and the damage is limited to the ranking of articles
  that session can read anyway.
* Rows of a previous model stay in the table (unused). The table references
  `articles`; a future hard delete of articles must delete these rows first.
* The corporate startup check requires the `semantic_index` migration even when
  semantic search is off; it is applied automatically at startup.
* The endpoint is never followed through a redirect.
