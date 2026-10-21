# Bounded reads and section search

These additions preserve existing `wiki_read` and `wiki_search` behavior when new arguments are omitted. Offsets and sizes count Unicode code points, not bytes or tokens. A slice can split Markdown syntax; concatenate slices before rendering.

## Reading

`wiki_read` accepts `max_chars` (1–16000) and `offset` (default 0). Supplying either opts into bounded mode, whose default size is 4000. `outline=true` cannot be combined with bounded mode. `anchor` selects the existing heading-to-next-peer section, otherwise offsets address the whole body. An offset greater than zero requires an explicit positive `revision`.

The bounded response contains `id`, `title`, `revision`, `anchor` (when selected), `partial:true`, `offset`, `chars`, `total_chars`, `has_more`, `text`, and, when more remains, `next_read` with all arguments for the next call. Historical/deletion markers follow existing read behavior. The first response pins `next_read.revision`; continuation reads the saved snapshot after edits, or returns an explicit revision-not-found error. It never silently switches to the latest body. Offsets past the end are errors; exactly at the end returns an empty final slice. Invalid types and ranges are errors. Existing full/section reads stay available.

## Searching sections

New tool `wiki_search_sections`: nonempty `q` required; optional `space_id`, `kind`, `pinned`, `tag`, `property` filters follow article search. `limit` defaults to 5, maximum 20; `offset` defaults to 0. Response: `total`, `offset`, `mode` (`all` or fallback `any`), `results`, optional `next_offset`. Search pages reflect the current database, not a frozen search snapshot.

Each result includes article `id`, `title`, `space_id`, `revision`, section `anchor`, `heading`, `heading_path`, bounded `snippet`, `score`, `matched_in`, and `read_args` for a revision-pinned bounded read. Search matches only the heading path and that heading's own text up to the next heading of any level, rather than indexing descendant text again in every ancestor. Preamble/headless text is a separate result with empty anchor; its read addresses the full body. Article metadata is used for filtering, not as a reason to return every section. Duplicate headings use existing parser anchors, except colliding slugs described below. Snippets are at most 240 code points. `read_args` starts near the matched text where practical and defaults to 4000 code points, so matches late in a large section are reachable directly. All returned addresses must be executable by `wiki_read` at the returned revision.

The legacy heading parser can produce a slug collision (for example Repeat, Repeat, Repeat 2). Section search detects colliding slugs and returns an empty anchor with a full-body rune offset for those hits, retaining heading/path for context. Use `read_args` rather than constructing a heading URL for these results. Legacy outline/anchor behavior is unchanged. This fallback reads a body slice near the found section and can include adjacent sections.

The initial implementation scans filtered SQLite articles with existing lexical morphology. It is not a persistent section index, semantic search, or an ACL implementation. Global queries exclude archived spaces; explicitly scoped queries preserve article-search archive behavior. Deleted articles are excluded. Access controls must be added consistently to search, current reads and historical continuations when authentication is implemented.

The cap limits `text` code points only, not JSON bytes or tokens; metadata and escaping add overhead. Explicit null values for bounded size/offset/revision, or section-search size/offset, are invalid. Older calls without the new arguments preserve their original behavior, while clients that previously sent ignored `max_chars` or `offset` keys must now send valid values or omit them.

## Acceptance and measurement

Verify Unicode reconstruction, beginning/middle/end offsets, nested and repeated headings, fences and setext headings, unchanged legacy reads, invalid input, historical continuation after rename/edit, global and scoped filters, no duplicate ancestor hits from descendant content, and direct late-match reads. Preserve baseline-v1 corpus and published baseline results. Report proxy payload costs separately from actual model usage, success and route compliance; whole-section reconstruction may cost more than one full read. New live comparison questions must be frozen before trials and explicitly distinguish the old ambiguous r06 wording.
