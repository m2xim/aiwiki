import { FormEvent, useEffect, useRef, useState } from 'react';
import { ArrowUpRight, RefreshCw, RotateCw, Search } from 'lucide-react';
import { AuditEvent, client } from './api';

const pageSize = 50;
const formatDate = (value: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'full', timeStyle: 'short' }).format(new Date(value)) : 'Дата неизвестна';
const changeNames: Record<string, string> = { created: 'создана статья', title: 'заголовок', summary: 'описание', body: 'текст', kind: 'тип', status: 'статус', space_id: 'пространство', parent_id: 'родитель', pinned: 'закрепление', tags: 'теги', properties: 'свойства' };

type Props = { spaceId: string; spaceName: string; onOpenArticle: (id: string) => void; onOpenHistory: (id: string) => void };

export function AuditPage({ spaceId, spaceName, onOpenArticle, onOpenHistory }: Props) {
  const [authorInput, setAuthorInput] = useState('');
  const [author, setAuthor] = useState('');
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [offset, setOffset] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const request = useRef(0);

  useEffect(() => {
    const current = ++request.current;
    let live = true;
    setLoading(true); setError('');
    client.audit(spaceId, pageSize, offset, author).then(page => {
      if (!live || current !== request.current) return;
      setEvents(value => offset === 0 ? page : [...value.slice(0, offset), ...page]);
      setHasMore(page.length === pageSize);
    }).catch(e => { if (live && current === request.current) setError((e as Error).message); })
      .finally(() => { if (live && current === request.current) setLoading(false); });
    return () => { live = false; };
  }, [spaceId, author, offset, reload]);

  const applyAuthor = (event: FormEvent) => {
    event.preventDefault(); setEvents([]); setOffset(0); if (author === authorInput) setReload(value => value + 1); else setAuthor(authorInput);
  };
  const refresh = () => { setEvents([]); setOffset(0); if (offset === 0) setReload(value => value + 1); };

  return <section className="audit-page">
    <div className="audit-page-top"><span className="eyebrow">ПРОСТРАНСТВО</span><button className="button ghost small" onClick={refresh} disabled={loading}><RefreshCw size={14}/>Обновить</button></div>
    <h1>Журнал изменений</h1><p className="audit-page-subtitle">{spaceName}</p>
    <p className="audit-page-note">Журнал содержит изменения статей. Имена авторов вводятся самими авторами.</p>
    <form className="audit-filter" onSubmit={applyAuthor}><label htmlFor="audit-author">Фильтр по автору</label><div><input id="audit-author" value={authorInput} onChange={event => setAuthorInput(event.target.value)} placeholder="Точное имя автора"/><button className="button ghost" type="submit"><Search size={15}/>Применить</button></div>{author&&<button className="audit-clear-filter" type="button" onClick={()=>{setAuthorInput('');setEvents([]);setOffset(0);setAuthor('');}}>Сбросить фильтр</button>}</form>
    {loading && offset === 0 && !events.length ? <div className="audit-state">Загружаем журнал…</div> : error && !events.length ? <div className="audit-state audit-state-error"><p>Не удалось загрузить журнал: {error}</p><button className="button ghost" onClick={()=>setReload(value=>value+1)}><RotateCw size={15}/>Повторить</button></div> : !events.length ? <div className="audit-state">{author ? 'Для этого автора изменений не найдено.' : 'В этом пространстве пока нет изменений статей.'}</div> : <div className="audit-list">{events.map(event => <article className="audit-event" key={event.id}><div className="audit-event-head"><div><strong>{event.author || 'Автор не указан'}</strong><span>·</span><time title={formatDate(event.created_at)}>{formatDate(event.created_at)}</time></div><span className="audit-revision">Версия {event.revision}</span></div><h2><button onClick={()=>onOpenArticle(event.article_id)}>{event.title || 'Без названия'}<ArrowUpRight size={14}/></button></h2><p className="audit-changes">{event.changes.map(change=>change==='deleted_at'?(event.deleted?'удаление статьи':'восстановление статьи'):(changeNames[change]||change)).join(' · ') || 'Изменения статьи'}</p>{event.message&&<p className="audit-message">{event.message}</p>}<button className="audit-history-link" onClick={()=>onOpenHistory(event.article_id)}>Открыть историю статьи</button></article>)}</div>}
    {error&&events.length>0&&<div className="audit-more-error" role="alert">Не удалось загрузить следующую страницу. <button onClick={()=>setReload(value=>value+1)}>Повторить</button></div>}
    {hasMore&&<div className="audit-load-more"><button className="button ghost" disabled={loading||Boolean(error)} onClick={()=>setOffset(value=>value+pageSize)}>{loading?'Загружаем…':'Загрузить ещё 50'}</button></div>}
  </section>;
}
