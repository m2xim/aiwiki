import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FileText, ImagePlus, Upload, X } from 'lucide-react';
import { client, type Article, type Project, type Space } from './api';
import { Markdown } from './markdown';
import {
  IMAGE_EXT, MARKDOWN_EXT, MAX_ATTACHMENT_BYTES, MAX_MARKDOWN_BYTES, WARNING_TITLES, analyzeMarkdown, deriveTitle, normalizeText,
  parseFrontMatter, rewriteImageUrls, type ImportWarning, type WarningKind,
} from './markdownImportCore';
import './ImportMarkdown.css';

type Props = {
  spaces: Space[]; projects: Project[]; corporate: boolean; defaultSpaceId: string; author: string;
  onClose(): void; onImported(article: Article): void;
};
type Report = { article: Article; errors: string[] };

const sizeText = (n: number) => n >= 1048576 ? `${(n / 1048576).toFixed(1)} МБ` : `${Math.max(1, Math.round(n / 1024))} КБ`;
const imageKey = (f: File) => `${f.webkitRelativePath || f.name}|${f.size}|${f.lastModified}`;

export function ImportMarkdownDialog({ spaces, projects, corporate, defaultSpaceId, author, onClose, onImported }: Props) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const markdownInput = useRef<HTMLInputElement>(null);
  const imagesInput = useRef<HTMLInputElement>(null);
  const [fileName, setFileName] = useState('');
  const [raw, setRaw] = useState<string | null>(null);
  const [images, setImages] = useState<File[]>([]);
  const [fileError, setFileError] = useState('');
  const [dragging, setDragging] = useState(false);
  const [title, setTitle] = useState('');
  const [titleTouched, setTitleTouched] = useState(false);
  const usable = useMemo(() => spaces.filter(s => !s.archived_at), [spaces]);
  const [spaceId, setSpaceId] = useState(() => usable.find(s => s.id === defaultSpaceId)?.id || usable[0]?.id || '');
  const [projectId, setProjectId] = useState('');
  const [tab, setTab] = useState<'preview' | 'report'>('preview');
  const [matches, setMatches] = useState<Article[]>([]);
  const [matchNote, setMatchNote] = useState('');
  const [mode, setMode] = useState<'new' | 'update'>('new');
  const [updateId, setUpdateId] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [report, setReport] = useState<Report | null>(null);

  const space = usable.find(s => s.id === spaceId);
  const spaceProjects = useMemo(() => projects.filter(p => p.space_id === spaceId && !p.archived_at), [projects, spaceId]);
  // Same rule as the article editor: a limited space (projects with separate access) has no direct documents.
  const projectRequired = corporate && Boolean(space?.limited);
  useEffect(() => {
    if (!corporate) { setProjectId(''); return; }
    setProjectId(current => spaceProjects.some(p => p.id === current) ? current : projectRequired ? spaceProjects[0]?.id || '' : '');
  }, [corporate, spaceProjects, projectRequired]);

  const parsed = useMemo(() => {
    if (raw === null) return null;
    const fm = parseFrontMatter(raw);
    const analysis = analyzeMarkdown(fm.body, images.map(f => f.webkitRelativePath || f.name));
    const warnings: ImportWarning[] = [...fm.warnings];
    if (fm.ignoredFields.length) warnings.push({ kind: 'frontmatter', message: `Поля front matter не поддерживаются и проигнорированы: ${fm.ignoredFields.join(', ')}.` });
    warnings.push(...analysis.warnings);
    const upload = [...new Set(analysis.images.filter(i => i.status === 'local' && i.start >= 0).map(i => i.fileIndex))];
    return { fm, analysis, warnings, upload, derivedTitle: deriveTitle(fm.body, fileName, fm.data.title) };
  }, [raw, images, fileName]);
  const unusedImages = parsed ? images.filter((_, i) => !parsed.upload.includes(i)).length : 0;

  useEffect(() => { if (parsed && !titleTouched) setTitle(parsed.derivedTitle); }, [parsed, titleTouched]);

  const trimmedTitle = title.trim();
  // Look for same-titled articles in the chosen place; an existing article is never overwritten silently.
  useEffect(() => {
    setMatches([]); setMatchNote(''); setMode('new'); setUpdateId('');
    if (!trimmedTitle || !spaceId) return;
    let cancelled = false;
    const timer = window.setTimeout(() => {
      client.articles({ space_id: spaceId, q: trimmedTitle, limit: '100' }).then(list => {
        if (cancelled) return;
        const wanted = trimmedTitle.toLowerCase();
        const found = list.filter(a => !a.deleted_at && a.title.trim().toLowerCase() === wanted && (!corporate || (a.project_id || '') === projectId));
        setMatches(found); setUpdateId(found[0]?.id || '');
      }).catch(() => { if (!cancelled) setMatchNote('Не удалось проверить, есть ли статья с таким названием.'); });
    }, 300);
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [trimmedTitle, spaceId, projectId, corporate]);

  const readMarkdown = useCallback(async (file: File) => {
    if (file.size > MAX_MARKDOWN_BYTES) { setFileError(`Файл «${file.name}» больше 2 МБ (${sizeText(file.size)}).`); return; }
    try {
      const text = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer());
      setRaw(normalizeText(text)); setFileName(file.name); setFileError(''); setTitleTouched(false); setError('');
    } catch { setFileError(`Файл «${file.name}» не в кодировке UTF-8.`); }
  }, []);
  const addFiles = useCallback((list: FileList | File[]) => {
    const files = Array.from(list); const problems: string[] = [];
    const md = files.filter(f => MARKDOWN_EXT.test(f.name)); const imgs = files.filter(f => IMAGE_EXT.test(f.name));
    if (md.length > 1) problems.push('Выбрано несколько .md-файлов: взят первый, импортируется один файл за раз.');
    for (const f of files) if (!MARKDOWN_EXT.test(f.name) && !IMAGE_EXT.test(f.name)) problems.push(`Файл «${f.name}» пропущен: нужны .md, .markdown, png, jpeg, gif или webp.`);
    setFileError(problems.join(' '));
    if (md[0]) void readMarkdown(md[0]).then(() => { if (problems.length) setFileError(v => v || problems.join(' ')); });
    if (imgs.length) setImages(prev => { const seen = new Set(prev.map(imageKey)); return [...prev, ...imgs.filter(f => !seen.has(imageKey(f)))]; });
  }, [readMarkdown]);

  // Focus handling: start inside, keep Tab inside, Esc closes, give focus back on exit.
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    dialogRef.current?.focus();
    return () => { previous?.focus?.(); };
  }, []);
  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); if (!saving) onClose(); return; }
    if (event.key !== 'Tab') return;
    const items = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled):not([hidden]), select:not(:disabled), textarea:not(:disabled), a[href]') || []).filter(el => el.offsetParent !== null);
    if (!items.length) { event.preventDefault(); return; }
    const first = items[0], last = items[items.length - 1], active = document.activeElement;
    if (event.shiftKey && (active === first || active === dialogRef.current)) { event.preventDefault(); last.focus(); }
    else if (!event.shiftKey && active === last) { event.preventDefault(); first.focus(); }
  };

  const canSave = Boolean(parsed && trimmedTitle && spaceId && (!projectRequired || projectId) && (mode === 'new' || updateId) && !saving);
  const save = async () => {
    if (!parsed || !canSave) return;
    setSaving(true); setError('');
    const { fm, analysis } = parsed; const body = fm.body;
    const errors: string[] = [];
    const meta: Record<string, unknown> = {};
    if (fm.data.summary !== undefined) meta.summary = fm.data.summary;
    if (fm.data.tags !== undefined) meta.tags = fm.data.tags;
    const uploadAll = async (articleId: string) => {
      const urls = new Map<number, string>();
      for (const index of parsed.upload) {
        const file = images[index];
        if (file.size > MAX_ATTACHMENT_BYTES) { errors.push(`Картинка «${file.name}» не загружена: больше 25 МБ (${sizeText(file.size)}).`); continue; }
        try { const added = await client.upload(articleId, file); urls.set(index, `/files/${added.id}`); }
        catch (e) { errors.push(`Картинка «${file.name}» не загружена: ${(e as Error).message}`); }
      }
      return urls;
    };
    const rewrite = (urls: Map<number, string>) => rewriteImageUrls(body, analysis.images.filter(i => i.status === 'local' && urls.has(i.fileIndex)).map(i => ({ start: i.start, end: i.end, url: urls.get(i.fileIndex)! })));
    let article: Article;
    try {
      if (mode === 'new') {
        article = await client.createArticle({
          space_id: spaceId, project_id: corporate ? projectId : '', parent_id: '', title: trimmedTitle, summary: fm.data.summary ?? '', body,
          kind: 'article', status: 'draft', pinned: false, tags: fm.data.tags ?? [], properties: {}, author, message: 'Импорт Markdown',
        });
        const urls = await uploadAll(article.id);
        if (urls.size) {
          try { article = await client.updateArticle(article.id, { body: rewrite(urls), base_revision: article.revision, author, message: 'Импорт Markdown: вложения' }); }
          catch (e) { errors.push(`Статья создана, но ссылки на картинки не обновлены: ${(e as Error).message}`); }
        }
      } else {
        const current = await client.article(updateId);
        const urls = await uploadAll(current.id);
        // The server merges an update by the fields present in it, so everything not listed here (title, kind, status, parent...) stays as it is.
        try { article = await client.updateArticle(current.id, { body: rewrite(urls), ...meta, author, base_revision: current.revision, message: 'Импорт Markdown' }); }
        catch (e) {
          const text = (e as Error).message;
          setError(/409|conflict|revision/i.test(text) ? 'Статью изменили, пока шёл импорт. Закройте окно, откройте статью заново и повторите импорт; загруженные картинки остались во вложениях статьи.' : `Не удалось обновить статью: ${text}`);
          setSaving(false); return;
        }
      }
    } catch (e) {
      setError(`Не удалось сохранить: ${(e as Error).message}`); setSaving(false); return;
    }
    setSaving(false);
    if (errors.length) setReport({ article, errors }); else onImported(article);
  };

  const grouped = useMemo(() => {
    const map = new Map<WarningKind, ImportWarning[]>();
    for (const w of parsed?.warnings || []) map.set(w.kind, [...(map.get(w.kind) || []), w]);
    return [...map.entries()];
  }, [parsed]);

  const matchLabel = (a: Article) => `${a.title}${a.updated_at ? ` (обновлена ${new Date(a.updated_at).toLocaleDateString('ru-RU')})` : ''}`;

  return <div className="delete-dialog-backdrop md-import-backdrop" onMouseDown={event => { if (event.target === event.currentTarget && !saving && !report) onClose(); }}>
    <div ref={dialogRef} className="delete-dialog md-import" role="dialog" aria-modal="true" aria-labelledby="md-import-title" tabIndex={-1} onKeyDown={onKeyDown}>
      <div className="md-import-head"><h2 id="md-import-title">Импорт Markdown</h2><button className="icon-button" type="button" aria-label="Закрыть" disabled={saving} onClick={onClose}><X size={18} /></button></div>
      {report ? <div className="md-import-report" role="status">
        <p>Статья «{report.article.title}» сохранена, но не всё удалось выполнить:</p>
        <ul>{report.errors.map((e, i) => <li key={i}>{e}</li>)}</ul>
        <div className="md-import-actions"><button className="button primary" type="button" autoFocus onClick={() => onImported(report.article)}>Открыть статью</button></div>
      </div> : <>
        <div className={`md-import-drop${dragging ? ' dragging' : ''}`}
          onDragOver={event => { event.preventDefault(); setDragging(true); }} onDragLeave={() => setDragging(false)}
          onDrop={event => { event.preventDefault(); setDragging(false); if (!saving) addFiles(event.dataTransfer.files); }}>
          <FileText size={22} aria-hidden="true" />
          <div className="md-import-drop-text">
            <strong>{fileName || 'Перетащите сюда .md-файл и картинки'}</strong>
            <span>{fileName ? `${raw?.length ?? 0} символов` : 'Один файл до 2 МБ в UTF-8; картинки png, jpeg, gif, webp — по желанию'}</span>
          </div>
          <div className="md-import-pick">
            <button className="button ghost small" type="button" disabled={saving} onClick={() => markdownInput.current?.click()}><Upload size={14} /> {fileName ? 'Другой файл' : 'Выбрать .md'}</button>
            <button className="button ghost small" type="button" disabled={saving} onClick={() => imagesInput.current?.click()}><ImagePlus size={14} /> Картинки</button>
          </div>
          <input ref={markdownInput} type="file" accept=".md,.markdown,text/markdown" hidden onChange={event => { if (event.target.files) addFiles(Array.from(event.target.files).slice(0, 1)); event.target.value = ''; }} />
          <input ref={imagesInput} type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple hidden onChange={event => { if (event.target.files) addFiles(event.target.files); event.target.value = ''; }} />
        </div>
        {fileError && <p className="md-import-error" role="alert">{fileError}</p>}
        {images.length > 0 && <div className="md-import-images">
          <span>Картинки ({images.length}):</span>
          {images.map((f, i) => <span key={imageKey(f)} className={`md-import-chip${parsed && !parsed.upload.includes(i) ? ' unused' : ''}`} title={parsed && !parsed.upload.includes(i) ? 'Не используется в тексте' : f.name}>
            {f.name}<button type="button" aria-label={`Убрать ${f.name}`} disabled={saving} onClick={() => setImages(v => v.filter((_, n) => n !== i))}><X size={11} /></button></span>)}
        </div>}

        {parsed && <>
          <div className="md-import-form">
            <label className="md-import-field md-import-wide">Название
              <input value={title} maxLength={300} disabled={saving} onChange={event => { setTitle(event.target.value); setTitleTouched(true); }} /></label>
            <label className="md-import-field">Пространство
              <select value={spaceId} disabled={saving} onChange={event => setSpaceId(event.target.value)}>{usable.map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select></label>
            {corporate && <label className="md-import-field">Проект
              <select value={projectId} disabled={saving} onChange={event => setProjectId(event.target.value)}>
                {!projectRequired && <option value="">Без проекта</option>}
                {spaceProjects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
              </select></label>}
          </div>
          {projectRequired && !spaceProjects.length && <p className="md-import-error" role="alert">В этом пространстве нет доступных проектов: выберите другое.</p>}

          {matches.length > 0 && <fieldset className="md-import-match" disabled={saving}>
            <legend>Статья с таким названием уже есть</legend>
            <label><input type="radio" name="md-import-mode" checked={mode === 'new'} onChange={() => setMode('new')} /> Создать новую статью</label>
            {matches.length === 1
              ? <label><input type="radio" name="md-import-mode" checked={mode === 'update'} onChange={() => { setMode('update'); setUpdateId(matches[0].id); }} /> Обновить существующую: {matchLabel(matches[0])}</label>
              : <>
                <label><input type="radio" name="md-import-mode" checked={mode === 'update'} onChange={() => setMode('update')} /> Обновить существующую:</label>
                <select aria-label="Какую статью обновить" value={updateId} onChange={event => { setUpdateId(event.target.value); setMode('update'); }}>{matches.map(a => <option key={a.id} value={a.id}>{matchLabel(a)}</option>)}</select>
              </>}
            {mode === 'update' && <p className="md-import-hint">Текст статьи будет заменён; прежняя версия останется в истории.{parsed.fm.data.summary !== undefined || parsed.fm.data.tags !== undefined ? ' Описание и теги из front matter тоже заменят прежние.' : ''}</p>}
          </fieldset>}
          {matchNote && <p className="md-import-hint">{matchNote}</p>}

          <ul className="md-import-summary" aria-label="Сводка">
            <li><b>Название:</b> {trimmedTitle || '—'}</li>
            <li><b>Теги:</b> {parsed.fm.data.tags?.length ? parsed.fm.data.tags.join(', ') : 'нет'}</li>
            <li><b>Картинок к загрузке:</b> {parsed.upload.length}{unusedImages ? ` (лишних выбранных: ${unusedImages})` : ''}</li>
            <li><b>Предупреждений:</b> {parsed.warnings.length}</li>
          </ul>

          <div className="md-import-tabs" role="tablist">
            <button role="tab" type="button" aria-selected={tab === 'preview'} className={tab === 'preview' ? 'active' : ''} onClick={() => setTab('preview')}>Предпросмотр</button>
            <button role="tab" type="button" aria-selected={tab === 'report'} className={tab === 'report' ? 'active' : ''} onClick={() => setTab('report')}>Отчёт{parsed.warnings.length ? ` (${parsed.warnings.length})` : ''}</button>
          </div>
          <div className="md-import-panel" role="tabpanel">
            {tab === 'preview'
              ? <>{parsed.upload.length > 0 && <p className="md-import-hint">Локальные картинки появятся в статье после загрузки; в предпросмотре они не показаны.</p>}<Markdown body={parsed.fm.body} comment /></>
              : grouped.length ? grouped.map(([kind, list]) => <section key={kind} className="md-import-group">
                <h3>{WARNING_TITLES[kind]} <span>{list.length}</span></h3>
                <ul>{list.map((w, i) => <li key={i}>{w.line ? <span className="md-import-line">стр. {w.line}</span> : null} {w.message}</li>)}</ul>
              </section>) : <p className="md-import-hint">Предупреждений нет.</p>}
          </div>
        </>}

        {error && <p className="md-import-error" role="alert">{error}</p>}
        <div className="md-import-actions">
          <button className="button ghost" type="button" disabled={saving} onClick={onClose}>Отмена</button>
          <button className="button primary" type="button" disabled={!canSave} aria-busy={saving} onClick={() => void save()}>{saving ? 'Импортирую…' : mode === 'update' ? 'Обновить статью' : 'Создать статью'}</button>
        </div>
      </>}
    </div>
  </div>;
}
