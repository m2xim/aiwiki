// Image gallery + lightbox for images stored in the artifact's own files (relative paths) or data: URLs.
import { h, resolve } from './dom.ts';
import type { Target } from './dom.ts';

export interface GalleryImage { src: string; alt?: string; caption?: string; thumb?: string }
export interface GalleryOptions { images?: (string | GalleryImage)[]; columns?: number }

const norm = (i: string | GalleryImage): GalleryImage => (typeof i === 'string' ? { src: i } : i);

/** Full-screen viewer: Esc closes, ←/→ navigate, click on the backdrop closes. */
export function lightbox(images: (string | GalleryImage)[], start = 0): { close(): void } {
  const list = images.map(norm);
  let i = Math.min(Math.max(0, start), list.length - 1);
  const prevFocus = document.activeElement as HTMLElement | null;
  const img = h('img', { class: 'aw-lb-img', alt: '' });
  const cap = h('div', { class: 'aw-lb-cap' });
  const count = h('div', { class: 'aw-lb-count aw-muted' });
  const btn = (cls: string, label: string, text: string, fn: () => void) => { const b = h('button', { type: 'button', class: `aw-lb-btn ${cls}`, 'aria-label': label }, text); b.addEventListener('click', e => { e.stopPropagation(); fn(); }); return b; };
  const go = (d: number) => { i = (i + d + list.length) % list.length; show(); };
  const close = () => { root.remove(); document.removeEventListener('keydown', onKey, true); prevFocus?.focus?.(); };
  const closeBtn = btn('aw-lb-close', 'Закрыть', '×', close);
  const root = h('div', { class: 'aw-lightbox', role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Просмотр изображения' },
    closeBtn, list.length > 1 ? btn('aw-lb-prev', 'Предыдущее', '‹', () => go(-1)) : null, h('figure', { class: 'aw-lb-fig' }, img, cap, count),
    list.length > 1 ? btn('aw-lb-next', 'Следующее', '›', () => go(1)) : null);
  function show() {
    const it = list[i];
    img.src = it.src; img.alt = it.alt ?? it.caption ?? '';
    cap.textContent = it.caption ?? it.alt ?? '';
    count.textContent = list.length > 1 ? `${i + 1} / ${list.length}` : '';
  }
  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') { e.stopPropagation(); close(); }
    else if (e.key === 'ArrowRight' && list.length > 1) go(1);
    else if (e.key === 'ArrowLeft' && list.length > 1) go(-1);
  }
  root.addEventListener('click', e => { if (e.target === root || e.target === (root.querySelector('.aw-lb-fig') as Element)) close(); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(root);
  show();
  closeBtn.focus();
  return { close };
}

/**
 * Thumbnail grid that opens the lightbox. Either pass `images`, or point at a container that already holds <img> elements
 * (they stay in place and become clickable).
 */
export function gallery(target: Target, options: GalleryOptions = {}): { el: HTMLElement } {
  const host = resolve(target, 'контейнер галереи');
  let items: GalleryImage[];
  let el: HTMLElement;
  if (options.images) {
    items = options.images.map(norm);
    el = h('div', { class: 'aw-gallery' });
    items.forEach((it, idx) => {
      const b = h('button', { type: 'button', class: 'aw-thumb', 'aria-label': it.caption || it.alt || `Изображение ${idx + 1}` }, h('img', { src: it.thumb || it.src, alt: it.alt ?? it.caption ?? '', loading: 'lazy' }));
      b.addEventListener('click', () => lightbox(items, idx));
      el.append(b);
    });
    host.append(el);
  } else {
    el = host;
    el.classList.add('aw-gallery');
    const imgs = [...el.querySelectorAll('img')];
    items = imgs.map(i => ({ src: i.currentSrc || i.src, alt: i.alt, caption: i.getAttribute('data-caption') || i.alt }));
    imgs.forEach((i, idx) => {
      i.classList.add('aw-thumb-img'); i.tabIndex = 0; i.setAttribute('role', 'button');
      i.addEventListener('click', () => lightbox(items, idx));
      i.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); lightbox(items, idx); } });
    });
  }
  if (options.columns) el.style.setProperty('--aw-gallery-cols', String(options.columns));
  return { el };
}
