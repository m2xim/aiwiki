import React, { useEffect, useId, useMemo, useRef, useState } from 'react';
import { AlertTriangle, ArrowRight, FolderInput, UserRound, Users, X } from 'lucide-react';
import { Article, CatalogSpace, MovePlace, MovePreview, MoveRequest, client } from './api';
import { ParentPicker } from './ParentPicker';
import { AccessChange, canEditRole, canManageRole, containerRole, countMembers, countUsers, describeAccessChange, eligibleTargets, roleLabel, summarizePreview } from './moveCore';
import './MoveDialog.css';

export type MoveSubject = { kind: 'article'; article: Article };
export type MoveDone = { kind: 'article'; id: string; moved: boolean };
type Props = { subject: MoveSubject; author: string; onClose: () => void; onDone: (done: MoveDone) => void };

const CHANGE_LABELS: Record<AccessChange, string> = { gain: 'Получит доступ', lose: 'Потеряет доступ', upgrade: 'Права расширятся', downgrade: 'Права сузятся' };
const place = (p: MovePlace) => p.space_name || '(скрыто)';
const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function MoveDialog({ subject, author, onClose, onDone }: Props) {
  const uid = useId();
  const article = subject.article;
  const dialogRef = useRef<HTMLDivElement>(null);
  const [catalog, setCatalog] = useState<CatalogSpace[] | null>(null);
  const [loadError, setLoadError] = useState('');
  const [spaceId, setSpaceId] = useState('');
  const [parentId, setParentId] = useState('');
  const [step, setStep] = useState<'target' | 'preview'>('target');
  const [preview, setPreview] = useState<MovePreview | null>(null);
  const [stale, setStale] = useState(false);
  const [busy, setBusy] = useState<'' | 'preview' | 'apply' | 'save'>('');
  const [error, setError] = useState('');
  const [spaceArticles, setSpaceArticles] = useState<Article[]>([]);
  const [articlesLoading, setArticlesLoading] = useState(false);
  const [articlesError, setArticlesError] = useState('');

  // Remember the opener and give focus back when the dialog goes away.
  useEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    return () => { opener?.focus?.(); };
  }, []);

  useEffect(() => {
    let live = true;
    client.catalog().then(value => { if (live) setCatalog(value); }).catch(e => { if (live) setLoadError(message(e)); });
    return () => { live = false; };
  }, []);

  const targets = useMemo(() => catalog ? eligibleTargets(catalog) : [], [catalog]);
  const current = targets.find(t => t.space.id === spaceId);
  // Editors (not managers) of the article's container may only change the parent inside it.
  const parentOnly = Boolean(catalog && !canManageRole(containerRole(catalog, article)));
  const noRights = Boolean(catalog && !canEditRole(containerRole(catalog, article)));

  // Initial selection: the current space when it is allowed, otherwise the first allowed one.
  useEffect(() => {
    if (!catalog || spaceId) return;
    if (parentOnly) { setSpaceId(article.space_id); setParentId(article.parent_id || ''); return; }
    const home = targets.find(t => t.space.id === article.space_id) || targets[0];
    if (!home) return;
    setSpaceId(home.space.id);
    if (home.space.id === article.space_id) setParentId(article.parent_id || '');
  }, [catalog, targets, spaceId, article, parentOnly]);

  // Articles of the target space feed the parent picker (articles only).
  useEffect(() => {
    if (!spaceId) return;
    let live = true;
    setArticlesLoading(true); setArticlesError(''); setSpaceArticles([]);
    (async () => {
      const result: Article[] = [];
      try {
        for (let offset = 0; ; offset += 200) {
          const page = await client.articles({ space_id: spaceId, limit: '200', offset: String(offset) });
          if (!live) return;
          result.push(...page);
          if (page.length < 200) break;
        }
        setSpaceArticles(result);
      } catch (e) { if (live) setArticlesError(message(e)); }
      finally { if (live) setArticlesLoading(false); }
    })();
    return () => { live = false; };
  }, [spaceId]);

  const parentOptions = useMemo(() => spaceArticles.filter(a => !a.deleted_at), [spaceArticles]);

  const sameContainer = Boolean(spaceId && article.space_id === spaceId);
  const unchanged = sameContainer && parentId === (article.parent_id || '');
  const canNext = (Boolean(current) || parentOnly) && !noRights && !unchanged && !busy && !articlesLoading;

  const onSpace = (id: string) => { setSpaceId(id); setError(''); setParentId(''); };

  const request = (fingerprint?: string): MoveRequest => ({ kind: 'article', id: article.id, base_revision: article.revision, target: { space_id: spaceId, parent_id: parentId }, ...(fingerprint ? { fingerprint } : {}) });

  const next = async () => {
    if (!canNext) return;
    setError('');
    if (sameContainer) {
      setBusy('save');
      try {
        await client.updateArticle(article.id, { parent_id: parentId, base_revision: article.revision, author, message: 'Смена родителя' });
        onDone({ kind: 'article', id: article.id, moved: false });
      } catch (e) { setError(message(e)); setBusy(''); }
      return;
    }
    setBusy('preview');
    try { setPreview(await client.movePreview(request())); setStale(false); setStep('preview'); }
    catch (e) { setError(message(e)); }
    finally { setBusy(''); }
  };

  const apply = async () => {
    if (!preview || busy) return;
    setError(''); setBusy('apply');
    try {
      const result = await client.moveApply(request(preview.fingerprint));
      if (result.conflict) { setPreview(result.preview); setStale(true); setBusy(''); return; }
      onDone({ kind: 'article', id: article.id, moved: true });
    } catch (e) { setError(message(e)); setBusy(''); }
  };

  const back = () => { if (busy) return; setStep('target'); setPreview(null); setStale(false); setError(''); };

  // Keep focus inside the dialog when its content changes.
  useEffect(() => {
    const root = dialogRef.current; if (!root) return;
    const first = root.querySelector<HTMLElement>('select:not(:disabled), input:not(:disabled), button.move-primary:not(:disabled)');
    (first || root).focus();
  }, [step, catalog === null]);

  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); if (!busy) onClose(); return; }
    if (event.key !== 'Tab' || !dialogRef.current) return;
    const items = Array.from(dialogRef.current.querySelectorAll<HTMLElement>('button:not(:disabled), select:not(:disabled), input:not(:disabled), [tabindex="0"]'));
    if (!items.length) { event.preventDefault(); return; }
    const first = items[0], last = items[items.length - 1];
    if (event.shiftKey && (document.activeElement === first || document.activeElement === dialogRef.current)) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  };

  const titleId = `${uid}-title`;
  // Red confirmation only when someone loses access entirely; a narrower role is shown in the table.
  const loses = Boolean(preview && preview.access.some(row => describeAccessChange(row.before, row.after) === 'lose'));
  const name = article.title || 'Без названия';

  const targetStep = <>
    {!catalog && !loadError && <p className="move-note" role="status">Загружаем доступные места…</p>}
    {loadError && <p className="move-error" role="alert">{loadError}</p>}
    {catalog && parentOnly && <div className="move-form">
      {noRights ? <p className="move-note">Недостаточно прав для изменения этой статьи.</p> : <>
        <p className="move-note">Переносить в другое пространство могут управляющие. Вы можете выбрать другую родительскую статью в этом же месте.</p>
        <div className="move-field"><span>Родительская статья <small>необязательно</small></span>
          <ParentPicker options={parentOptions} value={parentId} excludeId={article.id} onChange={id => { setParentId(id); setError(''); }}/>
          {articlesLoading && <small className="move-hint" role="status">Загружаем статьи…</small>}
          {articlesError && <small className="move-hint move-hint-error" role="alert">{articlesError}</small>}</div>
        {unchanged && <p className="move-note">Статья уже находится здесь.</p>}</>}
    </div>}
    {catalog && !parentOnly && !targets.length && <p className="move-note">Нет пространств, куда вы можете переместить статью: нужна роль «{roleLabel('manager')}» или «{roleLabel('owner')}».</p>}
    {catalog && !parentOnly && targets.length > 0 && <div className="move-form">
      <label className="move-field"><span>Пространство</span>
        <select aria-label="Пространство" value={spaceId} onChange={e => onSpace(e.target.value)} disabled={Boolean(busy)}>
          {targets.map(t => <option key={t.space.id} value={t.space.id}>{t.space.name}</option>)}
        </select></label>
      <div className="move-field"><span>Родительская статья <small>необязательно</small></span>
        <ParentPicker options={parentOptions} value={parentId} excludeId={article.id} onChange={id => { setParentId(id); setError(''); }}/>
        {articlesLoading && <small className="move-hint" role="status">Загружаем статьи…</small>}
        {articlesError && <small className="move-hint move-hint-error" role="alert">{articlesError}</small>}</div>
      {unchanged && <p className="move-note">Статья уже находится здесь.</p>}
      {sameContainer && !unchanged && <p className="move-note">Это то же пространство: будет изменён только родитель, без проверки прав доступа.</p>}
      {!sameContainer && <p className="move-note">Вместе со статьёй переместятся все дочерние статьи.</p>}
    </div>}
  </>;

  const previewStep = preview && <div className="move-preview">
    {stale && <div className="move-banner" role="alert"><AlertTriangle size={16} aria-hidden="true"/><span>Права или содержимое изменились — проверьте ещё раз.</span></div>}
    <ul className="move-summary">{summarizePreview(preview).map(line => <li key={line}>{line}</li>)}</ul>
    <div className="move-route" role="group" aria-label="Откуда и куда">
      <span><small>ОТКУДА</small>{place(preview.source)}</span>
      <ArrowRight size={16} aria-hidden="true"/>
      <span><small>КУДА</small>{place(preview.target)}</span>
    </div>
    {preview.access.length === 0
      ? <p className="move-note">Права доступа не изменятся.</p>
      : <div className="move-table-wrap"><table className="move-table">
        <caption>Изменения доступа</caption>
        <thead><tr><th scope="col">Кто</th><th scope="col">Было</th><th scope="col">Станет</th><th scope="col">Что изменится</th></tr></thead>
        <tbody>{preview.access.map(row => {
          const change = describeAccessChange(row.before, row.after);
          return <tr key={`${row.subject_type}:${row.subject_id}:${row.before}:${row.after}`} className={change ? `move-${change}` : ''}>
            <th scope="row"><span className="move-subject">{row.subject_type === 'user' ? <UserRound size={15} aria-label="Пользователь"/> : <Users size={15} aria-label={row.subject_type === 'team' ? 'Команда' : 'Группа'}/>}<span><b>{row.subject_type === 'other' ? 'Другие участники' : row.name || '(скрыто)'}</b>{row.subject_type === 'team' && typeof row.users === 'number' && <small>{countMembers(row.users)}</small>}{row.subject_type === 'other' && typeof row.users === 'number' && <small>{countUsers(row.users)}</small>}</span></span></th>
            <td className="move-before">{roleLabel(row.before)}</td><td className="move-after">{roleLabel(row.after)}</td>
            <td>{change && <span className={`move-badge move-badge-${change}`}>{CHANGE_LABELS[change]}</span>}</td>
          </tr>;
        })}</tbody>
      </table></div>}
    {preview.warnings?.length > 0 && <ul className="move-warnings" aria-label="Предупреждения">{preview.warnings.map((w, i) => <li key={i}><AlertTriangle size={14} aria-hidden="true"/>{w}</li>)}</ul>}
  </div>;

  return <div className="move-backdrop">
    <div ref={dialogRef} className="move-dialog" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1} onKeyDown={onKeyDown}>
      <div className="move-head">
        <div><div className="move-eyebrow"><FolderInput size={14} aria-hidden="true"/>{step === 'target' ? 'ШАГ 1 ИЗ 2 · КУДА' : 'ШАГ 2 ИЗ 2 · ПРОВЕРКА'}</div>
          <h2 id={titleId}>Переместить статью</h2>
          <p className="move-subtitle" title={name}>«{name}»</p></div>
        <button type="button" className="icon-button" onClick={onClose} disabled={busy === 'apply'} aria-label="Закрыть окно перемещения" title="Закрыть (Esc)"><X size={18}/></button>
      </div>
      <div className="move-body">{step === 'target' ? targetStep : previewStep}</div>
      {error && <p className="move-error" role="alert">{error}</p>}
      <div className="move-actions">
        {step === 'target'
          ? <><button type="button" className="button ghost" onClick={onClose} disabled={Boolean(busy)}>Отмена</button>
            <button type="button" className="button primary move-primary" onClick={() => void next()} disabled={!canNext}>
              {busy === 'preview' ? 'Проверяем…' : busy === 'save' ? 'Сохраняем…' : sameContainer ? 'Сменить родителя' : 'Далее'}</button></>
          : <><button type="button" className="button ghost" onClick={back} disabled={busy === 'apply'}>Назад</button>
            <button type="button" className={`button primary move-primary${loses ? ' move-danger' : ''}`} onClick={() => void apply()} disabled={Boolean(busy)}>
              {busy === 'apply' ? 'Перемещаем…' : loses ? 'Переместить, несмотря на потерю доступа' : 'Переместить'}</button></>}
      </div>
    </div>
  </div>;
}
