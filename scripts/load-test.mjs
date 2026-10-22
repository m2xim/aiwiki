#!/usr/bin/env node
// Load test for AI Wiki over /mcp (JSON-RPC tools/call). Node >= 20, no dependencies.
// Workload per iteration: 60% search, 25% read (outline, then one section by anchor),
// 10% small edit (append, base_revision), 5% create. See docs/OPERATIONS.md section 4
// and docs/LOAD.md. The bearer token is read from AIWIKI_LOAD_MCP_TOKEN and never printed.
import { writeFileSync } from 'node:fs';

const HELP = `Usage: node scripts/load-test.mjs [options]
  --target URL        server base URL (default http://127.0.0.1:8090)
  --vus N             concurrent virtual users (default 10)
  --duration S        test duration in seconds (default 60)
  --articles N        seed corpus size (default 500)
  --space-id ID       use an existing space instead of creating load-test-<timestamp>
  --project-id ID     project for created articles (corporate mode)
  --no-seed           do not seed; requires --space-id with an existing corpus
  --out FILE          also write the report as JSON
  --allow-remote      allow a non-loopback target
Environment: AIWIKI_LOAD_MCP_TOKEN (bearer token; omit in trusted mode)`;

function parseArgs(argv) {
  const o = { target: 'http://127.0.0.1:8090', vus: 10, duration: 60, articles: 500, spaceId: '', projectId: '', seed: true, out: '', allowRemote: false };
  const num = (k, v) => { const n = Number(v); if (!Number.isFinite(n) || n <= 0) die(`${k} must be a positive number`); return n; };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i], next = () => { if (i + 1 >= argv.length) die(`${a} needs a value`); return argv[++i]; };
    switch (a) {
      case '--target': o.target = next(); break;
      case '--vus': o.vus = Math.floor(num(a, next())); break;
      case '--duration': o.duration = num(a, next()); break;
      case '--articles': o.articles = Math.floor(num(a, next())); break;
      case '--space-id': o.spaceId = next(); break;
      case '--project-id': o.projectId = next(); break;
      case '--no-seed': o.seed = false; break;
      case '--out': o.out = next(); break;
      case '--allow-remote': o.allowRemote = true; break;
      case '-h': case '--help': console.log(HELP); process.exit(0); break;
      default: die(`unknown option ${a}\n${HELP}`);
    }
  }
  return o;
}
function die(msg) { console.error(msg); process.exit(2); }

const opt = parseArgs(process.argv.slice(2));
const token = process.env.AIWIKI_LOAD_MCP_TOKEN || '';
let base;
try { base = new URL(opt.target); } catch { die('invalid --target'); }
const host = base.hostname.replace(/^\[|\]$/g, '');
const loopback = host === 'localhost' || host === '::1' || /^127\.\d+\.\d+\.\d+$/.test(host);
if (!loopback && !opt.allowRemote) die('refusing non-loopback target without --allow-remote');
if (!opt.seed && !opt.spaceId) die('--no-seed needs --space-id');
const endpoint = new URL('/mcp', base).toString();

// ---- corpus generator (Russian and English) ----
const EN = ['server', 'database', 'index', 'revision', 'latency', 'cluster', 'backup', 'deployment', 'queue', 'cache', 'storage', 'network', 'policy', 'session', 'metric', 'timeout', 'schema', 'replica', 'gateway', 'pipeline'];
const RU = ['сервер', 'база', 'индекс', 'ревизия', 'задержка', 'кластер', 'резервная', 'выкладка', 'очередь', 'кэш', 'хранилище', 'сеть', 'политика', 'сессия', 'метрика', 'таймаут', 'схема', 'реплика', 'шлюз', 'конвейер'];
const FILL_EN = ['the', 'service', 'must', 'handle', 'every', 'request', 'with', 'bounded', 'memory', 'and', 'report', 'errors', 'to', 'operators', 'when', 'limits', 'are', 'reached'];
const FILL_RU = ['сервис', 'должен', 'обрабатывать', 'каждый', 'запрос', 'с', 'ограниченной', 'памятью', 'и', 'сообщать', 'об', 'ошибках', 'операторам', 'когда', 'достигнуты', 'лимиты'];
const rnd = (n) => Math.floor(Math.random() * n);
const pick = (a) => a[rnd(a.length)];
function sentence() {
  const ru = Math.random() < 0.5, topics = ru ? RU : EN, fill = ru ? FILL_RU : FILL_EN, w = [];
  for (let i = 0, n = 8 + rnd(10); i < n; i++) w.push(Math.random() < 0.3 ? pick(topics) : pick(fill));
  const s = w.join(' ');
  return s[0].toUpperCase() + s.slice(1) + '.';
}
function paragraph() { const s = []; for (let i = 0, n = 3 + rnd(4); i < n; i++) s.push(sentence()); return s.join(' '); }
function makeBody(target) {
  let b = '', n = 1;
  while (b.length < target) { b += `## Section ${n++} ${pick(EN)}\n\n${paragraph()}\n\n${paragraph()}\n\n`; }
  return b;
}
function bodyBytes() { return 1000 + rnd(1100); } // characters; Cyrillic is 2 bytes in UTF-8, so 2-8 KB overall
function query() { return Math.random() < 0.3 ? `${pick(RU)} ${pick(RU)}` : Math.random() < 0.5 ? `${pick(EN)} ${pick(EN)}` : pick(Math.random() < 0.5 ? RU : EN); }

// ---- JSON-RPC client ----
let rpcId = 0;
const revisions = new Map(); // article id -> last seen revision
const ids = [];
let spaceId = opt.spaceId;

// Returns {ms, kind:'ok'|'conflict'|'tool'|'http'|'rpc'|'net', code, data}
async function call(name, args) {
  const body = JSON.stringify({ jsonrpc: '2.0', id: ++rpcId, method: 'tools/call', params: { name, arguments: args } });
  const headers = { 'Content-Type': 'application/json' };
  if (token) headers.Authorization = 'Bearer ' + token;
  const t0 = performance.now();
  let res, text;
  try {
    res = await fetch(endpoint, { method: 'POST', headers, body, signal: AbortSignal.timeout(30000) });
    text = await res.text();
  } catch (e) {
    return { ms: performance.now() - t0, kind: 'net', code: e && e.name === 'TimeoutError' ? 'timeout' : 'network' };
  }
  const ms = performance.now() - t0;
  if (res.status !== 200) return { ms, kind: 'http', code: String(res.status) };
  let j;
  try { j = JSON.parse(text); } catch { return { ms, kind: 'rpc', code: 'bad-json' }; }
  if (j.error) return { ms, kind: 'rpc', code: 'rpc' + j.error.code };
  const r = j.result;
  let data = null;
  try { data = JSON.parse(r.content[0].text); } catch { /* keep null */ }
  if (r.isError) {
    const msg = String((data && data.error) || '');
    if (/revision conflict/i.test(msg)) return { ms, kind: 'conflict', code: 'revision-conflict' };
    return { ms, kind: 'tool', code: msg.slice(0, 60) || 'tool-error' };
  }
  return { ms, kind: 'ok', code: '', data };
}

// ---- statistics ----
const OPS = ['search', 'read_outline', 'read_section', 'edit_append', 'create'];
const stats = Object.fromEntries(OPS.map((o) => [o, { lat: [], ok: 0, conflicts: 0, errors: {} }]));
function record(op, r) {
  const s = stats[op];
  if (r.kind === 'ok') { s.ok++; s.lat.push(r.ms); } else if (r.kind === 'conflict') { s.conflicts++; s.lat.push(r.ms); } else {
    const k = r.kind === 'http' ? `http ${r.code}` : r.kind === 'tool' ? `tool: ${r.code}` : r.code;
    s.errors[k] = (s.errors[k] || 0) + 1;
  }
}
const pct = (sorted, p) => (sorted.length ? sorted[Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)] : 0);

// ---- operations ----
async function opSearch() { record('search', await call('wiki_search', { q: query(), space_id: spaceId, limit: 20 })); }
async function opRead() {
  const id = pick(ids);
  const o = await call('wiki_read', { id, outline: true });
  record('read_outline', o);
  if (o.kind !== 'ok' || !o.data) return;
  revisions.set(id, o.data.revision);
  const hs = o.data.headings || [];
  if (!hs.length) return;
  record('read_section', await call('wiki_read', { id, anchor: pick(hs).anchor }));
}
async function opEdit() {
  const id = pick(ids);
  let rev = revisions.get(id);
  if (!rev) { rev = 1; }
  const r = await call('wiki_edit', { id, base_revision: rev, op: 'append', text: `\n${sentence()}\n`, message: 'load test' });
  record('edit_append', r);
  if (r.kind === 'ok' && r.data) revisions.set(id, r.data.revision);
  else if (r.kind === 'conflict') revisions.delete(id); // re-learn via next outline read
  if (r.kind === 'conflict') { const o = await call('wiki_read', { id, outline: true }); if (o.kind === 'ok' && o.data) revisions.set(id, o.data.revision); }
}
let created = 0;
function article(title, size) {
  const a = { space_id: spaceId, title, summary: sentence(), body: makeBody(size), kind: 'article', status: 'active', tags: ['load-test'], author: 'load-test' };
  if (opt.projectId) a.project_id = opt.projectId;
  return a;
}
async function opCreate() {
  const r = await call('wiki_write', { article: article(`Load create ${Date.now()}-${++created}-${rnd(1e6)}`, bodyBytes()) });
  record('create', r);
  if (r.kind === 'ok' && r.data) { ids.push(r.data.id); revisions.set(r.data.id, r.data.revision); }
}

// ---- setup ----
async function createSpace() {
  const name = `load-test-${new Date().toISOString().replace(/[-:.TZ]/g, '').slice(0, 14)}`;
  const headers = { 'Content-Type': 'application/json' };
  if (token) headers.Authorization = 'Bearer ' + token;
  const res = await fetch(new URL('/api/spaces', base), { method: 'POST', headers, body: JSON.stringify({ name }), signal: AbortSignal.timeout(10000) });
  if (res.status !== 201) die(`could not create space ${name} (HTTP ${res.status}); create it in the UI and pass --space-id (and --project-id in corporate mode)`);
  const sp = await res.json();
  return { id: sp.id, name };
}

async function seed() {
  let next = 0, failed = 0;
  const t0 = performance.now();
  async function worker() {
    for (;;) {
      const i = next++;
      if (i >= opt.articles) return;
      const r = await call('wiki_write', { article: article(`Load seed ${i} ${pick(EN)} ${pick(RU)}`, bodyBytes()) });
      if (r.kind === 'ok' && r.data) { ids.push(r.data.id); revisions.set(r.data.id, r.data.revision); } else { failed++; if (failed === 1) console.error(`seed error: ${r.kind} ${r.code}`); }
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, opt.articles) }, worker));
  if (failed) die(`seeding failed for ${failed} articles`);
  console.log(`seeded ${ids.length} articles in ${((performance.now() - t0) / 1000).toFixed(1)} s`);
}

async function main() {
  const init = await fetch(endpoint, { method: 'POST', headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: 'Bearer ' + token } : {}) }, body: JSON.stringify({ jsonrpc: '2.0', id: 0, method: 'initialize', params: {} }), signal: AbortSignal.timeout(10000) }).catch((e) => die(`cannot reach ${opt.target}: ${e.cause?.code || e.message}`));
  if (init.status !== 200) die(`initialize failed: HTTP ${init.status}`);
  let spaceName = spaceId;
  if (!spaceId) { const sp = await createSpace(); spaceId = sp.id; spaceName = sp.name; }
  console.log(`target ${opt.target} space ${spaceName} (${spaceId}) auth ${token ? 'bearer' : 'none'} vus ${opt.vus} duration ${opt.duration}s`);
  if (opt.seed) await seed();
  else {
    const r = await call('wiki_search', { space_id: spaceId, limit: 100 });
    if (r.kind !== 'ok' || !r.data.results.length) die('no articles found in --space-id');
    for (const it of r.data.results) { ids.push(it.id); }
  }
  if (!ids.length) die('empty corpus');

  const deadline = performance.now() + opt.duration * 1000;
  const t0 = performance.now();
  async function vu() {
    while (performance.now() < deadline) {
      const x = rnd(100);
      if (x < 60) await opSearch(); else if (x < 85) await opRead(); else if (x < 95) await opEdit(); else await opCreate();
    }
  }
  await Promise.all(Array.from({ length: opt.vus }, vu));
  const elapsed = (performance.now() - t0) / 1000;

  const rows = [];
  let total = 0, totalErr = 0, totalConf = 0;
  for (const op of OPS) {
    const s = stats[op], sorted = s.lat.slice().sort((a, b) => a - b);
    const err = Object.values(s.errors).reduce((a, b) => a + b, 0);
    const count = s.ok + s.conflicts + err;
    total += count; totalErr += err; totalConf += s.conflicts;
    rows.push({ op, count, ok: s.ok, conflicts: s.conflicts, errors: err, errors_by_code: s.errors, p50_ms: pct(sorted, 50), p95_ms: pct(sorted, 95), p99_ms: pct(sorted, 99), max_ms: sorted.length ? sorted[sorted.length - 1] : 0 });
  }
  const report = { target: opt.target, vus: opt.vus, duration_s: opt.duration, elapsed_s: elapsed, seed_articles: opt.articles, space_id: spaceId, auth: token ? 'bearer' : 'none', total_requests: total, errors: totalErr, conflicts: totalConf, throughput_rps: total / elapsed, operations: rows };
  const f = (n) => n.toFixed(1).padStart(8);
  console.log('\n' + 'op'.padEnd(14) + ['count', 'ok', 'conflict', 'errors', 'p50 ms', 'p95 ms', 'p99 ms', 'max ms'].map((h) => h.padStart(9)).join(''));
  for (const r of rows) console.log(r.op.padEnd(14) + [r.count, r.ok, r.conflicts, r.errors].map((n) => String(n).padStart(9)).join('') + [r.p50_ms, r.p95_ms, r.p99_ms, r.max_ms].map((n) => f(n) + ' ').join(''));
  console.log(`\ntotal ${total} requests in ${elapsed.toFixed(1)} s = ${report.throughput_rps.toFixed(1)} req/s; errors ${totalErr}; revision conflicts ${totalConf}`);
  for (const r of rows) for (const [k, n] of Object.entries(r.errors_by_code)) console.log(`  error ${r.op}: ${k} x${n}`);
  if (opt.out) { writeFileSync(opt.out, JSON.stringify(report, null, 2) + '\n'); console.log(`report written to ${opt.out}`); }
  process.exit(totalErr ? 1 : 0);
}
main().catch((e) => { console.error(String(e && e.message || e).replaceAll(token || '\u0000', '***')); process.exit(2); });
