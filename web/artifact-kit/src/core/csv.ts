// RFC 4180 CSV parser/serializer (no DOM). Handles quotes, "" escapes, CR/LF/CRLF, newlines in fields, BOM.

export interface ParsedTable { columns: string[]; rows: Record<string, string>[] }

export function detectDelimiter(text: string): string {
  const counts: Record<string, number> = { ',': 0, ';': 0, '\t': 0 };
  let q = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '"') q = !q;
    else if (!q && (c === '\n' || c === '\r')) break;
    else if (!q && c in counts) counts[c]++;
  }
  let best = ',';
  for (const d of [',', ';', '\t']) if (counts[d] > counts[best]) best = d;
  return best;
}

/** Parse to a matrix of strings. Fully empty trailing lines are dropped. */
export function parseCSVMatrix(input: string, delimiter?: string): string[][] {
  const text = input.charCodeAt(0) === 0xfeff ? input.slice(1) : input;
  const d = delimiter || detectDelimiter(text);
  const out: string[][] = [];
  let row: string[] = [], field = '', q = false, i = 0, touched = false;
  const endField = () => { row.push(field); field = ''; };
  const endRow = () => { endField(); out.push(row); row = []; touched = false; };
  while (i < text.length) {
    const c = text[i];
    if (q) {
      if (c === '"') { if (text[i + 1] === '"') { field += '"'; i += 2; continue; } q = false; i++; continue; }
      field += c; i++; continue;
    }
    if (c === '"' && field === '') { q = true; touched = true; i++; continue; }
    if (c === d) { endField(); touched = true; i++; continue; }
    if (c === '\r') { endRow(); i += text[i + 1] === '\n' ? 2 : 1; continue; }
    if (c === '\n') { endRow(); i++; continue; }
    field += c; touched = true; i++;
  }
  if (touched || field !== '' || row.length) endRow();
  while (out.length && out[out.length - 1].length <= 1 && out[out.length - 1].every(v => v === '')) out.pop();
  return out;
}

/** CSV with a header row -> {columns, rows: objects}. Empty/duplicate header names get unique names. */
export function parseCSV(input: string, delimiter?: string): ParsedTable {
  const m = parseCSVMatrix(input, delimiter);
  if (!m.length) return { columns: [], rows: [] };
  const seen = new Set<string>();
  const columns = m[0].map((h, idx) => {
    const base = h.trim() || `Колонка ${idx + 1}`;
    let name = base;
    for (let n = 2; seen.has(name); n++) name = `${base} (${n})`;
    seen.add(name);
    return name;
  });
  const rows = m.slice(1).map(r => {
    const o: Record<string, string> = {};
    columns.forEach((c, idx) => { o[c] = r[idx] ?? ''; });
    return o;
  });
  return { columns, rows };
}

function quote(v: unknown, d: string): string {
  if (v === null || v === undefined) return '';
  const s = v instanceof Date ? v.toISOString() : String(v);
  return /["\r\n]/.test(s) || s.includes(d) || /^\s|\s$/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/** Matrix -> CSV text (CRLF line ends, no BOM; the caller adds it for Excel). */
export function toCSV(matrix: unknown[][], delimiter = ','): string {
  return matrix.map(r => r.map(v => quote(v, delimiter)).join(delimiter)).join('\r\n');
}

/** JSON text ([{...}] or {columns, rows}) or CSV text -> rows of objects. */
export function parseData(text: string): { columns: string[] | null; rows: Record<string, unknown>[] } {
  const t = text.replace(/^﻿/, '').trim();
  if (t.startsWith('[') || t.startsWith('{')) {
    const j = JSON.parse(t);
    if (Array.isArray(j)) return { columns: null, rows: j };
    if (j && Array.isArray(j.rows)) return { columns: Array.isArray(j.columns) ? j.columns.map(String) : null, rows: j.rows };
    throw new Error('JSON: ожидается массив объектов или {columns, rows}');
  }
  const p = parseCSV(t);
  return { columns: p.columns, rows: p.rows };
}
