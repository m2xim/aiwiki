import assert from 'node:assert/strict';
import { parseCatalogQuery, catalogQueryString, DEFAULT_CATALOG_QUERY } from '../web/src/catalogQuery.ts';

const base = new URLSearchParams('view=admin&tab=catalog');

// Defaults are not written.
assert.equal(catalogQueryString(DEFAULT_CATALOG_QUERY, base), 'view=admin&tab=catalog');
assert.deepEqual(parseCatalogQuery(''), DEFAULT_CATALOG_QUERY);
assert.deepEqual(parseCatalogQuery('?view=admin&tab=catalog'), DEFAULT_CATALOG_QUERY);

// Garbage is ignored.
assert.deepEqual(parseCatalogQuery('?sort=bogus&kind=everything&q=&space='), DEFAULT_CATALOG_QUERY);
assert.equal(parseCatalogQuery('?sort=NAME').sort, 'recent');
assert.equal(parseCatalogQuery('?q=' + 'x'.repeat(500)).q.length, 200);
assert.equal(parseCatalogQuery('?space=' + 'y'.repeat(500)).space, '');

// Valid values parse.
assert.deepEqual(parseCatalogQuery('?sort=count&kind=team&q=abc&space=s1'), { q: 'abc', sort: 'count', kind: 'team', space: 's1' });
assert.equal(parseCatalogQuery('q=a+b').q, 'a b');

// Round trip with Cyrillic, spaces and reserved characters.
for (const q of ['Привет мир', 'a b  c', 'тест&x=1', '100% #док', 'а=б?в']) {
  const state = { q, sort: 'name', kind: 'personal', space: 'пр-1' };
  const qs = catalogQueryString(state, base);
  assert.deepEqual(parseCatalogQuery('?' + qs), state, q);
  const again = new URLSearchParams(qs);
  assert.equal(again.get('view'), 'admin');
  assert.equal(again.get('tab'), 'catalog');
  assert.equal([...again.keys()].filter(k => k === 'q').length, 1);
}

// Only non-default values are written; stale catalog params are replaced; foreign params survive.
const dirty = new URLSearchParams('view=admin&tab=catalog&q=old&sort=name&kind=team&space=old&foo=bar&foo=baz');
assert.equal(catalogQueryString(DEFAULT_CATALOG_QUERY, dirty), 'view=admin&tab=catalog&foo=bar&foo=baz');
const partial = new URLSearchParams(catalogQueryString({ ...DEFAULT_CATALOG_QUERY, sort: 'count' }, dirty));
assert.equal(partial.get('sort'), 'count');
assert.equal(partial.has('q'), false);
assert.equal(partial.has('kind'), false);
assert.equal(partial.has('space'), false);
assert.deepEqual(partial.getAll('foo'), ['bar', 'baz']);
assert.equal(new URLSearchParams(catalogQueryString({ ...DEFAULT_CATALOG_QUERY, q: '   ' }, base)).has('q'), false);

// The base is not mutated.
assert.equal(dirty.get('q'), 'old');
console.log('Catalog query checks passed: defaults omitted, garbage ignored, Cyrillic/space round trip, foreign params kept.');
