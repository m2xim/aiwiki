// Pure text transformations behind the Markdown formatting toolbar.
// Every function takes the whole document plus a selection and returns one replacement
// (`from`..`to` becomes `insert`) and the selection to place afterwards, so the editor can
// apply it as a single CodeMirror transaction (one undo step).
export type FormatEdit = { from: number; to: number; insert: string; anchor: number; head: number };
export type LineKind = 'h1' | 'h2' | 'h3' | 'ul' | 'ol' | 'task' | 'quote';

const lineStartOf = (doc: string, pos: number) => doc.lastIndexOf('\n', pos - 1) + 1;
const lineEndOf = (doc: string, pos: number) => { const i = doc.indexOf('\n', pos); return i < 0 ? doc.length : i; };

/** Wraps the selection in `mark` (e.g. `**`), or unwraps it when it is already wrapped. */
export function toggleWrap(doc: string, from: number, to: number, mark: string): FormatEdit {
  const m = mark.length;
  const selected = doc.slice(from, to);
  // A single `*` must not be confused with one half of `**`.
  const single = mark === '*';
  if (from !== to && selected.length >= 2 * m && selected.startsWith(mark) && selected.endsWith(mark)
      && !(single && (selected.startsWith('**') || selected.endsWith('**')))) {
    const inner = selected.slice(m, selected.length - m);
    return { from, to, insert: inner, anchor: from, head: from + inner.length };
  }
  const before = doc.slice(Math.max(0, from - m), from), after = doc.slice(to, to + m);
  if (before === mark && after === mark && !(single && (doc[from - 2] === '*' || doc[to + 1] === '*'))) {
    return { from: from - m, to: to + m, insert: selected, anchor: from - m, head: from - m + selected.length };
  }
  return { from, to, insert: mark + selected + mark, anchor: from + m, head: from + m + selected.length };
}

const HEADING = /^#{1,6}[ \t]+/;
const TASK = /^[-*+][ \t]+\[[ xX]\][ \t]+/;
const UL = /^[-*+][ \t]+/;
const OL = /^\d+[.)][ \t]+/;
const QUOTE = /^>[ \t]?/;

function ownPrefix(kind: LineKind, line: string): string | null {
  if (kind === 'quote') return QUOTE.exec(line)?.[0] ?? null;
  if (kind === 'ol') return OL.exec(line)?.[0] ?? null;
  if (kind === 'task') return TASK.exec(line)?.[0] ?? null;
  if (kind === 'ul') return TASK.test(line) ? null : UL.exec(line)?.[0] ?? null;
  const level = Number(kind[1]);
  const h = HEADING.exec(line)?.[0];
  return h && h.trim().length === level ? h : null;
}

/** The prefix of the same family (heading / list / task, or quote) that `kind` replaces. */
function familyPrefix(kind: LineKind, line: string): string {
  if (kind === 'quote') return QUOTE.exec(line)?.[0] ?? '';
  return (HEADING.exec(line) ?? TASK.exec(line) ?? OL.exec(line) ?? UL.exec(line))?.[0] ?? '';
}

/** Applies (or, when every non-empty line already has it, removes) a line prefix on each selected line. */
export function toggleLines(doc: string, from: number, to: number, kind: LineKind): FormatEdit {
  const start = lineStartOf(doc, from);
  // A selection that ends exactly at the start of a line does not include that line.
  const end = lineEndOf(doc, to > from && doc[to - 1] === '\n' ? to - 1 : to);
  const lines = doc.slice(start, end).split('\n');
  let targets = lines.map((_, i) => i).filter(i => lines[i].trim() !== '');
  if (!targets.length) targets = lines.map((_, i) => i);
  const removing = targets.every(i => ownPrefix(kind, lines[i]) !== null);
  let n = 0;
  const out = lines.map((line, i) => {
    if (!targets.includes(i)) return line;
    if (removing) return line.slice(ownPrefix(kind, line)!.length);
    n++;
    const rest = line.slice(familyPrefix(kind, line).length);
    const prefix = kind === 'quote' ? '> ' : kind === 'ol' ? `${n}. ` : kind === 'ul' ? '- ' : kind === 'task' ? '- [ ] ' : '#'.repeat(Number(kind[1])) + ' ';
    return prefix + rest;
  });
  const insert = out.join('\n');
  if (from === to) {
    const offset = Math.max(0, Math.min(insert.length, from - start + (insert.length - (end - start))));
    return { from: start, to: end, insert, anchor: start + offset, head: start + offset };
  }
  return { from: start, to: end, insert, anchor: start, head: start + insert.length };
}

/** `[selection](url)` with `url` selected; a selected URL becomes the target of an empty `[]`. */
export function makeLink(doc: string, from: number, to: number): FormatEdit {
  const selected = doc.slice(from, to);
  if (/^https?:\/\/\S+$/i.test(selected)) {
    return { from, to, insert: `[](${selected})`, anchor: from + 1, head: from + 1 };
  }
  const text = selected || 'текст', url = 'url';
  const a = from + 1 + text.length + 2;
  return { from, to, insert: `[${text}](${url})`, anchor: a, head: a + url.length };
}

/** Fenced code block around the selection, always on its own lines. */
export function codeBlock(doc: string, from: number, to: number): FormatEdit {
  const selected = doc.slice(from, to);
  const lead = from > lineStartOf(doc, from) ? '\n' : '';
  const trail = to < lineEndOf(doc, to) ? '\n' : '';
  const open = lead + '```\n';
  const a = from + open.length;
  return { from, to, insert: open + selected + '\n```' + trail, anchor: a, head: a + selected.length };
}

function block(doc: string, from: number, to: number, snippet: string, selFrom: number, selTo: number): FormatEdit {
  const lead = doc.slice(lineStartOf(doc, from), from).trim() ? '\n\n' : '';
  const trail = to < lineEndOf(doc, to) ? '\n' : '';
  return { from, to, insert: lead + snippet + trail, anchor: from + lead.length + selFrom, head: from + lead.length + selTo };
}

export function insertTable(doc: string, from: number, to: number): FormatEdit {
  const header = 'Заголовок 1';
  return block(doc, from, to, `| ${header} | Заголовок 2 |\n| --- | --- |\n| Ячейка | Ячейка |\n`, 2, 2 + header.length);
}

export function insertRule(doc: string, from: number, to: number): FormatEdit {
  return block(doc, from, to, '---\n', 4, 4);
}
