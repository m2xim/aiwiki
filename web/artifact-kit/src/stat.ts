// KPI cards: AIWiki.stat(target, {label, value, delta, format}); AIWiki.stats(target, [..]) renders a grid.
import { h, resolve } from './dom.ts';
import type { Target } from './dom.ts';
import { formatSigned, formatValue } from './core/format.ts';
import type { FormatSpec } from './core/format.ts';

export interface StatOptions {
  label: string;
  value: number | string | null | undefined;
  /** Number (formatted with deltaFormat, default percent: 0.123 = +12,3 %) or ready text. */
  delta?: number | string | null;
  deltaFormat?: FormatSpec;
  /** Which direction is good: 'up' (default, growth is green), 'down' (costs), 'none' (neutral). */
  good?: 'up' | 'down' | 'none';
  format?: FormatSpec;
  currency?: string;
  digits?: number;
  hint?: string;
  locale?: string;
}
export interface StatHandle { el: HTMLElement; update(patch: Partial<StatOptions>): void }

function paint(el: HTMLElement, o: StatOptions) {
  const fo = { currency: o.currency, digits: o.digits, locale: o.locale };
  const parts: Node[] = [
    h('div', { class: 'aw-stat-label' }, o.label),
    h('div', { class: 'aw-stat-value' }, o.value === null || o.value === undefined || o.value === '' ? '—' : formatValue(o.value, o.format ?? (typeof o.value === 'number' ? 'number' : undefined), fo)),
  ];
  if (o.delta !== undefined && o.delta !== null && o.delta !== '') {
    const n = typeof o.delta === 'number' ? o.delta : null;
    const good = o.good ?? 'up';
    const tone = n === null || n === 0 || good === 'none' ? 'flat' : (n > 0) === (good === 'up') ? 'good' : 'bad';
    const arrow = n === null || n === 0 ? '' : n > 0 ? '▲ ' : '▼ ';
    parts.push(h('div', { class: `aw-stat-delta aw-${tone}` }, arrow + (n === null ? String(o.delta) : formatSigned(n, o.deltaFormat ?? 'percent', fo))));
  }
  if (o.hint) parts.push(h('div', { class: 'aw-stat-hint aw-muted' }, o.hint));
  el.replaceChildren(...parts);
}

export function stat(target: Target, options: StatOptions): StatHandle {
  const host = resolve(target, 'контейнер карточки');
  let o = { ...options };
  const el = h('div', { class: 'aw-stat' });
  host.append(el);
  paint(el, o);
  return { el, update(patch) { o = { ...o, ...patch }; paint(el, o); } };
}

export function stats(target: Target, items: StatOptions[]): StatHandle[] {
  const host = resolve(target, 'контейнер карточек');
  const grid = h('div', { class: 'aw-stats' });
  host.append(grid);
  return items.map(i => stat(grid, i));
}
