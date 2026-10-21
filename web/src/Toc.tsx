import React, { useEffect, useMemo, useRef, useState } from 'react';
import { ChevronRight } from 'lucide-react';
import { replaceUrl } from './nav';

export type TocHeading = { level: number; text: string; id: string };
type Props = {
  headings: TocHeading[];
  /** Open (unresolved) comments per heading id. */
  commentCounts?: Record<string, number>;
  /** Called after Toc has scrolled to the heading. When omitted, Toc only updates the URL hash itself. */
  onNavigate?: (id: string) => void;
};

const COLLAPSE_AFTER = 25;
const prefersReducedMotion = () => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
const scrollParent = (el: HTMLElement | null): HTMLElement | null => {
  for (let node = el?.parentElement ?? null; node; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if ((overflow === 'auto' || overflow === 'scroll') && node.scrollHeight > node.clientHeight) return node;
  }
  return null;
};

export function Toc({ headings, commentCounts, onNavigate }: Props) {
  const [activeId, setActiveId] = useState('');
  const [manual, setManual] = useState<Set<string>>(() => new Set());
  const listRef = useRef<HTMLElement>(null);
  const idsKey = headings.map(h => h.id).join('\n');

  // Relative level: the shallowest heading in the article is level 1 of the outline.
  const minLevel = useMemo(() => headings.reduce((min, h) => Math.min(min, h.level), 6), [headings]);
  const collapsible = headings.length > COLLAPSE_AFTER;
  // For every heading: its section root (nearest heading of relative level <= 2) and whether the root has hidden children.
  const model = useMemo(() => {
    const rootOf: string[] = []; const hasChildren = new Set<string>(); let root = '';
    headings.forEach(h => {
      const rel = h.level - minLevel + 1;
      if (rel <= 2) root = h.id; else if (root) hasChildren.add(root);
      rootOf.push(rel <= 2 ? h.id : root);
    });
    return { rootOf, hasChildren };
  }, [headings, minLevel]);
  const activeRoot = activeId ? model.rootOf[headings.findIndex(h => h.id === activeId)] ?? '' : '';
  const isExpanded = (rootId: string) => manual.has(rootId) || rootId === activeRoot;

  useEffect(() => { setManual(new Set()); setActiveId(''); }, [idsKey]);

  // Scroll-spy. Headings may be rendered after this component, so missing anchors are retried briefly.
  useEffect(() => {
    if (!headings.length || typeof IntersectionObserver === 'undefined') return;
    let observer: IntersectionObserver | null = null; let retry = 0; let timer = 0; let raf = 0;
    const recompute = () => {
      raf = 0;
      // Active = the last heading whose top has passed the reading line; the first heading before any has.
      let current = headings[0].id;
      for (const h of headings) {
        const el = document.getElementById(h.id); if (!el) continue;
        if (el.getBoundingClientRect().top <= 110) current = h.id; else break;
      }
      if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2 && document.documentElement.scrollHeight > window.innerHeight) {
        // Bottom of the page: short last sections can never reach the reading line, so prefer the last visible heading.
        for (let i = headings.length - 1; i >= 0; i--) { const el = document.getElementById(headings[i].id); if (el && el.getBoundingClientRect().top < window.innerHeight) { current = headings[i].id; break; } }
      }
      setActiveId(current);
    };
    const schedule = () => { if (!raf) raf = requestAnimationFrame(recompute); };
    const attach = () => {
      const els = headings.map(h => document.getElementById(h.id)).filter((el): el is HTMLElement => !!el);
      if (els.length < headings.length && retry++ < 8) { timer = window.setTimeout(attach, 150 * retry); }
      if (!els.length) return;
      observer?.disconnect();
      observer = new IntersectionObserver(schedule, { rootMargin: '-110px 0px -55% 0px', threshold: [0, 1] });
      els.forEach(el => observer!.observe(el));
      schedule();
    };
    attach();
    window.addEventListener('scroll', schedule, { passive: true }); window.addEventListener('resize', schedule);
    return () => { observer?.disconnect(); window.clearTimeout(timer); if (raf) cancelAnimationFrame(raf); window.removeEventListener('scroll', schedule); window.removeEventListener('resize', schedule); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [idsKey]);

  // Keep the active item visible inside the (separately scrollable) outline without moving the page.
  useEffect(() => {
    if (!activeId) return;
    const link = listRef.current?.querySelector<HTMLElement>('a[aria-current="location"]'); if (!link) return;
    const parent = scrollParent(link); if (!parent) return;
    const a = link.getBoundingClientRect(); const p = parent.getBoundingClientRect();
    if (a.top < p.top + 8) parent.scrollTop += a.top - p.top - 24;
    else if (a.bottom > p.bottom - 8) parent.scrollTop += a.bottom - p.bottom + 24;
  }, [activeId, idsKey]);

  const go = (event: React.MouseEvent, id: string) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    document.getElementById(id)?.scrollIntoView({ behavior: prefersReducedMotion() ? 'auto' : 'smooth', block: 'start' });
    setActiveId(id);
    if (onNavigate) onNavigate(id); else replaceUrl(`${location.pathname}${location.search}#${encodeURIComponent(id)}`);
  };
  const toggle = (rootId: string) => setManual(prev => { const next = new Set(prev); if (next.has(rootId)) next.delete(rootId); else next.add(rootId); return next; });

  if (!headings.length) return <nav className="toc-list" ref={listRef} aria-label="Оглавление"><span className="muted small-text">В статье нет заголовков</span></nav>;
  return <nav className="toc-list" ref={listRef} aria-label="Оглавление">{headings.map((h, index) => {
    const rel = h.level - minLevel + 1; const rootId = model.rootOf[index];
    // Sub-headings before the first level-1/2 heading have no section root and are always shown.
    if (collapsible && rel > 2 && rootId && !isExpanded(rootId)) return null;
    const expandable = collapsible && rel <= 2 && model.hasChildren.has(h.id);
    const expanded = expandable && isExpanded(h.id);
    const count = commentCounts?.[h.id] || 0; const active = h.id === activeId;
    return <div key={h.id} className={`toc-item${active ? ' active' : ''}`}>
      {collapsible && <span className="toc-toggle-slot">{expandable && <button type="button" className="toc-toggle" aria-expanded={expanded} aria-label={`${expanded ? 'Свернуть' : 'Развернуть'} подразделы: ${h.text}`} disabled={h.id === activeRoot} onClick={() => toggle(h.id)}><ChevronRight size={12} className={expanded ? 'open' : ''}/></button>}</span>}
      <a className={`toc-link${active ? ' active' : ''}`} style={{ paddingLeft: 12 + (rel - 1) * 12 }} href={`#${h.id}`} aria-current={active ? 'location' : undefined} title={h.text} onClick={event => go(event, h.id)}>{h.text}</a>
      {count > 0 && <span className="toc-badge" role="img" aria-label={`Открытых комментариев: ${count}`} title={`Открытых комментариев: ${count}`}>{count}</span>}
    </div>;
  })}</nav>;
}
