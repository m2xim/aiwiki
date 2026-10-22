import assert from 'node:assert/strict';
import { parseCatalogQuery, catalogQueryString, DEFAULT_CATALOG_QUERY, parseSpacesQuery, spacesQueryString, DEFAULT_SPACES_QUERY, filterSpaces, pluralRu, relativeDayRu } from '../web/src/catalogQuery.ts';

const base = new URLSearchParams('view=admin&tab=catalog');

// Defaults are not written.
assert.equal(catalogQueryString(DEFAULT_CATALOG_QUERY, base), 'view=admin&tab=catalog');
assert.deepEqual(parseCatalogQuery(''), DEFAULT_CATALOG_QUERY);
assert.deepEqual(parseCatalogQuery('?view=admin&tab=catalog'), DEFAULT_CATALOG_QUERY);

// Garbage is ignored.
assert.deepEqual(parseCatalogQuery('?sort=bogus&kind=everything&q=&cspace='), DEFAULT_CATALOG_QUERY);
assert.equal(parseCatalogQuery('?sort=NAME').sort, 'recent');
assert.equal(parseCatalogQuery('?q=' + 'x'.repeat(500)).q.length, 200);
assert.equal(parseCatalogQuery('?cspace=' + 'y'.repeat(500)).cspace, '');

// Valid values parse.
assert.deepEqual(parseCatalogQuery('?sort=count&kind=team&q=abc&cspace=s1'), { q: 'abc', sort: 'count', kind: 'team', cspace: 's1' });
assert.equal(parseCatalogQuery('q=a+b').q, 'a b');

// Round trip with Cyrillic, spaces and reserved characters.
for (const q of ['Привет мир', 'a b  c', 'тест&x=1', '100% #док', 'а=б?в']) {
  const state = { q, sort: 'name', kind: 'personal', cspace: 'пр-1' };
  const qs = catalogQueryString(state, base);
  assert.deepEqual(parseCatalogQuery('?' + qs), state, q);
  const again = new URLSearchParams(qs);
  assert.equal(again.get('view'), 'admin');
  assert.equal(again.get('tab'), 'catalog');
  assert.equal([...again.keys()].filter(k => k === 'q').length, 1);
}

// Only non-default values are written; stale catalog params are replaced; foreign params survive.
const dirty = new URLSearchParams('view=admin&tab=catalog&q=old&sort=name&kind=team&cspace=old&foo=bar&foo=baz');
assert.equal(catalogQueryString(DEFAULT_CATALOG_QUERY, dirty), 'view=admin&tab=catalog&foo=bar&foo=baz');
const partial = new URLSearchParams(catalogQueryString({ ...DEFAULT_CATALOG_QUERY, sort: 'count' }, dirty));
assert.equal(partial.get('sort'), 'count');
assert.equal(partial.has('q'), false);
assert.equal(partial.has('kind'), false);
assert.equal(partial.has('cspace'), false);
assert.deepEqual(partial.getAll('foo'), ['bar', 'baz']);
assert.equal(new URLSearchParams(catalogQueryString({ ...DEFAULT_CATALOG_QUERY, q: '   ' }, base)).has('q'), false);

// A foreign `space` (main interface workspace) is neither read nor removed.
assert.equal(parseCatalogQuery('?space=X').cspace, '');
assert.deepEqual(parseCatalogQuery('?space=X'), DEFAULT_CATALOG_QUERY);
assert.equal(new URLSearchParams(catalogQueryString(DEFAULT_CATALOG_QUERY, new URLSearchParams('view=admin&space=X'))).get('space'), 'X');

// The base is not mutated.
assert.equal(dirty.get('q'), 'old');

// ---- Spaces directory ----
const sbase = new URLSearchParams('view=spaces');
assert.equal(spacesQueryString(DEFAULT_SPACES_QUERY, sbase), 'view=spaces');
assert.deepEqual(parseSpacesQuery(''), DEFAULT_SPACES_QUERY);
assert.deepEqual(parseSpacesQuery('?view=spaces&sort=bogus&kind=x&access=y&q='), DEFAULT_SPACES_QUERY);
assert.deepEqual(parseSpacesQuery('?q=abc&kind=team&access=read&sort=count'), { q: 'abc', kind: 'team', access: 'read', sort: 'count' });
assert.equal(parseSpacesQuery('?q=' + 'x'.repeat(500)).q.length, 200);
for (const q of ['Привет мир', 'тест&x=1', 'а=б?в']) {
  const state = { q, kind: 'personal', access: 'write', sort: 'name' };
  const qs = spacesQueryString(state, sbase);
  assert.deepEqual(parseSpacesQuery('?' + qs), state, q);
  assert.equal(new URLSearchParams(qs).get('view'), 'spaces');
}
assert.equal(spacesQueryString({ ...DEFAULT_SPACES_QUERY, access: 'read' }, new URLSearchParams('view=spaces&q=old&sort=name&foo=1')), 'view=spaces&foo=1&access=read');

const dir = [
  { id: 'a', name: 'Бета', description: 'Документы отдела', personal: false, article_count: 5, last_updated: '2026-03-01T00:00:00Z', tags: ['hr'], effective_role: 'editor', projects: [{ name: 'Онбординг' }] },
  { id: 'b', name: 'Альфа', description: '', personal: true, article_count: 9, last_updated: '2026-05-01T00:00:00Z', tags: [], effective_role: 'owner', projects: [] },
  { id: 'c', name: 'Гамма', personal: false, article_count: 0, tags: [], effective_role: 'reader' },
  { id: 'd', name: 'Дельта', personal: false, article_count: 5, last_updated: '2026-04-01T00:00:00Z', tags: ['Финансы'], effective_role: 'commenter' },
];
const ids = (state) => filterSpaces(dir, { ...DEFAULT_SPACES_QUERY, ...state }).map(s => s.id).join('');
assert.equal(ids({}), 'bdac');
assert.equal(ids({ sort: 'name' }), 'bacd');
assert.equal(ids({ sort: 'count' }), 'badc');
assert.equal(ids({ kind: 'personal' }), 'b');
assert.equal(ids({ kind: 'team' }), 'dac');
assert.equal(ids({ access: 'write' }), 'ba');
assert.equal(ids({ access: 'read' }), 'dc');
assert.equal(ids({ q: 'онбординг' }), 'a');
assert.equal(ids({ q: 'ФИНАНСЫ' }), 'd');
assert.equal(ids({ q: 'отдела' }), 'a');
assert.equal(ids({ q: 'альфа', kind: 'team' }), '');
assert.equal(dir[0].id, 'a'); // input is not mutated

assert.deepEqual([1, 2, 4, 5, 11, 12, 21, 22, 25, 101, 111, 0].map(n => pluralRu(n, 'документ', 'документа', 'документов')),
  ['документ', 'документа', 'документа', 'документов', 'документов', 'документов', 'документ', 'документа', 'документов', 'документ', 'документов', 'документов']);
const now = new Date(2026, 5, 15, 12, 0, 0);
assert.equal(relativeDayRu(new Date(2026, 5, 15, 1).toISOString(), now), 'сегодня');
assert.equal(relativeDayRu(new Date(2026, 5, 14, 23).toISOString(), now), 'вчера');
assert.equal(relativeDayRu(new Date(2026, 5, 12, 9).toISOString(), now), '3 дня назад');
assert.equal(relativeDayRu(new Date(2026, 5, 10, 9).toISOString(), now), '5 дней назад');
assert.equal(relativeDayRu(new Date(2026, 4, 1).toISOString(), now), '');
assert.equal(relativeDayRu('', now), '');
assert.equal(relativeDayRu('garbage', now), '');
assert.equal(relativeDayRu(new Date(2026, 5, 20).toISOString(), now), '');
console.log('Catalog query checks passed: defaults omitted, garbage ignored, Cyrillic/space round trip, foreign params kept.');
