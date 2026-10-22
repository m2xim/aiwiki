# AI Wiki Kit: ready blocks for artifacts

A kit of tables, charts, controls, KPI cards and a gallery for an artifact written as one `index.html`
(see [ARTIFACTS.md](ARTIFACTS.md)). The interface is in Russian by default. Sources: `web/artifact-kit/`,
build: `cd web && npm run build` produces `web/dist/artifact-runtime/aiwiki-kit.js` (one IIFE, global `AIWiki`,
about 222 KiB, 80 KiB gzipped, with Chart.js 4.5.1 tree-shaken in) and `aiwiki-kit.css` (9 KiB).

## Connecting

```html
<link rel="stylesheet" href="/artifact-runtime/aiwiki-kit.css">
<script src="/artifact-runtime/aiwiki-kit.js"></script>
```

Put the `<script>` in `<head>` (custom elements such as `<aiwiki-table>` then upgrade as the page is parsed) and your
own code at the end of `<body>`. The stylesheet also styles `body` (padding, font, background, theme), headings and
links; helper classes: `aw-grid` (responsive columns), `aw-card`, `aw-row`, `aw-muted`, `aw-btn`, `aw-input`.

## What the sandbox gives and takes away

An artifact runs in `<iframe sandbox="allow-scripts allow-modals">` with a CSP (ARTIFACTS.md, "Rendering and isolation").
Therefore:

- No network: `fetch`, `XMLHttpRequest`, `WebSocket` and external scripts, fonts, images and styles do not work.
  **Data must be inline** (a JS literal, `<script type="application/json">` / `<script type="text/csv">`) or in a
  script file of the artifact (`<script src="data.js">` that assigns `window.DATA = [...]`). A `data.csv` or `data.json`
  file of the artifact cannot be read by `fetch`; paste its text into the page or convert it to a `.js` file.
- No cookies, `localStorage`/`sessionStorage`/IndexedDB (origin is `null`): state lives only until the page is closed.
- No `eval`, `new Function`, string timers: the kit uses none. Do not use libraries that do.
- No pop-up windows, no navigation of the top page, no forms submission. `alert`/`confirm`/`prompt` work (`allow-modals`).
- Images and scripts from the artifact's own files work by relative path (`img/photo.png`, `data:` URLs too).
- Downloads: the sandbox has no `allow-downloads`, so the browser may silently ignore a `Blob` download. The table's
  "Экспорт CSV" button tries to download anyway and always shows "Файл не скачался? **Показать CSV**": a dialog with the
  CSV text, a copy button, and Esc to close. Build your own export the same way if you need one.
- Height: the kit reports the document height itself (`parent.postMessage({type: 'aiwiki:resize', height}, '*')`,
  throttled, on `ResizeObserver`/`MutationObserver`/load). Do not size content with `100vh`: it would feed back into the
  frame height. `AIWiki.autoResize(false)` turns it off.

## Theme

The frame cannot see the wiki's theme, so the wiki opens it with `?theme=light` or `?theme=dark` in the URL of `index.html`; the kit applies it at start (like `AIWiki.setTheme`). Without the parameter the kit follows `prefers-color-scheme` of the document; an explicit
`AIWiki.setTheme('light' | 'dark' | 'auto')`, read with `AIWiki.getTheme()`. Colours are CSS variables (`--aw-bg`, `--aw-fg`,
`--aw-muted`, `--aw-border`, `--aw-surface`, `--aw-accent`, `--aw-good`, `--aw-warn`, `--aw-bad`) usable in your own CSS.
Charts redraw on a theme change. The layout holds down to 360 px.

## State and binding

```js
const s = AIWiki.state({ threshold: 100, region: 'Все' });   // shallow reactive object
AIWiki.bind('#slider', s, 'threshold');                      // two-way: control <-> s.threshold
AIWiki.effect(() => {                                        // runs now and after any read key changes
  grid.update({ rowFilter: (r) => r.amount >= s.threshold });
  AIWiki.chart... /* any code reading s.* */
});
```

- `state(obj)`: reading a key inside `effect` subscribes to it. Assign new values to change; for arrays/objects assign
  a new one (`s.list = [...s.list, x]`), mutation inside is not noticed. `s.$subscribe((key, value) => ...)` for raw
  change notifications.
- `effect(fn)` returns a stop function. Re-runs are batched in a microtask; the first run is synchronous. An exception
  in an effect is logged to the console and does not stop other effects.
- `bind(control, state, key)`: `control` is a selector, an element, an `<aiwiki-*>` control, or a native
  `<input>`/`<select>`/`<textarea>` (checkbox is boolean, number/range is a number). Returns an unbind function.
  Avoid writing to a key inside an effect that reads that same key.

## Table

```js
const t = AIWiki.table('#box', {
  title: 'Сделки',
  rows: [{ город: 'Москва', сумма: 1200, дата: '2026-10-01' }, ...],   // array of objects, or CSV/JSON text
  columns: [                                                          // optional: inferred from data
    'город',
    { key: 'сумма', label: 'Сумма', format: 'currency', agg: 'sum' },
    { key: 'дата', type: 'date' },
  ],
  groupBy: 'город', pageSize: 25, totals: true,
});
t.update({ rows: newRows });          // or { rowFilter: (row) => ..., groupBy, columns, sortBy, pageSize, ... }
t.getRows();                          // filtered and sorted rows (all pages)
t.exportCSV();                        // CSV text of the filtered rows
```

**Data sources.** `rows` is an array of objects, or a string: CSV (delimiter `,` `;` or tab is detected; quotes, `""`,
newlines inside quotes, BOM) or JSON (`[...]` or `{"columns": [...], "rows": [...]}`). Or `source: '#id'` of a
`<script type="text/csv">` / `<script type="application/json">` element.

**Column** (`string` = just `{key}`): `key`, `label`, `type` (`string | number | date | boolean`, otherwise inferred
from the first 200 values; `"1 234,5"` is a number, `"007"` is not; `yyyy-mm-dd` and `dd.mm.yyyy` are dates),
`format`, `currency`, `digits`, `filter` (`'text' | 'select' | 'range' | false`; default is range for numbers and dates,
select for booleans and for text with at most 12 distinct values over more than 12 rows, text otherwise), `agg`
(`'count' | 'sum' | 'avg' | 'min' | 'max'`, shown in group headers and the totals row), `align`, `sortable`, `width`.

**Format**: `'number'` (2 digits), `'integer'`, `'percent'` (0.25 is 25 %), `'currency'` (`currency: 'RUB'` by default),
`'date'` (`22.10.2026`), `'datetime'`, or a function `(value) => string`. Unparsable values are shown as they are, empty
ones as an empty cell. Texts are inserted via `textContent`: HTML in data is never interpreted.

**Behaviour.** Click a header: ascending, descending, off. Numbers, dates and strings (Russian collation, case-insensitive,
`file2` before `file10`) are compared by type; empty cells are always last. Global search (all words must match, over
shown and raw values) and per-column filters sit above the table. Grouping shows a header per group with a count,
aggregates, and a collapse toggle. Pagination: page size 10/25/50/100 (`pageSizes` to change), fine for 10 000 rows
(only a page is rendered). Row highlight: `highlight: (row) => 'good' | 'warn' | 'bad' | 'accent' | true | false`;
clicking a row selects it (`onRowClick(row)`). `onChange(rows)` fires after each recomputation.
**CSV export is protected against formula injection**: a text cell (or header) that starts with `=`, `+`, `-`, `@`, a tab or a carriage return (also after leading spaces) is written with a leading apostrophe and quoted, so Excel and similar programs show it as text instead of running it. Real numbers (JS `number`, also negative ones) are written as numbers without a prefix; the string `"-5"` from the data is a text and gets the apostrophe. The same text appears in the "Показать CSV" dialog. The apostrophe is part of the exported text, which is the intended trade-off.

Other options: `searchable`, `filters` (`false` hides the filter row), `exportable`, `csvName`, `csvDelimiter`
(default `;` plus a BOM: opens correctly in Russian-locale Excel), `locale`, `emptyText`.

**Element form**: `<aiwiki-table title="Города" group-by="Регион" page-size="10" totals csv-name="x" no-search
no-filters no-export src="#dataId">` with the CSV or JSON in a child `<script type="text/csv">`. Columns: the `columns`
attribute (JSON or comma-separated names) or `el.config = { columns, highlight, rowFilter, ... }` (later calls update).

## Charts

```js
AIWiki.lineChart('#c1', { labels: ['янв', 'фев'], series: { 'Выручка': [10, 12], 'Затраты': [7, 8] }, format: 'currency' });
AIWiki.barChart('#c2',  { labels, series, stacked: true, horizontal: false });
AIWiki.pieChart('#c3',  { labels: ['A', 'B'], series: [30, 70], donut: true });
const c = AIWiki.chart('#c4', { type: 'scatter', data: {...}, options: {...} });   // plain Chart.js config
c.update({ labels: [...], series: {...} });        // simple form; or { datasets: [...] } for the Chart.js form
c.chart;                                           // the Chart.js instance; c.destroy()
```

`series` is an object `{name: [numbers]}`, an array of `{label, data, color?}`, or (for a pie) just an array of numbers.
Options: `title`, `height` (px, default 320), `format`/`currency`/`digits` for axis and tooltip values, `smooth`, `fill`,
`stacked`, `horizontal`, `legend` (default: shown for pie/doughnut and when there is more than one series, re-evaluated on `update()`; `false`/`true` forces it), `options` (merged into the Chart.js options). Types registered: line, bar, pie, doughnut,
scatter. The target is a container (a canvas is created) or an existing `<canvas>`. Theme, palette, `ru-RU` numbers and
`prefers-reduced-motion` are applied automatically.

**Dates on an axis.** The kit does not bundle a Chart.js date adapter (moment, luxon, date-fns), so a real time scale is
unavailable. `Date` objects and ISO strings in `labels` are turned into `dd.mm.yyyy` category labels, spaced evenly, not by
distance. For unevenly spaced time series use `scatter` with your own `ticks.callback`.

## Controls

Web Components in the light DOM (no Shadow DOM): the kit stylesheet, your CSS, labels, focus and forms behave as usual and
theme variables apply without extra work. Each one fires a bubbling `change` event with `event.detail.value` and has a
`value` property.

| Element | Attributes | `value` |
| --- | --- | --- |
| `<aiwiki-slider>` | `label min max step value unit format` (`format="percent"`) | number, fires while dragging |
| `<aiwiki-number>` | `label min max step value` (arrows, buttons; decimal comma accepted) | number or `null` |
| `<aiwiki-toggle>` | `label checked` | boolean |
| `<aiwiki-select>` | `label value options`: `"A, B, C"`, `"code:Label, ..."` or JSON `[{"value":1,"label":"One"}]`; or child `<option>` | the option's value (string or the JSON value) |
| `<aiwiki-tabs>` | `value`; children `<section data-tab="Title" data-value="id">` | `data-value` (else the title) |

```html
<aiwiki-slider id="p" label="Порог" min="0" max="100" value="30"></aiwiki-slider>
<script>document.getElementById('p').addEventListener('change', (e) => console.log(e.detail.value));</script>
```

`select.options = [...]` replaces options from code.

## KPI cards and gallery

```js
AIWiki.stat('#k', { label: 'Выручка', value: 1234567, format: 'currency', digits: 0, delta: 0.123, hint: 'к прошлому месяцу' });
const k = AIWiki.stats('#kpis', [{ label: 'Заказы', value: 240, format: 'integer' }, ...]);   // a grid of cards
k[0].update({ value: 250 });
```

`delta`: a number is formatted with `deltaFormat` (default `'percent'`: `0.123` gives `▲ +12,3 %`); text is shown as is.
`good: 'up' | 'down' | 'none'` chooses which direction is green (costs: `'down'`).

```js
AIWiki.gallery('#photos', { images: ['img/a.png', { src: 'img/b.png', caption: 'Подпись', thumb: 'img/b-small.png' }], columns: 3 });
AIWiki.gallery('#existing');            // an element with <img>s already inside: they become clickable
AIWiki.lightbox(['img/a.png', 'img/b.png'], 1);
```

Or `<div data-aiwiki-gallery>` with `<img>` children, no script needed. Lightbox: Esc, ← →, click outside to close.

## Full dashboard

KPI, a table with grouping, charts and a slider, all tied by one state (the live file is
`web/artifact-kit/examples/dashboard.html`):

```html
<link rel="stylesheet" href="/artifact-runtime/aiwiki-kit.css">
<script src="/artifact-runtime/aiwiki-kit.js"></script>

<aiwiki-slider id="min" label="Минимальная сумма" min="0" max="500000" step="10000" value="0" unit="₽"></aiwiki-slider>
<div id="kpi"></div><div id="line"></div><div id="tbl"></div>

<script>
  const rows = [{ date: '2026-01-10', region: 'Центр', amount: 120000 }, /* ... */];
  const s = AIWiki.state({ min: 0 });
  AIWiki.bind('#min', s, 'min');
  const [sum, cnt] = AIWiki.stats('#kpi', [{ label: 'Выручка', value: 0, format: 'currency', digits: 0 }, { label: 'Сделок', value: 0, format: 'integer' }]);
  const line = AIWiki.lineChart('#line', { labels: [], series: { 'Выручка': [] }, format: 'currency', digits: 0 });
  const grid = AIWiki.table('#tbl', { rows, groupBy: 'region', columns: [
    { key: 'date', label: 'Дата', type: 'date' }, 'region', { key: 'amount', label: 'Сумма', format: 'currency', agg: 'sum' } ] });

  AIWiki.effect(() => {
    const f = rows.filter((r) => r.amount >= s.min);
    sum.update({ value: f.reduce((a, r) => a + r.amount, 0) });
    cnt.update({ value: f.length });
    line.update({ labels: f.map((r) => r.date), series: { 'Выручка': f.map((r) => r.amount) } });
    grid.update({ rows: f });
  });
</script>
```

## Testing the kit by hand

```
cd web && npm ci && npm run build
node artifact-kit/examples/serve.mjs 18765        # http://127.0.0.1:18765/
node ../scripts/qa-kit.mjs                        # logic tests and bundle checks
```

`serve.mjs` serves the examples in an iframe with the same `sandbox` attribute, CSP and resize handling as the wiki. With
`KIT_NO_CSP=1` it omits the CSP header (for tools that cannot load sub-resources of a CSP-sandboxed document). It serves `Cross-Origin-Resource-Policy: cross-origin`: with `same-origin` a sandboxed frame (origin null) cannot load its own scripts and styles, so the server that serves frames and `/artifact-runtime/` must send `cross-origin` (ARTIFACTS.md says `same-origin`; it needs correcting).
