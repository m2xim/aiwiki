import React from 'react';
import { Search } from 'lucide-react';
import type { Article, SearchHit, SearchResponse, Space } from './api';

const matchNames: Record<string, string> = { title: 'заголовок', tags: 'теги', summary: 'описание', properties: 'свойства', body: 'текст' };
type Props = {
  response: SearchResponse | null; results: SearchHit[]; loading: boolean; loadingMore: boolean; global: boolean;
  spaces: Space[]; currentSpace: string; articleById: Map<string, Article>; selectedId: string;
  onOpen: (id: string) => void; onMore: () => void; onReset: () => void;
};

/** Path of a result as grey text: parents only, middle shortened. Unknown outside the loaded space. */
function pathOf(hit: SearchHit, articleById: Map<string, Article>, currentSpace: string): string {
  if (hit.space_id !== currentSpace) return '';
  const titles: string[] = []; const seen = new Set<string>([hit.id]);
  let parent = hit.parent_id;
  while (parent && !seen.has(parent)) { seen.add(parent); const a = articleById.get(parent); if (!a) break; titles.unshift(a.title || 'Без названия'); parent = a.parent_id; }
  if (titles.length > 2) return `${titles[0]} › … › ${titles[titles.length - 1]}`;
  return titles.join(' › ');
}

export function SearchResultsList({ response, results, loading, loadingMore, global, spaces, currentSpace, articleById, selectedId, onOpen, onMore, onReset }: Props) {
  const names = new Map(spaces.map(s => [s.id, s.name]));
  if (!response && loading) return <div className="loading-line" role="status">Ищем…</div>;
  return <div className="search-results" aria-busy={loading}>
    {loading && <div className="loading-line search-loading" role="status">Ищем…</div>}
    {response?.mode === 'any' && results.length > 0 && <div className="search-mode-note">Полных совпадений нет — показаны статьи с частью слов</div>}
    {!loading && response && results.length === 0 && <div className="empty-tree"><Search size={18}/><span>Ничего не найдено. Измените запрос или фильтры.</span><button onClick={onReset}>Сбросить</button></div>}
    {results.map(hit => {
      const path = pathOf(hit, articleById, currentSpace);
      const matches = (hit.matched_in || []).filter(m => matchNames[m]);
      return <button key={hit.id} className={`search-hit ${selectedId === hit.id ? 'selected' : ''}`} onClick={() => onOpen(hit.id)}>
        <span className="search-hit-title">{hit.title || 'Без названия'}</span>
        {(global || hit.space_id !== currentSpace) && <span className="space-tag">{names.get(hit.space_id) || hit.space_id}</span>}
        {path && <span className="search-hit-path">{path}</span>}
        {hit.snippet && <span className="search-hit-snippet">{hit.snippet}</span>}
        {matches.length > 0 && <span className="search-hit-badges">{matches.map(m => <i key={m}>{matchNames[m]}</i>)}</span>}
      </button>;
    })}
    {response?.next_offset !== undefined && <button className="button ghost small search-more" onClick={onMore} disabled={loadingMore}>{loadingMore ? 'Загружаем…' : `Показать ещё${response.total > results.length ? ` (${response.total - results.length})` : ''}`}</button>}
  </div>;
}
