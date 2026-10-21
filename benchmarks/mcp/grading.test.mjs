import test from "node:test";
import assert from "node:assert/strict";
import { gradeTrial } from "./agent-trials.mjs";

const task = { answer: "FACT-42", headingTitle: "Facts", headingOccurrence: 1 };
const targetId = "target-article";
const targetAnchor = "facts";

function call(name, args, data, extra = {}) {
  return {
    method: "tools/call",
    params: { name, arguments: args },
    result: { content: [{ type: "text", text: JSON.stringify(data) }] },
    http: { status: 200, requestMethod: "POST" },
    ...extra,
  };
}

function grade(trace, { strategy = "outline", answer = "FACT-42", articleId = targetId, anchor = targetAnchor, cannotFind = false } = {}) {
  return gradeTrial({
    task,
    strategy,
    id: targetId,
    text: JSON.stringify({ answer, article_id: articleId, anchor, cannot_find: cannotFind }),
    trace,
    cli: { code: 0, signal: null, timedOut: false },
  });
}

test("search snippet answer plus outline and wrong target section is retrieval only", () => {
  const result = grade([
    call("wiki_search", { query: "facts" }, { results: [{ id: targetId, snippet: "FACT-42" }] }),
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: "unrelated" }, { id: targetId, section: "No matching fact here." }),
  ]);

  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.success, false);
  assert.equal(result.policyAdherent, false);
  assert.equal(result.anchorEvidence, false);
});

test("outline followed by the correct target section succeeds", () => {
  const result = grade([
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
  ]);

  assert.equal(result.success, true);
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.policyAdherent, true);
  assert.equal(result.anchorEvidence, true);
});

test("full target body supports an answer with a null anchor", () => {
  const result = grade([
    call("wiki_read", { id: targetId }, { id: targetId, body: "Facts: FACT-42" }),
  ], { strategy: "full", anchor: null });

  assert.equal(result.success, true);
  assert.equal(result.anchorEvidence, false);
});

test("citing a different article fails retrieval", () => {
  const result = grade([
    call("wiki_search", { query: "facts" }, { results: [{ id: targetId, snippet: "FACT-42" }] }),
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
  ], { articleId: "wrong-article" });

  assert.equal(result.retrievalSuccess, false);
  assert.equal(result.success, false);
});

test("full policy drifting to section reads can retrieve the answer but fails the route", () => {
  const result = grade([
    call("wiki_read", { id: targetId }, { id: targetId, body: "Facts: FACT-42" }),
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, headings: [{title: "Facts", anchor: "facts"}] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
  ], {strategy: "full", anchor: null});
  assert.equal(result.retrievalSuccess, true);
  assert.equal(result.policyAdherent, false);
  assert.equal(result.success, false);
});

test("expected discovery and method probes do not count as tool errors", () => {
  const trace = [
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
    { method: "tools/list", expectedProbe: true, rpcError: { code: -32601, message: "Method not found" }, http: { status: 200, requestMethod: "POST" } },
    { expectedProbe: true, http: { status: 405, requestMethod: "GET" } },
  ];
  const result = grade(trace);

  assert.equal(result.toolErrorCount, 0);
  assert.equal(result.success, true);
});

test("unexpected tool errors fail retrieval even when the answer is supported", () => {
  const trace = [
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
    call("wiki_read", { id: "other-article" }, null, { isError: true }),
  ];
  const result = grade(trace);

  assert.equal(result.toolErrorCount, 1);
  assert.equal(result.retrievalSuccess, false);
  assert.equal(result.success, false);
});

test("cannot_find=true fails retrieval despite matching evidence", () => {
  const result = grade([
    call("wiki_read", { id: targetId, outline: true }, { id: targetId, outline: ["Facts"] }),
    call("wiki_read", { id: targetId, anchor: targetAnchor }, { id: targetId, section: "FACT-42" }),
  ], { cannotFind: true });

  assert.equal(result.reportsCannotFind, true);
  assert.equal(result.retrievalSuccess, false);
  assert.equal(result.success, false);
});
