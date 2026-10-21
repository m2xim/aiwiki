// Independent paired evaluator for article-section search. See SECTION-EVAL-METHODOLOGY.md.
// This runner deliberately does not change corpus.mjs, run.mjs, queries, or runtime code.

import { spawn, execFile } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { fileURLToPath, pathToFileURL } from "node:url";
import { promisify } from "node:util";

const execFileP = promisify(execFile);
const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "..", "..");
const CACHE = path.join(ROOT, ".cache");
const CORPUS_FILE = path.join(HERE, "corpus.mjs");
const QUERY_FILE = path.join(HERE, "section-queries-v2.mjs");
const METHOD_FILE = path.join(HERE, "SECTION-EVAL-METHODOLOGY.md");
const EXPECTED_BASELINE_SHA256 = "7e5e70b2f5f11d59366ead32f16e025319bb0605929fccbb79d5ff08269edfc4";
const EXPECTED_QUERY_MANIFEST_SHA256 = "15f56b747a42cd7f82ec3b124b52b6cee0ac4837125fe698504a0d1aef62e3c7";
const DEFAULT_BASELINE_SOURCE_REF = "3803cb0";
const MIN_REPETITIONS = 5;
const MAX_REPETITIONS = 1000;
const PROTOCOL_VERSION = "2025-03-26";
const LIMIT = 5;
const BUILTIN_SPACES = ["engineering", "general", "genealogy"];
const BUILTIN_ARTICLES = 5;
const REQUEST_TIMEOUT_MS = 30_000;
const HEALTH_TIMEOUT_MS = 30_000;
const BUILD_TIMEOUT_MS = 5 * 60_000;
const AUTHOR = "aiwiki-section-search-eval";

const sha256 = (v) => createHash("sha256").update(v).digest("hex");
const bytes = (v) => Buffer.byteLength(v, "utf8");
const round3 = (n) => Math.round(n * 1000) / 1000;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const canonical = (v) => Array.isArray(v) ? `[${v.map(canonical).join(",")}]`
  : v && typeof v === "object" ? `{${Object.keys(v).sort().map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`).join(",")}}`
    : JSON.stringify(v);
const log = (s) => process.stderr.write(`[section-eval] ${s}\n`);
const ensure = (condition, message) => { if (!condition) throw new Error(message); };

function parseArgs(argv) {
  const opts = { baselineBin: null, baselineSha256: EXPECTED_BASELINE_SHA256, baselineSha256Source: "audited-windows-default", baselineSourceRef: DEFAULT_BASELINE_SOURCE_REF, repetitions: 20 };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const eq = arg.startsWith("--") ? arg.indexOf("=") : -1;
    const flag = eq < 0 ? arg : arg.slice(0, eq);
    const value = () => {
      if (eq >= 0) return arg.slice(eq + 1);
      if (++i >= argv.length) throw new Error(`${flag} needs a value`);
      return argv[i];
    };
    if (flag === "--baseline-bin") opts.baselineBin = value();
    else if (flag === "--baseline-sha256") { opts.baselineSha256 = value(); opts.baselineSha256Source = "caller-asserted"; }
    else if (flag === "--baseline-source-ref") opts.baselineSourceRef = value();
    else if (flag === "--repetitions") {
      const raw = value();
      if (!/^\d+$/.test(raw) || Number(raw) < MIN_REPETITIONS || Number(raw) > MAX_REPETITIONS) {
        throw new Error(`--repetitions must be an integer from ${MIN_REPETITIONS} to ${MAX_REPETITIONS}`);
      }
      opts.repetitions = Number(raw);
    } else throw new Error(`unknown argument ${arg}`);
  }
  if (!opts.baselineBin) throw new Error("--baseline-bin PATH is required");
  if (!/^[0-9a-f]{64}$/i.test(opts.baselineSha256)) throw new Error("--baseline-sha256 must be exactly 64 hexadecimal characters");
  if (!/^[0-9a-f]{7,40}$/i.test(opts.baselineSourceRef)) throw new Error("--baseline-source-ref must be a 7- to 40-character hexadecimal commit reference");
  return opts;
}

function assertInsideCache(input, label) {
  const abs = path.resolve(input);
  const rel = path.relative(CACHE, abs);
  ensure(rel !== "" && !rel.startsWith("..") && !path.isAbsolute(rel), `${label} must resolve inside this repository's .cache`);
  const realCache = fs.realpathSync(CACHE);
  const real = fs.realpathSync(abs);
  const realRel = path.relative(realCache, real);
  ensure(realRel !== "" && !realRel.startsWith("..") && !path.isAbsolute(realRel), `${label} resolves outside .cache through a link`);
  return real;
}

async function reservePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once("error", reject);
    srv.listen({ host: "127.0.0.1", port: 0 }, () => {
      const { port } = srv.address();
      srv.close((err) => err ? reject(err) : resolve(port));
    });
  });
}

async function buildCandidate(outPath) {
  const runDir = path.dirname(outPath);
  const goCache = path.join(runDir, "go-cache");
  const goTmp = path.join(runDir, "go-tmp");
  fs.mkdirSync(goTmp, { recursive: true });
  const env = { ...process.env, GOTOOLCHAIN: "local", GOPROXY: "off", GOFLAGS: "-mod=readonly", GOCACHE: goCache, GOTMPDIR: goTmp };
  const t0 = performance.now();
  const result = await execFileP("go", ["build", "-trimpath", "-mod=readonly", "-o", outPath, "./cmd/aiwiki"], {
    cwd: ROOT, env, timeout: BUILD_TIMEOUT_MS, maxBuffer: 8 * 1024 * 1024, windowsHide: true,
  });
  return { durationMs: round3(performance.now() - t0), stdout: result.stdout, stderr: result.stderr, sha256: sha256(fs.readFileSync(outPath)) };
}

const children = new Map();
function startServer(label, bin, dataDir, port, runDir) {
  const logPath = path.join(runDir, `${label}.server.log`);
  const fd = fs.openSync(logPath, "a");
  const proc = spawn(bin, ["-addr", `127.0.0.1:${port}`, "-data", dataDir, "-web="], {
    cwd: runDir, stdio: ["ignore", fd, fd], windowsHide: true,
  });
  fs.closeSync(fd);
  const state = { label, proc, logPath, exited: false, exitInfo: null };
  state.exitPromise = new Promise((resolve) => {
    proc.once("exit", (code, signal) => { state.exited = true; state.exitInfo = { code, signal }; resolve(); });
    proc.once("error", (err) => { state.exited = true; state.exitInfo = { error: String(err) }; resolve(); });
  });
  children.set(label, state);
  return state;
}

async function stopServer(state) {
  if (!state || state.exited) return;
  state.proc.kill();
  await Promise.race([state.exitPromise, sleep(8000)]);
  if (!state.exited) {
    state.proc.kill("SIGKILL");
    await Promise.race([state.exitPromise, sleep(4000)]);
  }
  ensure(state.exited, `${state.label} child process did not stop`);
}

async function stopAll() {
  const states = [...children.values()];
  for (const state of states.reverse()) await stopServer(state);
}

async function waitHealthy(state, base) {
  const deadline = performance.now() + HEALTH_TIMEOUT_MS;
  while (performance.now() < deadline) {
    if (state.exited) throw new Error(`${state.label} exited before becoming healthy: ${JSON.stringify(state.exitInfo)}\n${readTail(state.logPath)}`);
    try {
      const res = await fetch(`${base}/api/health`, { signal: AbortSignal.timeout(2000) });
      if (res.ok && (await res.json()).ok === true) return;
    } catch { /* retry until deadline */ }
    await sleep(150);
  }
  throw new Error(`${state.label} did not become healthy\n${readTail(state.logPath)}`);
}

function readTail(file) {
  try { return fs.readFileSync(file, "utf8").slice(-2000); } catch { return ""; }
}

const ctx = { seq: 0, rpcId: 0, traceFd: null, tokenizer: null, countByPhase: {}, variants: new Map() };
function trace(variant, phase, operation, request, response, durationMs, extra = {}) {
  const record = {
    seq: ++ctx.seq, at: new Date().toISOString(), variant, phase, operation,
    requestBytes: request ? bytes(request) : 0,
    responseBytes: response ? bytes(response) : 0,
    durationMs: round3(durationMs),
    ...(extra.requestTokens !== undefined ? { requestTokens: extra.requestTokens } : {}),
    ...(extra.responseTokens !== undefined ? { responseTokens: extra.responseTokens } : {}),
    ...(extra.httpStatus !== undefined ? { httpStatus: extra.httpStatus } : {}),
    request: extra.requestValue ?? null,
    response: extra.responseValue ?? null,
  };
  fs.writeSync(ctx.traceFd, `${JSON.stringify(record)}\n`);
  ctx.countByPhase[phase] = (ctx.countByPhase[phase] ?? 0) + 1;
  return record;
}

async function http(variant, phase, operation, method, route, value) {
  const body = value === undefined ? undefined : JSON.stringify(value);
  const start = performance.now();
  let res;
  try {
    res = await fetch(`${variant.base}${route}`, {
      method, headers: body === undefined ? {} : { "content-type": "application/json" }, body,
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (err) {
    const duration = performance.now() - start;
    trace(variant.label, phase, operation, body, "", duration, { requestValue: value ?? null, responseValue: { transportError: String(err) } });
    throw err;
  }
  let raw;
  try { raw = Buffer.from(await res.arrayBuffer()).toString("utf8"); }
  catch (err) {
    const duration = performance.now() - start;
    trace(variant.label, phase, operation, body, "", duration, { httpStatus: res.status, requestValue: value ?? null, responseValue: { bodyReadError: String(err) } });
    throw err;
  }
  const duration = performance.now() - start;
  let parsed;
  try { parsed = raw === "" ? null : JSON.parse(raw); } catch { parsed = raw; }
  trace(variant.label, phase, operation, body, raw, duration, { httpStatus: res.status, requestValue: value ?? null, responseValue: parsed });
  if (!res.ok) throw new Error(`${variant.label} ${method} ${route} returned HTTP ${res.status}: ${raw.slice(0, 500)}`);
  return { parsed, raw, durationMs: duration, requestBytes: body === undefined ? 0 : bytes(body), responseBytes: bytes(raw) };
}

async function mcp(variant, phase, name, args, { notification = false, method = "tools/call" } = {}) {
  const message = notification
    ? { jsonrpc: "2.0", method, ...(args ? { params: args } : {}) }
    : { jsonrpc: "2.0", id: ++ctx.rpcId, method, params: args };
  const text = JSON.stringify(message);
  const start = performance.now();
  let res;
  try {
    res = await fetch(`${variant.base}/mcp`, {
      method: "POST", headers: { "content-type": "application/json" }, body: text,
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (err) {
    const duration = performance.now() - start;
    trace(variant.label, phase, name ?? method, text, "", duration, { requestValue: message, responseValue: { transportError: String(err) } });
    throw err;
  }
  let raw;
  try { raw = Buffer.from(await res.arrayBuffer()).toString("utf8"); }
  catch (err) {
    const duration = performance.now() - start;
    trace(variant.label, phase, name ?? method, text, "", duration, { httpStatus: res.status, requestValue: message, responseValue: { bodyReadError: String(err) } });
    throw err;
  }
  const duration = performance.now() - start;
  let envelope = null;
  let parseError = null;
  try { envelope = raw === "" ? null : JSON.parse(raw); } catch (err) { parseError = String(err); }
  const result = envelope?.result ?? null;
  const contentText = name && Array.isArray(result?.content) && result.content.every((x) => x.type === "text" && typeof x.text === "string")
    ? result.content.map((x) => x.text).join("") : "";
  trace(variant.label, phase, name ?? method, text, raw, duration, {
    requestTokens: name ? ctx.tokenizer.count(name) + ctx.tokenizer.count(JSON.stringify(args.arguments ?? {})) : null,
    responseTokens: name && contentText ? ctx.tokenizer.count(contentText) : null,
    httpStatus: res.status, requestValue: message, responseValue: envelope ?? { raw, parseError },
  });
  if (notification) {
    // The response has already been persisted above, including a non-202 body.
    ensure(res.status === 202 && raw.length === 0, `${variant.label} ${method} expected empty 202`);
    return { durationMs: duration, requestBytes: bytes(text), responseBytes: 0, result: null, contentText: "" };
  }
  ensure(res.status === 200, `${variant.label} ${name ?? method} returned HTTP ${res.status}: ${raw.slice(0, 500)}`);
  ensure(!parseError && envelope, `${variant.label} returned invalid JSON-RPC: ${parseError ?? "empty response"}`);
  ensure(envelope.id === message.id && !envelope.error, `${variant.label} returned mismatched/error JSON-RPC: ${raw.slice(0, 500)}`);
  ensure(result && result.isError !== true, `${variant.label} ${name ?? method} tool error: ${JSON.stringify(result)}`);
  if (name) {
    ensure(result.structuredContent === undefined, `${name} unexpected structuredContent`);
    ensure(Array.isArray(result.content) && result.content.every((x) => x.type === "text" && typeof x.text === "string"), `${name} result is not text-only`);
  }
  let parsed = null;
  if (name) {
    try { parsed = JSON.parse(contentText); } catch { throw new Error(`${name} content was not JSON`); }
  }
  return { result, parsed, contentText, durationMs: duration, requestBytes: bytes(text), responseBytes: bytes(raw) };
}

async function tool(variant, phase, name, args) {
  return mcp(variant, phase, name, { name, arguments: args });
}

function resolveLinks(body, docs, ownKey) {
  return body.replace(/\[\[link:([^\]\s]+)\]\]/g, (token, key, offset, whole) => {
    const target = docs.get(key);
    ensure(target, `document ${ownKey} references missing link key ${key}`);
    if (whole.slice(Math.max(0, offset - 2), offset) === "](") return `/?article=${target.id}`;
    if (whole.slice(Math.max(0, offset - 8), offset) === "article=") return target.id;
    return `[${target.doc.title}](/?article=${target.id})`;
  });
}

async function seedCorpus(variant, corpus) {
  const initial = (await http(variant, "setup", "rest-export-fresh", "GET", "/api/export")).parsed;
  ensure(canonical(initial.spaces.map((x) => x.id).sort()) === canonical([...BUILTIN_SPACES].sort()), "seed server has unexpected built-in spaces");
  ensure(initial.articles.length === BUILTIN_ARTICLES && initial.articles.every((x) => x.author === "system"), "seed server is not fresh");
  for (const s of corpus.spaces) {
    const created = (await http(variant, "setup", "rest-create-space", "POST", "/api/spaces", { name: s.name, description: s.description ?? "" })).parsed;
    variant.spaces.set(s.key, created.id);
  }
  for (const doc of corpus.documents) {
    const created = (await http(variant, "setup", "rest-create-article", "POST", "/api/articles", {
      space_id: variant.spaces.get(doc.spaceKey), title: doc.title, summary: doc.summary, body: doc.body,
      tags: doc.tags, properties: doc.properties, author: AUTHOR, message: "seed fixture",
    })).parsed;
    variant.docs.set(doc.key, { id: created.id, spaceId: created.space_id, doc, body: created.body, revision: created.revision });
  }
  for (const [key, state] of variant.docs) {
    const linkedBody = resolveLinks(state.body, variant.docs, key);
    if (linkedBody === state.body) continue;
    const updated = (await http(variant, "setup", "rest-resolve-links", "PUT", `/api/articles/${state.id}`, {
      body: linkedBody, base_revision: state.revision, author: AUTHOR, message: "resolve fixture links",
    })).parsed;
    state.body = updated.body;
    state.revision = updated.revision;
  }
  const exported = (await http(variant, "setup", "rest-export-seeded", "GET", "/api/export")).parsed;
  ensure(exported.articles.length === BUILTIN_ARTICLES + corpus.documents.length, "seeded export count differs from fixture plus built-ins");
  ensure(exported.articles.length === 33, `expected exportcheck33docs; found ${exported.articles.length}`);
  const byId = new Map(exported.articles.map((a) => [a.id, a]));
  for (const [key, state] of variant.docs) {
    const row = byId.get(state.id);
    ensure(row && row.title === state.doc.title && row.body === state.body && row.revision === state.revision, `export verification failed for ${key}`);
    ensure((row.body.match(/\[\[link:/g) ?? []).length === 0, `unresolved fixture link in ${key}`);
  }
  variant.export = exported;
  variant.exportHash = sha256(canonical(exported));
  return { docs: exported.articles.length, spaces: exported.spaces.length, exportSha256: variant.exportHash };
}

async function initializeVariant(variant, corpus) {
  const init = await mcp(variant, "setup", null, {
    protocolVersion: PROTOCOL_VERSION, capabilities: {}, clientInfo: { name: "aiwiki-section-search-eval", version: "1" },
  }, { method: "initialize" });
  ensure(init.result?.protocolVersion === PROTOCOL_VERSION && typeof init.result.instructions === "string", `${variant.label} initialize contract mismatch`);
  await mcp(variant, "setup", null, undefined, { notification: true, method: "notifications/initialized" });
  const listed = await mcp(variant, "setup", null, {}, { method: "tools/list" });
  const names = listed.result?.tools?.map((x) => x.name) ?? [];
  for (const name of ["wiki_bootstrap", "wiki_search_sections", "wiki_read"]) ensure(names.includes(name), `${variant.label} lacks ${name}`);
  const firstSpace = variant.spaces.get(corpus.spaces[0].key);
  const boot = await tool(variant, "setup", "wiki_bootstrap", { space_id: firstSpace });
  ensure(boot.parsed !== null, `${variant.label} bootstrap response missing`);
}

function validateQueries(set) {
  ensure(set && typeof set.version === "string" && Array.isArray(set.queries) && set.queries.length > 0, "section query module must return {version, queries}");
  const seen = new Set();
  for (const q of set.queries) {
    ensure(typeof q.key === "string" && q.key && !seen.has(q.key), `query key missing/duplicate: ${q.key}`);
    seen.add(q.key);
    ensure(typeof q.category === "string" && typeof q.query === "string" && q.query.trim(), `query ${q.key} needs category/query`);
    ensure(Array.isArray(q.relevantSections), `query ${q.key} relevantSections must be an array`);
    ensure(typeof q.rationale === "string", `query ${q.key} rationale must be a string`);
    for (const gold of q.relevantSections) {
      ensure(typeof gold.articleKey === "string" && typeof gold.headingTitle === "string", `query ${q.key} gold section is incomplete`);
      ensure(Number.isInteger(gold.headingOccurrence) && gold.headingOccurrence >= 1, `query ${q.key} headingOccurrence must be >= 1`);
    }
  }
}

function parseHeadings(body) {
  const lines = body.split("\n");
  const headings = [];
  let pos = 0;
  for (let i = 0; i < lines.length; i++) {
    const m = /^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/.exec(lines[i]);
    if (m) headings.push({ line: i, start: pos, level: m[1].length, title: m[2].trim() });
    pos += [...lines[i]].length + 1;
  }
  return { lines, headings, bodyLength: [...body].length };
}

function assertFixtureHeadingParser(body, outline, articleKey) {
  const unsupported = body.split("\n").flatMap((line, i) => {
    if (/^\s{0,3}(?:```|~~~)/.test(line)) return [`fenced-code marker at line ${i + 1}`];
    if (/^\s*(?:=+|-+)\s*$/.test(line)) return [`setext underline at line ${i + 1}`];
    if (/^\s*#(?:include|define|if|ifdef|ifndef|elif|else|endif|pragma|region|endregion|nullable)\b/i.test(line)) return [`C/C# preprocessor directive at line ${i + 1}`];
    return [];
  });
  ensure(unsupported.length === 0, `fixture heading parser supports ATX headings only; ${articleKey} contains unsupported ${unsupported.join(", ")}`);
  const parsed = parseHeadings(body).headings;
  const actual = (outline.headings ?? []).map((h) => ({ title: String(h.title).trim(), level: Number(h.level) }));
  const local = parsed.map((h) => ({ title: h.title, level: h.level }));
  ensure(canonical(local) === canonical(actual),
    `fixture ATX heading parser disagrees with baseline wiki_read outline for ${articleKey}; local=${JSON.stringify(local)}, outline=${JSON.stringify(actual)} (fenced, setext, preprocessor, or other unsupported syntax)`);
}

function sectionBounds(body, title, occurrence) {
  const parsed = parseHeadings(body);
  const matches = parsed.headings.filter((h) => h.title === title);
  const h = matches[occurrence - 1];
  ensure(h, `heading ${JSON.stringify(title)} occurrence ${occurrence} is absent in fixture body`);
  const index = parsed.headings.indexOf(h);
  const end = index + 1 < parsed.headings.length ? parsed.headings[index + 1].start : parsed.bodyLength;
  return { ...h, end, text: [...body].slice(h.start, end).join("") };
}

async function getGolds(corpus, querySets, baseline) {
  const golds = new Map();
  const outlinesByArticle = new Map();
  const all = [...querySets.flatMap((set) => set.queries), ...corpus.readTasks.map((t) => ({
    key: t.key, query: t.query, category: "old7-read-task", rationale: "Frozen readTasks development set gold anchor.",
    relevantSections: [{ articleKey: t.articleKey, headingTitle: t.headingTitle, headingOccurrence: t.headingOccurrence }],
    factAnswer: t.answer,
  }))];
  for (const q of all) {
    const sections = [];
    for (const gold of q.relevantSections) {
      const doc = baseline.docs.get(gold.articleKey);
      ensure(doc, `${q.key} references missing corpus article ${gold.articleKey}`);
      let outline = outlinesByArticle.get(gold.articleKey);
      if (!outline) {
        outline = (await tool(baseline, "verify", "wiki_read", { id: doc.id, outline: true })).parsed;
        assertFixtureHeadingParser(doc.body, outline, gold.articleKey);
        outlinesByArticle.set(gold.articleKey, outline);
      }
      const matches = outline.headings.filter((h) => h.title.trim() === gold.headingTitle.trim());
      const heading = matches[gold.headingOccurrence - 1];
      ensure(heading, `${q.key}: expected outline heading ${gold.headingTitle} occurrence ${gold.headingOccurrence} not found`);
      const bodySection = sectionBounds(doc.body, gold.headingTitle, gold.headingOccurrence);
      sections.push({
        articleKey: gold.articleKey, id: doc.id, headingTitle: gold.headingTitle,
        headingOccurrence: gold.headingOccurrence, anchor: heading.anchor ?? "", revision: outline.revision,
        bodyStart: bodySection.start, bodyEnd: bodySection.end, sectionText: bodySection.text,
      });
    }
    golds.set(q.key, { ...q, resolvedSections: sections });
  }
  return golds;
}

function sameHit(hit, gold) {
  if (hit.id !== gold.id || hit.heading !== gold.headingTitle) return false;
  const args = hit.read_args ?? {};
  if (gold.anchor && hit.anchor === gold.anchor) return true;
  if (hit.anchor) return hit.anchor === gold.anchor;
  const off = Number(args.offset);
  return Number.isInteger(off) && off >= gold.bodyStart && off < gold.bodyEnd;
}

async function verifyQueryVariant(variant, query, gold) {
  const found = await tool(variant, "verify", "wiki_search_sections", { q: query.query, limit: LIMIT });
  const results = found.parsed.results ?? [];
  ensure(results.length <= LIMIT, `${variant.label} ${query.key}: section search returned more than five rows`);
  const verifiedReads = [];
  for (let i = 0; i < results.length; i++) {
    const hit = results[i];
    const args = hit.read_args;
    ensure(args && typeof args.id === "string" && args.id === hit.id, `${variant.label} ${query.key} rank ${i + 1}: read_args id mismatch`);
    ensure(Number.isInteger(args.revision) && args.revision === hit.revision, `${variant.label} ${query.key} rank ${i + 1}: read_args revision mismatch`);
    ensure(Number.isInteger(args.max_chars) && Number.isInteger(args.offset), `${variant.label} ${query.key} rank ${i + 1}: read_args missing bounds`);
    const state = variant.articlesById.get(hit.id);
    ensure(state, `${variant.label} ${query.key} rank ${i + 1}: search returned ID absent from exported snapshot`);
    ensure(args.revision === state.revision, `${variant.label} ${query.key} rank ${i + 1}: read_args is not pinned to exported revision`);
    let expectedText;
    if (args.anchor) {
      const legacy = await tool(variant, "verify", "wiki_read", { id: args.id, anchor: args.anchor, revision: args.revision });
      ensure(legacy.parsed.id === args.id && legacy.parsed.revision === args.revision, `${variant.label} ${query.key} legacy section context differs`);
      expectedText = legacy.parsed.section;
    } else expectedText = state.body;
    const expectedWindow = [...expectedText].slice(args.offset, args.offset + args.max_chars).join("");
    const actual = await tool(variant, "verify", "wiki_read", args);
    ensure(actual.parsed.id === args.id && actual.parsed.revision === args.revision, `${variant.label} ${query.key} rank ${i + 1}: executable read returned wrong ID/revision`);
    ensure(actual.parsed.offset === args.offset && actual.parsed.text === expectedWindow, `${variant.label} ${query.key} rank ${i + 1}: read text differs from expected source window`);
    verifiedReads.push({ rank: i + 1, id: hit.id, heading: hit.heading, anchor: hit.anchor, readArgs: args, textChars: actual.parsed.chars });
  }
  return { mode: found.parsed.mode, total: found.parsed.total, payloadSha256: sha256(canonical(found.parsed)), results, verifiedReads };
}

function evaluateRanks(hits, goldSections) {
  const relevantRanks = [];
  const seen = new Set();
  for (let i = 0; i < hits.length; i++) {
    const matchIndex = goldSections.findIndex((g) => sameHit(hits[i], g));
    if (matchIndex >= 0 && !seen.has(matchIndex)) { relevantRanks.push(i + 1); seen.add(matchIndex); }
  }
  const n = goldSections.length;
  return {
    relevantGoldCount: n,
    relevantFoundAt5: seen.size,
    recallAt5: n === 0 ? null : seen.size / n,
    top1Coverage: n === 0 ? null : relevantRanks.includes(1),
    top5Coverage: n === 0 ? null : relevantRanks.length > 0,
    reciprocalRankAt5: n === 0 ? null : (relevantRanks.length ? 1 / Math.min(...relevantRanks) : 0),
    relevantRanks,
  };
}

async function measureOne(variant, query, gold, rep, phase, verifiedReadArgs, verifiedPayloadSha256) {
  const search = await tool(variant, phase, "wiki_search_sections", { q: query.query, limit: LIMIT });
  const hits = search.parsed.results ?? [];
  const payloadSha256 = sha256(canonical(search.parsed));
  ensure(payloadSha256 === verifiedPayloadSha256,
    `${variant.label} ${query.key}: ${phase} search payload differs from the once-per-query verification response`);
  ensure(hits.length <= LIMIT, `${variant.label} ${query.key}: measured result exceeded top five`);
  for (const [i, hit] of hits.entries()) {
    ensure(verifiedReadArgs.has(canonical({ id: hit.id, read_args: hit.read_args })),
      `${variant.label} ${query.key} rank ${i + 1}: measured read_args was not covered by the once-per-query verification pass`);
  }
  const rankStats = evaluateRanks(hits, gold.resolvedSections);
  let boundedFact = null;
  if (query.factAnswer) {
    const first = hits[0];
    if (!first) boundedFact = { status: "no-hit", found: false };
    else {
      const read = await tool(variant, "measure-fact-read", "wiki_read", first.read_args);
      const text = read.parsed.text ?? "";
      const matchedGold = gold.resolvedSections.find((section) => sameHit(first, section));
      const answerIndexUtf16 = matchedGold?.sectionText.indexOf(query.factAnswer) ?? -1;
      const answerIndex = answerIndexUtf16 < 0 ? -1 : Array.from(matchedGold.sectionText.slice(0, answerIndexUtf16)).length;
      const offset = Number(first.read_args?.offset ?? 0);
      const answerOffset = answerIndex < 0 ? -1 : first.read_args?.anchor ? answerIndex : matchedGold.bodyStart + answerIndex;
      const answerBeforeReadOffset = answerOffset >= 0 && offset > answerOffset;
      const literalFound = text.includes(query.factAnswer);
      const correctExpectedSource = Boolean(matchedGold);
      boundedFact = {
        found: literalFound && correctExpectedSource,
        literalFound,
        correctExpectedSource,
        literalFoundFromWrongSource: literalFound && !correctExpectedSource,
        answerBeforeReadOffset,
        expectedSectionHasAnswer: answerIndex >= 0, boundedOffset: offset,
        answerOffsetInReadSource: answerOffset >= 0 ? answerOffset : null,
        returnedId: first.id, returnedHeading: first.heading, readChars: read.parsed.chars,
        requestBytes: read.requestBytes, responseBytes: read.responseBytes,
        outputTokens: ctx.tokenizer.count("wiki_read") + ctx.tokenizer.count(JSON.stringify(first.read_args)),
        inputTokens: ctx.tokenizer.count(read.contentText), durationMs: round3(read.durationMs),
      };
    }
  }
  return {
    set: query.setName, key: query.key, category: query.category, query: query.query,
    repetition: rep, mode: search.parsed.mode, totalResults: search.parsed.total,
    payloadSha256,
    sourceKeys: hits.map((hit) => ({ id: hit.id, articleKey: [...variant.docs].find(([, d]) => d.id === hit.id)?.[0] ?? null, heading: hit.heading, anchor: hit.anchor })),
    anchors: hits.map((hit) => ({ id: hit.id, anchor: hit.anchor, heading: hit.heading, headingPath: hit.heading_path })),
    ranks: rankStats, boundedFact,
    cost: { requestBytes: search.requestBytes, responseBytes: search.responseBytes,
      totalBytes: search.requestBytes + search.responseBytes,
      outputTokens: ctx.tokenizer.count("wiki_search_sections") + ctx.tokenizer.count(JSON.stringify({ q: query.query, limit: LIMIT })),
      inputTokens: ctx.tokenizer.count(search.contentText), totalTokens: ctx.tokenizer.count("wiki_search_sections") + ctx.tokenizer.count(JSON.stringify({ q: query.query, limit: LIMIT })) + ctx.tokenizer.count(search.contentText),
      durationMs: round3(search.durationMs) },
  };
}

function stats(values) {
  const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
  if (!sorted.length) return { n: 0, median: null, p95: null };
  const pick = (p) => sorted[Math.max(1, Math.ceil(p * sorted.length / 100)) - 1];
  return { n: sorted.length, median: pick(50), p95: pick(95), min: sorted[0], max: sorted.at(-1) };
}

function summarizeSet(records, set, variantLabel) {
  const rows = records.filter((r) => r.set === set.name && r.variant === variantLabel && !r.warmup);
  const scored = rows.filter((r) => r.ranks.recallAt5 !== null);
  const noHitExpected = rows.filter((r) => r.ranks.recallAt5 === null);
  const perQuery = set.queries.map((q) => {
    const samples = rows.filter((r) => r.key === q.key);
    return {
      key: q.key, category: q.category, query: q.query, rationale: q.rationale,
      relevantSections: q.relevantSections, sourceKeysAndAnchors: samples[0]?.anchors ?? [],
      verifiedPayloadSha256: samples[0]?.payloadSha256 ?? null,
      payloadStable: samples.every((r) => r.payloadSha256 === samples[0]?.payloadSha256),
      top1Coverage: samples.filter((r) => r.ranks.top1Coverage).length,
      top5Coverage: samples.filter((r) => r.ranks.top5Coverage).length,
      recallAt5Mean: mean(samples.map((r) => r.ranks.recallAt5).filter((x) => x !== null)),
      mrrAt5Mean: mean(samples.map((r) => r.ranks.reciprocalRankAt5).filter((x) => x !== null)),
      searchLatencyMs: stats(samples.map((r) => r.cost.durationMs)),
      payload: { requestBytes: stats(samples.map((r) => r.cost.requestBytes)), responseBytes: stats(samples.map((r) => r.cost.responseBytes)), outputTokens: stats(samples.map((r) => r.cost.outputTokens)), inputTokens: stats(samples.map((r) => r.cost.inputTokens)) },
      noHit: q.relevantSections.length === 0 ? { expectedNegative: true, trueNegative: samples.filter((r) => r.sourceKeys.length === 0).length, falsePositive: samples.filter((r) => r.sourceKeys.length > 0).length } : null,
      boundedFact: samples.some((r) => r.boundedFact) ? {
        found: samples.filter((r) => r.boundedFact?.found).length,
        literalFound: samples.filter((r) => r.boundedFact?.literalFound).length,
        literalFoundFromWrongSource: samples.filter((r) => r.boundedFact?.literalFoundFromWrongSource).length,
        misses: samples.filter((r) => r.boundedFact && !r.boundedFact.found).length,
        answerBeforeReadOffset: samples.filter((r) => r.boundedFact?.answerBeforeReadOffset).length,
        samples: samples.map((r) => r.boundedFact),
      } : null,
    };
  });
  return {
    queryCount: set.queries.length, repetitions: rows.length / Math.max(1, set.queries.length),
    top1Coverage: scored.length ? scored.filter((r) => r.ranks.top1Coverage).length / scored.length : null,
    top5Coverage: scored.length ? scored.filter((r) => r.ranks.top5Coverage).length / scored.length : null,
    recallAt5MacroMean: mean(set.queries.filter((q) => q.relevantSections.length).map((q) => {
      const a = rows.filter((r) => r.key === q.key); return mean(a.map((r) => r.ranks.recallAt5));
    })),
    mrrAt5MacroMean: mean(set.queries.filter((q) => q.relevantSections.length).map((q) => {
      const a = rows.filter((r) => r.key === q.key); return mean(a.map((r) => r.ranks.reciprocalRankAt5));
    })),
    noHit: { expectedNegativeQueries: set.queries.filter((q) => q.relevantSections.length === 0).length,
      samples: noHitExpected.length,
      trueNegative: noHitExpected.filter((r) => r.sourceKeys.length === 0).length,
      falsePositive: noHitExpected.filter((r) => r.sourceKeys.length > 0).length,
      accuracy: noHitExpected.length ? noHitExpected.filter((r) => r.sourceKeys.length === 0).length / noHitExpected.length : null },
    latencyMs: stats(rows.map((r) => r.cost.durationMs)), perQuery,
  };
}

function mean(values) {
  const v = values.filter(Number.isFinite);
  return v.length ? v.reduce((a, b) => a + b, 0) / v.length : null;
}

async function loadInputs() {
  ensure(fs.existsSync(CORPUS_FILE), "benchmarks/mcp/corpus.mjs missing");
  ensure(fs.existsSync(QUERY_FILE), "benchmarks/mcp/section-queries-v2.mjs missing; authoring must finish before evaluation");
  const corpusMod = await import(pathToFileURL(CORPUS_FILE).href);
  const queryMod = await import(pathToFileURL(QUERY_FILE).href);
  ensure(typeof corpusMod.makeCorpus === "function", "corpus.mjs must export makeCorpus()");
  ensure(typeof queryMod.makeSectionQueries === "function", "section-queries-v2.mjs must export makeSectionQueries()");
  const corpus = corpusMod.makeCorpus();
  const corpusCanonical = canonical(corpus);
  ensure(canonical(corpusMod.makeCorpus()) === corpusCanonical, "makeCorpus() is not deterministic");
  const queries = queryMod.makeSectionQueries();
  ensure(canonical(queryMod.makeSectionQueries()) === canonical(queries), "makeSectionQueries() is not deterministic");
  validateQueries(queries);
  ensure(Array.isArray(corpus.readTasks) && corpus.readTasks.length === 7, `frozen development readTasks must contain exactly seven entries; got ${corpus.readTasks?.length}`);
  for (const q of queries.queries) for (const s of q.relevantSections) {
    ensure(corpus.documents.some((d) => d.key === s.articleKey), `${q.key} references unknown article ${s.articleKey}`);
  }
  const oldSet = {
    name: "frozen-old7-readTasks-development",
    version: corpus.version,
    queries: corpus.readTasks.map((t) => ({ key: t.key, category: "old7-read-task", query: t.query,
      rationale: "Frozen readTasks development task; expected answer and heading are not exposed to ranking selection.",
      relevantSections: [{ articleKey: t.articleKey, headingTitle: t.headingTitle, headingOccurrence: t.headingOccurrence }], factAnswer: t.answer })),
  };
  const newSet = { name: queries.version, version: queries.version, queries: queries.queries };
  const sourceBytes = fs.readFileSync(CORPUS_FILE);
  const queryBytes = fs.readFileSync(QUERY_FILE);
  const queryManifestJsonSha256 = sha256(JSON.stringify(queries));
  ensure(queryManifestJsonSha256 === EXPECTED_QUERY_MANIFEST_SHA256,
    `section query manifest hash mismatch: expected frozen ${EXPECTED_QUERY_MANIFEST_SHA256}, got ${queryManifestJsonSha256}`);
  const frozen = {
    corpus: { version: corpus.version, sortedCanonicalJsonSha256: sha256(corpusCanonical), jsonStringifySha256: sha256(JSON.stringify(corpus)), sourceFileSha256: sha256(sourceBytes) },
    queryManifestJsonSha256,
    queryManifestSourceFileSha256: sha256(queryBytes),
    querySets: [oldSet, newSet].map((set) => ({ name: set.name, version: set.version, count: set.queries.length,
      sortedCanonicalSetSha256: sha256(canonical(set)), sourceFileSha256: set.name === oldSet.name ? sha256(sourceBytes) : sha256(queryBytes) })),
  };
  return { corpus, querySets: [oldSet, newSet], frozen };
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  fs.mkdirSync(CACHE, { recursive: true });
  const baselinePath = assertInsideCache(opts.baselineBin, "--baseline-bin");
  ensure(fs.statSync(baselinePath).isFile(), "--baseline-bin must point to a file");
  const baselineHash = sha256(fs.readFileSync(baselinePath));
  ensure(baselineHash === opts.baselineSha256, `baseline binary hash mismatch: expected ${opts.baselineSha256}, got ${baselineHash}`);

  const { corpus, querySets, frozen } = await loadInputs();
  const mod = await import("js-tiktoken");
  const enc = mod.getEncoding("cl100k_base");
  const tokenizerMeta = JSON.parse(fs.readFileSync(path.join(HERE, "node_modules", "js-tiktoken", "package.json"), "utf8"));
  ensure(tokenizerMeta.version === "1.0.21", `js-tiktoken version must be 1.0.21, got ${tokenizerMeta.version}`);
  ctx.tokenizer = { name: "cl100k_base", package: "js-tiktoken", version: "1.0.21", count: (s) => enc.encode(s, [], []).length };

  const runId = randomUUID();
  const runDir = path.join(CACHE, `mcp-section-eval-${runId}`);
  fs.mkdirSync(runDir, { recursive: false });
  const summaryPath = path.join(runDir, "summary.json");
  const tracesPath = path.join(runDir, "traces.jsonl");
  ctx.traceFd = fs.openSync(tracesPath, "wx");
  const summary = {
    schema: "aiwiki-mcp-section-search-eval/1", status: "running", runId,
    startedAt: new Date().toISOString(), options: { repetitions: opts.repetitions, warmupPasses: 1, resultLimit: LIMIT, ordering: "per-query alternation; first pair baseline→candidate, then candidate→baseline, repeated" },
    integrity: { ...frozen, baselineBinarySha256: baselineHash, requiredBaselineBinarySha256: opts.baselineSha256,
      baselineHashSource: opts.baselineSha256Source,
      baselineSourceRef: { value: opts.baselineSourceRef, status: "declared-not-verified-from-binary" } },
    definitions: {
      searchLatency: "local loopback HTTP wiki_search_sections duration, excluding read verification and setup",
      tokens: "cl100k_base proxy; tokens per individual query and variant, never a pooled between-question ratio",
      coverage: "top1/top5 ask whether any authored relevant section was found; recall@5 is relevant authored sections found divided by relevant section count; MRR@5 uses first relevant rank",
      noHit: "queries with an empty authored relevantSections list are reported separately as true-negative / false-positive counts",
      factQuality: "old7 top-1 hit's executable bounded wiki_read text is checked for the frozen answer literal; offset-before-answer is reported separately",
    },
    files: { runDir, summary: summaryPath, traces: tracesPath },
  };
  let seedState = null;
  try {
    const candidateBin = path.join(runDir, process.platform === "win32" ? "aiwiki-candidate.exe" : "aiwiki-candidate");
    const baselineSeedDir = path.join(runDir, "seed-data");
    fs.mkdirSync(baselineSeedDir);
    const seedPort = await reservePort();
    const seedVariant = { label: "seed", base: `http://127.0.0.1:${seedPort}`, docs: new Map(), spaces: new Map() };
    seedState = startServer("seed", baselinePath, baselineSeedDir, seedPort, runDir);
    await waitHealthy(seedState, seedVariant.base);
    const seeded = await seedCorpus(seedVariant, corpus);
    await stopServer(seedState);
    seedState = null;
    const seedExport = seedVariant.export;
    const baselineDataDir = path.join(runDir, "baseline-data");
    const candidateDataDir = path.join(runDir, "candidate-data");
    fs.cpSync(baselineSeedDir, baselineDataDir, { recursive: true, errorOnExist: true, force: false });
    fs.cpSync(baselineSeedDir, candidateDataDir, { recursive: true, errorOnExist: true, force: false });

    log("building candidate Go binary with offline module settings");
    const build = await buildCandidate(candidateBin);
    summary.environment = {
      baselineBinarySha256: baselineHash, candidateBinarySha256: build.sha256,
      candidateBuild: { durationMs: build.durationMs, command: "go build -trimpath -mod=readonly -o <run-dir>/aiwiki-candidate ./cmd/aiwiki", env: { GOTOOLCHAIN: "local", GOPROXY: "off", GOFLAGS: "-mod=readonly" } },
      node: process.version, goVersion: (await execFileP("go", ["version"], { cwd: ROOT, timeout: 10_000 })).stdout.trim(),
      platform: { os: process.platform, arch: process.arch, release: os.release(), cpu: os.cpus()[0]?.model ?? null },
      tokenizer: { package: ctx.tokenizer.package, version: ctx.tokenizer.version, encoding: ctx.tokenizer.name },
      runnerSha256: sha256(fs.readFileSync(fileURLToPath(import.meta.url))),
      methodologySha256: sha256(fs.readFileSync(METHOD_FILE)),
    };
    summary.setup = { seed: seeded, seedExportSha256: sha256(canonical(seedExport)), setupTrafficPhase: "setup", replicas: {} };

    const variants = [];
    for (const spec of [
      { label: "baseline", bin: baselinePath, dataDir: baselineDataDir },
      { label: "candidate", bin: candidateBin, dataDir: candidateDataDir },
    ]) {
      const port = await reservePort();
      const variant = { ...spec, base: `http://127.0.0.1:${port}`, docs: new Map(), spaces: new Map() };
      variant.state = startServer(spec.label, spec.bin, spec.dataDir, port, runDir);
      await waitHealthy(variant.state, variant.base);
      const exp = (await http(variant, "setup", "rest-export-replica", "GET", "/api/export")).parsed;
      ensure(exp.articles.length === 33, `${spec.label} replica export has ${exp.articles.length} docs, expected 33`);
      ensure(canonical(exp) === canonical(seedExport), `${spec.label} export differs from the stopped seeded snapshot`);
      variant.export = exp;
      variant.exportHash = sha256(canonical(exp));
      ensure(variant.exportHash === sha256(canonical(seedExport)), `${spec.label} snapshot hash mismatch`);
      variant.articlesById = new Map(exp.articles.map((a) => [a.id, { ...a, body: a.body ?? "" }]));
      for (const doc of corpus.documents) {
        const seededDoc = seedVariant.docs.get(doc.key);
        ensure(seededDoc, `seed ID map is missing fixture article ${doc.key}`);
        const row = variant.articlesById.get(seededDoc.id);
        ensure(row && row.title === doc.title, `${spec.label} export lost seed-mapped fixture article ${doc.key}`);
        variant.docs.set(doc.key, { id: seededDoc.id, spaceId: seededDoc.spaceId, doc, body: row.body, revision: row.revision });
      }
      for (const space of corpus.spaces) {
        const id = seedVariant.spaces.get(space.key);
        ensure(id && exp.spaces.some((row) => row.id === id && row.name === space.name), `${spec.label} export lost seed-mapped fixture space ${space.key}`);
        variant.spaces.set(space.key, id);
      }
      await initializeVariant(variant, corpus);
      summary.setup.replicas[spec.label] = { exportSha256: variant.exportHash, docs: exp.articles.length, initialized: true };
      variants.push(variant);
    }
    ensure(variants[0].exportHash === variants[1].exportHash, "baseline/candidate exports are not equal snapshots");
    for (const key of corpus.documents.map((x) => x.key)) {
      ensure(variants[0].docs.get(key).id === variants[1].docs.get(key).id, `replica article ID differs for ${key}`);
    }
    for (const key of corpus.spaces.map((x) => x.key)) ensure(variants[0].spaces.get(key) === variants[1].spaces.get(key), `replica space ID differs for ${key}`);

    // Freeze query hashes before any section-search request. Anchor resolution uses outlines only.
    summary.integrity.frozenAtBeforeSearch = new Date().toISOString();
    summary.anchorResolution = {};
    const golds = await getGolds(corpus, querySets, variants[0]);
    for (const set of querySets) {
      for (const q of set.queries) {
        const resolved = golds.get(q.key);
        summary.anchorResolution[q.key] = resolved.resolvedSections.map(({ articleKey, headingTitle, headingOccurrence, anchor, revision, bodyStart, bodyEnd }) => ({ articleKey, headingTitle, headingOccurrence, anchor, revision, bodyStart, bodyEnd }));
      }
    }

    const allQueries = querySets.flatMap((set) => set.queries.map((q) => ({ ...q, setName: set.name })));
    summary.verification = {};
    const verifiedArgsByQueryVariant = new Map();
    for (const query of allQueries) {
      const gold = golds.get(query.key);
      summary.verification[query.key] = {};
      for (const variant of variants) {
        const verified = await verifyQueryVariant(variant, query, gold);
        verifiedArgsByQueryVariant.set(`${query.setName}\u0000${query.key}\u0000${variant.label}`,
          new Set(verified.results.map((hit) => canonical({ id: hit.id, read_args: hit.read_args }))));
        summary.verification[query.key][variant.label] = {
          mode: verified.mode, total: verified.total, payloadSha256: verified.payloadSha256,
          results: verified.verifiedReads.map(({ rank, id, heading, anchor, readArgs, textChars }) => ({ rank, id, heading, anchor, readArgs, textChars })),
        };
        const verifyKey = `${query.setName}\u0000${query.key}\u0000${variant.label}`;
        verifiedArgsByQueryVariant.set(`${verifyKey}\u0000payload`, verified.payloadSha256);
      }
    }

    const records = [];
    for (const query of allQueries) {
      const gold = golds.get(query.key);
      const argsSetByVariant = new Map(variants.map((variant) => [variant.label,
        verifiedArgsByQueryVariant.get(`${query.setName}\u0000${query.key}\u0000${variant.label}`)]));
      const payloadHashByVariant = new Map(variants.map((variant) => [variant.label,
        verifiedArgsByQueryVariant.get(`${query.setName}\u0000${query.key}\u0000${variant.label}\u0000payload`)]));
      for (const variant of variants) {
        const sample = await measureOne(variant, query, gold, 0, "warmup", argsSetByVariant.get(variant.label), payloadHashByVariant.get(variant.label));
        records.push({ ...sample, variant: variant.label, warmup: true });
      }
      for (let rep = 1; rep <= opts.repetitions; rep++) {
        const order = rep % 2 ? variants : [...variants].reverse();
        for (const variant of order) {
          const sample = await measureOne(variant, query, gold, rep, "measure", argsSetByVariant.get(variant.label), payloadHashByVariant.get(variant.label));
          records.push({ ...sample, variant: variant.label, warmup: false });
        }
      }
    }
    summary.records = records;
    summary.results = {};
    for (const set of querySets) {
      summary.results[set.name] = {};
      for (const variant of variants) summary.results[set.name][variant.label] = summarizeSet(records, set, variant.label);
      summary.results[set.name].paired = set.queries.map((q) => {
        const pairRows = records.filter((r) => r.set === set.name && r.key === q.key && !r.warmup);
        return { key: q.key, baselineMedianMs: stats(pairRows.filter((r) => r.variant === "baseline").map((r) => r.cost.durationMs)).median,
          candidateMedianMs: stats(pairRows.filter((r) => r.variant === "candidate").map((r) => r.cost.durationMs)).median,
          baselineMedianBytes: stats(pairRows.filter((r) => r.variant === "baseline").map((r) => r.cost.totalBytes)).median,
          candidateMedianBytes: stats(pairRows.filter((r) => r.variant === "candidate").map((r) => r.cost.totalBytes)).median,
          baselineMedianTokens: stats(pairRows.filter((r) => r.variant === "baseline").map((r) => r.cost.totalTokens)).median,
          candidateMedianTokens: stats(pairRows.filter((r) => r.variant === "candidate").map((r) => r.cost.totalTokens)).median };
      });
    }
    const finalExportHashes = {};
    let finalExportsUnchanged = true;
    for (const variant of variants) {
      const finalExport = (await http(variant, "final-verify", "rest-export-final", "GET", "/api/export")).parsed;
      const finalHash = sha256(canonical(finalExport));
      finalExportHashes[variant.label] = finalHash;
      if (canonical(finalExport) !== canonical(seedExport)) finalExportsUnchanged = false;
    }
    summary.integrity.finalExportsUnchanged = finalExportsUnchanged;
    summary.integrity.finalExportCanonicalSha256 = finalExportHashes;
    ensure(finalExportsUnchanged, "one or both replica exports changed after the paired evaluation");
    summary.status = "ok";
  } catch (err) {
    summary.status = "failed";
    summary.error = String(err?.stack ?? err);
    log(`FATAL ${summary.error}`);
  } finally {
    try { if (seedState) await stopServer(seedState); } catch (e) { summary.cleanupError = String(e); }
    try { await stopAll(); } catch (e) { summary.cleanupError = [summary.cleanupError, String(e)].filter(Boolean).join("\n"); }
    summary.childProcesses = [...children.values()].map((s) => ({ variant: s.label, exited: s.exited, exitInfo: s.exitInfo }));
    summary.requestCountsByPhase = ctx.countByPhase;
    summary.finishedAt = new Date().toISOString();
    if (ctx.traceFd !== null) fs.closeSync(ctx.traceFd);
    fs.writeFileSync(summaryPath, `${JSON.stringify(summary, null, 2)}\n`);
  }
  const stdout = { status: summary.status, runId, localSummary: "written under this run's private .cache directory", querySets: summary.integrity?.querySets, baselineBinarySha256: summary.integrity?.baselineBinarySha256,
    results: summary.results ? Object.fromEntries(Object.entries(summary.results).map(([name, variants]) => [name, Object.fromEntries(["baseline", "candidate"].map((v) => [v, {
      top1Coverage: variants[v].top1Coverage, top5Coverage: variants[v].top5Coverage,
      recallAt5MacroMean: variants[v].recallAt5MacroMean, mrrAt5MacroMean: variants[v].mrrAt5MacroMean,
      latencyMs: variants[v].latencyMs,
    }]))])) : undefined,
  };
  process.stdout.write(`${JSON.stringify(stdout, null, 2)}\n`);
  if (summary.status !== "ok") process.exitCode = 1;
}

main().catch((e) => { process.stderr.write(`[section-eval] fatal before run directory: ${e?.stack ?? e}\n`); process.exitCode = 1; });
