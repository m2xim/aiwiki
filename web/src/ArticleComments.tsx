import React, { forwardRef, lazy, Suspense, useEffect, useImperativeHandle, useRef, useState } from 'react';
import { Check, Code2, Eye, Italic, List, MessageCircle, Pencil, Pilcrow, Save, X } from 'lucide-react';
import { Comment, client } from './api';
import { Markdown, smoothBehavior } from './markdown';
import type { MarkdownEditorHandle } from './MarkdownEditor';

const MarkdownEditor = lazy(() => import('./MarkdownEditor').then(module => ({ default: module.MarkdownEditor })));
export type CommentHeading = { id: string; text: string; level: number };
export type ArticleCommentsHandle = { openComment(anchor?: string): void };
type Props = { articleId: string; deleted?: boolean; toc: CommentHeading[]; theme: 'light' | 'dark'; author: string; onCount?: (open: number, total: number) => void; onAnchorCounts?: (counts: Record<string, number>) => void; onNavigate?: (id: string, anchor: string) => void };
/** One wording for the top button and the section heading: «2 открытых из 5». */
export const commentsLabel = (open: number, total: number) => total ? `${open} ${open % 10 === 1 && open % 100 !== 11 ? 'открытый' : 'открытых'} из ${total}` : '';
const dateTime = (value: string) => value ? new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value)) : '';

export const ArticleComments = forwardRef<ArticleCommentsHandle, Props>(function ArticleComments({ articleId, deleted, toc, theme, author, onCount, onAnchorCounts, onNavigate }, ref) {
  const [comments, setComments] = useState<Comment[]>([]);
  const [loading, setLoading] = useState(true); const [error, setError] = useState(''); const [reload, setReload] = useState(0);
  const [composerOpen, setComposerOpen] = useState(false); const [anchor, setAnchor] = useState(''); const [draft, setDraft] = useState(''); const [submitting, setSubmitting] = useState(false);
  const [editingId, setEditingId] = useState(''); const [editBody, setEditBody] = useState(''); const [editAnchor, setEditAnchor] = useState('');
  const [preview, setPreview] = useState(false); const [pending, setPending] = useState<Record<string, boolean>>({});
  const [editorSession, setEditorSession] = useState(0); const [showResolved, setShowResolved] = useState(false);
  const editorRef = useRef<MarkdownEditorHandle>(null); const requestRef = useRef(0); const articleRef = useRef(articleId); articleRef.current = articleId;
  const onCountRef = useRef(onCount); onCountRef.current = onCount; const onAnchorCountsRef = useRef(onAnchorCounts); onAnchorCountsRef.current = onAnchorCounts;
  const mutationBusy = submitting || Object.values(pending).some(Boolean);
  const focusEditor = () => requestAnimationFrame(() => document.querySelector<HTMLTextAreaElement>('#article-comments .cm-content')?.focus({ preventScroll: true }));
  useImperativeHandle(ref, () => ({ openComment(nextAnchor = '') { if (deleted || mutationBusy) return; setEditingId(''); setEditBody(''); setEditAnchor(''); setEditorSession(v=>v+1); setAnchor(nextAnchor); setDraft(''); setPreview(false); setComposerOpen(true); focusEditor(); } }), [deleted, mutationBusy]);
  useEffect(() => {
    const request = ++requestRef.current; setLoading(true); setError('');
    client.comments(articleId).then(items => { if (request === requestRef.current && articleRef.current === articleId) setComments(items); })
      .catch(e => { if (request === requestRef.current && articleRef.current === articleId) setError((e as Error).message); })
      .finally(() => { if (request === requestRef.current && articleRef.current === articleId) setLoading(false); });
    return () => { requestRef.current++; };
  }, [articleId, reload]);
  useEffect(() => { onCountRef.current?.(comments.filter(item => !item.resolved).length, comments.length); }, [comments]);
  useEffect(() => { setShowResolved(false); }, [articleId]);
  useEffect(() => {
    const counts: Record<string, number> = {};
    for (const item of comments) if (!item.resolved && item.anchor) counts[item.anchor] = (counts[item.anchor] || 0) + 1;
    onAnchorCountsRef.current?.(counts);
  }, [comments]);
  useEffect(() => () => { onAnchorCountsRef.current?.({}); }, [articleId]);
  const change = (_id: string, update: (items: Comment[]) => Comment[]) => setComments(items => update(items));
  const add = async (event: React.FormEvent) => {
    event.preventDefault(); if (deleted || !draft.trim() || submitting || loading) return;
    const id = articleId; setSubmitting(true); setError('');
    try { const item = await client.addComment(id, { anchor, body: draft.trim(), author: author.trim() || 'human' }); if (articleRef.current === id) { change(item.id, items => [...items, item]); setDraft(''); setAnchor(''); setComposerOpen(false); } }
    catch (e) { if (articleRef.current === id) setError((e as Error).message); }
    finally { if (articleRef.current === id) setSubmitting(false); }
  };
  const startEdit = (item: Comment) => { if (mutationBusy) return; setComposerOpen(false); setDraft(''); setEditingId(item.id); setEditBody(item.body); setEditAnchor(item.anchor || ''); setPreview(false); setEditorSession(v=>v+1); };
  const cancelEdit = () => { setEditingId(''); setEditBody(''); setEditAnchor(''); setPreview(false); setEditorSession(v=>v+1); };
  const saveEdit = async (item: Comment) => {
    if (!editBody.trim() || pending[item.id] || loading || deleted) return;
    const id = articleId; setPending(v => ({ ...v, [item.id]: true })); setError('');
    try { const result = await client.updateComment(item.id, { body: editBody.trim(), anchor: editAnchor }); if (articleRef.current === id) { change(item.id, items => items.map(current => current.id === item.id ? result : current)); cancelEdit(); } }
    catch (e) { if (articleRef.current === id) setError((e as Error).message); }
    finally { if (articleRef.current === id) setPending(v => ({ ...v, [item.id]: false })); }
  };
  const resolve = async (item: Comment) => {
    if (pending[item.id] || loading || deleted) return;
    const id = articleId; setPending(v => ({ ...v, [item.id]: true })); setError('');
    try { const result = await client.resolveComment(item.id, !item.resolved); if (articleRef.current === id) change(item.id, items => items.map(current => current.id === item.id ? result : current)); }
    catch (e) { if (articleRef.current === id) setError((e as Error).message); }
    finally { if (articleRef.current === id) setPending(v => ({ ...v, [item.id]: false })); }
  };
  const formatButton = (label: string, icon: React.ReactNode, before: string, after = before, disabled = false) => <button type="button" className="button ghost tiny" title={label} aria-label={label} disabled={disabled} onClick={() => editorRef.current?.wrapSelection(before, after)}>{icon}</button>;
  const editorControls = (value: string, setValue: (value: string) => void, disabled: boolean, key: string) => <>
    <div className="comment-editor-toolbar"><div>{formatButton('Полужирный', <b>B</b>, '**', '**', disabled)}{formatButton('Курсив', <Italic size={13}/>, '*', '*', disabled)}{formatButton('Ссылка', <span>↗</span>, '[', '](https://)', disabled)}{formatButton('Список', <List size={14}/>, '- ', '', disabled)}{formatButton('Код', <Code2 size={14}/>, '`', '`', disabled)}</div><div><button type="button" className={`button tiny ${!preview?'primary':''}`} onClick={()=>setPreview(false)} disabled={disabled}><Pilcrow size={13}/>Текст</button><button type="button" className={`button tiny ${preview?'primary':''}`} onClick={()=>setPreview(true)} disabled={disabled}><Eye size={13}/>Просмотр</button></div></div>
    <div className="comment-editor-panels"><div className="comment-editor-source" hidden={preview}><Suspense fallback={<div className="comment-editor-loading"/>}><MarkdownEditor key={`${key}-${editorSession}`} ref={editorRef} value={value} onChange={setValue} theme={theme} ariaLabel="Текст комментария" compact readOnly={disabled} /></Suspense></div><div className="comment-editor-preview" hidden={!preview}><Markdown body={value || 'Предпросмотр Markdown'} comment/></div></div>
  </>;
  const openComments = comments.filter(item => !item.resolved); const resolvedComments = comments.filter(item => item.resolved);
  const renderCard = (item: Comment) => <article className={`comment-card ${item.resolved?'resolved':''} ${editingId===item.id?'comment-card-editing':''}`} key={item.id}>
      <div className="comment-avatar">{(item.author||'?').slice(0,1).toLocaleUpperCase('ru')}</div><div className="comment-main"><div className="comment-author-line"><strong>{item.author||'Автор'}</strong><time dateTime={item.created_at}>{dateTime(item.created_at)}</time></div>
      {editingId===item.id?<div className="comment-edit-form"><label className="comment-edit-anchor">К разделу<select value={editAnchor} onChange={event=>setEditAnchor(event.target.value)} disabled={mutationBusy||loading}><option value="">Без привязки</option>{editAnchor&&!toc.some(h=>h.id===editAnchor)&&<option value={editAnchor}>{editAnchor}</option>}{toc.map(h=><option key={h.id} value={h.id}>{'#'.repeat(h.level)} {h.text}</option>)}</select></label>{editorControls(editBody,setEditBody,mutationBusy||loading,'edit')}<div className="comment-form-actions"><button type="button" className="button primary" disabled={!editBody.trim()||mutationBusy||loading} onClick={()=>void saveEdit(item)}><Save size={14}/>{pending[item.id]?'Сохраняем…':'Сохранить'}</button><button type="button" className="button ghost" disabled={mutationBusy} onClick={cancelEdit}><X size={14}/>Отмена</button></div></div>:<>{item.anchor&&<a className="comment-anchor" href={`#${item.anchor}`} onClick={event=>{event.preventDefault();document.getElementById(item.anchor)?.scrollIntoView({behavior:smoothBehavior()});}}>⌁ {toc.find(h=>h.id===item.anchor)?.text||item.anchor}</a>}<Markdown body={item.body} comment onNavigate={onNavigate}/><div className="comment-actions">{!deleted&&<><button className="resolve-button" disabled={mutationBusy||loading} onClick={()=>void resolve(item)}>{item.resolved?<><Check size={13}/>Решено · вернуть</>:<><Check size={13}/>Отметить решённым</>}</button><button className="resolve-button" disabled={mutationBusy||loading} onClick={()=>startEdit(item)}><Pencil size={13}/>Редактировать</button></>}</div></>}
      </div></article>;
  return <section className="article-comments" id="article-comments"><div className="section-title"><h2><MessageCircle size={17}/>Комментарии <span className="count">{commentsLabel(comments.filter(item=>!item.resolved).length,comments.length)}</span></h2>{!deleted&&!composerOpen&&<button type="button" className="button primary small" disabled={mutationBusy} onClick={()=>{setEditingId('');setEditBody('');setEditAnchor('');setEditorSession(v=>v+1);setComposerOpen(true);setAnchor('');setDraft('');setPreview(false);focusEditor();}}><MessageCircle size={14}/>Добавить комментарий</button>}</div>
    <p className="article-comments-note">Комментарии привязаны к статье или отдельному разделу.</p>
    {error&&<div className="comments-error" role="alert">{error}<button className="button ghost small" disabled={mutationBusy} onClick={()=>setReload(value=>value+1)}>Повторить</button></div>}
    {!deleted&&composerOpen&&<form className="article-comment-form" onSubmit={event=>void add(event)}><label>К разделу<select value={anchor} onChange={event=>setAnchor(event.target.value)} disabled={submitting||loading}><option value="">Без привязки</option>{toc.map(h=><option key={h.id} value={h.id}>{'#'.repeat(h.level)} {h.text}</option>)}</select></label>{editorControls(draft,setDraft,submitting||loading,'new')}<div className="comment-form-actions"><button className="button primary" disabled={!draft.trim()||submitting||loading}><MessageCircle size={14}/>{submitting?'Отправляем…':'Добавить комментарий'}</button><button type="button" className="button ghost" disabled={submitting} onClick={()=>{setDraft('');setAnchor('');setComposerOpen(false);setPreview(false);setEditorSession(v=>v+1);}}><X size={14}/>Отмена</button></div></form>}
    <div className="article-comment-list">{loading&&!comments.length?<div className="panel-empty">Загружаем комментарии…</div>:openComments.map(renderCard)}{!loading&&!comments.length&&!error&&<div className="panel-empty">Пока нет комментариев.</div>}{!loading&&!!comments.length&&!openComments.length&&<div className="panel-empty">Нет открытых комментариев.</div>}</div>{!!resolvedComments.length&&<div className="resolved-comments"><button type="button" className="resolved-toggle" aria-expanded={showResolved} aria-controls="resolved-comment-list" onClick={()=>setShowResolved(v=>!v)}>{showResolved?'Скрыть решённые':'Решённые'} ({resolvedComments.length})</button>{showResolved&&<div id="resolved-comment-list" className="article-comment-list">{resolvedComments.map(renderCard)}</div>}</div>}
  </section>;
});
