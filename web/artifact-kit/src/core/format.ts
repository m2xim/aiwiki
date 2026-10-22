// Pure value coercion and formatting (no DOM). Russian locale by default.

export type FormatName = 'number' | 'integer' | 'percent' | 'currency' | 'date' | 'datetime' | 'text';
export type FormatSpec = FormatName | ((value: unknown) => string) | undefined;
export interface FormatOptions { locale?: string; currency?: string; digits?: number }

export const DEFAULT_LOCALE = 'ru-RU';

export function isEmpty(v: unknown): boolean {
  return v === null || v === undefined || (typeof v === 'string' && v.trim() === '') || (typeof v === 'number' && Number.isNaN(v));
}

const NUM_RE = /^[-+]?(?:\d+(?:[.,]\d+)?|[.,]\d+)(?:e[-+]?\d+)?$/i;

/** Number or numeric string ("1 234,5", "12.5", "−3") -> number; anything else -> null. */
export function toNumber(v: unknown): number | null {
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v !== 'string') return null;
  const s = v.replace(/[\s  ]/g, '').replace('−', '-');
  if (!NUM_RE.test(s)) return null;
  if (/^[-+]?0\d/.test(s)) return null; // "007" is an identifier, not a number
  const n = Number(s.replace(',', '.'));
  return Number.isFinite(n) ? n : null;
}

const ISO_RE = /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$/;
const RU_DATE_RE = /^(\d{1,2})\.(\d{1,2})\.(\d{4})(?:\s+(\d{1,2}):(\d{2})(?::(\d{2}))?)?$/;

/** Date, ISO string (yyyy-mm-dd[Thh:mm[:ss]][Z]) or dd.mm.yyyy -> timestamp in ms; otherwise null. */
export function toDate(v: unknown): number | null {
  if (v instanceof Date) { const t = v.getTime(); return Number.isNaN(t) ? null : t; }
  if (typeof v !== 'string') return null;
  const s = v.trim();
  let m = ISO_RE.exec(s);
  let y: number, mo: number, d: number, h = 0, mi = 0, sec = 0;
  if (m) {
    y = +m[1]; mo = +m[2]; d = +m[3]; h = +(m[4] ?? 0); mi = +(m[5] ?? 0); sec = +(m[6] ?? 0);
    if (m[7]) { const t = Date.parse(s.replace(' ', 'T')); return Number.isNaN(t) ? null : t; }
  } else if ((m = RU_DATE_RE.exec(s))) {
    d = +m[1]; mo = +m[2]; y = +m[3]; h = +(m[4] ?? 0); mi = +(m[5] ?? 0); sec = +(m[6] ?? 0);
  } else return null;
  if (mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 59) return null;
  const date = new Date(y, mo - 1, d, h, mi, sec);
  if (date.getMonth() !== mo - 1 || date.getDate() !== d) return null; // 31 February
  return date.getTime();
}

const nfCache = new Map<string, Intl.NumberFormat>();
function nf(locale: string, o: Intl.NumberFormatOptions): Intl.NumberFormat {
  const key = locale + JSON.stringify(o);
  let f = nfCache.get(key);
  if (!f) { f = new Intl.NumberFormat(locale, o); nfCache.set(key, f); }
  return f;
}
const dfCache = new Map<string, Intl.DateTimeFormat>();
function df(locale: string, o: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const key = locale + JSON.stringify(o);
  let f = dfCache.get(key);
  if (!f) { f = new Intl.DateTimeFormat(locale, o); dfCache.set(key, f); }
  return f;
}

/** Format a cell/KPI value. Empty values give an empty string; unparsable values are returned as text. */
export function formatValue(v: unknown, fmt?: FormatSpec, opts: FormatOptions = {}): string {
  if (typeof fmt === 'function') return String(fmt(v));
  if (isEmpty(v)) return '';
  const locale = opts.locale || DEFAULT_LOCALE;
  if (typeof v === 'boolean') return v ? 'Да' : 'Нет';
  switch (fmt) {
    case 'number': case 'integer': {
      const n = toNumber(v); if (n === null) return String(v);
      const digits = fmt === 'integer' ? 0 : (opts.digits ?? 2);
      return nf(locale, { maximumFractionDigits: digits }).format(n);
    }
    case 'percent': {
      const n = toNumber(v); if (n === null) return String(v);
      return nf(locale, { style: 'percent', maximumFractionDigits: opts.digits ?? 1 }).format(n);
    }
    case 'currency': {
      const n = toNumber(v); if (n === null) return String(v);
      const o: Intl.NumberFormatOptions = { style: 'currency', currency: opts.currency || 'RUB' };
      if (opts.digits !== undefined) { o.minimumFractionDigits = opts.digits; o.maximumFractionDigits = opts.digits; }
      return nf(locale, o).format(n);
    }
    case 'date': case 'datetime': {
      const t = toDate(v); if (t === null) return String(v);
      const o: Intl.DateTimeFormatOptions = { day: '2-digit', month: '2-digit', year: 'numeric' };
      if (fmt === 'datetime') { o.hour = '2-digit'; o.minute = '2-digit'; }
      return df(locale, o).format(t);
    }
    default:
      if (v instanceof Date) { const t = v.getTime(); return Number.isNaN(t) ? '' : df(locale, { day: '2-digit', month: '2-digit', year: 'numeric' }).format(t); }
      return String(v);
  }
}

/** Number with explicit sign for deltas: +12,3 % / −4 / 0. */
export function formatSigned(n: number, fmt: FormatSpec = 'percent', opts: FormatOptions = {}): string {
  const s = formatValue(Math.abs(n), fmt, opts);
  if (n > 0) return '+' + s;
  if (n < 0) return '−' + s;
  return s;
}
