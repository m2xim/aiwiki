export type CatalogSort = 'recent' | 'name' | 'count';
export type CatalogKind = 'all' | 'personal' | 'team';
export type CatalogQuery = { q: string; sort: CatalogSort; kind: CatalogKind; cspace: string };

export const DEFAULT_CATALOG_QUERY: CatalogQuery = { q: '', sort: 'recent', kind: 'all', cspace: '' };
const CATALOG_KEYS = ['q', 'sort', 'kind', 'cspace'] as const;
const MAX_LEN = 200;

/** Reads catalog state from a query string; unknown or malformed values fall back to the defaults. */
export function parseCatalogQuery(search: string): CatalogQuery {
  const params = new URLSearchParams(search);
  const sort = params.get('sort');
  const kind = params.get('kind');
  const q = params.get('q') || '';
  const cspace = params.get('cspace') || '';
  return {
    q: q.length <= MAX_LEN ? q : q.slice(0, MAX_LEN),
    sort: sort === 'name' || sort === 'count' ? sort : 'recent',
    kind: kind === 'personal' || kind === 'team' ? kind : 'all',
    cspace: cspace.length <= MAX_LEN ? cspace : '',
  };
}

/** Returns the query string (without "?") for `base` with the catalog parameters replaced; defaults are omitted and foreign parameters are kept. */
export function catalogQueryString(state: CatalogQuery, base: URLSearchParams): string {
  const params = new URLSearchParams(base);
  CATALOG_KEYS.forEach(key => params.delete(key));
  if (state.q.trim()) params.set('q', state.q.slice(0, MAX_LEN));
  if (state.sort === 'name' || state.sort === 'count') params.set('sort', state.sort);
  if (state.kind === 'personal' || state.kind === 'team') params.set('kind', state.kind);
  if (state.cspace) params.set('cspace', state.cspace);
  return params.toString();
}

// ---- "Все пространства" page (?view=spaces) ----

export type SpacesAccess = 'any' | 'write' | 'read';
export type SpacesQuery = { q: string; kind: CatalogKind; access: SpacesAccess; sort: CatalogSort };
export const DEFAULT_SPACES_QUERY: SpacesQuery = { q: '', kind: 'all', access: 'any', sort: 'recent' };
const SPACES_KEYS = ['q', 'kind', 'access', 'sort'] as const;

/** Reads the directory filters from a query string; unknown or malformed values fall back to the defaults. */
export function parseSpacesQuery(search: string): SpacesQuery {
  const params = new URLSearchParams(search);
  const kind = params.get('kind');
  const access = params.get('access');
  const sort = params.get('sort');
  const q = params.get('q') || '';
  return {
    q: q.length <= MAX_LEN ? q : q.slice(0, MAX_LEN),
    kind: kind === 'personal' || kind === 'team' ? kind : 'all',
    access: access === 'write' || access === 'read' ? access : 'any',
    sort: sort === 'name' || sort === 'count' ? sort : 'recent',
  };
}

/** Returns the query string (without "?") for `base` with the directory parameters replaced; defaults are omitted and foreign parameters are kept. */
export function spacesQueryString(state: SpacesQuery, base: URLSearchParams): string {
  const params = new URLSearchParams(base);
  SPACES_KEYS.forEach(key => params.delete(key));
  if (state.q.trim()) params.set('q', state.q.slice(0, MAX_LEN));
  if (state.kind === 'personal' || state.kind === 'team') params.set('kind', state.kind);
  if (state.access === 'write' || state.access === 'read') params.set('access', state.access);
  if (state.sort === 'name' || state.sort === 'count') params.set('sort', state.sort);
  return params.toString();
}

export type DirectorySpace = {
  id: string; name: string; description?: string; personal?: boolean; article_count?: number; last_updated?: string; tags?: string[];
  effective_role?: string; projects?: { name: string }[];
};

const WRITE_ROLES = ['owner', 'manager', 'editor'];
const READ_ROLES = ['reader', 'commenter'];

/** Filters and sorts spaces for the directory; the input is not mutated. */
export function filterSpaces<T extends DirectorySpace>(spaces: T[], state: SpacesQuery): T[] {
  const needle = state.q.trim().toLocaleLowerCase('ru');
  const result = spaces.filter(space => {
    if (state.kind === 'personal' && !space.personal) return false;
    if (state.kind === 'team' && space.personal) return false;
    if (state.access === 'write' && !WRITE_ROLES.includes(space.effective_role || '')) return false;
    if (state.access === 'read' && !READ_ROLES.includes(space.effective_role || '')) return false;
    if (!needle) return true;
    const haystack = [space.name, space.description || '', ...(space.tags || []), ...(space.projects || []).map(p => p.name)].join('\n').toLocaleLowerCase('ru');
    return haystack.includes(needle);
  });
  const byName = (a: T, b: T) => a.name.localeCompare(b.name, 'ru');
  if (state.sort === 'name') result.sort(byName);
  else if (state.sort === 'count') result.sort((a, b) => (b.article_count || 0) - (a.article_count || 0) || byName(a, b));
  else result.sort((a, b) => (b.last_updated || '').localeCompare(a.last_updated || '') || byName(a, b));
  return result;
}

/** Russian plural form: one = 1, 21...; few = 2-4, 22...; many = 0, 5-20, 25.... */
export function pluralRu(n: number, one: string, few: string, many: string): string {
  const abs = Math.abs(n) % 100;
  const last = abs % 10;
  if (abs > 10 && abs < 20) return many;
  if (last === 1) return one;
  if (last >= 2 && last <= 4) return few;
  return many;
}

/** "сегодня", "вчера", "3 дня назад" for recent dates, empty string beyond 30 days or for invalid input. */
export function relativeDayRu(value: string, now: Date = new Date()): string {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return '';
  const day = (d: Date) => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  const diff = Math.round((day(now) - day(date)) / 86400000);
  if (diff < 0) return '';
  if (diff === 0) return 'сегодня';
  if (diff === 1) return 'вчера';
  if (diff <= 30) return `${diff} ${pluralRu(diff, 'день', 'дня', 'дней')} назад`;
  return '';
}

// ---- "Избранное" page (?view=favorites) ----

export type FavoritesSort = 'added' | 'changed' | 'title';
export type FavoritesQuery = { q: string; space: string; kind: string; sort: FavoritesSort };
export const DEFAULT_FAVORITES_QUERY: FavoritesQuery = { q: '', space: '', kind: '', sort: 'added' };
// The space filter lives in "fspace": plain "space" is the app's own route parameter.
const FAVORITES_KEYS = ['q', 'fspace', 'kind', 'sort'] as const;

/** Reads the favourites filters from a query string; unknown or malformed values fall back to the defaults. */
export function parseFavoritesQuery(search: string): FavoritesQuery {
  const params = new URLSearchParams(search);
  const sort = params.get('sort');
  const q = params.get('q') || '';
  const space = params.get('fspace') || '';
  const kind = params.get('kind') || '';
  return {
    q: q.length <= MAX_LEN ? q : q.slice(0, MAX_LEN),
    space: space.length <= MAX_LEN ? space : '',
    kind: kind.length <= 50 ? kind : '',
    sort: sort === 'changed' || sort === 'title' ? sort : 'added',
  };
}

/** Returns the query string (without "?") for `base` with the favourites parameters replaced; defaults are omitted and foreign parameters are kept. */
export function favoritesQueryString(state: FavoritesQuery, base: URLSearchParams): string {
  const params = new URLSearchParams(base);
  FAVORITES_KEYS.forEach(key => params.delete(key));
  if (state.q.trim()) params.set('q', state.q.slice(0, MAX_LEN));
  if (state.space) params.set('fspace', state.space);
  if (state.kind) params.set('kind', state.kind);
  if (state.sort === 'changed' || state.sort === 'title') params.set('sort', state.sort);
  return params.toString();
}

export type DirectoryFavorite = { id: string; title: string; space_id: string; kind: string; summary: string; updated_at: string; favorited_at: string };

/** Filters and sorts favourites; the input is not mutated. `spaceNames` lets the text search match the space name too. */
export function filterFavorites<T extends DirectoryFavorite>(items: T[], state: FavoritesQuery, spaceNames: Map<string, string> = new Map()): T[] {
  const needle = state.q.trim().toLocaleLowerCase('ru');
  const result = items.filter(item => {
    if (state.space && item.space_id !== state.space) return false;
    if (state.kind && item.kind !== state.kind) return false;
    if (!needle) return true;
    return [item.title, item.summary || '', spaceNames.get(item.space_id) || ''].join('\n').toLocaleLowerCase('ru').includes(needle);
  });
  const byTitle = (a: T, b: T) => (a.title || '').localeCompare(b.title || '', 'ru');
  if (state.sort === 'title') result.sort(byTitle);
  else if (state.sort === 'changed') result.sort((a, b) => (b.updated_at || '').localeCompare(a.updated_at || '') || byTitle(a, b));
  else result.sort((a, b) => (b.favorited_at || '').localeCompare(a.favorited_at || '') || byTitle(a, b));
  return result;
}
