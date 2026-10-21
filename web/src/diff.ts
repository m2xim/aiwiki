export type DiffLine = {
  type: 'context' | 'add' | 'remove' | 'gap';
  text: string;
  oldLine: number | null;
  newLine: number | null;
  omitted?: number;
};

export type LineDiff = {
  supported: boolean;
  reason?: string;
  additions: number;
  removals: number;
  lines: DiffLine[];
};

const MAX_TEXT_LENGTH = 2 * 1024 * 1024;
const MAX_LINE_COUNT = 4000;
const MAX_MATRIX_CELLS = 1_500_000;

/** Compute a line diff with bounded LCS work and a small context around edits. */
export function lineDiff(oldText: string, newText: string, context = 3): LineDiff {
  if (oldText.length > MAX_TEXT_LENGTH || newText.length > MAX_TEXT_LENGTH) {
    return unsupported('Документ больше 2 МБ: сравнение строк отключено для безопасной работы.');
  }
  const oldLines = oldText.length ? oldText.split('\n') : [];
  const newLines = newText.length ? newText.split('\n') : [];
  // Strip the common prefix and suffix first: a local edit in a huge article then only needs LCS on the changed middle.
  let prefix = 0;
  const maxCommon = Math.min(oldLines.length, newLines.length);
  while (prefix < maxCommon && oldLines[prefix] === newLines[prefix]) prefix++;
  let suffix = 0;
  while (suffix < maxCommon - prefix && oldLines[oldLines.length - 1 - suffix] === newLines[newLines.length - 1 - suffix]) suffix++;
  const oldMid = oldLines.slice(prefix, oldLines.length - suffix);
  const newMid = newLines.slice(prefix, newLines.length - suffix);
  const cells = (oldMid.length + 1) * (newMid.length + 1);
  if (oldMid.length > MAX_LINE_COUNT || newMid.length > MAX_LINE_COUNT || cells > MAX_MATRIX_CELLS) {
    return unsupported('Слишком много строк для расчёта сравнения в браузере.');
  }

  const width = newMid.length + 1;
  const table = new Uint32Array((oldMid.length + 1) * width);
  for (let i = oldMid.length - 1; i >= 0; i--) {
    for (let j = newMid.length - 1; j >= 0; j--) {
      table[i * width + j] = oldMid[i] === newMid[j]
        ? table[(i + 1) * width + j + 1] + 1
        : Math.max(table[(i + 1) * width + j], table[i * width + j + 1]);
    }
  }

  type Op = { type: 'context' | 'add' | 'remove'; text: string; oldLine: number | null; newLine: number | null };
  const ops: Op[] = [];
  for (let k = 0; k < prefix; k++) ops.push({ type: 'context', text: oldLines[k], oldLine: k + 1, newLine: k + 1 });
  let i = 0;
  let j = 0;
  while (i < oldMid.length || j < newMid.length) {
    if (i < oldMid.length && j < newMid.length && oldMid[i] === newMid[j]) {
      ops.push({ type: 'context', text: oldMid[i], oldLine: prefix + i + 1, newLine: prefix + j + 1 });
      i++; j++;
    } else if (j < newMid.length && (i === oldMid.length || table[i * width + j + 1] > table[(i + 1) * width + j])) {
      ops.push({ type: 'add', text: newMid[j], oldLine: null, newLine: prefix + j + 1 });
      j++;
    } else {
      ops.push({ type: 'remove', text: oldMid[i], oldLine: prefix + i + 1, newLine: null });
      i++;
    }
  }
  for (let k = 0; k < suffix; k++) {
    const o = oldLines.length - suffix + k; const n = newLines.length - suffix + k;
    ops.push({ type: 'context', text: oldLines[o], oldLine: o + 1, newLine: n + 1 });
  }

  const changed = ops.map((op, index) => op.type === 'context' ? -1 : index).filter(index => index >= 0);
  if (!changed.length) return { supported: true, additions: 0, removals: 0, lines: [] };
  const ranges: [number, number][] = [];
  for (const index of changed) {
    const start = Math.max(0, index - context);
    const end = Math.min(ops.length - 1, index + context);
    const last = ranges[ranges.length - 1];
    if (last && start <= last[1] + 1) last[1] = end;
    else ranges.push([start, end]);
  }
  const lines: DiffLine[] = [];
  ranges.forEach(([start, end], rangeIndex) => {
    if (rangeIndex && start > ranges[rangeIndex - 1][1] + 1) {
      lines.push({ type: 'gap', text: '', oldLine: null, newLine: null, omitted: start - ranges[rangeIndex - 1][1] - 1 });
    }
    lines.push(...ops.slice(start, end + 1));
  });
  return {
    supported: true,
    additions: ops.filter(op => op.type === 'add').length,
    removals: ops.filter(op => op.type === 'remove').length,
    lines,
  };
}

function unsupported(reason: string): LineDiff {
  return { supported: false, reason, additions: 0, removals: 0, lines: [] };
}

export type WordPart = { text: string; changed: boolean };
export type WordDiffResult = { old: WordPart[]; new: WordPart[] } | null;
const MAX_WORD_LINE = 2000;
const MAX_WORD_TOKENS = 600;
const tokenize = (text: string) => text.match(/\s+|[\p{L}\p{N}_]+|[^\s\p{L}\p{N}_]/gu) ?? [];
const pushPart = (parts: WordPart[], text: string, changed: boolean) => {
  const last = parts[parts.length - 1];
  if (last && last.changed === changed) last.text += text; else parts.push({ text, changed });
};

/** Intra-line diff of a removed/added line pair: tokens are words, whitespace runs and single punctuation marks.
 *  Returns null (render the plain lines) for very long lines or when the lines share nothing worth marking. */
export function wordDiff(oldText: string, newText: string, budget?: { cells: number }): WordDiffResult {
  if (oldText === newText || oldText.length > MAX_WORD_LINE || newText.length > MAX_WORD_LINE) return null;
  const a = tokenize(oldText); const b = tokenize(newText);
  if (a.length > MAX_WORD_TOKENS || b.length > MAX_WORD_TOKENS) return null;
  let prefix = 0;
  while (prefix < a.length && prefix < b.length && a[prefix] === b[prefix]) prefix++;
  let suffix = 0;
  while (suffix < a.length - prefix && suffix < b.length - prefix && a[a.length - 1 - suffix] === b[b.length - 1 - suffix]) suffix++;
  const am = a.slice(prefix, a.length - suffix); const bm = b.slice(prefix, b.length - suffix);
  if (budget) { const cells = (am.length + 1) * (bm.length + 1); if (cells > budget.cells) return null; budget.cells -= cells; }
  const width = bm.length + 1; const table = new Uint16Array((am.length + 1) * width);
  for (let i = am.length - 1; i >= 0; i--) for (let j = bm.length - 1; j >= 0; j--) {
    table[i * width + j] = am[i] === bm[j] ? table[(i + 1) * width + j + 1] + 1 : Math.max(table[(i + 1) * width + j], table[i * width + j + 1]);
  }
  const oldParts: WordPart[] = []; const newParts: WordPart[] = [];
  for (let k = 0; k < prefix; k++) { pushPart(oldParts, a[k], false); pushPart(newParts, a[k], false); }
  let i = 0; let j = 0; let common = prefix + suffix;
  while (i < am.length || j < bm.length) {
    if (i < am.length && j < bm.length && am[i] === bm[j]) { pushPart(oldParts, am[i], false); pushPart(newParts, bm[j], false); i++; j++; common++; }
    else if (j < bm.length && (i === am.length || table[i * width + j + 1] >= table[(i + 1) * width + j])) { pushPart(newParts, bm[j], true); j++; }
    else { pushPart(oldParts, am[i], true); i++; }
  }
  for (let k = suffix; k > 0; k--) { pushPart(oldParts, a[a.length - k], false); pushPart(newParts, b[b.length - k], false); }
  // Whitespace-only agreement is not similarity: fall back to whole-line colouring for unrelated lines.
  const shared = [...oldParts, ...newParts].filter(p => !p.changed && /[\p{L}\p{N}]/u.test(p.text)).length;
  if (!shared) return null;
  return { old: oldParts, new: newParts };
}

/** Pairs consecutive remove-runs with the add-run that follows them (index by line position in DiffLine[]). */
/** Total table cells that all intra-line diffs of one document may use; beyond it pairs are shown as whole lines. */
const WORD_DIFF_BUDGET = 200_000;
export function pairChangedLines(lines: DiffLine[]): Map<number, WordPart[]> {
  const result = new Map<number, WordPart[]>(); const budget = { cells: WORD_DIFF_BUDGET };
  let i = 0;
  while (i < lines.length) {
    if (lines[i].type !== 'remove') { i++; continue; }
    const rs = i; while (i < lines.length && lines[i].type === 'remove') i++;
    const as = i; while (i < lines.length && lines[i].type === 'add') i++;
    const pairs = Math.min(as - rs, i - as);
    for (let k = 0; k < pairs; k++) {
      const d = wordDiff(lines[rs + k].text, lines[as + k].text, budget);
      if (d) { result.set(rs + k, d.old); result.set(as + k, d.new); }
    }
  }
  return result;
}
