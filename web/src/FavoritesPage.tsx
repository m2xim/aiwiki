import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ArrowRight, Search, Star } from 'lucide-react';
import type { Favorite, Space } from './api';
import { DEFAULT_FAVORITES_QUERY, FavoritesQuery, FavoritesSort, favoritesQueryString, filterFavorites, parseFavoritesQuery, relativeDayRu } from './catalogQuery';
import { replaceUrl } from './nav';
import './SpacesDirectory.css';
import './FavoritesPage.css';

const fullDate = (value: string) => { const date = new Date(value); return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat('ru-RU', { dateStyle: 'full', timeStyle: 'short' }).format(date); };
const shortDate = (value: string) => { const date = new Date(value); return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium' }).format(date); };

type Props = {
  favorites: Favorite[]; spaces: Space[]; kindNames: Record<string, string>; pending: string[];
  onOpen: (id: string) => void; onRemove: (id: string) => void; onReload: () => void;
};

/** «Избранное»: starred materials of every space as cards; filters live in the URL like on the spaces page. */
export function FavoritesPage({ favorites, spaces, kindNames, pending, onOpen, onRemove, onReload }: Props) {
  const [state, setState] = useState<FavoritesQuery>(() => parseFavoritesQuery(location.search));
  useEffect(() => { onReload(); }, []); // the list may be stale after stars were toggled elsewhere

  useEffect(() => {
    const onPop = () => { if (new URLSearchParams(location.search).get('view') === 'favorites') setState(parseFavoritesQuery(location.search)); };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  const flushRef = useRef<(() => void) | null>(null);
  const lastQ = useRef(state.q);
  useEffect(() => {
    const typed = lastQ.current !== state.q; lastQ.current = state.q;
    const write = () => {
      const params = new URLSearchParams(location.search);
      if (params.get('view') !== 'favorites') return;
      const query = favoritesQueryString(state, params);
      const url = `${location.pathname}${query ? `?${query}` : ''}${location.hash}`;
      if (url !== `${location.pathname}${location.search}${location.hash}`) replaceUrl(url);
    };
    if (!typed) { flushRef.current = null; write(); return; }
    const timer = window.setTimeout(() => { flushRef.current = null; write(); }, 300);
    flushRef.current = () => { window.clearTimeout(timer); flushRef.current = null; write(); };
    return () => window.clearTimeout(timer);
  }, [state]);

  const open = useCallback((id: string) => { flushRef.current?.(); onOpen(id); }, [onOpen]);
  const update = useCallback((patch: Partial<FavoritesQuery>) => setState(value => ({ ...value, ...patch })), []);
  const names = useMemo(() => new Map(spaces.map(s => [s.id, s.name])), [spaces]);
  const shown = useMemo(() => filterFavorites(favorites, state, names), [favorites, state, names]);
  const spaceOptions = useMemo(() => Array.from(new Set(favorites.map(f => f.space_id))).map(id => ({ id, name: names.get(id) || id })).sort((a, b) => a.name.localeCompare(b.name, 'ru')), [favorites, names]);
  const kindOptions = useMemo(() => Array.from(new Set(favorites.map(f => f.kind).filter(Boolean))), [favorites]);
  const filtered = state.q.trim() !== '' || state.space !== '' || state.kind !== '';
  const total = favorites.length;

  return <section className="spaces-dir favorites-page">
    <div className="spaces-dir-top"><span className="eyebrow">ИЗБРАННОЕ</span></div>
    <div className="spaces-dir-title"><h1>Избранное</h1>{total > 0 && <span className="spaces-dir-count" aria-live="polite">{shown.length} из {total}</span>}</div>
    <p className="spaces-dir-note">Материалы, которые вы отметили звёздочкой, из всех пространств.</p>
    {total === 0 ? <div className="spaces-dir-state"><Star size={26}/><strong>В избранном пока пусто</strong><p className="favorites-hint">Откройте статью и нажмите звёздочку в её заголовке — материал появится здесь.</p></div> : <>
      <div className="spaces-dir-filters favorites-filters">
        <label className="spaces-dir-search"><Search size={15}/><input type="search" value={state.q} onChange={e => update({ q: e.target.value })} placeholder="Название, описание, пространство" aria-label="Поиск по избранному"/></label>
        <label><span>Пространство</span><select value={state.space} onChange={e => update({ space: e.target.value })}><option value="">Все</option>{spaceOptions.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select></label>
        <label><span>Тип</span><select value={state.kind} onChange={e => update({ kind: e.target.value })}><option value="">Все</option>{kindOptions.map(k => <option key={k} value={k}>{kindNames[k] || k}</option>)}</select></label>
        <label><span>Сортировка</span><select value={state.sort} onChange={e => update({ sort: e.target.value as FavoritesSort })}><option value="added">Недавно добавленные</option><option value="changed">Недавно изменённые</option><option value="title">По названию</option></select></label>
      </div>
      {shown.length === 0 ? <div className="spaces-dir-state"><strong>Ничего не найдено</strong>{filtered && <button className="button ghost" onClick={() => setState(DEFAULT_FAVORITES_QUERY)}>Сбросить фильтры</button>}</div>
        : <ul className="spaces-dir-list">{shown.map(item => {
          const updated = item.updated_at; const relative = relativeDayRu(updated);
          const title = item.title || 'Без названия';
          return <li key={item.id} className="spaces-dir-card">
            <div className="spaces-dir-open">
              <span className="spaces-dir-head">
                <strong className="spaces-dir-name">{title}</strong>
                <span className="spaces-dir-badge" title="Пространство">{names.get(item.space_id) || item.space_id}</span>
                {item.kind && <span className="spaces-dir-badge is-role">{kindNames[item.kind] || item.kind}</span>}
              </span>
              {item.summary ? <span className="spaces-dir-desc" title={item.summary}>{item.summary}</span> : <span className="spaces-dir-desc is-empty">Без описания</span>}
              <span className="spaces-dir-meta">
                {updated && fullDate(updated) ? <span title={fullDate(updated)}>Изменено {relative || shortDate(updated)}</span> : <span>Нет изменений</span>}
                {item.favorited_at && fullDate(item.favorited_at) && <span title={fullDate(item.favorited_at)}>В избранном с {shortDate(item.favorited_at)}</span>}
              </span>
              <span className="spaces-dir-actions">
                <button className="icon-button spaces-dir-edit favorites-unstar" disabled={pending.includes(item.id)} onClick={() => onRemove(item.id)} title="Убрать из избранного" aria-label={`Убрать из избранного: ${title}`}><Star size={15} fill="currentColor"/></button>
                <button className="button ghost small spaces-dir-go" onClick={() => open(item.id)} aria-label={`Открыть: ${title}`}>Открыть<ArrowRight size={14}/></button>
              </span>
            </div>
          </li>;
        })}</ul>}
    </>}
  </section>;
}
