import React, { useMemo, useState } from 'react';
import { AlertTriangle } from 'lucide-react';
import type { Article } from './api';
import { lineDiff } from './diff';
import type { DraftFields } from './draftStore';
import './editor-wave2.css';

type Props = {
  current: Article;
  mine: DraftFields;
  busy: boolean;
  onTakeTheirs: () => void;
  onOverwrite: () => void;
  onDismiss: () => void;
};

const formatDate = (value: string) => value ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'время неизвестно';
const propsText = (p: Record<string, string> | undefined) => Object.entries(p || {}).map(([k, v]) => `${k}=${v}`).join('\n');

/** Panel above the editor shown after a 409 on save: the article changed while the user was editing. */
export function EditorConflict({ current, mine, busy, onTakeTheirs, onOverwrite, onDismiss }: Props) {
  const [showDiff, setShowDiff] = useState(false);
  // old = their (server) version, new = my draft: «+» is what I would add, «−» is what I would remove from theirs.
  const diff = useMemo(() => showDiff ? lineDiff(current.body, mine.body) : null, [showDiff, current.body, mine.body]);
  const metaChanges = useMemo(() => {
    const rows: [string, string, string][] = [
      ['Заголовок', current.title, mine.title], ['Описание', current.summary, mine.summary], ['Тип', current.kind, mine.kind], ['Статус', current.status, mine.status],
      ['Пространство', current.space_id, mine.space_id], ['Родитель', current.parent_id, mine.parent_id], ['Закреплено', current.pinned ? 'да' : 'нет', mine.pinned ? 'да' : 'нет'],
      ['Теги', current.tags.join(', '), mine.tags], ['Свойства', propsText(current.properties), mine.properties],
    ];
    return rows.filter(([, theirs, my]) => theirs.trim() !== my.trim());
  }, [current, mine]);
  return <section className="editor-conflict" role="alert" aria-live="assertive">
    <div className="editor-conflict-head"><AlertTriangle size={18}/><div><strong>Статья изменена после начала правки</strong><p>Сейчас версия {current.revision}, автор {current.author || 'неизвестен'}, {formatDate(current.updated_at)}. Ваш черновик не потерян: он остаётся в редакторе и в локальном хранилище браузера.</p></div></div>
    <div className="editor-conflict-actions">
      <button type="button" className="button" onClick={() => setShowDiff(v => !v)} aria-expanded={showDiff}>{showDiff ? 'Скрыть отличия' : 'Показать отличия'}</button>
      <button type="button" className="button" onClick={onTakeTheirs} disabled={busy}>Взять их версию за основу</button>
      <button type="button" className="button primary" onClick={onOverwrite} disabled={busy}>{busy ? 'Сохраняем…' : 'Сохранить поверх'}</button>
      <button type="button" className="button ghost" onClick={onDismiss} disabled={busy}>Отмена</button>
    </div>
    {showDiff && <div className="editor-conflict-diff">
      {metaChanges.length > 0 && <div className="editor-conflict-meta"><b>Отличия в сведениях (версия {current.revision} → ваш черновик)</b>{metaChanges.map(([label, theirs, my]) => <div key={label}><b>{label}</b><span>{theirs || '—'}</span><span aria-hidden="true">→</span><span>{my || '—'}</span></div>)}</div>}
      {!diff?.supported ? <p className="history-diff-fallback">{diff?.reason}</p> : diff.lines.length ? <>
        <div className="editor-conflict-diff-head"><b>Текст: версия {current.revision} → ваш черновик</b><span><b className="diff-count-add">+{diff.additions}</b> <b className="diff-count-remove">−{diff.removals}</b></span></div>
        <div className="history-diff-table">{diff.lines.map((line, index) => line.type === 'gap'
          ? <div className="diff-row diff-gap" key={`gap-${index}`}><span/><span/><span/><code>… {line.omitted} неизменённых строк …</code></div>
          : <div className={`diff-row diff-${line.type}`} key={`${line.type}-${line.oldLine}-${line.newLine}-${index}`}><span>{line.oldLine ?? ''}</span><span>{line.newLine ?? ''}</span><span>{line.type === 'add' ? '+' : line.type === 'remove' ? '−' : ' '}</span><code>{line.text || ' '}</code></div>)}</div>
      </> : <p className="history-diff-fallback">Текст вашего черновика совпадает с текущей версией.</p>}
    </div>}
  </section>;
}
