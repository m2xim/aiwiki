// Compares lexical (wiki_search_sections), vector and hybrid section retrieval
// on the frozen corpus. Usage:
//   node benchmarks/mcp/semantic-eval.mjs --bin PATH [--embed-url URL] [--model NAME] [--out FILE]
// The embedding endpoint is OpenAI-compatible (/v1/embeddings), local only.
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { spawn } from "node:child_process";
import { makeCorpus } from "./corpus.mjs";
import { makeSectionQueries } from "./section-queries-v2.mjs";
import { makeSemanticQueries } from "./semantic-queries-v3.mjs";
import { makeSemanticQueriesV4 } from "./semantic-queries-v4.mjs";

const V3_SHA256 = "e55dedef632988fa1dcd43da9dec999f948f1167695b3757ed16e00c59145459";
const V4_SHA256 = "a893a0ce3e72881536cc309ac68bcb2fb0b5328c56f267523ebcc6f3aced3787";
const LEX_LIMIT = 20;
const FUSE_DEPTH = 20;
const RRF_K = 60;
const CHUNK_CHARS = 1200;

const args = Object.fromEntries(process.argv.slice(2).reduce((acc, a, i, all) => {
  // An explicit empty value (--doc-prefix "") is a value, not a missing one.
  if (a.startsWith("--")) acc.push([a.slice(2), all[i + 1] !== undefined && !all[i + 1].startsWith("--") ? all[i + 1] : "1"]);
  return acc;
}, []));
const BIN = args.bin;
const EMBED_URL = args["embed-url"] ?? "http://127.0.0.1:1234/v1/embeddings";
const MODEL = args.model ?? "text-embedding-nomic-embed-text-v1.5";
const QUERY_PREFIX = args["query-prefix"] ?? (MODEL.includes("nomic") ? "search_query: " : MODEL.includes("e5") ? "query: " : "");
const DOC_PREFIX = args["doc-prefix"] ?? (MODEL.includes("nomic") ? "search_document: " : MODEL.includes("e5") ? "passage: " : "");
if (!BIN) throw new Error("--bin PATH is required");
for (const u of [EMBED_URL]) if (!["127.0.0.1", "localhost", "[::1]"].includes(new URL(u).hostname)) throw new Error("embedding endpoint must be local: closed texts are not sent to external providers");

const sha256 = (s) => crypto.createHash("sha256").update(s).digest("hex");
const ensure = (c, m) => { if (!c) throw new Error(m); };

async function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer(); s.once("error", reject);
    s.listen({ host: "127.0.0.1", port: 0 }, () => { const { port } = s.address(); s.close(() => resolve(port)); });
  });
}

let rpc = 0;
async function call(base, name, a) {
  const res = await fetch(`${base}/mcp`, { method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: ++rpc, method: "tools/call", params: { name, arguments: a } }) });
  const env = await res.json();
  ensure(res.ok && !env.error && env.result && !env.result.isError, `${name} failed: ${JSON.stringify(env).slice(0, 300)}`);
  return JSON.parse(env.result.content.map((x) => x.text).join(""));
}
async function rest(base, method, route, body) {
  const res = await fetch(`${base}${route}`, { method, headers: { "content-type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  ensure(res.ok, `${method} ${route}: ${res.status} ${text.slice(0, 200)}`);
  return JSON.parse(text);
}

async function seed(base, corpus) {
  const spaces = new Map(), docs = new Map();
  for (const s of corpus.spaces) spaces.set(s.key, (await rest(base, "POST", "/api/spaces", { name: s.name })).id);
  for (const d of corpus.documents) {
    const a = await rest(base, "POST", "/api/articles", { space_id: spaces.get(d.spaceKey), title: d.title, summary: d.summary, body: d.body, tags: d.tags, properties: d.properties, author: "semantic-eval", message: "seed" });
    docs.set(d.key, { id: a.id, revision: a.revision, body: a.body, doc: d });
  }
  for (const [key, st] of docs) {
    const linked = st.body.replace(/\[\[link:([^\]\s]+)\]\]/g, (_, k) => `[${docs.get(k).doc.title}](/?article=${docs.get(k).id})`);
    if (linked === st.body) continue;
    const u = await rest(base, "PUT", `/api/articles/${st.id}`, { body: linked, base_revision: st.revision, author: "semantic-eval", message: "links" });
    st.revision = u.revision; st.body = u.body;
  }
  return docs;
}

// Section units exactly as the server addresses them: outline anchors and the
// section text returned by wiki_read with that anchor.
async function sectionUnits(base) {
  const exported = await rest(base, "GET", "/api/export");
  const units = [];
  for (const a of exported.articles) {
    const outline = await call(base, "wiki_read", { id: a.id, outline: true });
    for (const h of outline.headings ?? []) {
      if (h.level === 1 && outline.headings.length > 1) continue; // a document title heading wraps everything
      const sec = await call(base, "wiki_read", { id: a.id, anchor: h.anchor });
      units.push({ id: a.id, title: a.title, heading: h.title, anchor: h.anchor, text: sec.section ?? "" });
    }
  }
  return units;
}

function chunks(text) {
  const parts = text.split(/\n{2,}/);
  const out = []; let cur = "";
  for (const p of parts) {
    if ((cur + "\n\n" + p).length > CHUNK_CHARS && cur) { out.push(cur); cur = ""; }
    cur = cur ? `${cur}\n\n${p}` : p;
    while (cur.length > CHUNK_CHARS) { out.push(cur.slice(0, CHUNK_CHARS)); cur = cur.slice(CHUNK_CHARS); }
  }
  if (cur.trim()) out.push(cur);
  return out;
}

let embedMs = 0, embedCalls = 0;
async function embed(texts) {
  const out = [];
  for (let i = 0; i < texts.length; i += 32) {
    const t0 = performance.now();
    const res = await fetch(EMBED_URL, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ model: MODEL, input: texts.slice(i, i + 32) }) });
    const j = await res.json();
    ensure(res.ok && Array.isArray(j.data), `embedding failed: ${JSON.stringify(j).slice(0, 200)}`);
    embedMs += performance.now() - t0; embedCalls++;
    for (const d of j.data.sort((x, y) => x.index - y.index)) out.push(normalize(d.embedding));
  }
  return out;
}
function normalize(v) { const n = Math.hypot(...v); return v.map((x) => x / n); }
const dot = (a, b) => { let s = 0; for (let i = 0; i < a.length; i++) s += a[i] * b[i]; return s; };

function rankMetrics(list, golds) {
  const ranks = [];
  list.slice(0, 5).forEach((u, i) => { if (golds.some((g) => g.id === u.id && g.anchor === u.anchor)) ranks.push(i + 1); });
  const found = new Set(list.slice(0, 5).filter((u) => golds.some((g) => g.id === u.id && g.anchor === u.anchor)).map((u) => `${u.id}#${u.anchor}`));
  return { top1: ranks[0] === 1, top5: ranks.length > 0, rr: ranks.length ? 1 / ranks[0] : 0, recall: found.size / golds.length, firstRank: ranks[0] ?? null };
}

async function main() {
  const corpus = makeCorpus();
  const v2 = makeSectionQueries();
  const v3 = makeSemanticQueries();
  ensure(sha256(JSON.stringify(v3)) === V3_SHA256, "semantic-queries-v3 changed after freezing");
  const v4 = makeSemanticQueriesV4();
  ensure(sha256(JSON.stringify(v4)) === V4_SHA256, "semantic-queries-v4 changed after freezing");
  const old7 = { version: "old7", queries: corpus.readTasks.map((t) => ({ key: t.key, category: "old7", query: t.query, relevantSections: [{ articleKey: t.articleKey, headingTitle: t.headingTitle, headingOccurrence: t.headingOccurrence }] })) };

  const runDir = fs.mkdtempSync(path.join(path.resolve(".cache"), "semantic-eval-"));
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  // --server-semantic: the server itself runs hybrid search with the same model.
  const serverSemantic = Boolean(args["server-semantic"]);
  const env = { ...process.env };
  if (serverSemantic) Object.assign(env, { AIWIKI_EMBEDDINGS_URL: EMBED_URL, AIWIKI_EMBEDDINGS_MODEL: MODEL, AIWIKI_EMBEDDINGS_QUERY_PREFIX: QUERY_PREFIX, AIWIKI_EMBEDDINGS_DOC_PREFIX: DOC_PREFIX });
  const proc = spawn(path.resolve(BIN), ["-addr", `127.0.0.1:${port}`, "-data", path.join(runDir, "data"), "-web="], { stdio: "ignore", windowsHide: true, env });
  try {
    for (let i = 0; ; i++) { try { if ((await fetch(`${base}/api/health`)).ok) break; } catch {} ensure(i < 100, "server did not start"); await new Promise((r) => setTimeout(r, 100)); }
    const docs = await seed(base, corpus);
    const units = await sectionUnits(base);
    let serverIndexMs = null;
    if (serverSemantic) {
      const t = performance.now();
      for (let i = 0; ; i++) {
        const r = await call(base, "wiki_search_sections", { q: "warm up", limit: 1 });
        if (r.semantic === "on") break;
        ensure(r.semantic === "indexing" && i < 600, `server semantic status ${r.semantic}`);
        await new Promise((res) => setTimeout(res, 500));
      }
      serverIndexMs = Math.round(performance.now() - t);
    }

    // Resolve gold sections to anchors through the same outline.
    const anchorOf = new Map();
    for (const u of units) { const k = `${u.id}|${u.heading}`; anchorOf.set(k, [...(anchorOf.get(k) ?? []), u.anchor]); }
    const goldsOf = (q) => q.relevantSections.map((g) => {
      const id = docs.get(g.articleKey).id; const a = anchorOf.get(`${id}|${g.headingTitle}`)?.[g.headingOccurrence - 1];
      ensure(a, `${q.key}: gold ${g.articleKey}/${g.headingTitle} not found`); return { id, anchor: a };
    });

    // Embed every chunk of every section once.
    const chunkRows = [];
    units.forEach((u, ui) => chunks(u.text).forEach((c) => chunkRows.push({ ui, text: `${DOC_PREFIX}${u.title} > ${u.heading}\n${c}` })));
    const t0 = performance.now();
    const vecs = await embed(chunkRows.map((c) => c.text));
    const indexMs = performance.now() - t0;
    chunkRows.forEach((c, i) => { c.v = vecs[i]; });

    const sets = [{ name: "dev-v2", set: v2 }, { name: "dev-old7", set: old7 }, { name: "heldout-v3", set: v3 }, { name: "heldout-v4", set: v4 }];
    const rows = [];
    for (const { name, set } of sets) {
      for (const q of set.queries) {
        const lexRes = await call(base, "wiki_search_sections", { q: q.query, limit: LEX_LIMIT, semantic: false });
        const lex = lexRes.results.map((h) => ({ id: h.id, anchor: h.anchor }));
        const lexMode = lexRes.mode;
        const tq = performance.now();
        const [qv] = await embed([`${QUERY_PREFIX}${q.query}`]);
        const queryEmbedMs = performance.now() - tq;
        const best = new Map();
        for (const c of chunkRows) { const s = dot(qv, c.v); if (!best.has(c.ui) || s > best.get(c.ui)) best.set(c.ui, s); }
        const vec = [...best].sort((a, b) => b[1] - a[1]).map(([ui, score]) => ({ id: units[ui].id, anchor: units[ui].anchor, score }));
        const srv = serverSemantic ? (await call(base, "wiki_search_sections", { q: q.query, limit: LEX_LIMIT })) : null;
        if (srv) ensure(srv.semantic === "on", `${q.key}: server semantic ${srv.semantic}`);
        const serverHybrid = srv ? srv.results.map((h) => ({ id: h.id, anchor: h.anchor })) : [];
        rows.push({ set: name, key: q.key, category: q.category, query: q.query, lexMode, serverHybrid, golds: q.relevantSections.length ? goldsOf(q) : [], lex, vec, queryEmbedMs });
      }
    }

    // Threshold for "nothing relevant" on the vector side, chosen on the dev sets only.
    const dev = rows.filter((r) => r.set.startsWith("dev-"));
    const grid = [...new Set(dev.flatMap((r) => r.vec.slice(0, 5).map((v) => Math.round(v.score * 1000) / 1000)))].sort((a, b) => a - b);
    let tau = 0, bestScore = -1;
    for (const t of [0, ...grid]) {
      let s = 0;
      for (const r of dev) {
        const kept = r.vec.filter((v) => v.score >= t);
        if (r.golds.length === 0) s += kept.length === 0 ? 1 : 0;
        else s += rankMetrics(kept, r.golds).top5 ? 1 : 0;
      }
      if (s > bestScore) { bestScore = s; tau = t; }
    }

    const fuse = (lex, vec, lexWeight = 1) => {
      const score = new Map(), item = new Map();
      lex.slice(0, FUSE_DEPTH).forEach((u, i) => { const k = `${u.id}#${u.anchor}`; item.set(k, u); score.set(k, (score.get(k) ?? 0) + lexWeight / (RRF_K + i + 1)); });
      vec.filter((v) => v.score >= tau).slice(0, FUSE_DEPTH).forEach((u, i) => { const k = `${u.id}#${u.anchor}`; item.set(k, u); score.set(k, (score.get(k) ?? 0) + 1 / (RRF_K + i + 1)); });
      return [...score].sort((a, b) => b[1] - a[1]).map(([k]) => item.get(k));
    };

    const variants = {
      lexical: (r) => r.lex,
      vector: (r) => r.vec.filter((v) => v.score >= tau),
      hybrid: (r) => fuse(r.lex, r.vec),
      // Partial-fallback lexical hits (not every query term matched) count half.
      hybridModeWeighted: (r) => fuse(r.lex, r.vec, r.lexMode === "all" ? 1 : 0.5),
      // Only an all-terms lexical match takes part in the fusion.
      hybridAllOnly: (r) => fuse(r.lexMode === "all" ? r.lex : [], r.vec),
      ...(serverSemantic ? { serverHybrid: (r) => r.serverHybrid } : {}),
    };
    const summary = {};
    for (const set of ["dev-v2", "dev-old7", "heldout-v3", "heldout-v4"]) {
      summary[set] = {};
      for (const [vn, f] of Object.entries(variants)) {
        const rs = rows.filter((r) => r.set === set);
        const pos = rs.filter((r) => r.golds.length), neg = rs.filter((r) => !r.golds.length);
        const m = pos.map((r) => rankMetrics(f(r), r.golds));
        summary[set][vn] = {
          positives: pos.length, top1: m.filter((x) => x.top1).length, top5: m.filter((x) => x.top5).length,
          mrr5: +(m.reduce((s, x) => s + x.rr, 0) / (pos.length || 1)).toFixed(4),
          recall5: +(m.reduce((s, x) => s + x.recall, 0) / (pos.length || 1)).toFixed(4),
          negatives: neg.length, trueNoHit: neg.filter((r) => f(r).length === 0).length,
        };
      }
    }
    const perQuery = rows.map((r) => ({ set: r.set, key: r.key, category: r.category,
      lexical: r.golds.length ? rankMetrics(variants.lexical(r), r.golds).firstRank : variants.lexical(r).length,
      vector: r.golds.length ? rankMetrics(variants.vector(r), r.golds).firstRank : variants.vector(r).length,
      hybrid: r.golds.length ? rankMetrics(variants.hybrid(r), r.golds).firstRank : variants.hybrid(r).length,
      hybridModeWeighted: r.golds.length ? rankMetrics(variants.hybridModeWeighted(r), r.golds).firstRank : variants.hybridModeWeighted(r).length,
      hybridAllOnly: r.golds.length ? rankMetrics(variants.hybridAllOnly(r), r.golds).firstRank : variants.hybridAllOnly(r).length,
      lexMode: r.lexMode,
      ...(serverSemantic ? { serverHybrid: r.golds.length ? rankMetrics(variants.serverHybrid(r), r.golds).firstRank : variants.serverHybrid(r).length } : {}),
      topVectorScore: +(r.vec[0]?.score ?? 0).toFixed(3) }));
    const qms = rows.map((r) => r.queryEmbedMs).sort((a, b) => a - b);
    const report = {
      model: MODEL, queryPrefix: QUERY_PREFIX, docPrefix: DOC_PREFIX, dims: chunkRows[0].v.length,
      sections: units.length, chunks: chunkRows.length, chunkChars: CHUNK_CHARS, rrfK: RRF_K, fuseDepth: FUSE_DEPTH,
      serverSemantic, serverIndexMs,
      tau, tauChosenOn: "dev-v2 + dev-old7 (vector top-5 coverage + true no-hit)",
      indexEmbedMs: Math.round(indexMs), queryEmbedMsP50: +qms[Math.floor(qms.length / 2)].toFixed(1), queryEmbedMsMax: +qms.at(-1).toFixed(1),
      host: { cpu: os.cpus()[0]?.model, cores: os.cpus().length, ramGB: Math.round(os.totalmem() / 2 ** 30) },
      summary, perQuery,
    };
    if (args.out) fs.writeFileSync(args.out, JSON.stringify(report, null, 2));
    console.log(JSON.stringify({ ...report, perQuery: undefined }, null, 2));
    console.table(perQuery.filter((p) => p.set.startsWith("heldout")));
  } finally {
    proc.kill();
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
