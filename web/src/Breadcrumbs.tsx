import React, { useLayoutEffect, useRef, useState } from 'react';
import { ChevronRight } from 'lucide-react';

export type Crumb = { id: string; title: string };
type Props = { spaceName: string; ancestors: Crumb[]; current: string; onSpace: () => void; onOpen: (id: string) => void };

/**
 * «Пространство › предок › … › родитель › Статья». When the chain does not fit the real available width the middle
 * collapses into «…» (level 1: space … parent current; level 2: space … current); remaining overflow is cut with ellipsis.
 */
export function Breadcrumbs({ spaceName, ancestors, current, onSpace, onOpen }: Props) {
  const ref = useRef<HTMLElement>(null);
  const [level, setLevel] = useState(0);
  const [tick, setTick] = useState(0);
  const chainKey = `${spaceName}|${current}|${ancestors.map(a => a.id + a.title).join('/')}`;

  // Re-measure from the full chain whenever the content or the available width changes.
  useLayoutEffect(() => { setLevel(0); }, [chainKey, tick]);
  useLayoutEffect(() => {
    const el = ref.current; if (!el) return;
    // The current title may shrink with an ellipsis; collapse ancestors only when they themselves do not fit.
    // A link capped by its own max-width is fine; only links squeezed below that cap by lack of room count.
    const squeezed = (child: Element) => {
      const cap = parseFloat(getComputedStyle(child).maxWidth);
      return child.clientWidth + 1 < Math.min(child.scrollWidth, Number.isFinite(cap) ? cap : Infinity);
    };
    const truncated = el.scrollWidth > el.clientWidth + 1 || Array.from(el.querySelectorAll('.crumb-link')).some(squeezed);
    if (truncated && level < 2 && ancestors.length > (level === 0 ? 1 : 0)) setLevel(level + 1);
  }, [level, chainKey, tick, ancestors.length]);
  useLayoutEffect(() => {
    const el = ref.current; if (!el || typeof ResizeObserver === 'undefined') return;
    let width = el.clientWidth;
    const observer = new ResizeObserver(() => { if (Math.abs(el.clientWidth - width) > 1) { width = el.clientWidth; setTick(t => t + 1); } });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const sep = (key: string) => <ChevronRight key={key} size={13} className="crumb-sep" aria-hidden="true"/>;
  const link = (a: Crumb) => <button type="button" key={a.id} className="crumb-link" onClick={() => onOpen(a.id)} title={a.title || 'Без названия'}>{a.title || 'Без названия'}</button>;
  const shown = level === 0 ? ancestors : level === 1 ? ancestors.slice(-1) : [];
  const hidden = ancestors.slice(0, ancestors.length - shown.length);
  const out: React.ReactNode[] = [<button type="button" key="space" className="crumb-link" onClick={onSpace} title={spaceName}>{spaceName || 'Пространство'}</button>];
  if (hidden.length) out.push(sep('s-ellipsis'), <span key="ellipsis" className="crumb-ellipsis" title={hidden.map(a => a.title).join(' › ')}>…</span>);
  shown.forEach(a => out.push(sep(`s${a.id}`), link(a)));
  out.push(sep('s-current'), <span key="current" className="crumb-current" aria-current="page" title={current}>{current || 'Без названия'}</span>);
  return <nav ref={ref} data-level={level} className="breadcrumbs" aria-label="Путь к статье">{out}</nav>;
}
