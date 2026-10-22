// QA for the AI Wiki artifact kit: pure logic (format, CSV, table sort/filter/group/aggregate/paging, state)
// imported straight from TypeScript, plus checks of the built web/dist/artifact-runtime files.
// Run after `cd web && npm run build`.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import { formatSigned, formatValue, isEmpty, toDate, toNumber } from '../web/artifact-kit/src/core/format.ts';
import { parseCSV, parseCSVMatrix, parseData, toCSV, detectDelimiter } from '../web/artifact-kit/src/core/csv.ts';
import {
  aggregate, cellValue, filterRows, groupRows, inferType, layoutPage, nextSort, normalizeColumns, sortRows, exportMatrix, displayText,
} from '../web/artifact-kit/src/core/tablelogic.ts';
import { effect, state } from '../web/artifact-kit/src/core/state.ts';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
let checks = 0;
const ok = (name, fn) => { try { fn(); checks++; } catch (e) { e.message = `${name}: ${e.message}`; throw e; } };
const nbsp = /[  ]/g;
const norm = (s) => s.replace(nbsp, ' ');

// ---- numbers ----
ok('toNumber', () => {
  assert.equal(toNumber('1 234,5'), 1234.5);
  assert.equal(toNumber('1 234,5'), 1234.5);
  assert.equal(toNumber('12.5'), 12.5);
  assert.equal(toNumber('−3'), -3);
  assert.equal(toNumber(' 42 '), 42);
  assert.equal(toNumber('1e3'), 1000);
  assert.equal(toNumber(''), null);
  assert.equal(toNumber(null), null);
  assert.equal(toNumber('abc'), null);
  assert.equal(toNumber('12abc'), null);
  assert.equal(toNumber('007'), null, 'identifier with leading zeros is not a number');
  assert.equal(toNumber('0,5'), 0.5);
  assert.equal(toNumber('0'), 0);
  assert.equal(toNumber(NaN), null);
  assert.equal(toNumber(Infinity), null);
  assert.equal(toNumber(true), null);
});
ok('isEmpty', () => { assert.ok(isEmpty('')); assert.ok(isEmpty('  ')); assert.ok(isEmpty(null)); assert.ok(isEmpty(undefined)); assert.ok(isEmpty(NaN)); assert.ok(!isEmpty(0)); assert.ok(!isEmpty(false)); });

// ---- dates ----
ok('toDate', () => {
  const t = toDate('2026-10-22');
  assert.equal(new Date(t).getFullYear(), 2026); assert.equal(new Date(t).getMonth(), 9); assert.equal(new Date(t).getDate(), 22);
  assert.equal(toDate('22.10.2026'), t, 'dd.mm.yyyy equals ISO');
  assert.equal(new Date(toDate('2026-10-22T13:45:10')).getHours(), 13);
  assert.equal(toDate('2026-02-31'), null, 'invalid calendar day');
  assert.equal(toDate('2026-13-01'), null);
  assert.equal(toDate('Москва'), null);
  assert.equal(toDate(''), null);
  assert.equal(toDate(new Date(2026, 0, 5)), new Date(2026, 0, 5).getTime());
  assert.equal(toDate(new Date('x')), null);
  assert.equal(toDate('2026-10-22T10:00:00Z'), Date.UTC(2026, 9, 22, 10));
});

// ---- formatting ----
ok('formatValue', () => {
  assert.equal(norm(formatValue(1234.5, 'number')), '1 234,5');
  assert.equal(norm(formatValue('1 234,567', 'number')), '1 234,57');
  assert.equal(norm(formatValue(1234.5, 'integer')), '1 235');
  assert.equal(norm(formatValue(0.256, 'percent')), '25,6 %');
  assert.equal(norm(formatValue(0.25, 'percent', { digits: 0 })), '25 %');
  assert.equal(norm(formatValue(1234.5, 'currency')), '1 234,50 ₽');
  assert.equal(norm(formatValue(1234.5, 'currency', { currency: 'USD' })).replace('US$', '$'), '1 234,50 $');
  assert.equal(norm(formatValue(1000, 'currency', { digits: 0 })), '1 000 ₽');
  assert.equal(formatValue('2026-10-22', 'date'), '22.10.2026');
  assert.equal(formatValue('22.10.2026 13:05', 'datetime'), '22.10.2026, 13:05');
  assert.equal(formatValue(null, 'number'), '');
  assert.equal(formatValue('', 'date'), '');
  assert.equal(formatValue('нет данных', 'number'), 'нет данных', 'unparsable stays as text');
  assert.equal(formatValue('x', 'date'), 'x');
  assert.equal(formatValue(true), 'Да');
  assert.equal(formatValue(false, 'text'), 'Нет');
  assert.equal(formatValue(5, (v) => `<${v}>`), '<5>');
  assert.equal(formatValue('Привет'), 'Привет');
  assert.equal(norm(formatSigned(0.123)), '+12,3 %');
  assert.equal(norm(formatSigned(-0.05)), '−5 %');
  assert.equal(formatSigned(0, 'number'), '0');
});

// ---- CSV ----
ok('parseCSV basics', () => {
  const r = parseCSV('Город,Население\nМосква,"12,6"\nСанкт-Петербург,5.6\n');
  assert.deepEqual(r.columns, ['Город', 'Население']);
  assert.equal(r.rows.length, 2);
  assert.equal(r.rows[0]['Население'], '12,6');
});
ok('parseCSV quotes and newlines', () => {
  const r = parseCSV('a,b\r\n"x ""y"" z","line1\nline2"\r\n"",end');
  assert.equal(r.rows[0].a, 'x "y" z');
  assert.equal(r.rows[0].b, 'line1\nline2');
  assert.equal(r.rows[1].a, '');
  assert.equal(r.rows[1].b, 'end');
  assert.equal(r.rows.length, 2);
});
ok('parseCSV delimiters, BOM, empty cells', () => {
  assert.equal(detectDelimiter('a;b;c\n1;2;3'), ';');
  assert.equal(detectDelimiter('a\tb\n1\t2'), '\t');
  assert.equal(detectDelimiter('"a;b",c\n1,2'), ',', 'delimiter inside quotes is ignored');
  const r = parseCSV('﻿имя;сумма\nАня;\n;5');
  assert.deepEqual(r.columns, ['имя', 'сумма']);
  assert.deepEqual(r.rows, [{ 'имя': 'Аня', 'сумма': '' }, { 'имя': '', 'сумма': '5' }]);
});
ok('parseCSV headers and short rows', () => {
  const r = parseCSV('a,,a\n1\n1,2,3,4');
  assert.deepEqual(r.columns, ['a', 'Колонка 2', 'a (2)']);
  assert.deepEqual(r.rows[0], { a: '1', 'Колонка 2': '', 'a (2)': '' });
  assert.deepEqual(parseCSV('').rows, []);
  assert.deepEqual(parseCSV('\n\n').rows, []);
  assert.equal(parseCSV('a\n1\n\n\n').rows.length, 1, 'trailing blank lines dropped');
  assert.deepEqual(parseCSVMatrix('a,b\n1,2'), [['a', 'b'], ['1', '2']]);
  assert.deepEqual(parseCSVMatrix('"a"b,c'), [['ab', 'c']].map(r => r), 'junk after a closing quote is kept');
});
ok('toCSV roundtrip', () => {
  const m = [['Имя', 'Заметка'], ['Аня', 'a,b'], ['Боря', 'с "кавычками"'], ['Вера', 'две\nстроки'], ['Глеб', null], ['Дима', ' пробел'], [1.5, new Date(Date.UTC(2026, 0, 1))]];
  for (const d of [',', ';']) {
    const text = toCSV(m, d);
    const back = parseCSVMatrix(text, d);
    assert.equal(back[1][1], 'a,b'); assert.equal(back[2][1], 'с "кавычками"'); assert.equal(back[3][1], 'две\nстроки');
    assert.equal(back[4][1], ''); assert.equal(back[5][1], ' пробел'); assert.equal(back[6][0], '1.5');
  }
  assert.ok(toCSV([['a;b']], ';').startsWith('"'));
  assert.ok(!toCSV([['a;b']], ',').startsWith('"'));
});
ok('parseData', () => {
  assert.deepEqual(parseData('[{"a":1}]').rows, [{ a: 1 }]);
  assert.deepEqual(parseData('{"columns":["a"],"rows":[{"a":1}]}').columns, ['a']);
  assert.equal(parseData('a,b\n1,2').rows[0].b, '2');
  assert.throws(() => parseData('{"x":1}'));
  assert.throws(() => parseData('[1,'));
});

// ---- table logic ----
const data = [
  { город: 'Москва', регион: 'Центр', население: '12 600', дата: '2026-03-01', рост: 0.05, активен: true },
  { город: 'Санкт-Петербург', регион: 'Северо-Запад', население: '5600', дата: '2026-01-15', рост: -0.01, активен: true },
  { город: 'Екатеринбург', регион: 'Урал', население: '1 500,5', дата: '2026-02-10', рост: 0.02, активен: false },
  { город: 'Ёлки', регион: 'Урал', население: '', дата: '', рост: null, активен: false },
  { город: 'Архангельск', регион: 'Северо-Запад', население: '300', дата: '10.12.2025', рост: '0,03', активен: true },
  { город: 'абакан', регион: 'Сибирь', население: 'n/a', дата: 'не знаю', рост: 0, активен: false },
];
const cols = normalizeColumns([
  'город', { key: 'регион', filter: 'select' }, { key: 'население', type: 'number', label: 'Население', format: 'integer', agg: 'sum' },
  { key: 'дата' }, { key: 'рост', format: 'percent', agg: 'avg' }, { key: 'активен', agg: 'count' },
], data);
const byKey = Object.fromEntries(cols.map(c => [c.key, c]));
const names = (rows) => rows.map(r => r.город);

ok('inferType', () => {
  assert.equal(inferType(data, 'население'), 'string', 'a column with n/a is a string');
  assert.equal(inferType([{ x: '1' }, { x: '2,5' }, { x: '' }], 'x'), 'number');
  assert.equal(inferType([{ x: '2026-01-01' }, { x: '05.02.2026' }], 'x'), 'date');
  assert.equal(inferType([{ x: true }, { x: false }], 'x'), 'boolean');
  assert.equal(inferType([{ x: '007' }, { x: '008' }], 'x'), 'string');
  assert.equal(inferType([], 'x'), 'string');
  assert.equal(byKey.рост.type, 'number', 'numbers, null and "0,03" together are numeric');
});

ok('normalizeColumns', () => {
  assert.equal(byKey.население.align, 'right');
  assert.equal(byKey.дата.type, 'string', 'one unparsable date makes the column a string unless typed');
  const auto = normalizeColumns(undefined, [{ a: 1, b: 'x' }, { a: 2, c: 3 }]);
  assert.deepEqual(auto.map(c => c.key), ['a', 'b', 'c']);
  assert.equal(normalizeColumns(undefined, [], ['p', 'q']).length, 2);
  assert.equal(normalizeColumns(['a'], [{ a: 1 }])[0].filter, 'range');
  const many = Array.from({ length: 30 }, (_, i) => ({ k: i % 3 ? 'да' : 'нет', t: 'уникальный ' + i }));
  assert.equal(normalizeColumns(['k', 't'], many)[0].filter, 'select');
  assert.equal(normalizeColumns(['k', 't'], many)[1].filter, 'text');
});

ok('cellValue', () => {
  assert.equal(cellValue(data[0], byKey.население), 12600);
  assert.equal(cellValue(data[3], byKey.население), null);
  assert.equal(cellValue(data[5], byKey.население), null, 'n/a is not a number');
  assert.equal(cellValue(data[4], byKey.рост), 0.03);
  assert.equal(cellValue({ d: '10.12.2025' }, { key: 'd', type: 'date' }), toDate('2025-12-10'));
  assert.equal(cellValue({ b: 'Да' }, { key: 'b', type: 'boolean' }), true);
  assert.equal(cellValue({ b: 'нет' }, { key: 'b', type: 'boolean' }), false);
  assert.equal(cellValue({ b: '??' }, { key: 'b', type: 'boolean' }), null);
});

ok('sort strings (Russian locale, case-insensitive, ё)', () => {
  const asc = names(sortRows(data, cols, { key: 'город', dir: 'asc' }));
  assert.deepEqual(asc, ['абакан', 'Архангельск', 'Екатеринбург', 'Ёлки', 'Москва', 'Санкт-Петербург']);
  const desc = names(sortRows(data, cols, { key: 'город', dir: 'desc' }));
  assert.deepEqual(desc, [...asc].reverse());
});
ok('sort numbers: numeric order, empties last in both directions', () => {
  assert.deepEqual(names(sortRows(data, cols, { key: 'население', dir: 'asc' })).slice(0, 4), ['Архангельск', 'Екатеринбург', 'Санкт-Петербург', 'Москва']);
  const d = names(sortRows(data, cols, { key: 'население', dir: 'desc' }));
  assert.deepEqual(d.slice(0, 4), ['Москва', 'Санкт-Петербург', 'Екатеринбург', 'Архангельск']);
  assert.deepEqual(d.slice(4).sort(), ['Ёлки', 'абакан'].sort(), 'empty and n/a stay at the end');
});
ok('sort dates and stability', () => {
  const dcols = normalizeColumns([{ key: 'дата', type: 'date' }, 'город'], data);
  const asc = names(sortRows(data, dcols, { key: 'дата', dir: 'asc' }));
  assert.deepEqual(asc.slice(0, 4), ['Архангельск', 'Санкт-Петербург', 'Екатеринбург', 'Москва']);
  const ties = [{ k: 1, n: 'a' }, { k: 1, n: 'b' }, { k: 0, n: 'c' }, { k: 1, n: 'd' }];
  const tc = normalizeColumns(['k', 'n'], ties);
  assert.deepEqual(sortRows(ties, tc, { key: 'k', dir: 'asc' }).map(r => r.n), ['c', 'a', 'b', 'd']);
  assert.deepEqual(sortRows(ties, tc, { key: 'k', dir: 'desc' }).map(r => r.n), ['a', 'b', 'd', 'c']);
  assert.deepEqual(sortRows(ties, tc, null).map(r => r.n), ['a', 'b', 'c', 'd']);
  assert.deepEqual(sortRows(ties, tc, { key: 'нет', dir: 'asc' }).map(r => r.n), ['a', 'b', 'c', 'd']);
  const nat = [{ s: 'файл10' }, { s: 'файл2' }, { s: 'файл1' }];
  assert.deepEqual(sortRows(nat, normalizeColumns(['s'], nat), { key: 's', dir: 'asc' }).map(r => r.s), ['файл1', 'файл2', 'файл10']);
});
ok('sort does not mutate input', () => {
  const before = JSON.stringify(data);
  sortRows(data, cols, { key: 'город', dir: 'desc' });
  assert.equal(JSON.stringify(data), before);
});
ok('nextSort cycle', () => {
  let s = nextSort(null, 'a'); assert.deepEqual(s, { key: 'a', dir: 'asc' });
  s = nextSort(s, 'a'); assert.deepEqual(s, { key: 'a', dir: 'desc' });
  assert.equal(nextSort(s, 'a'), null);
  assert.deepEqual(nextSort({ key: 'a', dir: 'desc' }, 'b'), { key: 'b', dir: 'asc' });
});

ok('filter: global search (Cyrillic, case, formatted values, AND of words)', () => {
  assert.deepEqual(names(filterRows(data, cols, { search: 'МОСКВА' })), ['Москва']);
  assert.deepEqual(names(filterRows(data, cols, { search: 'ёлки' })), ['Ёлки']);
  assert.deepEqual(names(filterRows(data, cols, { search: 'урал' })), ['Екатеринбург', 'Ёлки']);
  assert.deepEqual(names(filterRows(data, cols, { search: 'урал ёл' })), ['Ёлки'], 'all words must match');
  assert.equal(filterRows(data, cols, { search: '12600' }).length, 1, 'raw value is searchable');
  assert.equal(filterRows(data, cols, { search: '   ' }).length, data.length);
  assert.equal(filterRows(data, cols, { search: 'нетТакого' }).length, 0);
  const cache = new WeakMap();
  assert.equal(filterRows(data, cols, { search: 'север' }, {}, cache).length, 2);
  assert.equal(filterRows(data, cols, { search: 'сибирь' }, {}, cache).length, 1, 'cache reuse is safe');
});
ok('filter: column text/select/range', () => {
  assert.deepEqual(names(filterRows(data, cols, { columns: { город: { text: 'ск' } } })), ['Москва', 'Архангельск']);
  assert.deepEqual(names(filterRows(data, cols, { columns: { регион: { values: ['Урал'] } } })), ['Екатеринбург', 'Ёлки']);
  assert.equal(filterRows(data, cols, { columns: { регион: { values: [] } } }).length, 6, 'empty selection = all');
  assert.deepEqual(names(filterRows(data, cols, { columns: { население: { min: 1000 } } })), ['Москва', 'Санкт-Петербург', 'Екатеринбург']);
  assert.deepEqual(names(filterRows(data, cols, { columns: { население: { min: 300, max: 1500.5 } } })), ['Екатеринбург', 'Архангельск']);
  assert.deepEqual(names(filterRows(data, cols, { columns: { население: { max: 0 } } })), [], 'empty values never match a range');
  assert.equal(filterRows(data, cols, { columns: { население: { min: null, max: null } } }).length, 6);
  const dcols = normalizeColumns([{ key: 'дата', type: 'date' }, 'город'], data);
  assert.deepEqual(names(filterRows(data, dcols, { columns: { дата: { min: toDate('2026-01-31'), max: toDate('2026-12-31') } } })), ['Москва', 'Екатеринбург']);
  assert.deepEqual(names(filterRows(data, cols, { search: 'урал', columns: { город: { text: 'ёл' } } })), ['Ёлки'], 'filters combine');
  assert.equal(filterRows(data, cols, { columns: { нет: { text: 'x' } } }).length, 6, 'unknown column is ignored');
  assert.deepEqual(names(filterRows(data, cols, { columns: { активен: { values: ['true'] } } })), ['Москва', 'Санкт-Петербург', 'Архангельск']);
});

ok('aggregate', () => {
  assert.equal(aggregate(data, byKey.население, 'sum'), 12600 + 5600 + 1500.5 + 300);
  assert.equal(aggregate(data, byKey.население, 'count'), 4);
  assert.equal(aggregate(data, byKey.население, 'min'), 300);
  assert.equal(aggregate(data, byKey.население, 'max'), 12600);
  assert.equal(aggregate(data, byKey.население, 'avg'), (12600 + 5600 + 1500.5 + 300) / 4);
  assert.equal(aggregate([], byKey.население, 'sum'), null);
  assert.equal(aggregate([{ население: '' }], byKey.население, 'avg'), null);
  assert.equal(aggregate(data, byKey.город, 'count'), 6);
  assert.ok(Math.abs(aggregate(data, byKey.рост, 'sum') - 0.09) < 1e-12);
  assert.equal(aggregate([{ v: 0 }, { v: 0 }], { key: 'v', type: 'number' }, 'min'), 0);
});

ok('group', () => {
  const g = groupRows(data, cols, 'регион');
  assert.deepEqual(g.map(x => x.label), ['Северо-Запад', 'Сибирь', 'Урал', 'Центр']);
  assert.deepEqual(g.map(x => x.rows.length), [2, 1, 2, 1]);
  assert.equal(g[0].aggs.население, 5600 + 300);
  assert.equal(g[2].aggs.население, 1500.5);
  assert.equal(g[1].aggs.население, null, 'group with no numeric values');
  assert.equal(g[2].aggs.активен, 2);
  assert.deepEqual(names(g[2].rows), ['Екатеринбург', 'Ёлки'], 'row order inside the group is preserved');
  const withEmpty = groupRows([{ к: 'б' }, { к: '' }, { к: 'а' }, { к: null }], normalizeColumns(['к'], [{ к: 'a' }]), 'к');
  assert.deepEqual(withEmpty.map(x => x.label), ['а', 'б', '(пусто)']);
  assert.equal(withEmpty[2].rows.length, 2);
  const one = groupRows(data, cols, 'нет-такой');
  assert.equal(one.length, 1); assert.equal(one[0].rows.length, 6);
  assert.deepEqual(groupRows([], cols, 'регион'), []);
  const numGroups = groupRows([{ n: '10' }, { n: '9' }, { n: '10' }], normalizeColumns([{ key: 'n', type: 'number' }], []), 'n');
  assert.deepEqual(numGroups.map(x => x.value), [9, 10], 'numeric groups sort numerically');
});

ok('pagination without groups', () => {
  const rows = Array.from({ length: 53 }, (_, i) => ({ i }));
  const p1 = layoutPage(null, rows, 1, 25);
  assert.equal(p1.pages, 3); assert.equal(p1.items.length, 25); assert.equal(p1.total, 53);
  assert.equal(layoutPage(null, rows, 3, 25).items.length, 3);
  assert.equal(layoutPage(null, rows, 99, 25).page, 3, 'page clamps to the last');
  assert.equal(layoutPage(null, rows, 0, 25).page, 1);
  assert.deepEqual(layoutPage(null, [], 1, 25), { items: [], total: 0, pages: 1, page: 1 });
  const big = Array.from({ length: 10000 }, (_, i) => ({ i }));
  assert.equal(layoutPage(null, big, 100, 100).items[0].row.i, 9900);
  assert.equal(layoutPage(null, big, 1, 0).items.length, 1, 'page size is at least 1');
});
ok('pagination with groups and collapsing', () => {
  const rows = Array.from({ length: 10 }, (_, i) => ({ g: i < 4 ? 'a' : i < 5 ? 'b' : 'c', i }));
  const gc = normalizeColumns(['g', 'i'], rows);
  const groups = groupRows(rows, gc, 'g');
  const kinds = (l) => l.items.map(x => (x.kind === 'group' ? 'G' + x.group.label + (x.collapsed ? '-' : '') : x.row.i)).join(' ');
  assert.equal(kinds(layoutPage(groups, rows, 1, 3)), 'Ga 0 1 2');
  assert.equal(kinds(layoutPage(groups, rows, 2, 3)), 'Ga 3 Gb 4 Gc 5', 'group headers repeat where a group continues or starts');
  assert.equal(kinds(layoutPage(groups, rows, 4, 3)), 'Gc 9');
  assert.equal(layoutPage(groups, rows, 1, 3).pages, 4);
  const collapsed = new Set([groups[0].key]);
  const cl = layoutPage(groups, rows, 1, 3, collapsed);
  assert.equal(kinds(cl), 'Ga- Gb 4 Gc 5 6', 'collapsed group shows only its header and does not count toward rows');
  assert.equal(cl.total, 6);
  const all = new Set(groups.map(g => g.key));
  const ac = layoutPage(groups, rows, 1, 3, all);
  assert.equal(kinds(ac), 'Ga- Gb- Gc-'); assert.equal(ac.pages, 1);
  assert.equal(kinds(layoutPage([], [], 1, 10)), '');
});

ok('exportMatrix and CSV of filtered data', () => {
  const m = exportMatrix(filterRows(data, cols, { search: 'урал' }), cols);
  assert.deepEqual(m[0], ['город', 'регион', 'Население', 'дата', 'рост', 'активен']);
  assert.equal(m.length, 3);
  const dcols = normalizeColumns(['город', { key: 'дата', type: 'date' }], data);
  assert.equal(exportMatrix([data[0]], dcols)[1][1], '2026-03-01');
  assert.equal(exportMatrix([data[3]], dcols)[1][1], '');
  assert.equal(exportMatrix([{ город: 'x', дата: '2026-03-01T10:20:30' }], dcols)[1][1], '2026-03-01T10:20:30');
  const csv = toCSV(exportMatrix([data[1]], cols), ';');
  assert.deepEqual(parseCSVMatrix(csv, ';')[1].slice(0, 3), ['Санкт-Петербург', 'Северо-Запад', '5600']);
});
ok('displayText', () => {
  assert.equal(norm(displayText(data[0], byKey.население)), '12 600');
  assert.equal(displayText(data[3], byKey.население), '');
  assert.equal(displayText(data[5], byKey.население), 'n/a', 'an unparsable value in a number column is shown as-is');
});

// ---- state / effect ----
await (async () => {
  const s = state({ a: 1, b: 2, list: [1] });
  let runs = 0, seen = null;
  const stop = effect(() => { runs++; seen = s.a * 10; });
  assert.equal(runs, 1); assert.equal(seen, 10);
  s.a = 2; s.a = 3;
  assert.equal(runs, 1, 'updates are batched');
  await Promise.resolve();
  assert.equal(runs, 2); assert.equal(seen, 30);
  s.b = 5; await Promise.resolve();
  assert.equal(runs, 2, 'unrelated key does not retrigger');
  s.a = 3; await Promise.resolve();
  assert.equal(runs, 2, 'same value does not retrigger');
  let dyn = 0;
  const stop2 = effect(() => { dyn++; if (s.a > 100) void s.b; });
  s.b = 6; await Promise.resolve(); assert.equal(dyn, 1, 'dependencies are tracked dynamically');
  s.a = 101; await Promise.resolve(); assert.equal(dyn, 2);
  s.b = 7; await Promise.resolve(); assert.equal(dyn, 3);
  const before = seen; stop(); stop2();
  s.a = 9; await Promise.resolve();
  assert.equal(seen, before, 'stopped effect does not run');
  const calls = [];
  const un = s.$subscribe((k, v) => calls.push([k, v]));
  s.b = 8; un(); s.b = 9;
  assert.deepEqual(calls, [['b', 8]]);
  let ls = 0;
  const stop3 = effect(() => { ls = s.list.length; });
  s.list = [...s.list, 2]; await Promise.resolve(); assert.equal(ls, 2);
  stop3();
  const chain = state({ x: 0, y: 0 });
  const stop4 = effect(() => { chain.y = chain.x * 2; });
  let ysee = -1; const stop5 = effect(() => { ysee = chain.y; });
  chain.x = 4; await Promise.resolve(); await Promise.resolve();
  assert.equal(ysee, 8, 'effects chained through state settle');
  stop4(); stop5();
  assert.deepEqual(Object.keys(chain), ['x', 'y']);
  const errState = state({ v: 0 });
  const origErr = console.error; let logged = 0; console.error = () => { logged++; };
  effect(() => { if (errState.v === 1) throw new Error('boom'); });
  errState.v = 1; await Promise.resolve(); console.error = origErr;
  assert.equal(logged, 1, 'an exception inside an effect is reported, not thrown');
  checks++;
})();

// ---- built files ----
const dist = path.join(root, 'web', 'dist', 'artifact-runtime');
const jsPath = path.join(dist, 'aiwiki-kit.js'), cssPath = path.join(dist, 'aiwiki-kit.css');
if (!fs.existsSync(jsPath) || !fs.existsSync(cssPath)) {
  console.error('Built kit not found in web/dist/artifact-runtime: run `cd web && npm run build` first.');
  process.exit(1);
}
const js = fs.readFileSync(jsPath, 'utf8'), css = fs.readFileSync(cssPath, 'utf8');
ok('bundle: size sanity', () => { assert.ok(js.length > 50000 && js.length < 450000, `aiwiki-kit.js is ${js.length} bytes`); assert.ok(css.length > 3000 && css.length < 60000); });
ok('bundle: no eval / new Function', () => {
  assert.ok(!/\beval\s*\(/.test(js), 'eval( found');
  assert.ok(!/new\s+Function\b/.test(js), 'new Function found');
  assert.ok(!/(?<![\w.$])Function\s*\(/.test(js), 'Function( constructor call found');
  assert.ok(!/\bset(Timeout|Interval)\s*\(\s*["'`]/.test(js), 'string timers');
});
ok('bundle: no network APIs', () => {
  for (const re of [/\bfetch\s*\(/, /XMLHttpRequest/, /\bWebSocket\b/, /EventSource/, /sendBeacon/, /importScripts/, /\bimport\s*\(/, /window\.open\s*\(/, /document\.cookie/, /localStorage|sessionStorage|indexedDB/]) {
    assert.ok(!re.test(js), `forbidden API in bundle: ${re}`);
  }
});
ok('bundle: no external URLs other than license/namespace strings', () => {
  const urls = [...js.matchAll(/https?:\/\/[^\s"'`)]+/g)].map(m => m[0]).filter(u => !/^https?:\/\/(www\.w3\.org|chartjs\.org|www\.chartjs\.org|github\.com)/.test(u));
  assert.deepEqual(urls, []);
});
ok('css: no remote resources', () => {
  assert.ok(!/@import|url\s*\(\s*["']?https?:|url\s*\(\s*["']?\/\//.test(css));
  assert.ok(css.includes('prefers-color-scheme:dark') || css.includes('prefers-color-scheme: dark'));
  assert.ok(css.includes('data-aiwiki-theme'));
});

// Load the IIFE without a DOM: it must define the global and the pure helpers must work.
ok('bundle: loads in a bare context and exposes the API', () => {
  const sandbox = { console, Intl, Date, Math, queueMicrotask, setTimeout, clearTimeout };
  sandbox.window = sandbox; sandbox.self = sandbox; sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(js, sandbox, { filename: 'aiwiki-kit.js' });
  const A = sandbox.AIWiki;
  assert.ok(A, 'global AIWiki');
  for (const k of ['table', 'chart', 'lineChart', 'barChart', 'pieChart', 'state', 'effect', 'bind', 'stat', 'stats', 'gallery', 'lightbox', 'setTheme', 'getTheme', 'format', 'parseCSV', 'toCSV', 'Chart', 'version']) assert.ok(k in A, `AIWiki.${k}`);
  assert.equal(norm(A.format(1234.5, 'currency')), '1 234,50 ₽');
  assert.equal(A.parseCSV('a,b\n"1,5",2').rows[0].a, '1,5');
  assert.equal(typeof A.state({ x: 1 }).x, 'number');
});

console.log(`Kit checks passed: ${checks} check groups (format, CSV, sort, filter, group, aggregate, pagination, export, state, bundle); aiwiki-kit.js ${(js.length / 1024).toFixed(0)} KiB, aiwiki-kit.css ${(css.length / 1024).toFixed(1)} KiB.`);
