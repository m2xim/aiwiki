// AIWiki.chart / lineChart / barChart / pieChart: thin wrapper over Chart.js 4.5.1 (tree-shaken registration).
// No date adapter is bundled, so there is no real time scale: dates become formatted category labels.
import {
  ArcElement, BarController, BarElement, CategoryScale, Chart, DoughnutController, Filler, Legend, LineController, LineElement,
  LinearScale, PieController, PointElement, ScatterController, Title, Tooltip,
} from 'chart.js';
import type { ChartConfiguration, ChartDataset, ChartType } from 'chart.js';
import { h, resolve } from './dom.ts';
import type { Target } from './dom.ts';
import { DEFAULT_LOCALE, formatValue, toDate } from './core/format.ts';
import type { FormatSpec } from './core/format.ts';
import { legendVisible } from './core/legend.ts';
import { cssVar, onThemeChange } from './theme.ts';

Chart.register(
  LineController, BarController, PieController, DoughnutController, ScatterController,
  LineElement, PointElement, BarElement, ArcElement, CategoryScale, LinearScale, Title, Tooltip, Legend, Filler,
);
Chart.defaults.locale = DEFAULT_LOCALE;
Chart.defaults.font.family = 'inherit';

export const PALETTE = ['#2f6fed', '#e8743b', '#19a974', '#a05bd6', '#e0b200', '#12a5b8', '#d6457a', '#7d8794'];

const live = new Set<Chart>();
function applyTheme() {
  Chart.defaults.color = cssVar('--aw-muted', '#5b6573');
  Chart.defaults.borderColor = cssVar('--aw-border', '#d9dee5');
  Chart.defaults.elements.arc.borderColor = cssVar('--aw-bg', '#ffffff');
  Chart.defaults.plugins.tooltip.titleColor = '#fff';
  Chart.defaults.plugins.tooltip.bodyColor = '#fff';
  Chart.defaults.plugins.tooltip.backgroundColor = 'rgba(20,24,31,0.92)';
  live.forEach(c => { try { c.update('none'); } catch { /* chart already destroyed */ } });
}
onThemeChange(applyTheme);

const reduceMotion = () => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;

export type Series = number[] | Record<string, number[]> | { label: string; data: (number | null)[]; color?: string }[];
export interface SimpleChartOptions {
  labels: (string | number | Date)[];
  series: Series;
  title?: string;
  height?: number;
  /** Axis/tooltip value format: 'number' | 'percent' | 'currency' | function. */
  format?: FormatSpec;
  currency?: string;
  digits?: number;
  stacked?: boolean;
  horizontal?: boolean;
  smooth?: boolean;
  fill?: boolean;
  legend?: boolean;
  /** Extra Chart.js options merged on top (shallow by section). */
  options?: Record<string, unknown>;
}
export interface ChartHandle {
  chart: Chart;
  el: HTMLElement;
  /** Replace labels/series (simple form) or labels/datasets (Chart.js form) and redraw. */
  update(data?: { labels?: SimpleChartOptions['labels']; series?: Series; datasets?: ChartDataset[] }): void;
  destroy(): void;
}

function normSeries(series: Series): { label: string; data: (number | null)[]; color?: string }[] {
  if (Array.isArray(series)) {
    if (series.length && typeof series[0] === 'object' && series[0] !== null) return series as { label: string; data: (number | null)[]; color?: string }[];
    return [{ label: 'Значение', data: series as number[] }];
  }
  return Object.entries(series).map(([label, data]) => ({ label, data }));
}

/** Labels: Date objects and date-like strings (ISO, dd.mm.yyyy) become dd.mm.yyyy; the rest is left as is. */
export function normLabels(labels: SimpleChartOptions['labels']): string[] {
  return labels.map(l => {
    if (l instanceof Date) return formatValue(l, 'date');
    if (typeof l === 'string' && /^\d{4}-\d{2}-\d{2}/.test(l) && toDate(l) !== null) return formatValue(l, /[T ]\d{2}:\d{2}/.test(l) ? 'datetime' : 'date');
    return String(l);
  });
}

function mount(target: Target, height: number | undefined): { wrap: HTMLElement; canvas: HTMLCanvasElement } {
  const host = resolve(target, 'контейнер графика');
  if (host instanceof HTMLCanvasElement) {
    const wrap = host.parentElement as HTMLElement;
    return { wrap, canvas: host };
  }
  const canvas = h('canvas', { role: 'img' });
  const wrap = h('div', { class: 'aw-chart' }, canvas);
  if (height) wrap.style.setProperty('--aw-chart-h', `${height}px`);
  host.append(wrap);
  return { wrap, canvas };
}

function withColors(type: ChartType, ds: ChartDataset[]): ChartDataset[] {
  return ds.map((d, i) => {
    const color = PALETTE[i % PALETTE.length];
    const o = d as unknown as Record<string, unknown>;
    if (type === 'pie' || type === 'doughnut') {
      if (!o.backgroundColor) o.backgroundColor = ((d.data as unknown[]) || []).map((_, j) => PALETTE[j % PALETTE.length]);
    } else if (type === 'line') {
      if (!o.borderColor) o.borderColor = color;
      if (!o.backgroundColor) o.backgroundColor = color + '33';
      if (!o.pointBackgroundColor) o.pointBackgroundColor = o.borderColor;
    } else {
      if (!o.backgroundColor) o.backgroundColor = color;
      if (!o.borderColor) o.borderColor = color;
    }
    return d;
  });
}

/**
 * Chart.js configuration with kit defaults (theme colours, ru-RU numbers, no animation under prefers-reduced-motion).
 * `target` is a selector/element (a canvas is created inside) or an existing <canvas>.
 */
export function chart(target: Target, config: ChartConfiguration & { height?: number }): ChartHandle {
  const { canvas } = mount(target, config.height);
  const cfg = { ...config } as ChartConfiguration;
  delete (cfg as { height?: number }).height;
  cfg.data = { ...cfg.data, datasets: withColors(cfg.type as ChartType, cfg.data.datasets) };
  const o = (cfg.options ??= {}) as Record<string, unknown>;
  o.responsive ??= true;
  o.maintainAspectRatio ??= false;
  if (reduceMotion()) o.animation ??= false;
  applyTheme();
  const c = new Chart(canvas, cfg);
  live.add(c);
  if (!canvas.getAttribute('aria-label')) canvas.setAttribute('aria-label', (cfg.options as { plugins?: { title?: { text?: string } } })?.plugins?.title?.text || 'График');
  return {
    chart: c,
    el: canvas,
    update(data) {
      if (data) {
        if (data.labels) c.data.labels = normLabels(data.labels);
        if (data.datasets) c.data.datasets = withColors(cfg.type as ChartType, data.datasets);
        else if (data.series) {
          const ds = normSeries(data.series).map((s, i) => ({ ...(c.data.datasets[i] ?? {}), label: s.label, data: s.data, ...(s.color ? { borderColor: s.color, backgroundColor: s.color } : {}) })) as ChartDataset[];
          c.data.datasets = withColors(cfg.type as ChartType, ds);
        }
      }
      c.update();
    },
    destroy() { live.delete(c); c.destroy(); },
  };
}

function simple(type: 'line' | 'bar' | 'pie' | 'doughnut', target: Target, o: SimpleChartOptions): ChartHandle {
  const fmt = (v: unknown) => formatValue(v, o.format ?? 'number', { currency: o.currency, digits: o.digits });
  const series = normSeries(o.series);
  const circular = type === 'pie' || type === 'doughnut';
  const datasets = series.map(s => {
    const d: Record<string, unknown> = { label: s.label, data: s.data };
    if (s.color) { d.backgroundColor = circular ? undefined : s.color; d.borderColor = s.color; }
    if (type === 'line') { d.tension = o.smooth === false ? 0 : 0.3; d.fill = !!o.fill; d.pointRadius = s.data.length > 60 ? 0 : 3; }
    if (type === 'bar') d.borderRadius = 3;
    return d as unknown as ChartDataset;
  });
  const plugins: Record<string, unknown> = {
    legend: { display: legendVisible(type, series.length, o.legend), position: circular ? 'right' : 'top' },
    title: { display: !!o.title, text: o.title ?? '', font: { size: 14, weight: '600' } },
    tooltip: { callbacks: { label: (ctx: { dataset: { label?: string }; parsed: unknown; raw: unknown }) => {
      const p = ctx.parsed as number | { x: number; y: number };
      const v = typeof p === 'object' && p !== null ? (o.horizontal ? p.x : p.y) : (ctx.raw as number);
      return `${ctx.dataset.label ? ctx.dataset.label + ': ' : ''}${fmt(v)}`;
    } } },
  };
  const options: Record<string, unknown> = { plugins, interaction: { mode: circular ? 'nearest' : 'index', intersect: circular } };
  if (!circular) {
    const valueAxis = { stacked: !!o.stacked, beginAtZero: type === 'bar', ticks: { callback: (v: unknown) => fmt(v) } };
    const catAxis = { stacked: !!o.stacked, grid: { display: false } };
    options.indexAxis = o.horizontal ? 'y' : 'x';
    options.scales = o.horizontal ? { x: valueAxis, y: catAxis } : { x: catAxis, y: valueAxis };
  }
  const extra = (o.options ?? {}) as Record<string, unknown>;
  Object.assign(options, extra, { plugins: { ...plugins, ...((extra.plugins as object) ?? {}) } });
  const h0 = chart(target, { type, data: { labels: normLabels(o.labels), datasets }, options, height: o.height } as ChartConfiguration & { height?: number });
  const baseUpdate = h0.update;
  h0.update = (data) => {
    if (data && (data.series || data.datasets)) {
      const n = data.datasets ? data.datasets.length : normSeries(data.series as Series).length;
      const lg = (h0.chart.options.plugins ??= {}).legend ??= {};
      if (!((extra.plugins as { legend?: { display?: boolean } } | undefined)?.legend?.display !== undefined)) lg.display = legendVisible(type, n, o.legend);
    }
    baseUpdate(data);
  };
  return h0;
}

export const lineChart = (target: Target, o: SimpleChartOptions) => simple('line', target, o);
export const barChart = (target: Target, o: SimpleChartOptions) => simple('bar', target, o);
export const pieChart = (target: Target, o: SimpleChartOptions & { donut?: boolean }) => simple(o.donut ? 'doughnut' : 'pie', target, o);

export { Chart };
