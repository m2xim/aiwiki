// AIWiki.table(): sortable, filterable, groupable, paginated table; CSV export. Light DOM, styled by aiwiki-kit.css.
import { h, modal, nextId, resolve, debounce } from './dom.ts';
import type { Target } from './dom.ts';
import { DEFAULT_LOCALE, formatValue, toDate, toNumber } from './core/format.ts';
import type { FormatOptions } from './core/format.ts';
import { parseData, toCSV } from './core/csv.ts';
import {
  aggregate, cellValue, displayText, distinctValues, exportMatrix, filterRows, groupRows, layoutPage, nextSort, normalizeColumns, sortRows,
} from './core/tablelogic.ts';
import type { Column, ColumnInput, Filters, Row, SortSpec, SearchCache, Group } from './core/tablelogic.ts';

export interface TableOptions {
  columns?: (string | ColumnInput)[];
  /** Array of objects, or CSV / JSON text. */
  rows?: Row[] | string;
  /** Selector of a <script type="text/csv"> or <script type="application/json"> element with the data. */
  source?: string;
  title?: string;
  searchable?: boolean;
  /** Per-column filters under the header (default true). */
  filters?: boolean;
  groupBy?: string | null;
  sortBy?: SortSpec | null;
  pageSize?: number;
  pageSizes?: number[];
  exportable?: boolean;
  csvName?: string;
  /** CSV delimiter for export; ';' (default) opens in Russian-locale Excel. */
  csvDelimiter?: string;
  /** Totals row for columns that have `agg`. */
  totals?: boolean;
  locale?: string;
  currency?: string;
  emptyText?: string;
  /** Row highlight: returns true | 'good' | 'warn' | 'bad' | 'accent' | falsy. */
  highlight?: (row: Row) => boolean | 'good' | 'warn' | 'bad' | 'accent' | null | undefined;
  /** Extra programmatic filter (e.g. driven by a slider). Applied together with the UI filters. */
  rowFilter?: ((row: Row) => boolean) | null;
  onRowClick?: (row: Row) => void;
  /** Called after every recomputation with the filtered and sorted rows (before pagination). */
  onChange?: (rows: Row[]) => void;
}

export interface TableHandle {
  el: HTMLElement;
  /** Merge new options (rows, columns, rowFilter, groupBy, ...) and re-render, keeping sort/filters/page. */
  update(patch: Partial<TableOptions>): void;
  /** Filtered and sorted rows (all pages). */
  getRows(): Row[];
  setSearch(text: string): void;
  exportCSV(): string;
  destroy(): void;
}

const PAGE_SIZES = [10, 25, 50, 100];
const AGG_LABEL = { count: 'n', sum: 'Σ', avg: 'ср.', min: 'мин', max: 'макс' } as const;

function readSource(sel: string): { columns: string[] | null; rows: Row[] } {
  const el = document.querySelector(sel);
  if (!el) throw new Error(`AIWiki.table: не найден источник данных «${sel}»`);
  return parseData(el.textContent || '');
}

export function table(target: Target, options: TableOptions = {}): TableHandle {
  const host = resolve(target, 'контейнер таблицы');
  const id = nextId('awt');
  let opts: TableOptions = { searchable: true, filters: true, exportable: true, pageSize: 25, ...options };
  let rows: Row[] = [];
  let knownKeys: string[] | null = null;
  let cols: Column[] = [];
  let sort: SortSpec | null = opts.sortBy ?? null;
  let page = 1;
  let pageSize = opts.pageSize || 25;
  const filters: Filters = { search: '', columns: {} };
  const collapsed = new Set<string>();
  let selected: Row | null = null;
  let filtered: Row[] = [];
  const cache: SearchCache = new WeakMap();
  let rowsChanged = true;
  const filterCells = new Map<string, HTMLElement>();
  let toastTimer: ReturnType<typeof setTimeout> | undefined;

  const fo = (): FormatOptions => ({ locale: opts.locale || DEFAULT_LOCALE, currency: opts.currency });

  function loadData() {
    let src: Row[] | string | undefined = opts.rows;
    knownKeys = null;
    if (src === undefined && opts.source) { const p = readSource(opts.source); src = p.rows; knownKeys = p.columns; }
    if (typeof src === 'string') { const p = parseData(src); rows = p.rows; knownKeys = p.columns; }
    else rows = Array.isArray(src) ? src : [];
  }

  function setup() {
    loadData();
    cols = normalizeColumns(opts.columns, rows, knownKeys);
    filterCells.clear();
    rowsChanged = false;
  }

  // ---- DOM skeleton -------------------------------------------------------------------------
  const root = h('div', { class: 'aw-table' });
  host.append(root);
  const searchInput = h('input', { type: 'search', class: 'aw-input aw-search', 'data-aw-key': 'search', placeholder: 'Поиск по таблице…', 'aria-label': 'Поиск по таблице' });
  const countEl = h('span', { class: 'aw-muted aw-count', 'aria-live': 'polite' });
  const exportBtn = h('button', { type: 'button', class: 'aw-btn' }, 'Экспорт CSV');
  const toolbar = h('div', { class: 'aw-toolbar' });
  const scroll = h('div', { class: 'aw-scroll', tabindex: '0', role: 'region', 'aria-label': opts.title || 'Таблица' });
  const pager = h('div', { class: 'aw-pager' });
  const toast = h('div', { class: 'aw-toast', hidden: true });
  root.append(toolbar, scroll, pager, toast);

  const onSearch = debounce(() => { filters.search = searchInput.value; page = 1; render(); }, 120);
  searchInput.addEventListener('input', onSearch);
  exportBtn.addEventListener('click', () => doExport());

  function doExport() {
    const text = exportCSV();
    const name = (opts.csvName || 'table') + '.csv';
    try {
      const blob = new Blob(['﻿', text], { type: 'text/csv;charset=utf-8' });
      const url = URL.createObjectURL(blob);
      const a = h('a', { href: url, download: name });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10000);
    } catch { /* fall through to the text view below */ }
    showToast(name, text);
  }

  function showToast(name: string, text: string) {
    toast.replaceChildren(
      h('span', null, `Экспорт «${name}»: ${filtered.length} строк. Файл не скачался? `),
      h('button', { type: 'button', class: 'aw-link', onclick: () => showCSV(text) }, 'Показать CSV'),
    );
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 12000);
  }

  function showCSV(text: string) {
    const ta = h('textarea', { class: 'aw-input aw-csv', readonly: true, rows: 14, 'aria-label': 'Текст CSV' });
    ta.value = text;
    const status = h('span', { class: 'aw-muted' });
    const copy = h('button', { type: 'button', class: 'aw-btn' }, 'Копировать');
    copy.addEventListener('click', async () => {
      ta.focus(); ta.select();
      let ok = false;
      try { await navigator.clipboard.writeText(text); ok = true; } catch { /* sandbox may deny clipboard */ }
      if (!ok) { try { ok = document.execCommand('copy'); } catch { ok = false; } }
      status.textContent = ok ? 'Скопировано' : 'Не удалось скопировать автоматически — нажмите Ctrl+C';
    });
    modal('CSV для копирования', h('div', { class: 'aw-modal-body' }, ta, h('div', { class: 'aw-row' }, copy, status)));
    ta.select();
  }

  function exportCSV(): string {
    return toCSV(exportMatrix(filtered, cols), opts.csvDelimiter || ';');
  }

  // ---- filters UI ---------------------------------------------------------------------------
  function filterCell(col: Column): HTMLElement {
    const td = h('th', { class: 'aw-fcell' });
    if (!col.filter) return td;
    const f = () => (filters.columns[col.key] ??= {});
    const apply = debounce(() => { page = 1; render(); }, 150);
    if (col.filter === 'text') {
      const i = h('input', { type: 'search', class: 'aw-input aw-fi', placeholder: 'Фильтр', 'data-aw-key': `f:${col.key}:text`, 'aria-label': `Фильтр: ${col.label}` });
      i.value = filters.columns[col.key]?.text ?? '';
      i.addEventListener('input', () => { f().text = i.value; apply(); });
      td.append(i);
    } else if (col.filter === 'select') {
      const s = h('select', { class: 'aw-input aw-fi', 'data-aw-key': `f:${col.key}:sel`, 'aria-label': `Фильтр: ${col.label}` }, h('option', { value: '' }, 'Все'));
      const vals = distinctValues(rows, col).sort((a, b) => col.type === 'number' ? Number(a) - Number(b) : a.localeCompare(b, 'ru', { numeric: true }));
      for (const v of vals) {
        const sample = rows.find(r => String(cellValue(r, col)) === v);
        s.append(h('option', { value: v }, sample ? displayText(sample, col, fo()) : v));
      }
      s.value = filters.columns[col.key]?.values?.[0] ?? '';
      s.addEventListener('change', () => { f().values = s.value ? [s.value] : []; page = 1; render(); });
      td.append(s);
    } else {
      const isDate = col.type === 'date';
      const mk = (ph: string, which: 'min' | 'max') => {
        const i = h('input', { type: isDate ? 'date' : 'text', inputmode: isDate ? null : 'decimal', class: 'aw-input aw-fi aw-range', 'data-aw-key': `f:${col.key}:${which}`, placeholder: ph, 'aria-label': `${ph} ${col.label}` });
        const cur = filters.columns[col.key]?.[which];
        if (cur !== undefined && cur !== null && !isDate) i.value = String(cur).replace('.', ',');
        i.addEventListener('input', () => {
          let v: number | null = null;
          if (i.value.trim()) {
            v = isDate ? toDate(i.value) : toNumber(i.value);
            if (isDate && v !== null && which === 'max') v += 86400000 - 1;
          }
          i.setAttribute('aria-invalid', i.value.trim() && v === null ? 'true' : 'false');
          f()[which] = v;
          apply();
        });
        return i;
      };
      td.append(h('div', { class: 'aw-range-wrap' }, mk('от', 'min'), mk('до', 'max')));
    }
    return td;
  }

  // ---- render -------------------------------------------------------------------------------
  function compute() {
    let base = rows;
    if (opts.rowFilter) { const rf = opts.rowFilter; base = base.filter(r => { try { return rf(r); } catch { return false; } }); }
    filtered = sortRows(filterRows(base, cols, filters, fo(), cache), cols, sort, opts.locale);
    opts.onChange?.(filtered);
  }

  function aggText(col: Column, v: number | null): string {
    if (v === null || !col.agg) return '';
    const f = col.agg === 'count' ? 'integer' : col.format;
    return `${AGG_LABEL[col.agg]} ${formatValue(v, f, { ...fo(), currency: col.currency, digits: col.digits })}`;
  }

  function render() {
    const ae = document.activeElement as HTMLInputElement | null;
    const key = ae && root.contains(ae) ? ae.getAttribute('data-aw-key') : null;
    const selStart = key && typeof ae!.selectionStart === 'number' ? ae!.selectionStart : null;
    const selEnd = key && typeof ae!.selectionEnd === 'number' ? ae!.selectionEnd : null;
    draw();
    if (key) {
      const el = [...root.querySelectorAll<HTMLInputElement>('[data-aw-key]')].find(x => x.getAttribute('data-aw-key') === key);
      if (el && document.activeElement !== el && !(el as HTMLButtonElement).disabled) {
        el.focus({ preventScroll: true });
        try { if (selStart !== null) el.setSelectionRange(selStart, selEnd); } catch { /* not a text field */ }
      }
    }
  }

  function draw() {
    if (rowsChanged) setup();
    compute();
    const groupKey = opts.groupBy && cols.some(c => c.key === opts.groupBy) ? opts.groupBy : null;
    const groups: Group[] | null = groupKey ? groupRows(filtered, cols, groupKey, fo()) : null;
    const lay = layoutPage(groups, filtered, page, pageSize, collapsed);
    page = lay.page;

    // toolbar (rebuilt cheaply; the search input is preserved so it keeps focus)
    const tb: Node[] = [];
    if (opts.title) tb.push(h('strong', { class: 'aw-title' }, opts.title));
    if (opts.searchable !== false) tb.push(searchInput);
    tb.push(h('span', { class: 'aw-spacer' }), countEl);
    if (opts.exportable !== false) tb.push(exportBtn);
    toolbar.replaceChildren(...tb);
    countEl.textContent = filtered.length === rows.length ? `${rows.length} строк` : `${filtered.length} из ${rows.length}`;

    // table
    const tbl = h('table', { class: 'aw-t', id });
    const head = h('tr');
    for (const c of cols) {
      const th = h('th', { scope: 'col', class: `aw-a-${c.align}`, style: c.width ? `width:${c.width}` : null });
      const dir = sort && sort.key === c.key ? sort.dir : null;
      th.setAttribute('aria-sort', dir === 'asc' ? 'ascending' : dir === 'desc' ? 'descending' : 'none');
      if (c.sortable) {
        const b = h('button', { type: 'button', class: 'aw-th-btn', 'data-aw-key': `s:${c.key}`, title: 'Сортировать' }, c.label, h('span', { class: 'aw-sort', 'aria-hidden': 'true' }, dir === 'asc' ? '▲' : dir === 'desc' ? '▼' : ''));
        b.addEventListener('click', () => { sort = nextSort(sort, c.key); page = 1; render(); });
        th.append(b);
      } else th.append(c.label);
      head.append(th);
    }
    const thead = h('thead', null, head);
    if (opts.filters !== false && cols.some(c => c.filter)) thead.append(h('tr', { class: 'aw-filters' }, ...cols.map(c => { let cell = filterCells.get(c.key); if (!cell) { cell = filterCell(c); filterCells.set(c.key, cell); } return cell; })));
    tbl.append(thead);

    const tbody = h('tbody');
    for (const it of lay.items) {
      if (it.kind === 'group') {
        const g = it.group;
        const tr = h('tr', { class: 'aw-group' });
        const gcol = cols.find(c => c.key === groupKey)!;
        const firstAgg = cols.findIndex((c, i) => i > 0 && c.agg && c.key in g.aggs);
        const span = firstAgg > 0 ? firstAgg : cols.length;
        const c0 = cols[0];
        const label = h('td', { colspan: span });
        const btn = h('button', { type: 'button', class: 'aw-group-btn', 'data-aw-key': `g:${g.key}`, 'aria-expanded': String(!it.collapsed) },
          h('span', { class: 'aw-caret', 'aria-hidden': 'true' }, it.collapsed ? '▸' : '▾'), ` ${gcol.label}: ${g.label} `, h('span', { class: 'aw-muted' }, `(${g.rows.length})`));
        btn.addEventListener('click', () => { if (collapsed.has(g.key)) collapsed.delete(g.key); else collapsed.add(g.key); render(); });
        label.append(btn);
        if (c0.agg && c0.key in g.aggs) label.append(h('span', { class: 'aw-agg' }, ' · ' + aggText(c0, g.aggs[c0.key])));
        tr.append(label);
        for (let i = span; i < cols.length; i++) {
          const c = cols[i];
          tr.append(h('td', { class: `aw-a-${c.align}` }, c.agg && c.key in g.aggs ? h('span', { class: 'aw-agg' }, aggText(c, g.aggs[c.key])) : null));
        }
        tbody.append(tr);
      } else {
        const r = it.row;
        const hl = opts.highlight?.(r);
        const tr = h('tr', { class: 'aw-tr' + (hl ? ` aw-hl aw-hl-${hl === true ? 'accent' : hl}` : '') + (selected === r ? ' aw-selected' : '') });
        for (const c of cols) tr.append(h('td', { class: `aw-a-${c.align}` }, displayText(r, c, fo())));
        tr.addEventListener('click', () => {
          selected = selected === r ? null : r;
          tbody.querySelectorAll('.aw-selected').forEach(x => x.classList.remove('aw-selected'));
          if (selected) tr.classList.add('aw-selected');
          if (selected) opts.onRowClick?.(r);
        });
        tbody.append(tr);
      }
    }
    if (!lay.items.length) tbody.append(h('tr', null, h('td', { colspan: cols.length || 1, class: 'aw-empty' }, opts.emptyText || (rows.length ? 'Нет строк, подходящих под фильтры' : 'Нет данных'))));
    tbl.append(tbody);

    if (opts.totals && cols.some(c => c.agg)) {
      const tr = h('tr', { class: 'aw-total' });
      const t = (c: Column) => c.agg ? aggText(c, aggregate(filtered, c, c.agg)) : '';
      cols.forEach((c, i) => tr.append(h('td', { class: `aw-a-${c.align}` }, i === 0 ? h('strong', null, 'Итого' + (c.agg ? ' · ' + t(c) : '')) : t(c))));
      tbl.append(h('tfoot', null, tr));
    }
    scroll.replaceChildren(tbl);

    // pager
    const sizes = [...(opts.pageSizes && opts.pageSizes.length ? opts.pageSizes : PAGE_SIZES)];
    if (!sizes.includes(pageSize)) sizes.push(pageSize);
    const sel = h('select', { class: 'aw-input aw-fi', 'data-aw-key': 'pg:size', 'aria-label': 'Строк на странице' }, ...sizes.sort((a, b) => a - b).map(n => h('option', { value: n }, String(n))));
    sel.value = String(pageSize);
    sel.addEventListener('change', () => { pageSize = +sel.value; page = 1; render(); });
    const go = (p: number) => { page = p; render(); };
    const first = (page - 1) * pageSize + 1;
    const prev = h('button', { type: 'button', class: 'aw-btn', 'data-aw-key': 'pg:prev', 'aria-label': 'Предыдущая страница', disabled: page <= 1 }, '‹');
    const next = h('button', { type: 'button', class: 'aw-btn', 'data-aw-key': 'pg:next', 'aria-label': 'Следующая страница', disabled: page >= lay.pages }, '›');
    prev.addEventListener('click', () => go(page - 1));
    next.addEventListener('click', () => go(page + 1));
    pager.replaceChildren(
      h('span', { class: 'aw-muted' }, lay.total ? `Строки ${first}–${Math.min(lay.total, first + pageSize - 1)} из ${lay.total}` : 'Нет строк'),
      h('span', { class: 'aw-spacer' }),
      h('label', { class: 'aw-muted aw-inline' }, 'На странице ', sel),
      prev, h('span', { class: 'aw-muted' }, `${page} / ${lay.pages}`), next,
    );
    pager.hidden = lay.total <= Math.min(...sizes) && lay.pages === 1;
  }

  render();

  return {
    el: root,
    update(patch) {
      const dataChanged = 'rows' in patch || 'source' in patch;
      const colsChanged = 'columns' in patch;
      opts = { ...opts, ...patch };
      if (dataChanged || colsChanged) { rowsChanged = true; if (dataChanged) selected = null; }
      if (patch.sortBy !== undefined) sort = patch.sortBy;
      if (patch.pageSize) pageSize = patch.pageSize;
      if (dataChanged || 'rowFilter' in patch || 'groupBy' in patch) page = 1;
      render();
    },
    getRows: () => filtered.slice(),
    setSearch(text) { searchInput.value = text; filters.search = text; page = 1; render(); },
    exportCSV,
    destroy() { root.remove(); },
  };
}
