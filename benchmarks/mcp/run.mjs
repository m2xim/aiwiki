// MCP baseline and scripted candidate benchmark runner. See both methodology files.
//
// Builds the real server from this repository, starts it as a child process on a
// loopback ephemeral port with a fresh data directory under .cache/mcp-bench-*,
// seeds the frozen corpus (corpus.mjs, makeCorpus()) through the REST API and then
// measures real MCP JSON-RPC calls over HTTP. There is no mock server, no LLM and
// no network access beyond the child's loopback port.
//
// Usage: node run.mjs [--repetitions N (default 20, minimum 5)] [--out .cache/<file>.json]
// stdout: concise JSON summary. stderr: progress. Files: summary JSON + traces JSONL.

import { execFile, spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { fileURLToPath, pathToFileURL } from "node:url";
import { promisify } from "node:util";

const execFileP = promisify(execFile);

const BENCH_DIR = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(BENCH_DIR, "..", "..");
const CACHE = path.join(ROOT, ".cache");

const TOKENIZER_PACKAGE = "js-tiktoken";
const TOKENIZER_VERSION = "1.0.21";
const ENCODING = "cl100k_base";
const MIN_REPETITIONS = 5;
const SEARCH_LIMIT = 10; // top-10 page; top-5 is its prefix
const SECTION_SEARCH_LIMIT = 5;
const BOUNDED_READ_CHARS = 4000;
const MAX_BOUNDED_PAGES = 128;
const BUILD_TIMEOUT_MS = 5 * 60 * 1000;
const HEALTH_TIMEOUT_MS = 30 * 1000;
const REQUEST_TIMEOUT_MS = 30 * 1000;
const PROTOCOL_VERSION = "2025-03-26"; // what the server's initialize advertises
const AUTHOR = "aiwiki-mcp-bench";
const BUILTIN_SPACES = ["engineering", "general", "genealogy"];
const BUILTIN_ARTICLES = 5;

const METRICS = ["calls", "requests", "requestBytes", "responseBytes", "totalBytes", "outputTokens", "inputTokens", "totalTokens", "durationMs"];
const RATIO_METRICS = ["requests", "responseBytes", "totalBytes", "inputTokens", "outputTokens", "totalTokens", "durationMs"];

const DEFINITIONS = {
  requestBytes: "UTF-8 bytes of the JSON-RPC request body (headers excluded)",
  responseBytes: "bytes of the raw HTTP response body (headers excluded)",
  outputTokens: "cl100k_base tokens of canonical tool name plus arguments JSON (fixed invocation proxy)",
  inputTokens: "cl100k_base tokens of the tool result content text (what a model would read)",
  tokenizerScope: "fixed tokenizer proxy: not Claude tokens, not billed usage, not full chat serialization; SDK prefixes and JSON-RPC envelope are not tokenized",
  durationMs: "monotonic (performance.now) local HTTP request-to-body-read time, summed over the calls of a task; script time between calls excluded",
  percentiles: "nearest rank on sorted samples: value at index ceil(p*n/100)-1; used for both median (p50) and p95; p95IsMax flags when p95 is the sample maximum (n <= 19)",
  tasks: "scripted tasks; B/C choose the heading with fixture ground truth, D chooses section-search rank 1 without oracle filtering, and E uses a known target anchor as an oracle lower bound",
};

const LIMITATIONS = [
  "No LLM run: scripted calls only; no model reasoning, autonomous navigation or provider usage/billing is measured. Candidate answers and sources are graded against fixture literals.",
  "Baseline A/B/C retains article-level search; B/C select sections with fixture ground truth. Candidate D uses section-search top-1 without oracle filtering, while E is a known-target-anchor lower bound.",
  "Tokens are a cl100k_base proxy (js-tiktoken), not Claude tokens; SDK-specific prefixes and JSON-RPC envelopes are not tokenized.",
  "Local loopback HTTP on one machine: clocks, OS scheduling, GC and SQLite caching add noise; durations are not model latency or production latency.",
  "No ACL/authentication (the app is unauthenticated), no concurrency load, no production-scale data: sample counts are small and are not capacity measurements.",
  "The corpus is authored by hand (synthetic, frozen); results do not generalize beyond it and ground truth is not tuned against the measured ranking.",
  "The fresh server contains the application's built-in seeded Russian articles; they compete in global search and appear in wiki_bootstrap output.",
  "Standalone [[link:KEY]] tokens are resolved by this runner into [Title](/?article=ID); inside ](...) or after article= they become /?article=ID or ID.",
];

// ---------------------------------------------------------------- utilities

const log = (message) => process.stderr.write(`[bench] ${message}\n`);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const sha256 = (data) => createHash("sha256").update(data).digest("hex");
const utf8Bytes = (text) => Buffer.byteLength(text, "utf8");
const round3 = (n) => Math.round(n * 1000) / 1000;

class ExpectationError extends Error {}

function rank(sorted, p) {
  return sorted[Math.max(1, Math.ceil((p * sorted.length) / 100)) - 1];
}

function stats(values) {
  const v = values.filter((x) => Number.isFinite(x)).sort((a, b) => a - b);
  if (v.length === 0) return { n: 0, median: null, p95: null, p95IsMax: false, min: null, max: null, mean: null };
  const p95 = rank(v, 95);
  return { n: v.length, median: rank(v, 50), p95, p95IsMax: Math.ceil(0.95 * v.length) === v.length, min: v[0], max: v[v.length - 1], mean: v.reduce((a, b) => a + b, 0) / v.length };
}

function canon(value) {
  if (Array.isArray(value)) return `[${value.map(canon).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((k) => `${JSON.stringify(k)}:${canon(value[k])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function countOccurrences(text, needle) {
  return needle === "" ? 0 : text.split(needle).length - 1;
}

function replaceOnce(text, needle, replacement) {
  if (countOccurrences(text, needle) !== 1) throw new Error(`expected exactly one occurrence of ${JSON.stringify(needle)}`);
  const i = text.indexOf(needle);
  return text.slice(0, i) + replacement + text.slice(i + needle.length);
}

function parseArgs(argv) {
  const opts = { repetitions: 20, out: null };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const eq = arg.startsWith("--") ? arg.indexOf("=") : -1;
    const flag = eq >= 0 ? arg.slice(0, eq) : arg;
    const value = () => {
      if (eq >= 0) return arg.slice(eq + 1);
      if (i + 1 >= argv.length) throw new Error(`${flag} needs a value`);
      return argv[++i];
    };
    if (flag === "--repetitions") {
      const v = value();
      if (!/^\d+$/.test(v) || Number(v) < MIN_REPETITIONS || Number(v) > 1000) {
        throw new Error(`--repetitions must be an integer from ${MIN_REPETITIONS} to 1000`);
      }
      opts.repetitions = Number(v);
    } else if (flag === "--out") {
      opts.out = value();
    } else {
      throw new Error(`unknown argument ${arg}`);
    }
  }
  return opts;
}

// --out must be a local file path inside <repo>/.cache; URLs and escapes are rejected.
function resolveOut(out) {
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(out)) throw new Error("--out must be a local path inside .cache, not a URL");
  const abs = path.resolve(out);
  const rel = path.relative(CACHE, abs);
  if (rel === "" || rel.startsWith("..") || path.isAbsolute(rel)) throw new Error(`--out must be inside ${CACHE}`);
  if (!/\.json$/i.test(abs)) throw new Error("--out must end with .json");
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  const realCache = fs.realpathSync(CACHE);
  const realParent = fs.realpathSync(path.dirname(abs));
  const realRel = path.relative(realCache, realParent);
  if (realRel.startsWith("..") || path.isAbsolute(realRel)) throw new Error("--out resolves outside .cache (symlink)");
  const traces = abs.replace(/\.json$/i, "") + ".traces.jsonl";
  for (const f of [abs, traces]) if (fs.existsSync(f)) throw new Error(`refusing to overwrite ${f}`);
  return { summary: abs, traces };
}

// ---------------------------------------------------------------- corpus and tokenizer

async function loadCorpus() {
  const file = path.join(BENCH_DIR, "corpus.mjs");
  if (!fs.existsSync(file)) throw new Error("corpus.mjs is missing next to run.mjs");
  const mod = await import(pathToFileURL(file).href);
  if (typeof mod.makeCorpus !== "function") throw new Error("corpus.mjs must export makeCorpus()");
  const corpus = mod.makeCorpus();
  const canonical = JSON.stringify(corpus);
  if (JSON.stringify(mod.makeCorpus()) !== canonical) throw new Error("makeCorpus() is not deterministic");
  validateCorpus(corpus);
  return { corpus, hashes: { fileSha256: sha256(fs.readFileSync(file)), contentSha256: sha256(canonical) } };
}

function validateCorpus(c) {
  const bad = (msg) => { throw new Error(`corpus: ${msg}`); };
  const str = (v, what) => { if (typeof v !== "string" || v === "") bad(`${what} must be a non-empty string`); };
  for (const k of ["spaces", "documents", "searchTasks", "readTasks"]) if (!Array.isArray(c[k]) || c[k].length === 0) bad(`${k} must be a non-empty array`);
  if (c.version === undefined || c.version === null) bad("version is missing");
  const unique = (list, what) => {
    const seen = new Set();
    for (const x of list) {
      str(x.key, `${what}.key`);
      if (seen.has(x.key)) bad(`duplicate ${what} key ${x.key}`);
      seen.add(x.key);
    }
    return seen;
  };
  const spaces = unique(c.spaces, "space");
  for (const s of c.spaces) str(s.name, `space ${s.key} name`);
  const docs = unique(c.documents, "document");
  const bodies = new Map();
  for (const d of c.documents) {
    if (!spaces.has(d.spaceKey)) bad(`document ${d.key} has unknown space ${d.spaceKey}`);
    str(d.title, `document ${d.key} title`);
    if (typeof d.summary !== "string" || typeof d.body !== "string") bad(`document ${d.key} summary/body must be strings`);
    if (!Array.isArray(d.tags) || d.tags.some((t) => typeof t !== "string")) bad(`document ${d.key} tags must be strings`);
    if (!d.properties || typeof d.properties !== "object" || Object.values(d.properties).some((v) => typeof v !== "string")) bad(`document ${d.key} properties must map strings to strings`);
    bodies.set(d.key, d.body);
  }
  unique(c.searchTasks, "searchTask");
  for (const t of c.searchTasks) {
    str(t.query, `searchTask ${t.key} query`);
    str(t.category, `searchTask ${t.key} category`);
    if (!Array.isArray(t.relevantArticleKeys)) bad(`searchTask ${t.key} relevantArticleKeys must be an array`);
    for (const k of t.relevantArticleKeys) if (!docs.has(k)) bad(`searchTask ${t.key} references unknown document ${k}`);
  }
  unique(c.readTasks, "readTask");
  for (const t of c.readTasks) {
    str(t.query, `readTask ${t.key} query`);
    str(t.headingTitle, `readTask ${t.key} headingTitle`);
    str(t.answer, `readTask ${t.key} answer`);
    if (!docs.has(t.articleKey)) bad(`readTask ${t.key} references unknown document ${t.articleKey}`);
    if (!Number.isInteger(t.headingOccurrence) || t.headingOccurrence < 1) bad(`readTask ${t.key} headingOccurrence must be an integer >= 1`);
    if (!bodies.get(t.articleKey).includes(t.answer)) bad(`readTask ${t.key} answer is not a literal substring of its article body`);
  }
}

async function loadTokenizer() {
  let mod;
  try {
    mod = await import(TOKENIZER_PACKAGE);
  } catch (e) {
    throw new Error(`${TOKENIZER_PACKAGE}@${TOKENIZER_VERSION} is not installed (npm install in benchmarks/mcp): ${e.message}`);
  }
  if (typeof mod.getEncoding !== "function") throw new Error(`${TOKENIZER_PACKAGE} does not export getEncoding; check the installed package exports`);
  const version = installedPackageVersion(TOKENIZER_PACKAGE);
  if (version !== TOKENIZER_VERSION) throw new Error(`${TOKENIZER_PACKAGE} version is ${version}, expected ${TOKENIZER_VERSION}`);
  const enc = mod.getEncoding(ENCODING);
  // Plain text that merely looks like a special token must be counted as text, not rejected.
  const count = (text) => enc.encode(text, [], []).length;
  count("<|endoftext|>");
  return { count, info: { package: TOKENIZER_PACKAGE, version, encoding: ENCODING, api: "getEncoding", specialTokens: "counted as ordinary text" } };
}

function installedPackageVersion(name) {
  let dir = path.dirname(fileURLToPath(import.meta.resolve(name)));
  for (;;) {
    const file = path.join(dir, "package.json");
    if (fs.existsSync(file)) {
      const pkg = JSON.parse(fs.readFileSync(file, "utf8"));
      if (pkg.name === name) return pkg.version;
    }
    const parent = path.dirname(dir);
    if (parent === dir) throw new Error(`cannot find package.json of ${name}`);
    dir = parent;
  }
}

// ---------------------------------------------------------------- environment, build, child

async function gitInfo() {
  try {
    const git = (args) => execFileP("git", args, { cwd: ROOT, maxBuffer: 64 * 1024 * 1024 });
    const commit = (await git(["rev-parse", "HEAD"])).stdout.trim();
    const lines = (await git(["status", "--porcelain=v1", "--untracked-files=all"])).stdout.split("\n").filter(Boolean);
    const untracked = lines.filter((l) => l.startsWith("??")).map((l) => l.slice(3));
    const tracked = lines.filter((l) => !l.startsWith("??"));
    return { commit, dirty: lines.length > 0, trackedChanges: tracked, untracked };
  } catch (e) {
    return { commit: null, dirty: null, error: String(e.message) };
  }
}

async function buildServer(binPath) {
  const env = { ...process.env, GOPROXY: "off", GOTOOLCHAIN: "local", GOFLAGS: "-mod=readonly" };
  const t0 = performance.now();
  try {
    await execFileP("go", ["build", "-o", binPath, "./cmd/aiwiki"], { cwd: ROOT, env, timeout: BUILD_TIMEOUT_MS, maxBuffer: 16 * 1024 * 1024 });
  } catch (e) {
    const why = e.killed ? `timed out after ${BUILD_TIMEOUT_MS} ms` : `${e.stderr || e.message}`;
    throw new Error(`go build failed (GOPROXY=off, modules must be in the local cache): ${why}`);
  }
  const goVersion = (await execFileP("go", ["version"], { cwd: ROOT, env })).stdout.trim();
  return { buildMs: round3(performance.now() - t0), goVersion, binarySha256: sha256(fs.readFileSync(binPath)) };
}

function reservePort() {
  return new Promise((resolve, reject) => {
    const probe = net.createServer();
    probe.once("error", reject);
    probe.listen({ host: "127.0.0.1", port: 0 }, () => {
      const { port } = probe.address();
      probe.close((err) => (err ? reject(err) : resolve(port)));
    });
  });
}

const child = { proc: null, exited: false, exitInfo: null, exitPromise: null, logPath: null };

function startChild(binPath, dataDir, port, runDir) {
  child.logPath = path.join(runDir, "server.log");
  const logFd = fs.openSync(child.logPath, "a");
  // -web= (empty) disables static file serving; cwd is the run dir so nothing relative touches the repo.
  const proc = spawn(binPath, ["-addr", `127.0.0.1:${port}`, "-data", dataDir, "-web="], { cwd: runDir, stdio: ["ignore", logFd, logFd], windowsHide: true });
  fs.closeSync(logFd);
  child.proc = proc;
  child.exitPromise = new Promise((resolve) => {
    proc.once("exit", (code, signal) => { child.exited = true; child.exitInfo = { code, signal }; resolve(); });
    proc.once("error", (err) => { child.exited = true; child.exitInfo = { error: String(err) }; resolve(); });
  });
}

function killChildSync() {
  if (child.proc && !child.exited) child.proc.kill();
}

async function stopChild() {
  if (!child.proc || child.exited) return;
  child.proc.kill();
  await Promise.race([child.exitPromise, sleep(10_000)]);
  if (!child.exited) {
    child.proc.kill("SIGKILL");
    await Promise.race([child.exitPromise, sleep(5_000)]);
  }
  if (!child.exited) log(`WARNING: child pid ${child.proc.pid} did not exit`);
}

function logTail() {
  try {
    return fs.readFileSync(child.logPath, "utf8").slice(-2000);
  } catch {
    return "";
  }
}

async function waitHealthy(base) {
  const deadline = performance.now() + HEALTH_TIMEOUT_MS;
  while (performance.now() < deadline) {
    if (child.exited) throw new Error(`server child exited before becoming healthy: ${JSON.stringify(child.exitInfo)}\n${logTail()}`);
    try {
      const res = await fetch(`${base}/api/health`, { signal: AbortSignal.timeout(2000) });
      if (res.ok && (await res.json()).ok === true) {
        if (child.exited) throw new Error(`server child exited while health was answered (another server may own the port): ${logTail()}`);
        return;
      }
    } catch (e) {
      if (child.exited) continue;
    }
    await sleep(100);
  }
  throw new Error(`server did not become healthy within ${HEALTH_TIMEOUT_MS} ms\n${logTail()}`);
}

// ---------------------------------------------------------------- traced transport

const ctx = {
  base: null,
  rpcId: 0,
  seq: 0,
  tracesFd: null,
  count: null, // request counts by phase
  countTokens: null,
  corpusVersion: null,
  failures: [],
  docs: new Map(), // fixture key -> {id, spaceId, doc, body, revision}
  spaces: new Map(), // fixture key -> server id
};

function newMeta(phase, scenario, strategy, repetition, scope) {
  return { phase, scenario: scenario ?? null, strategy: strategy ?? null, repetition: repetition ?? null, scope: scope ?? null };
}

function record(meta, rec) {
  const full = { seq: ++ctx.seq, corpusVersion: ctx.corpusVersion, phase: meta.phase, scenario: meta.scenario, strategy: meta.strategy, repetition: meta.repetition, ...rec };
  fs.appendFileSync(ctx.tracesFd, JSON.stringify(full) + "\n"); // loops until the whole line is written
  if (meta.scope) {
    meta.scope.push({
      isTool: rec.name !== null && rec.method === "tools/call",
      requestBytes: rec.requestBytes,
      responseBytes: rec.responseBytes,
      argsTokens: rec.tokens ? rec.tokens.args : null,
      contentTokens: rec.tokens ? rec.tokens.content : null,
      durationMs: rec.durationMs,
      payloadSha256: sha256(canon({ name: rec.name, args: rec.args, result: rec.result?.result })),
    });
  }
  ctx.count[meta.phase] = (ctx.count[meta.phase] ?? 0) + 1;
}

async function send(method, route, bodyText) {
  const t0 = performance.now();
  const res = await fetch(ctx.base + route, {
    method,
    headers: bodyText === undefined ? {} : { "content-type": "application/json" },
    body: bodyText,
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
  });
  const buf = Buffer.from(await res.arrayBuffer());
  return { status: res.status, buf, durationMs: performance.now() - t0 };
}

async function rest(method, route, body, meta, expectStatus = [200, 201]) {
  const bodyText = body === undefined ? undefined : JSON.stringify(body);
  const r = await send(method, route, bodyText);
  const text = r.buf.toString("utf8");
  let parsed;
  try { parsed = JSON.parse(text); } catch { parsed = text; }
  record(meta, {
    transport: "rest", method, name: null, path: route, args: body ?? null, result: parsed, httpStatus: r.status,
    requestBytes: bodyText === undefined ? 0 : utf8Bytes(bodyText), responseBytes: r.buf.length, tokens: null,
    durationMs: round3(r.durationMs), expected: `status:${expectStatus.join("|")}`, isError: r.status >= 400,
  });
  if (!expectStatus.includes(r.status)) throw new Error(`${method} ${route} -> HTTP ${r.status}: ${text.slice(0, 500)}`);
  return parsed;
}

// One JSON-RPC request. tokensFor(result) returns {args, content} token counts or null.
async function mcpRequest(method, params, meta, { name = null, args = null, notification = false, tokensFor = null, expected = "ok" } = {}) {
  const message = notification
    ? { jsonrpc: "2.0", method, ...(params ? { params } : {}) }
    : { jsonrpc: "2.0", id: ++ctx.rpcId, method, params };
  const bodyText = JSON.stringify(message);
  const r = await send("POST", "/mcp", bodyText);
  let envelope = null;
  let parseError = null;
  if (!notification) {
    try { envelope = JSON.parse(r.buf.toString("utf8")); } catch (e) { parseError = e; }
  }
  const result = envelope ? envelope.result ?? null : null;
  let tokens = null;
  let tokenAccountingError = null;
  try {
    tokens = result && tokensFor ? tokensFor(result) : null;
  } catch (e) {
    tokenAccountingError = String(e?.message ?? e);
  }
  const isError = result && result.isError === true;
  record(meta, {
    transport: "mcp", method, name, args, result: notification ? null : envelope, httpStatus: r.status,
    requestBytes: utf8Bytes(bodyText), responseBytes: r.buf.length, tokens, durationMs: round3(r.durationMs),
    expected, isError: Boolean(isError), hasStructuredContent: Boolean(result && result.structuredContent !== undefined),
    ...(tokenAccountingError ? { tokenAccountingError } : {}),
  });
  if (tokenAccountingError) throw new Error(tokenAccountingError);
  if (notification) {
    if (r.status !== 202 || r.buf.length !== 0) throw new Error(`${method}: expected empty 202, got ${r.status} (${r.buf.length} bytes)`);
    return { result: null, requestBytes: utf8Bytes(bodyText), responseBytes: 0, durationMs: r.durationMs };
  }
  if (parseError || r.status !== 200) throw new Error(`${method}: HTTP ${r.status}, unparsable or failed response`);
  if (envelope.error) throw new Error(`${method}: JSON-RPC error ${JSON.stringify(envelope.error)}`);
  if (envelope.id !== message.id) throw new Error(`${method}: JSON-RPC id mismatch`);
  return { result, requestBytes: utf8Bytes(bodyText), responseBytes: r.buf.length, durationMs: r.durationMs };
}

// expect: "ok" (tool error aborts), "either" (caller inspects isError), or a RegExp
// that an expected tool error text must match (success or other error throws).
async function callTool(name, args, meta, expect = "ok") {
  let text = "";
  const tokensFor = (result) => {
    if (result.structuredContent !== undefined) throw new Error(`${name}: structuredContent requires explicit token accounting`);
    if (!Array.isArray(result.content) || result.content.some((c) => c.type !== "text" || typeof c.text !== "string")) {
      throw new Error(`${name}: tool result is not text content`);
    }
    text = result.content.map((c) => c.text).join("");
    return { args: ctx.countTokens(name) + ctx.countTokens(JSON.stringify(args)), content: ctx.countTokens(text) };
  };
  const expected = expect instanceof RegExp ? `error:${expect.source}` : expect;
  const r = await mcpRequest("tools/call", { name, arguments: args }, meta, { name, args, tokensFor, expected });
  if (typeof r.result.isError !== "boolean") throw new Error(`${name}: result.isError is missing`);
  const isError = r.result.isError;
  if (expect === "ok" && isError) throw new Error(`${name}: unexpected tool error: ${text}`);
  if (expect instanceof RegExp) {
    if (!isError) throw new ExpectationError(`${name}: expected error /${expect.source}/ but the call succeeded`);
    if (!expect.test(text)) throw new ExpectationError(`${name}: error does not match /${expect.source}/: ${text}`);
  }
  let parsed = null;
  try { parsed = JSON.parse(text); } catch { if (!isError) throw new Error(`${name}: success result is not JSON`); }
  return { isError, text, parsed };
}

function sumScope(scope) {
  const s = { calls: 0, requests: scope.length, requestBytes: 0, responseBytes: 0, outputTokens: 0, inputTokens: 0, durationMs: 0 };
  for (const c of scope) {
    if (c.isTool) s.calls++;
    s.requestBytes += c.requestBytes;
    s.responseBytes += c.responseBytes;
    s.outputTokens += c.argsTokens ?? 0;
    s.inputTokens += c.contentTokens ?? 0;
    s.durationMs += c.durationMs;
  }
  s.totalBytes = s.requestBytes + s.responseBytes;
  s.totalTokens = s.outputTokens + s.inputTokens;
  s.durationMs = round3(s.durationMs);
  return s;
}

function fail(scenario, strategy, repetition, check, detail) {
  ctx.failures.push({ scenario, strategy, repetition, check, detail });
  log(`FAIL ${scenario} ${strategy ?? ""} rep=${repetition}: ${check}: ${detail}`);
}

// ---------------------------------------------------------------- seeding (setup, not measured)

function resolveLinks(body, docs, ownKey) {
  return body.replace(/\[\[link:([^\]\s]+)\]\]/g, (token, key, offset, whole) => {
    const target = docs.get(key);
    if (!target) throw new Error(`document ${ownKey}: [[link:${key}]] references an unknown document`);
    if (whole.slice(Math.max(0, offset - 2), offset) === "](") return `/?article=${target.id}`;
    if (whole.slice(Math.max(0, offset - 8), offset) === "article=") return target.id;
    return `[${target.doc.title}](/?article=${target.id})`;
  });
}

async function seed(corpus) {
  const meta = newMeta("setup", "seed");
  const t0 = performance.now();
  const fresh = await rest("GET", "/api/export", undefined, meta);
  const spaceIds = fresh.spaces.map((s) => s.id).sort();
  if (canon(spaceIds) !== canon([...BUILTIN_SPACES].sort()) || fresh.articles.length !== BUILTIN_ARTICLES || fresh.articles.some((a) => a.author !== "system")) {
    throw new Error("server is not a fresh instance (unexpected spaces/articles); refusing to continue");
  }
  const builtin = { spaces: spaceIds, articles: fresh.articles.map((a) => a.id).sort() };

  for (const space of corpus.spaces) {
    const created = await rest("POST", "/api/spaces", { name: space.name, description: space.description ?? "" }, meta);
    ctx.spaces.set(space.key, created.id);
  }
  for (const doc of corpus.documents) {
    const created = await rest("POST", "/api/articles", {
      space_id: ctx.spaces.get(doc.spaceKey), title: doc.title, summary: doc.summary, body: doc.body,
      tags: doc.tags, properties: doc.properties, author: AUTHOR, message: "seed",
    }, meta);
    ctx.docs.set(doc.key, { id: created.id, spaceId: created.space_id, doc, body: created.body, revision: created.revision });
  }
  // Second pass: article IDs are known now, so [[link:KEY]] can be resolved using base revisions.
  let linked = 0;
  for (const [key, state] of ctx.docs) {
    const body = resolveLinks(state.body, ctx.docs, key);
    if (body === state.body) continue;
    const updated = await rest("PUT", `/api/articles/${state.id}`, { body, base_revision: state.revision, author: AUTHOR, message: "resolve links" }, meta);
    state.body = updated.body;
    state.revision = updated.revision;
    linked++;
  }
  const after = await rest("GET", "/api/export", undefined, meta);
  if (after.articles.length !== BUILTIN_ARTICLES + corpus.documents.length) throw new Error("seeded article count does not match the corpus");
  for (const task of corpus.readTasks) {
    const bodyMatches = after.articles.reduce((n, article) => n + countOccurrences(article.body ?? "", task.answer), 0);
    if (bodyMatches !== 1) throw new Error(`read answer ${task.key} occurs ${bodyMatches} times across exported article bodies; expected exactly one`);
    const metadata = after.articles.flatMap((article) => [article.title ?? "", article.summary ?? "", ...(article.tags ?? []), ...String(article.body ?? "").split(/\r?\n/).filter((line) => /^\s{0,3}#{1,6}\s/.test(line))]);
    if (metadata.some((value) => value.includes(task.answer))) throw new Error(`read answer ${task.key} appears in an exported title, summary, tag, or heading`);
  }
  return { seedMs: round3(performance.now() - t0), builtin, spaces: corpus.spaces.length, documents: corpus.documents.length, documentsWithResolvedLinks: linked };
}

// Resolves each read task's anchor once with the outline call (setup phase, not measured).
async function resolveReadContexts(corpus) {
  const info = new Map();
  for (const task of corpus.readTasks) {
    const state = ctx.docs.get(task.articleKey);
    const { parsed } = await callTool("wiki_read", { id: state.id, outline: true }, newMeta("setup", `anchor:${task.key}`));
    const h = pickHeading(parsed.headings, task.headingTitle, task.headingOccurrence);
    const section = await callTool("wiki_read", { id: state.id, anchor: h.anchor }, newMeta("setup", `section-offset:${task.key}`));
    const sectionText = String(section.parsed.section ?? "");
    const answerIndex = sectionText.indexOf(task.answer);
    if (answerIndex < 0) throw new Error(`setup: answer ${task.key} is not inside its expected section`);
    const answerOffset = Array.from(sectionText.slice(0, answerIndex)).length;
    info.set(task.key, {
      id: state.id, anchor: h.anchor, level: h.level, sectionChars: h.chars, bodyChars: parsed.chars, bodyBytes: utf8Bytes(state.body),
      headings: parsed.headings.length, answerOccurrencesInBody: countOccurrences(state.body, task.answer), answerOffsetWithinSection: answerOffset,
    });
  }
  return info;
}

function pickHeading(headings, title, occurrence) {
  const matches = headings.filter((h) => h.title.trim() === title.trim());
  const h = matches[occurrence - 1];
  if (!h) throw new Error(`oracle: heading ${JSON.stringify(title)} occurrence ${occurrence} not found (${matches.length} with that title)`);
  return h;
}

// ---------------------------------------------------------------- shared setup overhead

async function measureSetupOverhead(firstSpaceId) {
  const meta = newMeta("setup_overhead", "session-setup");
  const init = await mcpRequest("initialize", {
    protocolVersion: PROTOCOL_VERSION, capabilities: {}, clientInfo: { name: "aiwiki-mcp-bench", version: "0.1.0" },
  }, meta, { tokensFor: (r) => ({ args: null, content: ctx.countTokens(String(r.instructions ?? "")) }) });
  if (typeof init.result.instructions !== "string") throw new Error("initialize result has no instructions");
  const note = await mcpRequest("notifications/initialized", undefined, meta, { notification: true });
  const list = await mcpRequest("tools/list", {}, meta, { tokensFor: (r) => ({ args: null, content: ctx.countTokens(JSON.stringify(r.tools)) }) });
  const names = list.result.tools.map((t) => t.name);
  for (const need of ["wiki_bootstrap", "wiki_search", "wiki_search_sections", "wiki_read", "wiki_edit", "wiki_write"]) {
    if (!names.includes(need)) throw new Error(`tools/list lacks ${need}`);
  }
  const scope = [];
  const boot = await callTool("wiki_bootstrap", { space_id: firstSpaceId }, { ...meta, scope });
  const bs = sumScope(scope);
  const instructionsTokens = ctx.countTokens(init.result.instructions);
  const toolsTokens = ctx.countTokens(JSON.stringify(list.result.tools));
  const requestBytes = init.requestBytes + note.requestBytes + list.requestBytes + bs.requestBytes;
  const responseBytes = init.responseBytes + note.responseBytes + list.responseBytes + bs.responseBytes;
  const overhead = {
    calls: 1,
    requests: 4,
    requestBytes,
    responseBytes,
    totalBytes: requestBytes + responseBytes,
    outputTokens: bs.outputTokens,
    inputTokens: instructionsTokens + toolsTokens + bs.inputTokens,
    totalTokens: bs.outputTokens + instructionsTokens + toolsTokens + bs.inputTokens,
    durationMs: round3(init.durationMs + note.durationMs + list.durationMs + bs.durationMs),
  };
  return {
    overhead,
    parts: {
      initializeInstructions: { chars: init.result.instructions.length, bytes: utf8Bytes(init.result.instructions), tokens: instructionsTokens, requestBytes: init.requestBytes, responseBytes: init.responseBytes, durationMs: round3(init.durationMs) },
      initializedNotification: { requestBytes: note.requestBytes, responseBytes: note.responseBytes, durationMs: round3(note.durationMs) },
      toolsList: { tools: names.length, schemaBytes: utf8Bytes(JSON.stringify(list.result.tools)), tokens: toolsTokens, requestBytes: list.requestBytes, responseBytes: list.responseBytes, durationMs: round3(list.durationMs) },
      bootstrap: { ...bs, spaceArgument: "first fixture space", contentChars: boot.text.length },
    },
    note: "single sample per run; shared by every strategy and added once per session/task view, never per call",
  };
}

// ---------------------------------------------------------------- search quality

function evaluateSearch(task, parsed, idToKey) {
  const relevant = new Set(task.relevantArticleKeys.map((k) => ctx.docs.get(k).id));
  const ids = parsed.results.map((r) => r.id);
  const top = (k) => ids.slice(0, k);
  const recall = (k) => (relevant.size ? top(k).filter((id) => relevant.has(id)).length / relevant.size : null);
  const rr = (k) => {
    if (!relevant.size) return null;
    const i = top(k).findIndex((id) => relevant.has(id));
    return i < 0 ? 0 : 1 / (i + 1);
  };
  return {
    task: task.key, category: task.category, relevantCount: relevant.size, returned: ids.length, total: parsed.total, mode: parsed.mode ?? "",
    recall5: recall(5), recall10: recall(10), rr5: rr(5), rr10: rr(10),
    noHitCorrect: relevant.size === 0 ? ids.length === 0 : null,
    top10: ids.map((id) => idToKey.get(id) ?? `unmapped:${id}`),
  };
}

async function runSearchPass(corpus, phase, repetition, idToKey) {
  const out = [];
  for (const task of corpus.searchTasks) {
    const meta = newMeta(phase, `search:${task.key}`, "search", repetition, []);
    const { parsed } = await callTool("wiki_search", { q: task.query, limit: SEARCH_LIMIT }, meta);
    out.push({ ...evaluateSearch(task, parsed, idToKey), rep: repetition, durationMs: sumScope(meta.scope).durationMs });
  }
  return out;
}

function qualityBlock(samples) {
  const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null);
  const withRel = samples.filter((s) => s.relevantCount > 0);
  const noHit = samples.filter((s) => s.relevantCount === 0);
  const modes = {};
  for (const s of samples) modes[s.mode === "" ? "(empty)" : s.mode] = (modes[s.mode === "" ? "(empty)" : s.mode] ?? 0) + 1;
  return {
    samples: samples.length,
    tasks: new Set(samples.map((s) => s.task)).size,
    withGroundTruth: {
      samples: withRel.length,
      recallAt5: mean(withRel.map((s) => s.recall5)),
      recallAt10: mean(withRel.map((s) => s.recall10)),
      mrrAt5: mean(withRel.map((s) => s.rr5)),
      mrrAt10: mean(withRel.map((s) => s.rr10)),
      anyRelevantInTop5: mean(withRel.map((s) => (s.recall5 > 0 ? 1 : 0))),
      anyRelevantInTop10: mean(withRel.map((s) => (s.recall10 > 0 ? 1 : 0))),
    },
    noHit: { samples: noHit.length, correct: noHit.filter((s) => s.noHitCorrect).length, accuracy: noHit.length ? noHit.filter((s) => s.noHitCorrect).length / noHit.length : null, meanReturned: mean(noHit.map((s) => s.returned)) },
    modeCounts: modes,
    durationMs: stats(samples.map((s) => s.durationMs)),
  };
}

function summarizeSearch(samples, corpus) {
  const byCategory = {};
  for (const cat of [...new Set(corpus.searchTasks.map((t) => t.category))].sort()) byCategory[cat] = qualityBlock(samples.filter((s) => s.category === cat));
  const perTask = corpus.searchTasks.map((t) => {
    const mine = samples.filter((s) => s.task === t.key);
    const first = mine[0];
    const stable = mine.every((s) => canon([s.top10, s.mode, s.total]) === canon([first.top10, first.mode, first.total]));
    return { task: t.key, category: t.category, relevant: t.relevantArticleKeys, mode: first.mode, total: first.total, top10: first.top10, recallAt5: first.recall5, recallAt10: first.recall10, rrAt10: first.rr10, noHitCorrect: first.noHitCorrect, stableAcrossRepetitions: stable, durationMs: stats(mine.map((s) => s.durationMs)) };
  });
  return {
    pageSize: SEARCH_LIMIT,
    scope: "global article-level ranking over all spaces including the app's built-in seeded articles",
    overall: qualityBlock(samples),
    byCategory,
    perTask,
    unstableTasks: perTask.filter((t) => !t.stableAcrossRepetitions).map((t) => t.task),
  };
}

// ---------------------------------------------------------------- read comparison

async function runReadTask(strategy, task, info, phase, repetition) {
  const scope = [];
  const scenario = `read:${task.key}`;
  const meta = newMeta(phase, scenario, strategy, repetition, scope);
  const state = ctx.docs.get(task.articleKey);
  const search = await callTool("wiki_search", { q: task.query, limit: SEARCH_LIMIT }, meta);
  const ids = search.parsed.results.map((r) => r.id);
  const rankIndex = ids.indexOf(state.id);
  let verified;
  if (strategy === "A") {
    const { parsed } = await callTool("wiki_read", { id: state.id }, meta);
    verified = typeof parsed.body === "string" && parsed.body.includes(task.answer);
  } else if (strategy === "B") {
    const outline = await callTool("wiki_read", { id: state.id, outline: true }, meta);
    const h = pickHeading(outline.parsed.headings, task.headingTitle, task.headingOccurrence);
    const { parsed } = await callTool("wiki_read", { id: state.id, anchor: h.anchor }, meta);
    verified = typeof parsed.section === "string" && parsed.section.includes(task.answer);
  } else {
    const { parsed } = await callTool("wiki_read", { id: state.id, anchor: info.anchor }, meta);
    verified = typeof parsed.section === "string" && parsed.section.includes(task.answer);
  }
  if (!verified) fail(scenario, strategy, repetition, "answer literal not found in returned content", task.answer);
  return { task: task.key, strategy, rep: repetition, ...sumScope(scope), payloadSha256: sha256(scope.map((call) => call.payloadSha256).join("|")), answerInSearchSnippet: search.parsed.results.some((hit) => String(hit.snippet ?? "").includes(task.answer)), targetInSearchPage: rankIndex >= 0, targetRank: rankIndex >= 0 ? rankIndex + 1 : null, searchMode: search.parsed.mode ?? "", verified };
}

// D/E use bounded reads with explicit revision-pinned continuation arguments.
// Tool-level and address errors are classified as protocol failures by the caller.
async function boundedPages(firstArgs, task, meta, stopWhenFound) {
  let args = firstArgs;
  let collected = "";
  let pages = 0;
  let hasMore = false;
  let error = null;
  let firstResult = null;
  let addressValid = true;
  const addressChecks = [];
  while (pages < MAX_BOUNDED_PAGES) {
    const call = await callTool("wiki_read", args, meta, "either");
    pages++;
    if (call.isError) { error = call.text; break; }
    const parsed = call.parsed;
    if (!parsed || typeof parsed.text !== "string") { error = "bounded wiki_read response did not contain text"; break; }
    if (firstResult === null) firstResult = parsed;
    const check = {
      id: parsed.id === args.id,
      revision: parsed.revision === args.revision,
      anchor: String(parsed.anchor ?? "") === String(args.anchor ?? ""),
    };
    addressChecks.push(check);
    if (!check.id || !check.revision || !check.anchor) addressValid = false;
    collected += parsed.text;
    hasMore = parsed.has_more === true;
    if (stopWhenFound && collected.includes(task.answer)) break;
    if (!hasMore) break;
    if (!parsed.next_read || typeof parsed.next_read !== "object") { error = "has_more response omitted next_read"; break; }
    if (parsed.next_read.id !== firstArgs.id || parsed.next_read.revision !== firstArgs.revision || String(parsed.next_read.anchor ?? "") !== String(firstArgs.anchor ?? "")) {
      addressValid = false;
    }
    args = parsed.next_read;
  }
  const capReached = pages >= MAX_BOUNDED_PAGES && hasMore && !collected.includes(task.answer);
  return { collected, pages, hasMore, error, firstResult, capReached, addressValid, addressChecks };
}

async function runCandidateTask(strategy, task, info, phase, repetition) {
  const scope = [];
  const scenario = `read:${task.key}`;
  const meta = newMeta(phase, scenario, strategy, repetition, scope);
  const state = ctx.docs.get(task.articleKey);
  let sectionHits = [];
  let read = { collected: "", pages: 0, error: null, firstResult: null, capReached: false, addressValid: false, addressChecks: [] };
  let snippetAnswer = false;
  let selected = null;
  let searchError = null;
  if (strategy === "D") {
    const searched = await callTool("wiki_search_sections", { q: task.query, limit: SECTION_SEARCH_LIMIT }, meta, "either");
    if (!searched.isError) sectionHits = searched.parsed.results ?? [];
    else searchError = searched.text;
    snippetAnswer = sectionHits.some((hit) => String(hit.snippet ?? "").includes(task.answer));
    selected = sectionHits[0] ?? null; // rank only; never inspect fixture labels before selection
    if (selected) {
      const readArgs = { ...selected.read_args, max_chars: BOUNDED_READ_CHARS };
      read = await boundedPages(readArgs, task, meta, true);
    } else if (searched.isError) {
      read.addressValid = true;
    } else {
      read.addressValid = true; // no top-1 means no read address was expected
    }
  } else {
    const firstArgs = { id: state.id, anchor: info.anchor, revision: state.revision, max_chars: BOUNDED_READ_CHARS };
    read = await boundedPages(firstArgs, task, meta, true);
  }
  const selectedArticleKey = selected ? [...ctx.docs].find(([, v]) => v.id === selected.id)?.[0] ?? null : strategy === "E" ? task.articleKey : null;
  const selectedAnchor = selected ? String(selected.anchor ?? "") : strategy === "E" ? info.anchor : null;
  const rightArticle = selectedArticleKey === task.articleKey;
  const rightAnchor = selectedAnchor === info.anchor;
  const answerFound = read.collected.includes(task.answer);
  const searchProtocolFailure = Boolean(searchError);
  const readProtocolFailure = Boolean(read.error || ((strategy === "E" || selected) && !read.addressValid));
  const protocolFailure = searchProtocolFailure || readProtocolFailure;
  const verified = rightArticle && rightAnchor && answerFound && !protocolFailure && !read.capReached;
  const failureKind = verified ? null : searchProtocolFailure ? "protocol_failure" : readProtocolFailure ? "protocol_failure" : read.capReached ? "page_cap" : !selected && strategy === "D" ? "no_section_hit" : !rightArticle || !rightAnchor ? "wrong_source" : !answerFound && rightArticle && rightAnchor && info.answerOffsetWithinSection < (selected ? Number(selected.read_args?.offset ?? 0) : 0) ? "answer_before_read_start" : "answer_not_found";
  if (searchProtocolFailure) fail(scenario, strategy, repetition, "candidate section-search protocol", searchError);
  if (readProtocolFailure) fail(scenario, strategy, repetition, "candidate bounded-read protocol", read.error ?? JSON.stringify(read.addressChecks));
  const selectedReadOffset = Number(selected ? selected.read_args?.offset ?? 0 : 0);
  const expectedAnswerOffset = info.answerOffsetWithinSection;
  return {
    task: task.key, strategy, rep: repetition, ...sumScope(scope),
    payloadSha256: sha256(scope.map((call) => call.payloadSha256).join("|")),
    selectedArticle: selectedArticleKey, selectedAnchor, rightArticle, rightAnchor,
    answerFound, verified, failureKind, protocolFailure, protocolFailureSource: searchProtocolFailure ? "section_search" : readProtocolFailure ? "bounded_read" : null,
    readAddressesValid: read.addressValid, readPages: read.pages, continuationUsed: read.pages > 1,
    selectedReadOffset, expectedAnswerOffset,
    stopConditionOracleAssisted: true,
    pageCapReached: read.capReached, toolError: read.error,
    answerInSearchSnippet: snippetAnswer, forcedReadAfterSnippetAnswer: strategy === "D" && snippetAnswer && read.pages > 0,
    top5ArticleAnchorHit: strategy === "D" ? sectionHits.some((hit) => hit.id === state.id && String(hit.anchor ?? "") === info.anchor) : null,
    top5HitCount: strategy === "D" ? sectionHits.length : null,
    searchError,
  };
}

function summarizeCandidates(records, taskKeys, repetitions) {
  const byTask = ["D", "E"].flatMap((strategy) => taskKeys.map((task) => {
    const mine = records.filter((r) => r.strategy === strategy && r.task === task);
    const first = mine[0];
    const stable = mine.every((r) => r.payloadSha256 === first?.payloadSha256);
    return {
      strategy, task, runs: mine.length, verified: mine.filter((r) => r.verified).length,
      rightArticle: mine.filter((r) => r.rightArticle).length,
      rightAnchor: mine.filter((r) => r.rightAnchor).length,
      answerFound: mine.filter((r) => r.answerFound).length,
      metrics: Object.fromEntries(METRICS.map((m) => [m, stats(mine.map((r) => r[m]))])),
      readPages: stats(mine.map((r) => r.readPages)),
      answerInSearchSnippet: mine.filter((r) => r.answerInSearchSnippet).length,
      forcedReadAfterSnippetAnswer: mine.filter((r) => r.forcedReadAfterSnippetAnswer).length,
      payloadStableAcrossRepetitions: stable,
      protocolFailures: mine.filter((r) => r.protocolFailure).length,
      failures: mine.filter((r) => !r.verified).map((r) => ({ rep: r.rep, kind: r.failureKind, rightArticle: r.rightArticle, rightAnchor: r.rightAnchor, answerFound: r.answerFound, selectedReadOffset: r.selectedReadOffset, expectedAnswerOffset: r.expectedAnswerOffset, protocolFailureSource: r.protocolFailureSource, toolError: r.toolError, searchError: r.searchError, pageCapReached: r.pageCapReached })),
    };
  }));
  return {
    repetitions, scope: "per-task only; no pooled ratios or cross-task cost aggregates",
    labels: {
      D: "wiki_search_sections(original task.query, limit 5), rank-1 section, bounded read + continuation; no oracle filtering for selection; fixture answer literal controls stop condition",
      E: "known target article and anchor, bounded read + continuation; oracle lower bound",
    },
    stopCondition: "D and E stop when the fixture's literal answer is present in collected read text or the section ends; this is oracle-assisted stopping, not autonomous agent behavior",
    stopConditionOracleAssisted: true,
    byStrategy: Object.fromEntries(["D", "E"].map((strategy) => {
      const mine = records.filter((r) => r.strategy === strategy);
      return [strategy, {
        runs: mine.length, verified: mine.filter((r) => r.verified).length,
        answerInSearchSnippet: mine.filter((r) => r.answerInSearchSnippet).length,
        forcedReadAfterSnippetAnswer: mine.filter((r) => r.forcedReadAfterSnippetAnswer).length,
        top5ArticleAnchorHits: strategy === "D" ? mine.filter((r) => r.top5ArticleAnchorHit).length : null,
        top5Coverage: strategy === "D" ? { denominator: mine.length, hits: mine.filter((r) => r.top5ArticleAnchorHit).length, rate: mine.length ? mine.filter((r) => r.top5ArticleAnchorHit).length / mine.length : null } : null,
        failures: mine.filter((r) => !r.verified).length,
        protocolFailures: mine.filter((r) => r.protocolFailure).length,
      }];
    })),
    byTask,
    payloadStability: byTask.map(({ strategy, task, payloadStableAcrossRepetitions }) => ({ strategy, task, stable: payloadStableAcrossRepetitions })),
    perRun: records,
  };
}

// ---------------------------------------------------------------- write comparison

async function makeClone(docKey, meta) {
  const state = ctx.docs.get(docKey);
  const d = state.doc;
  const created = await rest("POST", "/api/articles", {
    space_id: state.spaceId, title: d.title, summary: d.summary, body: state.body, tags: d.tags, properties: d.properties,
    author: `${AUTHOR}-clone`, message: "bench clone",
  }, meta);
  return { id: created.id, revision: created.revision, body: created.body, metadata: articleMetadata(created) };
}

function articleMetadata(a) {
  return { space_id: a.space_id, parent_id: a.parent_id, title: a.title, summary: a.summary, kind: a.kind, status: a.status, pinned: a.pinned, tags: a.tags, properties: a.properties };
}

// Verification traffic runs in phase "verify": outside task metrics, labeled separately.
async function verifyArticle(clone, expectedBody, expectedRevision, scenario, strategy, repetition) {
  const { parsed } = await callTool("wiki_read", { id: clone.id }, newMeta("verify", scenario, strategy, repetition));
  const checks = {
    bodyExact: parsed.body === expectedBody,
    metadataUnchanged: canon(articleMetadata(parsed)) === canon(clone.metadata),
    revision: parsed.revision === expectedRevision,
  };
  for (const [name, ok] of Object.entries(checks)) if (!ok) fail(scenario, strategy, repetition, `verification ${name}`, `article ${clone.id}`);
  return checks;
}

async function runWriteTask(strategy, task, phase, repetition) {
  const scope = [];
  const scenario = `write:${task.key}`;
  const meta = newMeta(phase, scenario, strategy, repetition, scope);
  const clone = await makeClone(task.articleKey, newMeta("setup", scenario, strategy, repetition));
  const replacement = `BENCH-EDITED-${task.key}`;
  const expectedBody = replaceOnce(clone.body, task.answer, replacement);
  let result;
  if (strategy === "full_write") {
    const full = await callTool("wiki_read", { id: clone.id }, meta);
    if (full.parsed.body !== clone.body) fail(scenario, strategy, repetition, "full read body differs from seeded body", clone.id);
    const body = replaceOnce(full.parsed.body, task.answer, replacement);
    result = await callTool("wiki_write", { id: clone.id, article: { body, base_revision: full.parsed.revision, author: AUTHOR, message: "bench full write" } }, meta);
  } else {
    const outline = await callTool("wiki_read", { id: clone.id, outline: true }, meta);
    const h = pickHeading(outline.parsed.headings, task.headingTitle, task.headingOccurrence);
    const section = await callTool("wiki_read", { id: clone.id, anchor: h.anchor }, meta);
    if (!section.parsed.section.includes(task.answer)) fail(scenario, strategy, repetition, "answer literal not found in section", task.answer);
    result = await callTool("wiki_edit", { id: clone.id, base_revision: outline.parsed.revision, op: "replace_text", old: task.answer, text: replacement, author: AUTHOR, message: "bench section edit" }, meta);
  }
  if (result.parsed.revision !== clone.revision + 1) fail(scenario, strategy, repetition, "write returned unexpected revision", String(result.parsed.revision));
  const verification = await verifyArticle(clone, expectedBody, clone.revision + 1, scenario, strategy, repetition);
  return { task: task.key, strategy, rep: repetition, ...sumScope(scope), verification, verified: Object.values(verification).every(Boolean) };
}

// ---------------------------------------------------------------- correctness checks

async function guarded(results, name, task, repetition, body) {
  const entry = { check: name, task: task.key, rep: repetition, passed: false, details: {} };
  try {
    await body(entry.details);
    entry.passed = true;
  } catch (e) {
    if (!(e instanceof ExpectationError)) throw e;
    entry.details.error = e.message;
    fail(`correctness:${name}`, null, repetition, "expectation", e.message);
  }
  results.push(entry);
}

function ensure(cond, message) {
  if (!cond) throw new ExpectationError(message);
}

async function checkStaleRevision(results, task, rep) {
  await guarded(results, "staleRevisionRejected", task, rep, async (details) => {
    const scenario = `correctness:stale:${task.key}`;
    const clone = await makeClone(task.articleKey, newMeta("setup", scenario, null, rep));
    const meta = newMeta("correctness", scenario, null, rep);
    const first = await callTool("wiki_edit", { id: clone.id, base_revision: clone.revision, op: "replace_text", old: task.answer, text: `BENCH-FIRST-${task.key}`, author: AUTHOR }, meta);
    ensure(first.parsed.revision === clone.revision + 1, "first edit did not advance the revision");
    const expected = replaceOnce(clone.body, task.answer, `BENCH-FIRST-${task.key}`);
    const staleEdit = await callTool("wiki_edit", { id: clone.id, base_revision: clone.revision, op: "replace_text", old: `BENCH-FIRST-${task.key}`, text: "STALE", author: AUTHOR }, meta, /revision conflict/);
    const staleWrite = await callTool("wiki_write", { id: clone.id, article: { body: "STALE", base_revision: clone.revision, author: AUTHOR } }, meta, /revision conflict/);
    details.staleEditError = staleEdit.text;
    details.staleWriteError = staleWrite.text;
    const checks = await verifyArticle(clone, expected, clone.revision + 1, scenario, null, rep);
    ensure(checks.bodyExact && checks.metadataUnchanged && checks.revision, "stale attempts changed the article");
  });
}

async function checkConcurrentEdit(results, task, rep) {
  await guarded(results, "concurrentEditExactlyOneWinner", task, rep, async (details) => {
    const scenario = `correctness:concurrent:${task.key}`;
    const clone = await makeClone(task.articleKey, newMeta("setup", scenario, null, rep));
    const meta = newMeta("correctness", scenario, null, rep);
    const texts = [`BENCH-A-${task.key}`, `BENCH-B-${task.key}`];
    const calls = await Promise.all(texts.map((text) =>
      callTool("wiki_edit", { id: clone.id, base_revision: clone.revision, op: "replace_text", old: task.answer, text, author: AUTHOR }, meta, "either")));
    const winners = calls.map((c, i) => (c.isError ? -1 : i)).filter((i) => i >= 0);
    const losers = calls.filter((c) => c.isError);
    details.winners = winners.length;
    details.loserErrors = losers.map((c) => c.text);
    ensure(winners.length === 1, `expected exactly one winner, got ${winners.length}`);
    ensure(losers.length === 1 && /revision conflict/.test(losers[0].text), "the losing edit did not fail with a revision conflict");
    const checks = await verifyArticle(clone, replaceOnce(clone.body, task.answer, texts[winners[0]]), clone.revision + 1, scenario, null, rep);
    ensure(checks.bodyExact && checks.metadataUnchanged && checks.revision, "final article does not match the single winner");
  });
}

async function checkHistoricalAnchorAfterRename(results, task, rep) {
  await guarded(results, "historicalSectionAfterHeadingRename", task, rep, async (details) => {
    const scenario = `correctness:rename:${task.key}`;
    const clone = await makeClone(task.articleKey, newMeta("setup", scenario, null, rep));
    const meta = newMeta("correctness", scenario, null, rep);
    const outline = await callTool("wiki_read", { id: clone.id, outline: true }, meta);
    const h = pickHeading(outline.parsed.headings, task.headingTitle, task.headingOccurrence);
    const original = (await callTool("wiki_read", { id: clone.id, anchor: h.anchor }, meta)).parsed.section;
    const lines = original.split("\n");
    const atx = /^ {0,3}#{1,6}[ \t]/.test(lines[0]);
    const newTitle = `${h.title} RENAMED`;
    const renamed = [`${"#".repeat(h.level)} ${newTitle}`, ...lines.slice(atx ? 1 : 2)].join("\n");
    const edit = await callTool("wiki_edit", { id: clone.id, base_revision: outline.parsed.revision, op: "replace_section", anchor: h.anchor, text: renamed, author: AUTHOR }, meta);
    const historical = (await callTool("wiki_read", { id: clone.id, anchor: h.anchor, revision: outline.parsed.revision }, meta)).parsed;
    ensure(historical.historical === true && historical.revision === outline.parsed.revision, "historical read is not flagged as the old revision");
    ensure(historical.section === original, "historical section differs from the section read before the rename");
    // After the rename the old anchor either fails or (duplicate headings shift slugs) resolves to another section.
    const current = await callTool("wiki_read", { id: clone.id, anchor: h.anchor }, meta, "either");
    details.oldAnchor = h.anchor;
    details.oldAnchorOnCurrentRevision = current.isError ? { outcome: "error", text: current.text } : { outcome: "resolves", sameAsRenamedSection: current.parsed.section === renamed, sameAsOriginal: current.parsed.section === original };
    ensure(current.isError ? /heading not found/.test(current.text) : current.parsed.section !== original, "old anchor on the current revision unexpectedly returned the original section");
    const after = await callTool("wiki_read", { id: clone.id, outline: true }, meta);
    ensure(after.parsed.revision === edit.parsed.revision && after.parsed.headings.some((x) => x.title === newTitle), "renamed heading is missing from the current outline");
  });
}

async function checkLongSectionReconstruction(results, task, info, rep) {
  await guarded(results, "boundedLongSectionExactReconstruction", task, rep, async (details) => {
    const state = ctx.docs.get(task.articleKey);
    const meta = newMeta("correctness", `correctness:reconstruct:${task.key}`, "bounded-read", rep);
    const full = await callTool("wiki_read", { id: state.id, anchor: info.anchor }, meta);
    ensure(typeof full.parsed.section === "string", "full section read omitted section text");
    let args = { id: state.id, anchor: info.anchor, revision: full.parsed.revision, max_chars: BOUNDED_READ_CHARS };
    let actual = "";
    let pages = 0;
    for (; pages < MAX_BOUNDED_PAGES; pages++) {
      const page = await callTool("wiki_read", args, meta);
      ensure(page.parsed.revision === full.parsed.revision, "bounded page changed revision during reconstruction");
      actual += page.parsed.text;
      if (!page.parsed.has_more) { pages++; break; }
      ensure(page.parsed.next_read, "bounded page omitted next_read");
      args = page.parsed.next_read;
    }
    ensure(pages < MAX_BOUNDED_PAGES, "page cap reached before section reconstruction completed");
    ensure(actual === full.parsed.section, "concatenated bounded pages differ from full section");
    details.revision = full.parsed.revision;
    details.pages = pages;
    details.sectionChars = full.parsed.section.length;
    details.exact = true;
  });
}

async function checkHistoricalBoundedContinuation(results, task, rep) {
  await guarded(results, "boundedContinuationUsesPinnedRevisionAfterEdit", task, rep, async (details) => {
    const scenario = `correctness:bounded-history:${task.key}`;
    const clone = await makeClone(task.articleKey, newMeta("setup", scenario, null, rep));
    const meta = newMeta("correctness", scenario, null, rep);
    const before = await callTool("wiki_read", { id: clone.id }, meta);
    const outline = await callTool("wiki_read", { id: clone.id, outline: true }, meta);
    const h = pickHeading(outline.parsed.headings, task.headingTitle, task.headingOccurrence);
    const section = await callTool("wiki_read", { id: clone.id, anchor: h.anchor }, meta);
    ensure(section.parsed.section.length > BOUNDED_READ_CHARS, "fixture section is too short to exercise continuation");
    const first = await callTool("wiki_read", { id: clone.id, anchor: h.anchor, revision: before.parsed.revision, max_chars: BOUNDED_READ_CHARS }, meta);
    ensure(first.parsed.has_more && first.parsed.next_read, "first bounded read did not offer a continuation");
    const expectedRevision = before.parsed.revision;
    const expectedSection = section.parsed.section;
    const edit = await callTool("wiki_write", { id: clone.id, article: { body: `${before.parsed.body}\n\nREVISION-CONTINUATION-CHECK`, base_revision: expectedRevision, author: AUTHOR, message: "bounded continuation correctness check" } }, meta);
    ensure(edit.parsed.revision === expectedRevision + 1, "edit did not advance revision exactly once");
    let args = first.parsed.next_read;
    let actual = first.parsed.text;
    let pages = 1;
    while (pages < MAX_BOUNDED_PAGES) {
      const next = await callTool("wiki_read", args, meta);
      ensure(next.parsed.revision === expectedRevision, "continuation silently switched to the edited revision");
      actual += next.parsed.text;
      pages++;
      if (!next.parsed.has_more) break;
      ensure(next.parsed.next_read, "continuation omitted next_read");
      args = next.parsed.next_read;
    }
    ensure(pages < MAX_BOUNDED_PAGES, "page cap reached during historical continuation");
    ensure(actual === expectedSection, "historical continuation did not reconstruct the original section");
    details.originalRevision = expectedRevision;
    details.editedRevision = edit.parsed.revision;
    details.pages = pages;
    details.exactOriginalSnapshot = true;
  });
}

// ---------------------------------------------------------------- summaries

function groupStats(records, overhead) {
  const metrics = {};
  const withSetup = {};
  for (const m of METRICS) {
    metrics[m] = stats(records.map((r) => r[m]));
    withSetup[m] = stats(records.map((r) => r[m] + overhead[m]));
  }
  return { runs: records.length, metrics, singleTaskWithSetupOnce: withSetup };
}

function sessionView(records, repetitions, overhead) {
  const perRep = [];
  for (let rep = 1; rep <= repetitions; rep++) {
    const mine = records.filter((r) => r.rep === rep);
    const total = {};
    for (const m of METRICS) total[m] = mine.reduce((a, r) => a + r[m], 0) + overhead[m];
    perRep.push({ tasks: mine.length, total });
  }
  const metrics = {};
  for (const m of METRICS) metrics[m] = stats(perRep.map((p) => p.total[m]));
  return { note: "each repetition = every task once + shared setup overhead added once", sessions: repetitions, tasksPerSession: perRep[0]?.tasks ?? 0, metrics };
}

function ratioBlock(pairs, overhead) {
  const out = {};
  for (const m of RATIO_METRICS) {
    const plain = [];
    const withSetup = [];
    let skipped = 0;
    for (const [base, other] of pairs) {
      if (base[m] === 0) { skipped++; continue; }
      plain.push(other[m] / base[m]);
      withSetup.push((other[m] + overhead[m]) / (base[m] + overhead[m]));
    }
    const ratioStats = (values) => {
      const { mean, ...summary } = stats(values);
      return { ...summary, primary: "median" };
    };
    out[m] = { withoutSetup: ratioStats(plain), withSetupOnce: ratioStats(withSetup), skippedZeroBaseline: skipped };
  }
  return out;
}

function pairedRatios(records, baseline, other, tasks, overhead) {
  const key = (r) => `${r.task}|${r.rep}`;
  const base = new Map(records.filter((r) => r.strategy === baseline).map((r) => [key(r), r]));
  const pairs = records.filter((r) => r.strategy === other && base.has(key(r))).map((r) => ({ task: r.task, pair: [base.get(key(r)), r] }));
  return {
    definition: `${other} / ${baseline} per task and repetition (ratio < 1 means ${other} is cheaper)`,
    pairs: pairs.length,
    byTask: tasks.map((t) => ({ task: t, ...ratioBlock(pairs.filter((p) => p.task === t).map((p) => p.pair), overhead) })),
  };
}

function summarizeStrategies(records, strategies, taskInfo, taskKeys, repetitions, overhead, comparisons) {
  const byStrategy = {};
  for (const s of strategies) {
    const mine = records.filter((r) => r.strategy === s);
    byStrategy[s] = { ...groupStats(mine, overhead), session: sessionView(mine, repetitions, overhead), verified: mine.filter((r) => r.verified).length, failedVerification: mine.filter((r) => !r.verified).length };
  }
  const byTask = taskKeys
    .map((t) => ({ task: t, document: taskInfo.get(t), strategies: Object.fromEntries(strategies.map((s) => [s, groupStats(records.filter((r) => r.task === t && r.strategy === s), overhead)])) }))
    .sort((a, b) => a.document.bodyBytes - b.document.bodyBytes);
  const paired = Object.fromEntries(comparisons.map(([base, other]) => [`${other}_vs_${base}`, pairedRatios(records, base, other, taskKeys, overhead)]));
  return { byStrategy, byTask, pairedRatios: paired };
}

function observedHits(records, strategy) {
  const mine = records.filter((r) => r.strategy === strategy);
  return {
    runs: mine.length,
    oracleConditional: { answerVerified: mine.filter((r) => r.verified).length, note: "the oracle chooses article and heading regardless of search" },
    observed: {
      targetInSearchPage: mine.filter((r) => r.targetInSearchPage).length,
      answerInSearchSnippet: mine.filter((r) => r.answerInSearchSnippet).length,
      targetInSearchPageAndAnswerVerified: mine.filter((r) => r.targetInSearchPage && r.verified).length,
      targetRank: stats(mine.map((r) => r.targetRank)),
    },
  };
}

// ---------------------------------------------------------------- main

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const startedAt = new Date().toISOString();
  const runId = `${startedAt.replace(/[-:.TZ]/g, "").slice(0, 14)}-${randomBytes(3).toString("hex")}`;
  const { corpus, hashes } = await loadCorpus();
  ctx.corpusVersion = corpus.version;
  const tokenizer = await loadTokenizer();
  ctx.countTokens = tokenizer.count;
  ctx.count = {};

  fs.mkdirSync(CACHE, { recursive: true });
  const runDir = path.join(CACHE, `mcp-bench-${runId}`);
  fs.mkdirSync(runDir);
  const dataDir = path.join(runDir, "data");
  const binPath = path.join(runDir, process.platform === "win32" ? "aiwiki-bench.exe" : "aiwiki-bench");
  const out = opts.out ? resolveOut(opts.out) : { summary: path.join(runDir, "summary.json"), traces: path.join(runDir, "traces.jsonl") };
  ctx.tracesFd = fs.openSync(out.traces, "wx");

  const summary = {
    schema: "aiwiki-mcp-bench-summary/1",
    status: "running",
    runId,
    startedAt,
    options: { repetitions: opts.repetitions, warmupPasses: 1, searchPageSize: SEARCH_LIMIT, strategiesOrder: "alternates each repetition (odd: forward, even: reversed)" },
    definitions: DEFINITIONS,
    limitations: LIMITATIONS,
    files: { summary: out.summary, traces: out.traces, runDir, dataDir, serverLog: path.join(runDir, "server.log") },
  };
  const onSignal = () => { killChildSync(); process.exit(130); };
  process.once("SIGINT", onSignal);
  process.once("SIGTERM", onSignal);
  process.once("exit", killChildSync);

  try {
    const git = await gitInfo();
    log(`building server (run ${runId})`);
    const build = await buildServer(binPath);
    const port = await reservePort();
    ctx.base = `http://127.0.0.1:${port}`;
    const tHealth = performance.now();
    startChild(binPath, dataDir, port, runDir);
    await waitHealthy(ctx.base);
    const healthMs = round3(performance.now() - tHealth);
    log(`server healthy on ${ctx.base}`);

    summary.environment = {
      source: git,
      build: { goVersion: build.goVersion, binarySha256: build.binarySha256, binary: binPath },
      corpus: { version: corpus.version, ...hashes },
      files: {
        runnerSha256: sha256(fs.readFileSync(fileURLToPath(import.meta.url))),
        methodologySha256: sha256(fs.readFileSync(path.join(BENCH_DIR, "METHODOLOGY.md"))),
        candidateMethodologySha256: sha256(fs.readFileSync(path.join(BENCH_DIR, "CANDIDATE-METHODOLOGY.md"))),
      },
      platform: { os: process.platform, arch: process.arch, release: os.release(), cpu: os.cpus()[0]?.model ?? null, cpus: os.cpus().length, memoryBytes: os.totalmem() },
      node: process.version,
      tokenizer: tokenizer.info,
      transport: { base: ctx.base, protocolVersion: PROTOCOL_VERSION, http: "node fetch (undici) keep-alive, loopback" },
    };

    const seeded = await seed(corpus);
    const probe = await callTool("wiki_search", { q: "OFR-7319", limit: SEARCH_LIMIT }, newMeta("setup", "search-probe"));
    const probeDoc = ctx.docs.get(corpus.readTasks.find((task) => task.answer === "OFR-7319")?.articleKey);
    if (!probeDoc || !probe.parsed.results.some((hit) => hit.id === probeDoc.id)) {
      throw new Error("setup search probe did not return the unique target OFR-7319");
    }
    const taskInfo = await resolveReadContexts(corpus);
    const idToKey = new Map([...ctx.docs].map(([key, s]) => [s.id, key]));
    const setupOverhead = await measureSetupOverhead(ctx.spaces.get(corpus.spaces[0].key));
    const overhead = setupOverhead.overhead;
    summary.setup = { buildMs: build.buildMs, healthMs, seedMs: seeded.seedMs, seeded, setupOverhead };
    summary.corpus = {
      version: corpus.version, spaces: corpus.spaces.length, documents: corpus.documents.length, searchTasks: corpus.searchTasks.length, readTasks: corpus.readTasks.length,
      documentBodyBytes: corpus.documents.map((d) => ({ key: d.key, bytes: utf8Bytes(ctx.docs.get(d.key).body) })),
    };
    const reps = Array.from({ length: opts.repetitions }, (_, i) => i + 1);

    // ---- search quality and latency
    log("search");
    await runSearchPass(corpus, "warmup", 0, idToKey);
    const searchSamples = [];
    for (const rep of reps) searchSamples.push(...(await runSearchPass(corpus, "measure", rep, idToKey)));
    summary.search = summarizeSearch(searchSamples, corpus);

    // ---- read: A search+full, B search+outline+section, C search+known-anchor section
    log("read");
    const orderFor = (rep, list) => (rep % 2 === 1 ? list : [...list].reverse());
    const readStrategies = ["A", "B", "C"];
    for (const task of corpus.readTasks) for (const s of orderFor(1, readStrategies)) await runReadTask(s, task, taskInfo.get(task.key), "warmup", 0);
    const readRecords = [];
    for (const rep of reps) {
      for (const task of corpus.readTasks) {
        for (const s of orderFor(rep, readStrategies)) readRecords.push(await runReadTask(s, task, taskInfo.get(task.key), "measure", rep));
      }
    }
    const readKeys = corpus.readTasks.map((t) => t.key);
    const readPayloadStability = readKeys.flatMap((task) => readStrategies.map((strategy) => {
      const hashes = new Set(readRecords.filter((r) => r.task === task && r.strategy === strategy).map((r) => r.payloadSha256));
      if (hashes.size !== 1) fail(`read:${task}`, strategy, null, "read payload drift", `${hashes.size} different normalized payloads`);
      return { task, strategy, stable: hashes.size === 1 };
    }));
    summary.read = {
      payloadStability: readPayloadStability,
      labels: { A: "search + full read", B: "search + outline + anchored section (oracle heading)", C: "search + direct anchored section (known-anchor lower bound)" },
      ...summarizeStrategies(readRecords, readStrategies, taskInfo, readKeys, opts.repetitions, overhead, [["A", "B"], ["A", "C"]]),
      searchHits: Object.fromEntries(readStrategies.map((s) => [s, observedHits(readRecords, s)])),
    };

    // ---- scripted candidates are separate from frozen baseline A/B/C/write
    log("candidate reads D/E");
    const candidateStrategies = ["D", "E"];
    for (const task of corpus.readTasks) for (const s of orderFor(1, candidateStrategies)) await runCandidateTask(s, task, taskInfo.get(task.key), "warmup", 0);
    const candidateRecords = [];
    for (const rep of reps) {
      for (const task of corpus.readTasks) {
        for (const s of orderFor(rep, candidateStrategies)) candidateRecords.push(await runCandidateTask(s, task, taskInfo.get(task.key), "measure", rep));
      }
    }
    summary.candidates = summarizeCandidates(candidateRecords, corpus.readTasks.map((t) => t.key), opts.repetitions);

    // ---- write: runs after reads/search because each run adds clone articles to the wiki
    log("write");
    const eligible = corpus.readTasks.filter((t) => taskInfo.get(t.key).answerOccurrencesInBody === 1);
    const skipped = corpus.readTasks.filter((t) => !eligible.includes(t)).map((t) => ({ task: t.key, reason: `answer occurs ${taskInfo.get(t.key).answerOccurrencesInBody} times in the body, replace_text needs exactly one` }));
    if (eligible.length === 0) throw new Error("no read task has an answer that occurs exactly once in its body; cannot run write comparison");
    const writeStrategies = ["full_write", "section_edit"];
    for (const task of eligible) for (const s of orderFor(1, writeStrategies)) await runWriteTask(s, task, "warmup", 0);
    const writeRecords = [];
    for (const rep of reps) {
      for (const task of eligible) {
        for (const s of orderFor(rep, writeStrategies)) writeRecords.push(await runWriteTask(s, task, "measure", rep));
      }
    }
    summary.write = {
      labels: { full_write: "full read + wiki_write(body, base_revision)", section_edit: "outline + section read + wiki_edit(replace_text, base_revision)" },
      skippedTasks: skipped,
      verificationTraffic: "full read after the edit, phase verify, excluded from task metrics",
      ...summarizeStrategies(writeRecords, writeStrategies, taskInfo, eligible.map((t) => t.key), opts.repetitions, overhead, [["full_write", "section_edit"]]),
    };
    summary.write.byTask = summary.write.byTask.map((t) => ({ ...t, verified: writeRecords.filter((r) => r.task === t.task).every((r) => r.verified) }));

    // ---- correctness checks (not throughput claims)
    log("correctness");
    const correctness = [];
    const largest = [...eligible].sort((a, b) => taskInfo.get(b.key).bodyBytes - taskInfo.get(a.key).bodyBytes)[0];
    const staleTasks = [...new Set([eligible[0], largest])];
    const renameTasks = [...new Set([corpus.readTasks.find((t) => t.headingOccurrence === 1), corpus.readTasks.find((t) => t.headingOccurrence > 1)].filter(Boolean))];
    for (const rep of reps) {
      for (const task of staleTasks) {
        await checkStaleRevision(correctness, task, rep);
        await checkConcurrentEdit(correctness, task, rep);
      }
      for (const task of renameTasks) await checkHistoricalAnchorAfterRename(correctness, task, rep);
    }
    const longSectionTask = corpus.readTasks.find((t) => t.key === "r06");
    if (longSectionTask) await checkLongSectionReconstruction(correctness, longSectionTask, taskInfo.get(longSectionTask.key), 1);
    const historicalTask = corpus.readTasks.find((t) => t.key === "r04");
    if (historicalTask) await checkHistoricalBoundedContinuation(correctness, historicalTask, 1);
    const byCheck = {};
    for (const c of correctness) {
      byCheck[c.check] ??= { runs: 0, passed: 0, failed: 0 };
      byCheck[c.check].runs++;
      byCheck[c.check][c.passed ? "passed" : "failed"]++;
    }
    summary.correctness = { byCheck, runs: correctness };

    summary.status = ctx.failures.length === 0 ? "ok" : "completed_with_failures";
  } catch (e) {
    summary.status = "failed";
    summary.error = String(e && e.stack ? e.stack : e);
    log(`FATAL ${summary.error}`);
  } finally {
    await stopChild();
    summary.child = { exited: child.exited, exitInfo: child.exitInfo };
    summary.finishedAt = new Date().toISOString();
    summary.failures = ctx.failures;
    summary.requestCountsByPhase = ctx.count;
    fs.closeSync(ctx.tracesFd);
    fs.writeFileSync(out.summary, JSON.stringify(summary, null, 2) + "\n");
  }

  const pick = (s) => ({ runs: s.runs, medianTokens: s.metrics.totalTokens.median, p95Tokens: s.metrics.totalTokens.p95, medianMs: s.metrics.durationMs.median, p95Ms: s.metrics.durationMs.p95, medianRequests: s.metrics.requests.median });
  const concise = {
    status: summary.status,
    runId,
    files: summary.files,
    environment: summary.environment && { commit: summary.environment.source.commit, dirty: summary.environment.source.dirty, binarySha256: summary.environment.build.binarySha256, corpus: summary.environment.corpus, tokenizer: summary.environment.tokenizer, node: summary.environment.node, go: summary.environment.build.goVersion, platform: summary.environment.platform.os },
    setupOverhead: summary.setup && summary.setup.setupOverhead.overhead,
    search: summary.search && { recallAt5: summary.search.overall.withGroundTruth.recallAt5, recallAt10: summary.search.overall.withGroundTruth.recallAt10, mrrAt10: summary.search.overall.withGroundTruth.mrrAt10, noHitAccuracy: summary.search.overall.noHit.accuracy, modes: summary.search.overall.modeCounts, medianMs: summary.search.overall.durationMs.median, p95Ms: summary.search.overall.durationMs.p95 },
    read: summary.read && Object.fromEntries(Object.entries(summary.read.byStrategy).map(([k, v]) => [k, { ...pick(v), failedVerification: v.failedVerification }])),
    candidates: summary.candidates && summary.candidates.byStrategy,
    write: summary.write && Object.fromEntries(Object.entries(summary.write.byStrategy).map(([k, v]) => [k, { ...pick(v), failedVerification: v.failedVerification }])),
    correctness: summary.correctness && summary.correctness.byCheck,
    failures: ctx.failures.length,
    error: summary.error ? summary.error.split("\n")[0] : undefined,
  };
  process.stdout.write(JSON.stringify(concise, null, 2) + "\n");
  process.exitCode = summary.status === "ok" ? 0 : 1;
}

main().catch((e) => {
  killChildSync();
  process.stderr.write(`[bench] ${e && e.message ? e.message : e}\n`);
  process.exitCode = 1;
});
