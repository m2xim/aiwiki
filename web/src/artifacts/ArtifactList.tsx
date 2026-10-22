import { useEffect, useRef, useState } from 'react';
import { Box, Files, Lock, Plus, RefreshCw, Search, Users } from 'lucide-react';
import { client } from '../api';
import type { ArtifactSummary, Project } from '../api';
import { visibilityLabel } from '../artifactsCore';

const formatDate = (value?: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : '';
export const filesLabel = (count: number) => { const n = Math.abs(count) % 100; const m = n % 10; return `${count} ${n > 10 && n < 20 ? 'файлов' : m === 1 ? 'файл' : m >= 2 && m <= 4 ? 'файла' : 'файлов'}`; };

type Props = {
  spaceId: string; spaceName: string; q: string; project: string;
  onFilters: (q: string, project: string) => void;
  onOpen: (id: string) => void;
  onCreate: () => void;
  reloadKey: number;
};

export function ArtifactList({ spaceId, spaceName, q, project, onFilters, onOpen, onCreate, reloadKey }: Props) {
  const [items, setItems] = useState<ArtifactSummary[] | null>(null);
  const [projects, setProjects] = useState<Project[]>([]);
  const [error, setError] = useState('');
  const [text, setText] = useState(q);
  const [reload, setReload] = useState(0);
  const request = useRef(0);
  useEffect(() => { setText(q); }, [q]);
  // The query goes into the URL after a short pause.
  useEffect(() => {
    if (text.trim() === q.trim()) return;
    const timer = window.setTimeout(() => onFilters(text.trim(), project), 350);
    return () => window.clearTimeout(timer);
  }, [text]);
  useEffect(() => { if (!spaceId) return; let live = true; client.projects(spaceId).then(list => { if (live) setProjects(list); }).catch(() => { if (live) setProjects([]); }); return () => { live = false; }; }, [spaceId]);
  useEffect(() => {
    if (!spaceId) { setItems([]); return; }
    const current = ++request.current;
    setError('');
    client.artifacts({ space_id: spaceId, project_id: project, q }).then(list => { if (current === request.current) setItems(list); })
      .catch(e => { if (current === request.current) { setError((e as Error).message); setItems([]); } });
  }, [spaceId, project, q, reload, reloadKey]);

  const filtered = Boolean(q || project);
  return <section className="art-page art-list">
    <div className="art-top"><span className="eyebrow">АРТЕФАКТЫ</span></div>
    <div className="art-title-row"><div><h1>Артефакты</h1><p className="art-muted">Интерактивные страницы пространства «{spaceName}»: таблицы, дашборды, мини-приложения.{items ? ` Всего: ${items.length}.` : ''}</p></div>
      <button type="button" className="button primary" onClick={onCreate} disabled={!spaceId}><Plus size={16}/>Создать артефакт</button></div>
    <form className="art-filters" role="search" onSubmit={event => { event.preventDefault(); onFilters(text.trim(), project); }}>
      <div className="art-search"><Search size={15} aria-hidden="true"/><input type="search" value={text} onChange={event => setText(event.target.value)} placeholder="Поиск по названию, описанию и содержимому" aria-label="Поиск артефактов"/></div>
      <select value={project} onChange={event => onFilters(text.trim(), event.target.value)} aria-label="Фильтр по проекту"><option value="">Все проекты</option>{projects.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
    </form>
    {error && <div className="art-alert error" role="alert">Не удалось загрузить список: {error} <button type="button" className="button ghost small" onClick={() => setReload(v => v + 1)}><RefreshCw size={13}/>Повторить</button></div>}
    {items === null ? <p className="art-muted" role="status">Загружаем…</p> : !items.length && !error ? (filtered
      ? <div className="art-empty"><Search size={22} aria-hidden="true"/><p>Ничего не найдено.</p><button type="button" className="button" onClick={() => { setText(''); onFilters('', ''); }}>Сбросить фильтры</button></div>
      : <div className="art-empty"><Box size={26} aria-hidden="true"/><h2>Здесь пока нет артефактов</h2>
        <p>Артефакт — это маленькая живая страница внутри вики. Например:</p>
        <ul><li>таблица с фильтрами и сортировкой по данным команды;</li><li>дашборд с графиком и ползунком для сценариев «что если»;</li><li>калькулятор, чек-лист или мини-игра для обучения.</li></ul>
        <button type="button" className="button primary" onClick={onCreate} disabled={!spaceId}><Plus size={16}/>Создать первый артефакт</button></div>)
      : <ul className="art-cards">{items.map(item => <li key={item.id}>
        <a className="art-card" href={`/?artifact=${encodeURIComponent(item.id)}`} onClick={event => { if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); onOpen(item.id); }}>
          <span className="art-card-title"><Box size={16} aria-hidden="true"/>{item.title || 'Без названия'}</span>
          {item.description && <span className="art-card-desc">{item.description}</span>}
          <span className="art-card-meta">
            <span className={`art-badge ${item.visibility === 'private' ? 'private' : ''}`}>{item.visibility === 'private' ? <Lock size={12} aria-hidden="true"/> : <Users size={12} aria-hidden="true"/>}{visibilityLabel(item.visibility)}</span>
            {(item.owner_name || item.author) && <span>{item.owner_name || item.author}</span>}
            <time dateTime={item.updated_at}>{formatDate(item.updated_at)}</time>
            <span><Files size={12} aria-hidden="true"/> {filesLabel(item.file_count)}</span>
          </span>
        </a></li>)}</ul>}
  </section>;
}
