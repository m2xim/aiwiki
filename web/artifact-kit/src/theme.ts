// Theme: follows prefers-color-scheme of the frame document (the iframe cannot see the parent's theme),
// the wiki's ?theme=light|dark frame parameter (applied once at start), or a forced 'light' / 'dark' set by AIWiki.setTheme(). CSS reacts to the data-aiwiki-theme attribute.

import { parseThemeParam } from './core/themeparam.ts';

export type ThemeMode = 'light' | 'dark' | 'auto';

let mode: ThemeMode = 'auto';
const listeners = new Set<() => void>();
const mq = typeof matchMedia === 'function' ? matchMedia('(prefers-color-scheme: dark)') : null;

export function getTheme(): 'light' | 'dark' {
  return mode === 'auto' ? (mq?.matches ? 'dark' : 'light') : mode;
}
export function getThemeMode(): ThemeMode { return mode; }

export function setTheme(next: ThemeMode): void {
  if (next !== 'light' && next !== 'dark' && next !== 'auto') throw new Error("AIWiki.setTheme: 'light' | 'dark' | 'auto'");
  mode = next;
  const root = document.documentElement;
  if (next === 'auto') root.removeAttribute('data-aiwiki-theme'); else root.setAttribute('data-aiwiki-theme', next);
  listeners.forEach(l => l());
}

/** Subscribe to the effective theme change (OS switch or setTheme). Returns unsubscribe. */
export function onThemeChange(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

mq?.addEventListener?.('change', () => { if (mode === 'auto') listeners.forEach(l => l()); });

export function cssVar(name: string, fallback = ''): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback;
}

// Start-up: ?theme=light|dark from the frame URL; a later AIWiki.setTheme() by the author overrides it.
if (typeof location !== 'undefined' && typeof document !== 'undefined') {
  const initial = parseThemeParam(location.search);
  if (initial) setTheme(initial);
}
