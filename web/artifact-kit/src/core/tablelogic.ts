// Pure table logic: column normalisation, sort, filter, group, aggregate, pagination, export matrix. No DOM.
import { DEFAULT_LOCALE, formatValue, isEmpty, toDate, toNumber } from './format.ts';
import type { FormatOptions, FormatSpec } from './format.ts';

export type ColType = 'string' | 'number' | 'date' | 'boolean';
export type AggName = 'count' | 'sum' | 'avg' | 'min' | 'max';
export type FilterKind = 'text' | 'select' | 'range' | false;
export type Row = Record<string, unknown>;

export interface ColumnInput {
  key: string; label?: string; type?: ColType; format?: FormatSpec; currency?: string; digits?: number;
  filter?: FilterKind; agg?: AggName; align?: 'left' | 'right' | 'center'; sortable?: boolean; width?: string;
}
export interface Column {
  key: string; label: string; type: ColType; format: FormatSpec; currency?: string; digits?: number;
  filter: FilterKind; agg?: AggName; align: 'left' | 'right' | 'center'; sortable: boolean; width?: string;
}
export interface ColFilter { text?: string; values?: string[]; min?: number | null; max?: number | null }
export interface Filters { search: string; columns: Record<string, ColFilter> }
export interface SortSpec { key: string; dir: 'asc' | 'desc' }

const collators = new Map<string, Intl.Collator>();
export function collator(locale = DEFAULT_LOCALE): Intl.Collator {
  let c = collators.get(locale);
  if (!c) { c = new Intl.Collator(locale, { numeric: true, sensitivity: 'base' }); collators.set(locale, c); }
  return c;
}

/** Coerce a raw cell to its comparable form for the column type; null for empty/unparsable. */
export function cellValue(row: Row, col: Pick<Column, 'key' | 'type'>): string | number | boolean | null {
  const v = row[col.key];
  if (isEmpty(v)) return null;
  switch (col.type) {
    case 'number': return toNumber(v);
    case 'date': return toDate(v);
    case 'boolean': return typeof v === 'boolean' ? v : /^(true|да|yes|1)$/i.test(String(v).trim()) ? true : /^(false|нет|no|0)$/i.test(String(v).trim()) ? false : null;
    default: return v instanceof Date ? v.toISOString() : String(v);
  }
}

export function inferType(rows: Row[], key: string): ColType {
  let n = 0, num = 0, date = 0, bool = 0;
  for (const r of rows) {
    const v = r[key];
    if (isEmpty(v)) continue;
    if (++n > 200) break;
    if (typeof v === 'boolean') bool++;
    else if (v instanceof Date) date++;
    else if (toNumber(v) !== null) num++;
    else if (typeof v === 'string' && toDate(v) !== null) date++;
  }
  if (!n) return 'string';
  if (bool === n) return 'boolean';
  if (num === n) return 'number';
  if (date === n) return 'date';
  return 'string';
}

export function distinctValues(rows: Row[], col: Pick<Column, 'key' | 'type'>, limit = Infinity): string[] {
  const seen = new Set<string>();
  for (const r of rows) {
    const v = cellValue(r, col);
    if (v === null) continue;
    seen.add(String(v));
    if (seen.size > limit) break;
  }
  return [...seen];
}

/** Columns may be strings, partial definitions, or omitted (then inferred from the first rows' keys). */
export function normalizeColumns(input: (string | ColumnInput)[] | undefined | null, rows: Row[], knownKeys?: string[] | null): Column[] {
  let defs: (string | ColumnInput)[] = input && input.length ? input : [];
  if (!defs.length) {
    const keys: string[] = knownKeys ? [...knownKeys] : [];
    for (const r of rows.slice(0, 50)) for (const k of Object.keys(r)) if (!keys.includes(k)) keys.push(k);
    defs = keys;
  }
  return defs.map((d): Column => {
    const c: ColumnInput = typeof d === 'string' ? { key: d } : d;
    const type = c.type ?? inferType(rows, c.key);
    let filter = c.filter;
    if (filter === undefined) {
      if (type === 'number' || type === 'date') filter = 'range';
      else if (type === 'boolean') filter = 'select';
      else filter = distinctValues(rows, { key: c.key, type }, 12).length <= 12 && rows.length > 12 ? 'select' : 'text';
    }
    return {
      key: c.key, label: c.label ?? c.key, type, filter,
      format: c.format ?? (type === 'date' ? 'date' : type === 'number' ? 'number' : undefined),
      currency: c.currency, digits: c.digits, agg: c.agg,
      align: c.align ?? (type === 'number' ? 'right' : 'left'), sortable: c.sortable !== false, width: c.width,
    };
  });
}

export function displayText(row: Row, col: Column, fo: FormatOptions = {}): string {
  const v = row[col.key];
  if (isEmpty(v)) return '';
  return formatValue(col.type === 'number' ? toNumber(v) ?? v : v, col.format, { ...fo, currency: col.currency ?? fo.currency, digits: col.digits ?? fo.digits });
}

/** Compare two coerced values; empty values always sort last, regardless of direction. */
export function sortRows<T extends Row>(rows: T[], cols: Column[], sort: SortSpec | null, locale = DEFAULT_LOCALE): T[] {
  if (!sort) return rows.slice();
  const col = cols.find(c => c.key === sort.key);
  if (!col) return rows.slice();
  const coll = collator(locale);
  const sign = sort.dir === 'desc' ? -1 : 1;
  const keyed = rows.map((row, i) => ({ row, i, v: cellValue(row, col) }));
  keyed.sort((a, b) => {
    if (a.v === null || b.v === null) return a.v === b.v ? a.i - b.i : a.v === null ? 1 : -1;
    let r: number;
    if (typeof a.v === 'string' && typeof b.v === 'string') r = coll.compare(a.v, b.v);
    else r = a.v < b.v ? -1 : a.v > b.v ? 1 : 0;
    return r === 0 ? a.i - b.i : r * sign;
  });
  return keyed.map(k => k.row);
}

/** Next sort state on header click: asc -> desc -> none. */
export function nextSort(cur: SortSpec | null, key: string): SortSpec | null {
  if (!cur || cur.key !== key) return { key, dir: 'asc' };
  return cur.dir === 'asc' ? { key, dir: 'desc' } : null;
}

export type SearchCache = WeakMap<object, string>;

export function searchText(row: Row, cols: Column[], fo: FormatOptions, cache?: SearchCache): string {
  let s = cache?.get(row);
  if (s === undefined) {
    s = cols.map(c => { const raw = row[c.key]; if (isEmpty(raw)) return ''; const plain = c.type === 'number' ? String(toNumber(raw) ?? '') : ''; return displayText(row, c, fo) + ' ' + (raw instanceof Date ? '' : String(raw)) + ' ' + plain; }).join('\n').toLocaleLowerCase('ru');
    cache?.set(row, s);
  }
  return s;
}

export function filterRows<T extends Row>(rows: T[], cols: Column[], filters: Partial<Filters>, fo: FormatOptions = {}, cache?: SearchCache): T[] {
  const q = (filters.search ?? '').trim().toLocaleLowerCase('ru');
  const active = Object.entries(filters.columns ?? {}).map(([key, f]) => ({ col: cols.find(c => c.key === key), f }))
    .filter((x): x is { col: Column; f: ColFilter } => !!x.col && !!x.f);
  const terms = q ? q.split(/\s+/) : [];
  return rows.filter(row => {
    if (terms.length) {
      const hay = searchText(row, cols, fo, cache);
      for (const t of terms) if (!hay.includes(t)) return false;
    }
    for (const { col, f } of active) {
      if (f.text && f.text.trim()) {
        const hay = (displayText(row, col, fo) + ' ' + String(row[col.key] ?? '')).toLocaleLowerCase('ru');
        if (!hay.includes(f.text.trim().toLocaleLowerCase('ru'))) return false;
      }
      if (f.values && f.values.length) {
        const v = cellValue(row, col);
        if (v === null || !f.values.includes(String(v))) return false;
      }
      if ((f.min !== undefined && f.min !== null) || (f.max !== undefined && f.max !== null)) {
        const v = cellValue(row, col);
        if (typeof v !== 'number') return false;
        if (f.min !== undefined && f.min !== null && v < f.min) return false;
        if (f.max !== undefined && f.max !== null && v > f.max) return false;
      }
    }
    return true;
  });
}

export function aggregate(rows: Row[], col: Pick<Column, 'key' | 'type'>, kind: AggName): number | null {
  if (kind === 'count') return rows.filter(r => cellValue(r, col) !== null).length;
  let n = 0, sum = 0, min = Infinity, max = -Infinity;
  for (const r of rows) {
    const v = toNumber(r[col.key]);
    if (v === null) continue;
    n++; sum += v; if (v < min) min = v; if (v > max) max = v;
  }
  if (!n) return null;
  return kind === 'sum' ? sum : kind === 'avg' ? sum / n : kind === 'min' ? min : max;
}

export interface Group { key: string; value: string | number | boolean | null; label: string; rows: Row[]; aggs: Record<string, number | null> }

/** Group rows (keeping their incoming order inside a group). Groups are ordered by the group column; empty values form the last group "(пусто)". */
export function groupRows(rows: Row[], cols: Column[], groupKey: string, fo: FormatOptions = {}): Group[] {
  const gcol = cols.find(c => c.key === groupKey);
  if (!gcol) return [{ key: '', value: null, label: '', rows, aggs: groupAggs(rows, cols) }];
  const map = new Map<string, Group>();
  for (const r of rows) {
    const v = cellValue(r, gcol);
    const k = v === null ? '\u0000' : String(v);
    let g = map.get(k);
    if (!g) { g = { key: k, value: v, label: v === null ? '(пусто)' : displayText(r, gcol, fo), rows: [], aggs: {} }; map.set(k, g); }
    g.rows.push(r);
  }
  const coll = collator(fo.locale);
  const groups = [...map.values()].sort((a, b) => {
    if (a.value === null || b.value === null) return a.value === b.value ? 0 : a.value === null ? 1 : -1;
    if (typeof a.value === 'string' && typeof b.value === 'string') return coll.compare(a.value, b.value);
    return a.value < b.value ? -1 : a.value > b.value ? 1 : 0;
  });
  for (const g of groups) g.aggs = groupAggs(g.rows, cols);
  return groups;
}

export function groupAggs(rows: Row[], cols: Column[]): Record<string, number | null> {
  const o: Record<string, number | null> = {};
  for (const c of cols) if (c.agg) o[c.key] = aggregate(rows, c, c.agg);
  return o;
}

export type Item = { kind: 'group'; group: Group; collapsed: boolean } | { kind: 'row'; row: Row };

/**
 * Pagination over data rows. Grouped: group headers are inserted for groups that have rows on the page;
 * a collapsed group (its rows hidden and not counted) shows its header on the page where it falls.
 */
export function layoutPage(groups: Group[] | null, rows: Row[], page: number, pageSize: number, collapsed: ReadonlySet<string> = new Set()): { items: Item[]; total: number; pages: number; page: number } {
  const size = Math.max(1, pageSize | 0);
  if (!groups) {
    const pages = Math.max(1, Math.ceil(rows.length / size));
    const p = Math.min(Math.max(1, page | 0), pages);
    return { items: rows.slice((p - 1) * size, p * size).map(row => ({ kind: 'row', row })), total: rows.length, pages, page: p };
  }
  const total = groups.reduce((n, g) => n + (collapsed.has(g.key) ? 0 : g.rows.length), 0);
  const pages = Math.max(1, Math.ceil(total / size));
  const p = Math.min(Math.max(1, page | 0), pages);
  const start = (p - 1) * size, end = start + size;
  const items: Item[] = [];
  let idx = 0;
  for (const g of groups) {
    if (collapsed.has(g.key)) {
      if (idx >= start && (idx < end || p === pages)) items.push({ kind: 'group', group: g, collapsed: true });
      continue;
    }
    const from = Math.max(start, idx), to = Math.min(end, idx + g.rows.length);
    if (from < to) {
      items.push({ kind: 'group', group: g, collapsed: false });
      for (let i = from; i < to; i++) items.push({ kind: 'row', row: g.rows[i - idx] });
    }
    idx += g.rows.length;
  }
  return { items, total, pages, page: p };
}

/** Matrix for CSV export: header = labels, cells = raw (coerced) values; dates as ISO date text. */
export function exportMatrix(rows: Row[], cols: Column[]): unknown[][] {
  const head = cols.map(c => c.label);
  const body = rows.map(r => cols.map(c => {
    const v = cellValue(r, c);
    if (v === null) return '';
    if (c.type === 'date') { const d = new Date(v as number); const p = (x: number) => String(x).padStart(2, '0'); const date = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`; return d.getHours() || d.getMinutes() || d.getSeconds() ? `${date}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}` : date; }
    return v;
  }));
  return [head, ...body];
}
