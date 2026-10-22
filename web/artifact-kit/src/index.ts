// AI Wiki Kit entry: the IIFE build exposes everything below as the global `AIWiki`.
import './kit.css';
import { setTheme, getTheme } from './theme.ts';
import { startAutoResize, autoResize } from './resize.ts';
import { effect, state } from './core/state.ts';
import { bind } from './controls.ts';
import { table } from './table.ts';
import { Chart, barChart, chart, lineChart, pieChart } from './chart.ts';
import { stat, stats } from './stat.ts';
import { gallery, lightbox } from './gallery.ts';
import { formatSigned, formatValue, toDate, toNumber } from './core/format.ts';
import { parseCSV, parseData, toCSV } from './core/csv.ts';

export const version = '1.0.0';
export {
  state, effect, bind, table, chart, lineChart, barChart, pieChart, stat, stats, gallery, lightbox, setTheme, getTheme, autoResize,
  parseCSV, parseData, toCSV, toNumber, toDate, Chart,
};
/** Locale-aware formatter: AIWiki.format(1234.5, 'currency') -> «1 234,50 ₽». */
export const format = formatValue;
export const formatDelta = formatSigned;

if (typeof document !== 'undefined') {
  startAutoResize();
  const auto = () => document.querySelectorAll<HTMLElement>('[data-aiwiki-gallery]').forEach(el => gallery(el));
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', auto, { once: true }); else auto();
}
