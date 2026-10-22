import React from 'react';
import { Download, FileText, FileDown, X } from 'lucide-react';
import type { Article } from './api';
import './ArticleExportMenu.css';

type Kind = 'html' | 'pdf';

export function ArticleExportMenu({ article, spaceName }: { article: Article; spaceName?: string }) {
  const [open, setOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [warnings, setWarnings] = React.useState<string[]>([]);
  const [error, setError] = React.useState('');
  const root = React.useRef<HTMLDivElement>(null);
  const trigger = React.useRef<HTMLButtonElement>(null);

  React.useEffect(() => {
    if (!open && !warnings.length && !error) return;
    const onDown = (event: MouseEvent) => { if (root.current && !root.current.contains(event.target as Node)) { setOpen(false); setWarnings([]); setError(''); } };
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      setOpen(false); setWarnings([]); setError(''); trigger.current?.focus();
    };
    document.addEventListener('mousedown', onDown); document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open, warnings.length, error]);

  React.useEffect(() => { if (open) root.current?.querySelector<HTMLElement>('[role=menuitem]')?.focus(); }, [open]);

  const run = async (kind: Kind) => {
    setOpen(false); setWarnings([]); setError(''); setBusy(true);
    try {
      // Loaded on demand: keeps react-dom/server out of the main bundle until the first export.
      const mod = await import('./articleExport');
      if (kind === 'html') {
        const result = await mod.buildArticleExport(article, spaceName);
        mod.downloadHtml(article, result.html);
        setWarnings(result.warnings);
      } else {
        const result = await mod.buildArticlePdf(article, spaceName);
        mod.downloadPdf(article, result.blob);
        setWarnings(result.warnings);
      }
    } catch (e) { setError(e instanceof Error ? e.message : 'Не удалось подготовить экспорт.'); }
    finally { setBusy(false); }
  };

  const onMenuKey = (event: React.KeyboardEvent) => {
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    event.preventDefault();
    const items = [...root.current!.querySelectorAll<HTMLElement>('[role=menuitem]')];
    const index = items.indexOf(document.activeElement as HTMLElement);
    items[(index + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length]?.focus();
  };

  return <div className="export-menu" ref={root}>
    <button ref={trigger} type="button" className="button ghost" aria-haspopup="menu" aria-expanded={open} aria-busy={busy} disabled={busy}
      onClick={() => { setWarnings([]); setError(''); setOpen(value => !value); }}>
      <Download size={16}/>{busy ? 'Готовим…' : 'Экспорт'}
    </button>
    {open && <div className="export-menu-list" role="menu" aria-label="Экспорт статьи" onKeyDown={onMenuKey}>
      <button type="button" role="menuitem" onClick={() => void run('html')}><FileText size={15}/>HTML-файл</button>
      <button type="button" role="menuitem" onClick={() => void run('pdf')}><FileDown size={15}/>PDF-файл</button>
    </div>}
    {(warnings.length > 0 || error) && <div className="export-menu-note" role="status">
      <button type="button" className="icon-button" aria-label="Закрыть" onClick={() => { setWarnings([]); setError(''); }}><X size={14}/></button>
      {error ? <strong>{error}</strong> : <><strong>Экспорт выполнен с предупреждениями:</strong><ul>{warnings.map((w, i) => <li key={i}>{w}</li>)}</ul></>}
    </div>}
  </div>;
}
