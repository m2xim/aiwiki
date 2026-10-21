import test from "node:test";
import assert from "node:assert/strict";
import { gradeTrial } from "./agent-trials.mjs";

const task = { answer: "FACT-42", headingTitle: "Facts", headingOccurrence: 1 };
const id = "target-article";
const anchor = "facts";
const revision = 7;

function call(name, args, data, extra = {}) {
  return {
    method: "tools/call",
    params: { name, arguments: args },
    result: { content: [{ type: "text", text: JSON.stringify(data) }] },
    http: { status: 200, requestMethod: "POST" },
    ...extra,
  };
}

function bounded(args, text, { more = false, next = null, rev = args.revision, selectedAnchor = args.anchor, offset = args.offset ?? 0, total = null } = {}) {
  return call("wiki_read", args, {
    id, title: "Target", revision: rev, anchor: selectedAnchor, partial: true,
    offset, chars: Array.from(text).length, total_chars: total ?? offset + Array.from(text).length + (more ? 3 : 0),
    has_more: more, ...(more ? { next_read: next } : {}), text,
  });
}

function grade(strategy, trace, { answer = "FACT-42", articleId = id, citedAnchor = anchor } = {}) {
  return gradeTrial({
    task, strategy, id,
    text: JSON.stringify({ answer, article_id: articleId, anchor: citedAnchor, cannot_find: false }),
    trace,
    cli: { code: 0, signal: null, timedOut: false },
  });
}

function outlinePrefix() {
  return [
    call("wiki_search", { query: "facts" }, { results: [{ id, title: "Target" }] }),
    call("wiki_read", { id, outline: true }, { id, revision, headings: [{ title: "Facts", anchor }] }),
  ];
}

function sectionsPrefix(readArgs = { id, revision, anchor, max_chars: 4000, offset: 0 }) {
  return [call("wiki_search_sections", { q: "facts" }, {
    results: [{ id, revision, anchor, heading: "Facts", read_args: readArgs, snippet: "preview" }],
  })];
}

test("outline-bounded accepts an answer in the selected bounded target section", () => {
  const args = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const result = grade("outline-bounded", [...outlinePrefix(), bounded(args, "Evidence FACT-42.")]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, true);
  assert.equal(result.success, true);
});

test("sections-bounded follows the returned address and next_read continuation", () => {
  const firstArgs = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const nextArgs = { offset: 10, max_chars: 4000, anchor, revision, id };
  const trace = [
    ...sectionsPrefix(firstArgs),
    bounded(firstArgs, "First part", { more: true, next: nextArgs, total: 19 }),
    bounded(nextArgs, " FACT-42.", { offset: 10, total: 19 }),
  ];
  const result = grade("sections-bounded", trace);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.boundedAnswer, true);
  assert.equal(result.routeCompliance, true);
  assert.equal(result.success, true);
});

test("wrong revision cannot satisfy sections-bounded route", () => {
  const readArgs = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const wrongArgs = { ...readArgs, revision: revision + 1 };
  const result = grade("sections-bounded", [
    ...sectionsPrefix(readArgs), bounded(wrongArgs, "FACT-42.", { rev: revision + 1 }),
  ]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
  assert.equal(result.success, false);
});

test("sections-bounded rejects a read that precedes section search", () => {
  const args = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const result = grade("sections-bounded", [bounded(args, "FACT-42."), ...sectionsPrefix(args)]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
});

test("outline-bounded rejects a read that precedes its target outline", () => {
  const args = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const [search, outline] = outlinePrefix();
  const result = grade("outline-bounded", [search, bounded(args, "FACT-42."), outline]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
});

test("outline-bounded rejects an outline read that precedes article search", () => {
  const args = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const [search, outline] = outlinePrefix();
  const result = grade("outline-bounded", [outline, search, bounded(args, "FACT-42.")]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
});

test("unbounded target read can retrieve the answer but never passes route", () => {
  const result = grade("outline-bounded", [
    ...outlinePrefix(), call("wiki_read", { id, anchor }, { id, revision, anchor, section: "FACT-42." }),
  ]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
  assert.equal(result.success, false);
});

test("wrong anchor and a matching search snippet do not pass route", () => {
  const args = { id, revision, anchor: "other-section", max_chars: 4000, offset: 0 };
  const result = grade("sections-bounded", [
    call("wiki_search_sections", { q: "facts" }, {
      results: [{ id, revision, anchor, heading: "Facts", read_args: { ...args, anchor }, snippet: "FACT-42" }],
    }),
    bounded(args, "FACT-42.", { selectedAnchor: args.anchor }),
  ]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.routeCompliance, false);
  assert.equal(result.success, false);
});

test("snippet-only answer is retrieval success but not bounded route compliance", () => {
  const args = { id, revision, anchor, max_chars: 4000, offset: 0 };
  const result = grade("sections-bounded", [
    call("wiki_search_sections", { q: "facts" }, {
      results: [{ id, revision, anchor, heading: "Facts", read_args: { ...args, offset: 40 }, snippet: "FACT-42" }],
    }),
    bounded({ ...args, offset: 40 }, "No answer in this section."),
  ]);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.boundedAnswer, false);
  assert.equal(result.routeCompliance, false);
  assert.equal(result.success, false);
});
