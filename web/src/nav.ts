import { useCallback, useEffect, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent, PointerEvent as ReactPointerEvent } from 'react';

export type Route = { article: string; space: string; view: string; anchor: string };

export function readRoute(): Route {
  const params = new URLSearchParams(location.search);
  let anchor = location.hash.slice(1);
  try { anchor = decodeURIComponent(anchor); } catch { /* keep the malformed fragment as text */ }
  return { article: params.get('article') || '', space: params.get('space') || '', view: params.get('view') || '', anchor };
}

export const articleUrl = (id: string, opts: { view?: string; anchor?: string } = {}) =>
  `/?article=${encodeURIComponent(id)}${opts.view ? `&view=${opts.view}` : ''}${opts.anchor ? `#${encodeURIComponent(opts.anchor)}` : ''}`;
export const spaceUrl = (space: string, view = '') =>
  space ? `/?space=${encodeURIComponent(space)}${view ? `&view=${view}` : ''}` : '/';
export const currentUrl = () => `${location.pathname}${location.search}${location.hash}`;

/** Last URL written by the app; shared so any module can keep it in sync and a refused Back step can restore it. */
export const lastUrlRef = { current: currentUrl() };
export const replaceUrl = (url: string) => { history.replaceState(history.state, '', url); lastUrlRef.current = url; };

export const UNSAVED_MESSAGE = 'Есть несохранённые изменения. Уйти без сохранения?';

/** True when the keyboard focus sits in a text field, select or rich editor (where global hotkeys must not fire). */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  return ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName) || Boolean(target.closest('.cm-editor'));
}

const widthKey = 'aiwiki.sidebarWidth';
export const SIDEBAR_MIN = 220; export const SIDEBAR_MAX = 600; export const SIDEBAR_DEFAULT = 260;
const clampWidth = (value: number) => Math.min(SIDEBAR_MAX, Math.max(SIDEBAR_MIN, Math.round(value)));
const readWidth = (): number | null => {
  try { const raw = localStorage.getItem(widthKey); if (!raw) return null; const value = Number(raw); return Number.isFinite(value) ? clampWidth(value) : null; } catch { return null; }
};
const storeWidth = (value: number | null) => {
  try { if (value === null) localStorage.removeItem(widthKey); else localStorage.setItem(widthKey, String(value)); } catch { /* width only lasts for this session */ }
};

/** Resizable left panel: pointer drag, keyboard, double-click reset, persisted in localStorage. */
export function useSidebarWidth(active = true) {
  const [width, setWidth] = useState<number | null>(readWidth);
  const [box, setBox] = useState<{ right: number; width: number } | null>(null);
  const [dragging, setDragging] = useState(false);
  const sidebarRef = useRef<HTMLElement | null>(null);
  const workspaceRef = useRef<HTMLDivElement | null>(null);
  const widthRef = useRef(width); widthRef.current = width;
  const boxRef = useRef(box); boxRef.current = box;

  // The handle is position:fixed and follows the real right edge of the sidebar, whatever the breakpoint default is.
  useEffect(() => {
    const element = sidebarRef.current; if (!active || !element) return;
    const measure = () => { const rect = element.getBoundingClientRect(); setBox(prev => rect.width <= 0 ? null : prev && prev.right === rect.right && prev.width === rect.width ? prev : { right: rect.right, width: rect.width }); };
    measure();
    const observer = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(measure) : null;
    observer?.observe(element);
    window.addEventListener('resize', measure);
    return () => { observer?.disconnect(); window.removeEventListener('resize', measure); };
  }, [active]);

  const commit = useCallback((value: number | null) => { setWidth(value); storeWidth(value); }, []);
  const current = () => widthRef.current ?? Math.round(boxRef.current?.width || SIDEBAR_DEFAULT);

  const onPointerDown = useCallback((event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !sidebarRef.current) return;
    event.preventDefault();
    const handle = event.currentTarget; handle.setPointerCapture(event.pointerId);
    const left = sidebarRef.current.getBoundingClientRect().left;
    let latest = current();
    setDragging(true);
    const move = (e: PointerEvent) => {
      latest = clampWidth(e.clientX - left);
      workspaceRef.current?.style.setProperty('--left-w', `${latest}px`);
      workspaceRef.current?.classList.add('left-custom');
    };
    const end = () => {
      handle.removeEventListener('pointermove', move); handle.removeEventListener('pointerup', end); handle.removeEventListener('pointercancel', end);
      setDragging(false); commit(latest);
    };
    handle.addEventListener('pointermove', move); handle.addEventListener('pointerup', end); handle.addEventListener('pointercancel', end);
  }, [commit]);

  const onKeyDown = useCallback((event: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = event.shiftKey ? 48 : 16;
    if (event.key === 'ArrowLeft') { event.preventDefault(); commit(clampWidth(current() - step)); }
    else if (event.key === 'ArrowRight') { event.preventDefault(); commit(clampWidth(current() + step)); }
    else if (event.key === 'Home') { event.preventDefault(); commit(SIDEBAR_MIN); }
    else if (event.key === 'End') { event.preventDefault(); commit(SIDEBAR_MAX); }
    else if (event.key === 'Enter') { event.preventDefault(); commit(null); }
  }, [commit]);

  const reset = useCallback(() => { commit(null); }, [commit]);
  return { width, handleLeft: box ? box.right : null, shownWidth: width ?? Math.round(box?.width || SIDEBAR_DEFAULT), dragging, sidebarRef, workspaceRef, onPointerDown, onKeyDown, reset, current };
}
