export type CatalogSort = 'recent' | 'name' | 'count';
export type CatalogKind = 'all' | 'personal' | 'team';
export type CatalogQuery = { q: string; sort: CatalogSort; kind: CatalogKind; space: string };

export const DEFAULT_CATALOG_QUERY: CatalogQuery = { q: '', sort: 'recent', kind: 'all', space: '' };
const CATALOG_KEYS = ['q', 'sort', 'kind', 'space'] as const;
const MAX_LEN = 200;

/** Reads catalog state from a query string; unknown or malformed values fall back to the defaults. */
export function parseCatalogQuery(search: string): CatalogQuery {
  const params = new URLSearchParams(search);
  const sort = params.get('sort');
  const kind = params.get('kind');
  const q = params.get('q') || '';
  const space = params.get('space') || '';
  return {
    q: q.length <= MAX_LEN ? q : q.slice(0, MAX_LEN),
    sort: sort === 'name' || sort === 'count' ? sort : 'recent',
    kind: kind === 'personal' || kind === 'team' ? kind : 'all',
    space: space.length <= MAX_LEN ? space : '',
  };
}

/** Returns the query string (without "?") for `base` with the catalog parameters replaced; defaults are omitted and foreign parameters are kept. */
export function catalogQueryString(state: CatalogQuery, base: URLSearchParams): string {
  const params = new URLSearchParams(base);
  CATALOG_KEYS.forEach(key => params.delete(key));
  if (state.q.trim()) params.set('q', state.q.slice(0, MAX_LEN));
  if (state.sort === 'name' || state.sort === 'count') params.set('sort', state.sort);
  if (state.kind === 'personal' || state.kind === 'team') params.set('kind', state.kind);
  if (state.space) params.set('space', state.space);
  return params.toString();
}
