import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FolderPlus, LayoutGrid, RotateCw, Search, User, Users } from 'lucide-react';
import { CatalogSpace, client } from './api';
import { CatalogSort, DEFAULT_SPACES_QUERY, SpacesAccess, SpacesQuery, filterSpaces, parseSpacesQuery, pluralRu, relativeDayRu, spacesQueryString } from './catalogQuery';
import { replaceUrl } from './nav';
import './SpacesDirectory.css';

const ROLE_LABELS: Record<string, string> = { owner: 'Владелец', manager: 'Управление', editor: 'Запись', commenter: 'Комментарии', reader: 'Чтение' };
const MAX_TAGS = 5;
const fullDate = (value: string) => { const date = new Date(value); return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat('ru-RU', { dateStyle: 'full', timeStyle: 'short' }).format(date); };
const shortDate = (value: string) => { const date = new Date(value); return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium' }).format(date); };

type Props = { trusted: boolean; currentSpaceId: string; onOpenSpace: (id: string) => void; onCreate: () => void };

export function SpacesDirectory({ trusted, currentSpaceId, onOpenSpace, onCreate }: Props) {
  const [spaces, setSpaces] = useState<CatalogSpace[] | null>(null);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);
  const [state, setState] = useState<SpacesQuery>(() => parseSpacesQuery(location.search));
  const request = useRef(0);

  useEffect(() => {
    const current = ++request.current;
    setError('');
    client.catalog().then(list => { if (current === request.current) setSpaces(list || []); })
      .catch(e => { if (current === request.current) setError((e as Error).message || 'Неизвестная ошибка'); });
  }, [reload]);

  // Filters live in the URL: Back / Forward re-read them.
  useEffect(() => {
    const onPop = () => { if (new URLSearchParams(location.search).get('view') === 'spaces') setState(parseSpacesQuery(location.search)); };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  const flushRef = useRef<(() => void) | null>(null);
  const lastQ = useRef(state.q);
  useEffect(() => {
    const typed = lastQ.current !== state.q; lastQ.current = state.q;
    const write = () => {
      const params = new URLSearchParams(location.search);
      if (params.get('view') !== 'spaces') return;
      const query = spacesQueryString(state, params);
      const url = `${location.pathname}${query ? `?${query}` : ''}${location.hash}`;
      if (url !== `${location.pathname}${location.search}${location.hash}`) replaceUrl(url);
    };
    if (!typed) { flushRef.current = null; write(); return; }
    const timer = window.setTimeout(() => { flushRef.current = null; write(); }, 300);
    flushRef.current = () => { window.clearTimeout(timer); flushRef.current = null; write(); };
    return () => window.clearTimeout(timer);
  }, [state]);

  // A click inside the debounce window must still leave the typed filter in this history entry, so Back restores it.
  const openSpace = useCallback((id: string) => { flushRef.current?.(); onOpenSpace(id); }, [onOpenSpace]);
  const update = useCallback((patch: Partial<SpacesQuery>) => setState(value => ({ ...value, ...patch })), []);
  const shown = useMemo(() => spaces ? filterSpaces(spaces, state) : [], [spaces, state]);
  const filtered = state.q.trim() !== '' || state.kind !== 'all' || state.access !== 'any';
  const total = spaces?.length || 0;

  return <section className="spaces-dir">
    <div className="spaces-dir-top"><span className="eyebrow">ПРОСТРАНСТВА</span></div>
    <div className="spaces-dir-title"><h1>Все пространства</h1>{spaces && total > 0 && <span className="spaces-dir-count" aria-live="polite">{shown.length} из {total}</span>}</div>
    <p className="spaces-dir-note">Пространства, к которым у вас есть доступ.</p>
    {error && !spaces ? <div className="spaces-dir-state spaces-dir-error" role="alert"><p>Не удалось загрузить пространства: {error}</p><button className="button ghost" onClick={() => setReload(v => v + 1)}><RotateCw size={15}/>Повторить</button></div>
      : !spaces ? <div className="spaces-dir-state">Загружаем пространства…</div>
      : total === 0 ? <div className="spaces-dir-state"><LayoutGrid size={26}/><strong>Нет доступных пространств</strong><button className="button primary" onClick={onCreate}><FolderPlus size={16}/>Создать личное пространство</button></div>
      : <>
        <div className="spaces-dir-filters">
          <label className="spaces-dir-search"><Search size={15}/><input type="search" value={state.q} onChange={e => update({ q: e.target.value })} placeholder="Название, описание, теги, проекты" aria-label="Поиск по пространствам"/></label>
          <label><span>Тип</span><select value={state.kind} onChange={e => update({ kind: e.target.value as SpacesQuery['kind'] })}><option value="all">Все</option><option value="personal">Личные</option><option value="team">Командные</option></select></label>
          <label><span>Доступ</span><select value={state.access} onChange={e => update({ access: e.target.value as SpacesAccess })}><option value="any">Любой</option><option value="write">Могу редактировать</option><option value="read">Только чтение</option></select></label>
          <label><span>Сортировка</span><select value={state.sort} onChange={e => update({ sort: e.target.value as CatalogSort })}><option value="recent">Недавно изменённые</option><option value="name">По названию</option><option value="count">По числу документов</option></select></label>
        </div>
        {error && <div className="spaces-dir-more-error" role="alert">Не удалось обновить список: {error} <button onClick={() => setReload(v => v + 1)}>Повторить</button></div>}
        {shown.length === 0 ? <div className="spaces-dir-state"><strong>Ничего не найдено</strong>{filtered && <button className="button ghost" onClick={() => setState(DEFAULT_SPACES_QUERY)}>Сбросить фильтры</button>}</div>
          : <ul className="spaces-dir-list">{shown.map(space => <SpaceCard key={space.id} space={space} trusted={trusted} current={space.id === currentSpaceId} onOpen={openSpace}/>)}</ul>}
      </>}
  </section>;
}

function SpaceCard({ space, trusted, current, onOpen }: { space: CatalogSpace; trusted: boolean; current: boolean; onOpen: (id: string) => void }) {
  const description = (space.description || '').trim();
  const count = space.article_count || 0;
  const projects = space.projects?.length || 0;
  const tags = space.tags || [];
  const updated = space.last_updated || '';
  const relative = relativeDayRu(updated);
  const role = trusted ? 'Полный доступ' : ROLE_LABELS[space.effective_role || ''] || '';
  return <li className={`spaces-dir-card${current ? ' is-current' : ''}`}>
    <button className="spaces-dir-open" onClick={() => onOpen(space.id)} aria-current={current ? 'true' : undefined}>
      <span className="spaces-dir-head">
        <strong className="spaces-dir-name">{space.name}</strong>
        {current && <span className="spaces-dir-badge is-current">Выбрано</span>}
        {!trusted && <span className="spaces-dir-badge">{space.personal ? <User size={12}/> : <Users size={12}/>}{space.personal ? 'Личное' : 'Командное'}</span>}
        {role && <span className="spaces-dir-badge is-role" title="Ваш доступ">{role}</span>}
      </span>
      {description ? <span className="spaces-dir-desc" title={description}>{description}</span> : <span className="spaces-dir-desc is-empty">Без описания</span>}
      {space.limited && <span className="spaces-dir-limited">Доступ только к части проектов</span>}
      <span className="spaces-dir-meta">
        <span>{count} {pluralRu(count, 'документ', 'документа', 'документов')}</span>
        {projects > 0 && <span>{projects} {pluralRu(projects, 'проект', 'проекта', 'проектов')}</span>}
        {updated && fullDate(updated) ? <span title={fullDate(updated)}>Изменено {relative || shortDate(updated)}</span> : <span>Нет изменений</span>}
      </span>
      {tags.length > 0 && <span className="spaces-dir-tags">{tags.slice(0, MAX_TAGS).map(tag => <span key={tag} className="spaces-dir-tag">{tag}</span>)}{tags.length > MAX_TAGS && <span className="spaces-dir-tag is-more" title={tags.slice(MAX_TAGS).join(', ')}>+{tags.length - MAX_TAGS}</span>}</span>}
    </button>
  </li>;
}
