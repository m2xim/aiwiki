# AI Wiki pilot contract

Local-first Go + SQLite. React TypeScript. Russian UI with a left article tree, central Markdown reading area and table of contents. SQLite is source of truth, immutable revision snapshots; blobs in data/files. No embedded LLM calls or credentials needed. Default loopback port 8090.

## Data and JSON
All JSON keys snake_case. Dates UTC RFC3339. IDs opaque strings. Arrays always [] not null. API errors `{error:string}` with suitable HTTP codes, revision mismatch 409. Space `{id,name,description}`. Article `{id,space_id,parent_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author}`. parent_id empty string for root; kind one of article,instruction,research,plan,source; status one of draft,active,verified,archived; tags string[], properties object string:string. revision positive integer, monotonically increasing. Summary manually authored short discovery text. Article title max 300, body max 2MB. Stable article URLs UI `/?article=ID`, Markdown internal links `/?article=ID#anchor`. Headings anchor: lowercase Unicode letters/numbers, replace runs of other characters with -, trim -. Duplicate suffix -2, -3. Links/backlinks scan these internal links (relative or absolute URL, inside `(...)` or `<...>`).

## HTTP
All routes validate the request Host: loopback or the configured allowed origin's
host and effective port. Origin checks apply independently when that header is
present. Generic loopback development origins are accepted only for loopback
request hosts. These guards do not authenticate clients. See SECURITY.md.

- GET /api/health -> {ok:true}
- GET /api/spaces -> active Space[]; `?include_archived=true` includes archived spaces. Space also has optional `archived_at` (UTC timestamp).
- POST /api/spaces {name,description} -> Space 201. PATCH /api/spaces/{id} {name?,description?,archived?} -> Space; archive with true, restore with false. Archiving preserves articles, revisions, comments and attachments, and is idempotent. All spaces, including built-in ones, can be archived. Archived spaces are excluded from ordinary navigation, unscoped article search and bootstrap; explicit space queries and direct article links remain available. Export includes all spaces and archive state.
- GET /api/articles?space_id=&q=&tag=&property=key:value&kind=&pinned=true -> Article[] (body may empty on list). Multiple tag/property filters AND (exact match). Without q: updated descending. With q: ranked by relevance (see Search). Default and maximum limit 200 (`limit` 1-199 lowers it), optional offset. REST returns no `total`/`mode`.
- GET /api/tags?space_id= -> sorted unique string[] from all articles in that space (not limited to the first 200 articles).
- POST /api/articles -> full article input excluding generated fields, author default human, message optional -> Article 201
- GET /api/articles/{id} -> Article; PUT /api/articles/{id} -> editable fields plus base_revision, message, author -> Article. Merge semantics: only fields present in the JSON replace current values (space_id,parent_id,title,summary,body,kind,status,pinned,tags,properties); absent fields keep their value, a present empty value (`""`, `[]`, `{}`) clears it, except empty kind/status which become `article`/`draft`. Missing base_revision -> 400; mismatch -> 409 `revision conflict: current revision is N`. Also rejects cyclic parent, cross-space parent.
- Changing an article's space moves its entire current descendant tree atomically, including soft-deleted descendants without restoring them. IDs, descendant parent links, content, comments and attachments are preserved. Every moved article gets a new revision and snapshot with the operation's author and timestamp; stale writes to moved descendants return 409. Select a parent in the destination space or an empty parent_id for a root. Revision restore across spaces applies the same subtree move, without restoring descendant content from history.
- DELETE /api/articles/{id} with {base_revision,author} -> Article with deleted_at and a new revision. Soft deletion hides the article from navigation/search/bootstrap, retaining history and attachments. Active children block deletion with 409; move or delete children first. Direct GET/history/export remain available.
- POST /api/articles/{id}/undelete with {base_revision,author} -> restored Article and a new revision. Deleted parent blocks restoration. PUT and revision restore on deleted articles return 409 until undeleted.
- GET /api/articles/{id}/history -> Revision[] `{id,article_id,revision,author,message,created_at,snapshot:Article}` newest first
- GET /api/spaces/{id}/audit?limit=50&offset=0&author= -> compact article revision events `{id,article_id,title,revision,author,message,created_at,changes,space_id,previous_space_id,deleted}`. `changes` lists changed snapshot fields, or `created` for the first revision. Exact optional author filter; limit 1–200, nonnegative offset. Newest revision ID first. Historical space attribution uses snapshots; a move appears in both source and destination, while earlier edits remain in their original space. Archived spaces remain queryable. Unknown space returns 404. This journal covers article revisions, including existing history, deletion, restoration and subtree moves; space settings, comments and files are not revision events. Authors are caller-supplied names, not authenticated user identities.
- POST /api/articles/{id}/restore -> {revision,base_revision,author,message} -> Article (NEW revision; never delete old history)
- GET /api/articles/{id}/links -> {outgoing:Article[],incoming:Article[]} (no bodies). Outgoing: distinct live articles linked from the body, no self-links; incoming: live articles linking here, no self. Recognized forms: `[t](/?article=ID#a)`, absolute `http(s)://host/?article=ID`, `<...>` autolinks.
- GET /api/articles/{id}/comments -> Comment[] `{id,article_id,anchor,body,author,created_at,resolved}`
- POST /api/articles/{id}/comments -> {anchor,body,author} -> Comment 201
- PATCH /api/comments/{id} -> {body?:string,anchor?:string,resolved?:boolean} -> Comment. At least one editable field is required; body must contain non-whitespace text and be at most 2 MB. Omitted fields stay unchanged. Editing preserves author, article ID and creation timestamp; an empty anchor removes the heading binding.
- GET /api/articles/{id}/attachments -> Attachment[] `{id,article_id,name,mime,size,url,created_at}`
- POST /api/articles/{id}/attachments multipart field file -> Attachment 201, max 25MB; random server filename, original name metadata; images can embed URL in Markdown
- DELETE /api/articles/{id}/attachments/{file_id} -> 204, idempotent for an attachment owned by that article; wrong owner -> 404. Soft deletion: attachment is hidden from lists and its file URL returns 404. Original bytes are retained; existing Markdown references are not automatically rewritten. Export includes deleted attachment metadata with deleted_at. No restore UI yet.
- GET /files/{id} -> original bytes; images inline only safe raster MIME, other content downloaded with nosniff
- GET /api/bootstrap?space_id= -> {instructions:Article[],spaces:Space[],recent:Article[],guide:string}; instructions includes global pinned (space 'general') plus selected space pinned. All pinned if no space. Full body instructions, compact recent. Stable built-in space IDs general, genealogy, engineering.
- GET /api/export -> downloadable JSON `{format:"aiwiki-v1",spaces,articles,revisions,comments,attachments,favorites}` (`favorites` = `[{article_id,created_at}]`) with attachments metadata; README explains full backup must include blobs and consistent SQLite snapshot.
- GET /api/favorites -> `[{id,title,space_id,parent_id,kind,status,summary,updated_at,favorited_at}]`, shared by all devices (single owner). Only existing, non-deleted articles in non-archived spaces; `favorited_at` descending; empty -> `[]`. PUT /api/favorites/{id} -> 204, idempotent (repeat keeps the original time); unknown article 404 `{error}`, deleted article 409. DELETE /api/favorites/{id} -> 204, idempotent (no record is also 204). Other methods 405; id outside `[A-Za-z0-9_-]` 400. Table `favorites(article_id PRIMARY KEY, created_at)`.
- GET /api/search?q=&space_id=&kind=&tag=&property=key:value&pinned=true&limit=&offset= -> `{total,offset,mode?,results:[{id,title,space_id,parent_id,kind,status,summary,tags,updated_at,score?,snippet?,matched_in?}],next_offset?}`. Same engine as MCP wiki_search (partial `any` fallback enabled). limit default 30, max 100. Without q: updated descending and `mode`/`score`/`snippet`/`matched_in` are omitted. `score` has 2 decimals; `next_offset` only when more results exist. Filter errors -> 400 `{error}`. `tags` is always an array.
- GET /api/articles/{id}/markdown -> text/markdown
- POST /mcp JSON-RPC Streamable HTTP stateless JSON responses: initialize protocolVersion 2025-03-26 (result includes `instructions`: short workflow text bootstrap -> search -> read outline/anchor -> links -> edit/write), notifications/initialized ->202, ping, tools/list, tools/call. Tools return text JSON and isError for failures (`{error:string}`). GET /mcp 405 (no SSE).

## Search
Shared by REST q and MCP wiki_search. Ranking is computed in Go over candidates after filters (an FTS table exists but is not used for queries). Space omitted -> non-archived spaces only; deleted articles excluded.
- Query: whitespace-separated terms, lowercased, ё=е, deduplicated. Word terms match by Snowball stem (Russian for Cyrillic-only words, English for Latin-only; hyphenated/digit/mixed words unstemmed) or, for terms of 3+ chars, as word prefix. Terms with punctuation (paths, IPs) are substring-only.
- Weak match: term found only as a substring inside a field scores 0.15 x field weight.
- Field weights: title 10, tags 6, summary 4, properties 3 (matched on "key value" text), body 1. Strong match score = weight x (1 + 0.2 ln tf); body = min(1 + ln(1+tf), 4). Bonus +5 if every term strongly matches the title.
- Modes: `all` = articles matching every term. If none and 2+ terms, mode `any` returns articles matching some terms, sorted by number of matched terms, then score. Ties keep updated descending. (REST applies the same fallback but does not report mode.)
- Hit extras (MCP): `score` (2 decimals), `snippet` (~200 chars around best match, from body, else summary, `…` marks cuts), `matched_in` (subset of title,tags,summary,properties,body).

## MCP tools
IDs: letters, digits, `_`, `-`. Compact article = `{id,title,space_id,kind,status,summary,tags,updated_at}` (empty fields omitted, no body).
- wiki_bootstrap `{space_id?}` -> `{spaces,instructions,recent,guide}` plus `other_pinned` without space_id. Without space_id: instructions = full pinned articles of `general` only; other_pinned = compact pinned articles of other spaces. With space_id: instructions = pinned of general + that space in full, no other_pinned. recent: up to 12 compact, excluding listed instructions, archived and deleted. Agents should call it with their project's space_id.
- wiki_search `{q?,space_id?,tag[]?,property[]? ("key:value"),kind?,pinned?,limit?,offset?}` -> `{total,offset,mode?,results:[compact+score,snippet,matched_in],next_offset?}`. limit default 20, max 100. next_offset present when more results remain. No q -> updated descending, no score/snippet/mode.
- wiki_read `{id,outline?,anchor?,revision?}`. Plain: full Article. outline: `{id,title,summary,space_id,parent_id,kind,status,revision,updated_at,chars,headings:[{level,title,anchor,chars}]}`. anchor: `{id,title,revision,anchor,partial:true,section}` (heading to next heading of same or higher level; ATX and Setext, fenced code skipped; unknown -> error `heading not found`). anchor takes precedence over outline. revision: older snapshot, adds `historical:true`; unknown -> `revision not found`. Do not base full replacement on a partial read.
- wiki_links `{id}` -> `{article:{id,title},parent|null,children,outgoing:[compact+anchor?],incoming}`; compact entries, deleted skipped, children by title.
- wiki_edit `{id,base_revision,op,anchor?,text?,old?,author?,message?}` -> `{id,revision,updated_at,chars}`. base_revision required. Ops: replace_section (anchor and text required; text must start with the section's heading line; replaces heading and subsections), append (text required; end of article, or end of section at anchor), replace_text (old must occur exactly once in body, else error with the count; empty text deletes). Conflict -> error `revision conflict: current revision is N`.
- wiki_write `{id?,article:{...,base_revision}}` -> `{id,revision,updated_at,title,space_id}` (no body). Create: space_id and title required. Update: base_revision required, same merge semantics as REST PUT.
- wiki_history `{id,revision?}` -> compact `[{revision,author,message,created_at}]` newest first; with revision the full Revision with snapshot.
- wiki_restore, wiki_comment: REST proxies as before.
- wiki_comments `{id}` -> complete Comment[] including resolved comments, oldest creation timestamp first, with original Markdown body, heading anchor, author and full creation timestamp. No comments returns `[]`; missing/invalid ID or an unknown article returns `isError:true`.

### Bounded reads and section search

`wiki_read` additionally accepts `max_chars` (1–16000) and `offset` (default 0).
Presence of either selects bounded mode (default size 4000); `outline:true` is
incompatible. Nonzero offsets require an explicit positive `revision`. Response:
`{id,title,revision,anchor?,partial:true,offset,chars,total_chars,has_more,text,next_read?}`,
plus historical/deletion markers when applicable. Sizes count Unicode code points;
`next_read` contains complete revision-pinned arguments. An edit does not change a
continuation's snapshot. Unknown snapshots, invalid types/ranges and offsets past
the end are errors. Offset exactly at the end is an empty final slice.

`wiki_search_sections {q,space_id?,kind?,pinned?,tag[]?,property[]?,limit?,offset?}`
searches heading paths and each heading's own text, without descendant duplication.
Default page 5, maximum 20. Returns `{total,offset,mode,results,next_offset?}`; hits
include article ID/title/space/revision, anchor, heading path, score, snippet and
executable bounded `read_args` near the match. Empty anchor denotes preamble or
headless text. Metadata filters match article search; article metadata alone does
not produce section hits. Pages reflect current data. Full specification and
limitations: [MCP-SECTION-CONTRACT.md](MCP-SECTION-CONTRACT.md).

## Seed
Only seed empty DB. Spaces: Общее, Родословная, Инженерия. Helpful instruction articles: start here; neutral agent rules (follow owner instructions, verify results, report actual checks, obtain authorization for external publishing); how to write knowledge including evidence vs hypotheses and unsuccessful searches. Example research/engineering templates are explicitly marked as templates, with no fabricated findings. Article demo content links to instructions. Attachments and comments UI starts empty.

## User-facing behavior
Space journal URL `/?space=ID&view=audit`: author filter, pagination, refresh and links to article history. Opening space management locks background scrolling and restores the previous position on close while the dialog can scroll independently. Comments and the comment form appear below the article; the top Comments button scrolls there. Heading comment shortcuts select that heading in the inline form. Switching articles clears the previous article's comment list and composer.
Comments support Markdown and GFM rendered with the same safe ReactMarkdown defaults as articles. Their headings do not acquire article anchors or comment shortcuts. Compact CodeMirror editors support preview, formatting and cancellation for creation and inline edits. Comment creation timestamps include hours and minutes; editing preserves the original timestamp.
Create/edit Markdown + preview, properties/tags, select space/parent, pin. Search with space/tag/property filters. Nested tree incl children matching search navigable. History compare full snapshots (body diff UI), restore with current base revision. Comments linked to heading. Upload and show/download attachments, insert image Markdown. AI connection panel with endpoint/bootstrap/HTTP examples. Mobile responsive. Clear empty/loading/error states; persistent author field localStorage.

## Pilot bounds
No users/ACL, bind loopback. Any LAN or untrusted-network exposure requires a separate secure access layer (VPN, authenticating reverse proxy). No branching/merging Git engine: linear immutable revision history with conflict checks, restore, diff. No OCR or automatic visual region annotation initially: source images and heading comments supported. No semantic search or synonyms. Import and richer annotation later; full JSON export and Markdown download now.
