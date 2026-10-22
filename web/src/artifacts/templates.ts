// Starter files for new artifacts.
// NOTE: written in plain HTML/JS because docs/ARTIFACT-KIT.md was not available in this working copy. They only *load*
// /artifact-runtime/aiwiki-kit.js and .css (contract: served by the server) and do not call any kit API. Compare with the kit
// and replace the bodies with kit blocks when its API is known.
import type { DraftFile } from '../artifactsCore';
import { textBytes } from '../artifactsCore';

export type TemplateId = 'empty' | 'table' | 'dashboard';
export type Template = { id: TemplateId; title: string; description: string; files: () => DraftFile[] };

const head = (title: string) => `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title>
<link rel="stylesheet" href="/artifact-runtime/aiwiki-kit.css">
<script src="/artifact-runtime/aiwiki-kit.js"></script>
`;
const resizer = `<script>
// Tell the wiki how tall the page is so the frame fits it.
(function () {
  function report() { parent.postMessage({ type: 'aiwiki:resize', height: document.documentElement.scrollHeight }, '*'); }
  window.addEventListener('load', report);
  window.addEventListener('resize', report);
  if (window.ResizeObserver) new ResizeObserver(report).observe(document.body);
})();
</script>
`;

const EMPTY = `${head('Артефакт')}<style>
body { font-family: system-ui, sans-serif; margin: 0; padding: 20px; color: #222; background: transparent; }
@media (prefers-color-scheme: dark) { body { color: #eee; } }
</style>
</head>
<body>
<h1>Новый артефакт</h1>
<p>Измените этот файл: здесь можно разместить любой HTML, CSS и JavaScript.</p>
${resizer}</body>
</html>
`;

const TABLE = `${head('Таблица с фильтрами')}<style>
body { font-family: system-ui, sans-serif; margin: 0; padding: 16px; color: #222; }
.bar { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; }
input, select { font: inherit; padding: 6px 8px; border: 1px solid #bbb; border-radius: 6px; background: transparent; color: inherit; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid #ddd; }
th { cursor: pointer; user-select: none; }
.wrap { overflow-x: auto; }
@media (prefers-color-scheme: dark) { body { color: #eee; } th, td { border-color: #444; } input, select { border-color: #666; } }
</style>
</head>
<body>
<div class="bar">
  <input id="q" type="search" placeholder="Поиск" aria-label="Поиск">
  <select id="group" aria-label="Статус"><option value="">Все статусы</option></select>
</div>
<div class="wrap"><table><thead><tr id="head"></tr></thead><tbody id="body"></tbody></table></div>
<script>
// Replace the data below (or load it from data.json / data.csv next to this file via <script> data).
var COLUMNS = [['name', 'Название'], ['owner', 'Ответственный'], ['status', 'Статус'], ['count', 'Количество']];
var ROWS = [
  { name: 'Первая задача', owner: 'Анна', status: 'В работе', count: 12 },
  { name: 'Вторая задача', owner: 'Борис', status: 'Готово', count: 7 },
  { name: 'Третья задача', owner: 'Анна', status: 'Новая', count: 3 }
];
var sortKey = null, sortDir = 1;
var q = document.getElementById('q'), group = document.getElementById('group');
ROWS.map(function (r) { return r.status; }).filter(function (v, i, a) { return a.indexOf(v) === i; }).forEach(function (s) {
  var o = document.createElement('option'); o.value = s; o.textContent = s; group.appendChild(o);
});
var head = document.getElementById('head');
COLUMNS.forEach(function (c) {
  var th = document.createElement('th'); th.textContent = c[1]; th.tabIndex = 0;
  function sort() { sortDir = sortKey === c[0] ? -sortDir : 1; sortKey = c[0]; render(); }
  th.onclick = sort; th.onkeydown = function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); sort(); } };
  head.appendChild(th);
});
function render() {
  var text = q.value.trim().toLowerCase(), status = group.value;
  var rows = ROWS.filter(function (r) {
    return (!status || r.status === status) && (!text || COLUMNS.some(function (c) { return String(r[c[0]]).toLowerCase().indexOf(text) >= 0; }));
  });
  if (sortKey) rows.sort(function (a, b) { return (a[sortKey] > b[sortKey] ? 1 : a[sortKey] < b[sortKey] ? -1 : 0) * sortDir; });
  var body = document.getElementById('body'); body.textContent = '';
  rows.forEach(function (r) {
    var tr = document.createElement('tr');
    COLUMNS.forEach(function (c) { var td = document.createElement('td'); td.textContent = r[c[0]]; tr.appendChild(td); });
    body.appendChild(tr);
  });
}
q.oninput = render; group.onchange = render; render();
</script>
${resizer}</body>
</html>
`;

const DASHBOARD = `${head('Дашборд с графиком')}<style>
body { font-family: system-ui, sans-serif; margin: 0; padding: 16px; color: #222; }
.row { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; margin-bottom: 12px; }
.card { border: 1px solid #ccc; border-radius: 10px; padding: 10px 14px; min-width: 120px; }
.card b { display: block; font-size: 24px; }
svg { width: 100%; height: 240px; display: block; }
input[type=range] { flex: 1; min-width: 160px; }
@media (prefers-color-scheme: dark) { body { color: #eee; } .card { border-color: #555; } }
</style>
</head>
<body>
<div class="row">
  <label for="months">Месяцев на графике: <output id="out">12</output></label>
  <input id="months" type="range" min="3" max="12" value="12">
</div>
<div class="row"><div class="card">Сумма<b id="sum">0</b></div><div class="card">Максимум<b id="max">0</b></div></div>
<svg id="chart" viewBox="0 0 600 240" role="img" aria-label="График по месяцам" preserveAspectRatio="none"></svg>
<script>
// Replace the numbers with your own data.
var DATA = [12, 18, 15, 22, 30, 28, 35, 33, 40, 38, 45, 52];
var range = document.getElementById('months'), svg = document.getElementById('chart');
function render() {
  var n = Number(range.value), v = DATA.slice(0, n), max = Math.max.apply(null, v);
  document.getElementById('out').textContent = n;
  document.getElementById('sum').textContent = v.reduce(function (a, b) { return a + b; }, 0);
  document.getElementById('max').textContent = max;
  var w = 600 / v.length, html = '';
  v.forEach(function (x, i) {
    var h = x / max * 200;
    html += '<rect x="' + (i * w + 4) + '" y="' + (220 - h) + '" width="' + (w - 8) + '" height="' + h + '" rx="3" fill="#7357d9"><title>' + (i + 1) + ': ' + x + '</title></rect>';
  });
  svg.innerHTML = html;
}
range.oninput = render; render();
</script>
${resizer}</body>
</html>
`;

const file = (path: string, content: string): DraftFile => ({ path, encoding: 'utf8', content, original: null, size: textBytes(content) });

export const TEMPLATES: Template[] = [
  { id: 'empty', title: 'Пустой', description: 'Страница с заголовком: начните с чистого листа.', files: () => [file('index.html', EMPTY)] },
  { id: 'table', title: 'Таблица с фильтрами', description: 'Таблица с поиском, фильтром по статусу и сортировкой по столбцам.', files: () => [file('index.html', TABLE)] },
  { id: 'dashboard', title: 'Дашборд с графиком и ползунком', description: 'Показатели и столбчатый график, который меняется ползунком.', files: () => [file('index.html', DASHBOARD)] },
];
