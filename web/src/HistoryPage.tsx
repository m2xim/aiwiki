import React, { useEffect, useMemo, useState } from 'react';
import { ArrowLeft, History, RefreshCw } from 'lucide-react';
import { Article, Revision } from './api';
import { lineDiff, pairChangedLines, WordPart } from './diff';

type Props = {
  article: Article;
  revisions: Revision[];
  loading: boolean;
  error: string;
  restoring: boolean;
  onRetry: () => void;
  onBack: () => void;
  onRestore: (revision: Revision) => void;
  deleted?: boolean;
};

const fieldRows = (article: Article) => [
  ['Заголовок', article.title], ['Описание', article.summary], ['Тип', article.kind], ['Статус', article.status],
  ['Пространство', article.space_id], ['Родитель', article.parent_id || '—'], ['Закреплено', article.pinned ? 'Да' : 'Нет'],
  ['Теги', article.tags.join(', ') || '—'], ['Свойства', Object.entries(article.properties || {}).map(([k, v]) => `${k}=${v}`).join(', ') || '—'],
];
const renderParts = (parts: WordPart[], tag: 'del' | 'ins') => parts.map((part, index) => part.changed ? (tag === 'del' ? <del key={index} className="diff-word">{part.text}</del> : <ins key={index} className="diff-word">{part.text}</ins>) : part.text);
const formatDate = (value: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Дата неизвестна';
const downloadSnapshot = (article: Article, revision: number) => {
  const blob = new Blob([article.body], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  const safeTitle = (article.title || 'article').replace(/[^\p{L}\p{N}._-]+/gu, '_');
  link.href = url; link.download = `${safeTitle}-rev-${revision}.md`; link.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
};

export function HistoryPage({ article, revisions, loading, error, restoring, onRetry, onBack, onRestore, deleted = false }: Props) {
  const [targetRevision, setTargetRevision] = useState<number | null>(null);
  const [baseRevision, setBaseRevision] = useState<number | null>(null);
  const target = revisions.find(item => item.revision === targetRevision) || null;
  const bases = target ? revisions.filter(item => item.revision < target.revision) : [];
  const base = bases.find(item => item.revision === baseRevision) || null;

  useEffect(() => {
    setTargetRevision(revisions[0]?.revision ?? null);
    setBaseRevision(revisions[1]?.revision ?? null);
  }, [article.id, article.revision, revisions]);
  useEffect(() => {
    if (target && baseRevision !== null && !bases.some(item => item.revision === baseRevision)) {
      setBaseRevision(bases[0]?.revision ?? null);
    }
  }, [targetRevision, baseRevision, revisions]);

  const diff = useMemo(() => target ? lineDiff(base?.snapshot.body ?? '', target.snapshot.body) : null, [base, target]);
  const wordParts = useMemo(() => diff?.supported ? pairChangedLines(diff.lines) : new Map<number, WordPart[]>(), [diff]);
  const targetFields = target ? fieldRows(target.snapshot) : [];
  const baseFields = base ? fieldRows(base.snapshot) : [];
  const metadataChanges = target ? targetFields.flatMap(([label, value], index) => {
    const prior = baseFields[index]?.[1] ?? '—';
    return prior === value ? [] : [{ label, prior, value }];
  }) : [];

  return <section className="history-page">
    <div className="history-page-top"><button className="button ghost" onClick={onBack}><ArrowLeft size={16}/>К статье</button><span className="eyebrow">ИСТОРИЯ ИЗМЕНЕНИЙ</span></div>
    <h1>История версий</h1><p className="history-page-subtitle">{article.title} · Версий: {revisions.length}</p>
    <p className="history-page-note">{deleted?'Чтобы снова редактировать статью, сначала восстановите её на странице статьи. История и снимки доступны для просмотра.':'Восстановление создаёт новую версию по выбранному снимку. Изменения текста показаны ниже.'}</p>
    {loading ? <div className="history-state">Загружаем историю…</div> : error ? <div className="history-state history-state-error"><p>Не удалось загрузить историю: {error}</p><button className="button ghost" onClick={onRetry}><RefreshCw size={15}/>Повторить</button></div> : !revisions.length ? <div className="history-state">У этой статьи пока нет сохранённых версий.</div> : <div className="history-layout">
      <nav className="history-version-list" aria-label="Версии статьи"><h2>Версии</h2>{revisions.map((revision, index) => <button key={revision.id} className={`history-version ${targetRevision === revision.revision ? 'selected' : ''}`} onClick={() => setTargetRevision(revision.revision)}>
        <span className="history-version-heading"><strong>Версия {revision.revision}</strong>{revision.revision === article.revision && <small>ТЕКУЩАЯ</small>}</span>
        <span>{revision.message || 'Сохранение статьи'}</span><time>{formatDate(revision.created_at)}</time><span className="history-version-author">{revision.author || 'Автор неизвестен'}</span>
      </button>)}</nav>
      <div className="history-comparison">
        {target && <><div className="history-compare-controls"><label>Показать версию<select value={target.revision} onChange={event => setTargetRevision(Number(event.target.value))}>{revisions.map(item => <option key={item.id} value={item.revision}>Версия {item.revision}</option>)}</select></label><label>Сравнить с<select value={base?.revision ?? ''} onChange={event => setBaseRevision(event.target.value ? Number(event.target.value) : null)}><option value="">Пустой документ</option>{bases.map(item => <option key={item.id} value={item.revision}>Версия {item.revision}</option>)}</select></label></div>
          <div className="history-target-heading"><div><span className="eyebrow">ВЕРСИЯ {target.revision}</span><h2>{target.snapshot.title || 'Без названия'}</h2><p>{target.message || 'Сохранение статьи'} · {target.author || 'Автор неизвестен'} · {formatDate(target.created_at)}</p></div>{!deleted && target.revision < article.revision && <button className="button primary" disabled={restoring} onClick={() => onRestore(target)}><History size={15}/>{restoring?'Восстанавливаем…':'Восстановить эту версию'}</button>}</div>
          {metadataChanges.length > 0 && <section className="history-metadata"><h3>Изменения сведений</h3>{metadataChanges.map(change => <div key={change.label}><b>{change.label}</b><span>{change.prior || '—'}</span><span aria-hidden="true">→</span><span>{change.value || '—'}</span></div>)}</section>}
          <section className="history-diff"><div className="history-diff-heading"><h3>Изменения текста</h3>{diff?.supported && <span><b className="diff-count-add">+{diff.additions}</b><b className="diff-count-remove">−{diff.removals}</b></span>}</div>
            {!diff?.supported ? <><p className="history-diff-fallback">{diff?.reason} Полные снимки доступны для скачивания.</p><div className="history-downloads"><button className="button ghost small" onClick={() => downloadSnapshot(target.snapshot, target.revision)}>Скачать версию {target.revision}</button>{base&&<button className="button ghost small" onClick={() => downloadSnapshot(base.snapshot, base.revision)}>Скачать версию {base.revision}</button>}</div></> : diff.lines.length ? <div className="history-diff-table">{diff.lines.map((line, index) => line.type === 'gap' ? <div className="diff-row diff-gap" key={`gap-${index}`}><span/><span/><span/><code>… {line.omitted} неизменённых строк …</code></div> : <div className={`diff-row diff-${line.type}`} key={`${line.type}-${line.oldLine}-${line.newLine}-${index}`}><span>{line.oldLine ?? ''}</span><span>{line.newLine ?? ''}</span><span>{line.type === 'add' ? '+' : line.type === 'remove' ? '−' : ' '}</span><code>{wordParts.has(index) ? renderParts(wordParts.get(index)!, line.type === 'remove' ? 'del' : 'ins') : (line.text || ' ')}</code></div>)}</div> : <p className="history-diff-fallback">Текст совпадает с выбранной базовой версией.</p>}
          </section></>}
      </div>
    </div>}
  </section>;
}
