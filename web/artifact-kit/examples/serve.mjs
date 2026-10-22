// Manual test harness for the kit: serves the examples inside a sandboxed iframe exactly as the wiki will
// (CSP from docs/ARTIFACTS.md, iframe sandbox="allow-scripts allow-modals", resize via postMessage).
//
//   cd web && npm run build
//   node artifact-kit/examples/serve.mjs [port]      # default 18765, then open http://127.0.0.1:18765/
//
// Not part of the product: it only reads files from web/dist/artifact-runtime and this folder.
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const runtime = path.resolve(here, '../../dist/artifact-runtime');
const port = Number(process.argv[2] || process.env.PORT || 18765);
const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml', '.json': 'application/json', '.csv': 'text/csv; charset=utf-8', '.png': 'image/png' };
const CSP = "sandbox allow-scripts allow-modals; default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'";
const examples = fs.readdirSync(here).filter((f) => f.endsWith('.html')).sort();

const hostPage = `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Kit: тестовая страница</title>
<style>body{font:15px system-ui,sans-serif;margin:0;background:#e9edf2;color:#18202b}header{padding:10px 16px;background:#fff;border-bottom:1px solid #ccd3dc;display:flex;gap:8px;flex-wrap:wrap;align-items:center}
a{padding:4px 10px;border:1px solid #ccd3dc;border-radius:6px;text-decoration:none;color:inherit}a.on{background:#2f6fed;color:#fff;border-color:#2f6fed}
main{max-width:1100px;margin:16px auto;padding:0 12px}iframe{width:100%;border:1px solid #ccd3dc;background:#fff;border-radius:8px;height:300px;display:block}
#info{margin-left:auto;color:#5b6573}</style></head><body>
<header><b>Kit</b>${examples.map((e) => `<a href="?f=${e}">${e}</a>`).join('')}<a href="?f=${examples[0]}&w=360">360 px</a><span id="info"></span></header>
<main><iframe id="f" sandbox="allow-scripts allow-modals" referrerpolicy="no-referrer" title="artifact"></iframe></main>
<script>
const q = new URLSearchParams(location.search), file = q.get('f') || ${JSON.stringify(examples[0])}, f = document.getElementById('f');
if (q.get('w')) f.style.maxWidth = q.get('w') + 'px';
document.querySelectorAll('header a').forEach((a) => { if (a.getAttribute('href') === '?f=' + file && !q.get('w') || (q.get('w') && a.textContent === '360 px')) a.className = 'on'; });
f.src = '/examples/' + file;
addEventListener('message', (e) => {
  if (e.source !== f.contentWindow || !e.data || e.data.type !== 'aiwiki:resize') return;
  const h = Math.min(4000, Math.max(120, Number(e.data.height) || 0));
  f.style.height = h + 'px';
  document.getElementById('info').textContent = 'высота фрейма: ' + h + ' px';
});
</script></body></html>`;

http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  let p = decodeURIComponent(url.pathname);
  // ?slow=1 delays the load event (headless Firefox screenshots fire on load, before charts paint).
  if (p === '/slow') { setTimeout(() => { res.writeHead(200, { 'content-type': 'image/gif' }); res.end(Buffer.from('R0lGODlhAQABAAAAACw=', 'base64')); }, 4000); return; }
  if (p === '/') { res.writeHead(200, { 'content-type': types['.html'] }); res.end(url.searchParams.get('slow') ? hostPage.replace('</main>', '</main><img src="/slow" alt="">') : hostPage); return; }
  let file = null;
  if (p.startsWith('/artifact-runtime/')) file = path.join(runtime, p.slice('/artifact-runtime/'.length));
  else if (p.startsWith('/examples/')) file = path.join(here, p.slice('/examples/'.length));
  const base = p.startsWith('/artifact-runtime/') ? runtime : here;
  if (!file || !path.resolve(file).startsWith(base + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile() || path.basename(file) === 'serve.mjs') {
    res.writeHead(404, { 'content-type': 'text/plain' }); res.end('not found'); return;
  }
  res.writeHead(200, {
    'content-type': types[path.extname(file)] || 'application/octet-stream',
    ...(process.env.KIT_NO_CSP ? {} : { 'content-security-policy': CSP }), 'x-content-type-options': 'nosniff', 'referrer-policy': 'no-referrer',
    'cross-origin-resource-policy': 'cross-origin', 'cache-control': 'no-store',
  });
  fs.createReadStream(file).pipe(res);
}).listen(port, '127.0.0.1', () => console.log(`Kit test page: http://127.0.0.1:${port}/`));
