// Pure logic of the single-file Markdown import: front matter, title, analysis of images and links.
// No React and no DOM here, so scripts/qa-import.mjs can run it directly under Node.
import remarkGfm from 'remark-gfm';
import remarkParse from 'remark-parse';
import { unified } from 'unified';

export type WarningKind = 'frontmatter' | 'image-missing' | 'image-external' | 'image-data' | 'image-ambiguous' | 'link-relative' | 'link-scheme' | 'html' | 'wikilink';
export type ImportWarning = { kind: WarningKind; message: string; line?: number };

export const WARNING_TITLES: Record<WarningKind, string> = {
  'frontmatter': 'Front matter',
  'image-missing': 'Картинка не найдена среди выбранных файлов',
  'image-external': 'Внешняя картинка',
  'image-data': 'Картинка data:',
  'image-ambiguous': 'Неоднозначная картинка',
  'link-relative': 'Относительная ссылка',
  'link-scheme': 'Ссылка с неподдерживаемой схемой',
  'html': 'HTML',
  'wikilink': 'Ссылка [[...]]',
};

export type FrontMatter = { title?: string; summary?: string; tags?: string[] };
export type FrontMatterResult = { found: boolean; data: FrontMatter; body: string; ignoredFields: string[]; warnings: ImportWarning[] };

const unquote = (value: string): string => {
  const v = value.trim();
  if (v.length >= 2 && ((v[0] === '"' && v[v.length - 1] === '"') || (v[0] === "'" && v[v.length - 1] === "'"))) {
    const inner = v.slice(1, -1);
    return v[0] === '"' ? inner.replace(/\\(["\\])/g, '$1') : inner.replace(/''/g, "'");
  }
  return v;
};
// Splits "a, 'b, c', d" on top-level commas.
const splitList = (value: string): string[] => {
  const out: string[] = []; let current = ''; let quote = '';
  for (const ch of value) {
    if (quote) { current += ch; if (ch === quote) quote = ''; continue; }
    if (ch === '"' || ch === "'") { quote = ch; current += ch; continue; }
    if (ch === ',') { out.push(current); current = ''; continue; }
    current += ch;
  }
  out.push(current);
  return out.map(unquote).map(s => s.trim()).filter(Boolean);
};

/** Text must be already normalized (BOM removed, LF line endings). */
export function parseFrontMatter(text: string): FrontMatterResult {
  const empty: FrontMatterResult = { found: false, data: {}, body: text, ignoredFields: [], warnings: [] };
  if (!text.startsWith('---\n') && text !== '---') return empty;
  const lines = text.split('\n');
  let close = -1;
  for (let i = 1; i < lines.length; i++) if (/^---[ \t]*$/.test(lines[i])) { close = i; break; }
  if (close < 0) return { ...empty, warnings: [{ kind: 'frontmatter', message: 'Файл начинается с «---», но закрывающей «---» нет: front matter не распознан и остался в тексте.' }] };

  const data: FrontMatter = {}; const ignored: string[] = []; const warnings: ImportWarning[] = [];
  const warn = (message: string, index: number) => warnings.push({ kind: 'frontmatter', message, line: index + 1 });
  const known = new Set(['title', 'summary', 'tags']);
  for (let i = 1; i < close; i++) {
    const raw = lines[i];
    if (!raw.trim() || raw.trim().startsWith('#')) continue;
    if (/^\s/.test(raw)) { warn(`Строка с отступом вне списка не понята и пропущена: «${raw.trim()}»`, i); continue; }
    const m = /^([^:\s][^:]*?)\s*:(?:\s+(.*)|\s*)$/.exec(raw);
    if (!m) { warn(`Не удалось разобрать строку: «${raw.trim()}»`, i); continue; }
    const key = m[1].trim().toLowerCase(); const value = (m[2] ?? '').trim();
    // Collect the block that belongs to this key: following indented lines and "- item" lines.
    const block: number[] = [];
    while (i + 1 < close && (/^\s/.test(lines[i + 1]) || /^-(\s|$)/.test(lines[i + 1]) || !lines[i + 1].trim())) { i++; block.push(i); }
    const blockLines = block.map(n => lines[n]).filter(l => l.trim());
    if (!known.has(key)) { if (!ignored.includes(m[1].trim())) ignored.push(m[1].trim()); continue; }
    if (key === 'tags') {
      let tags: string[] | null = null;
      if (value.startsWith('[')) {
        if (value.endsWith(']') && !blockLines.length) tags = splitList(value.slice(1, -1));
        else warn('Список tags в квадратных скобках должен быть в одной строке: теги не взяты.', i);
      } else if (!value && blockLines.length) {
        const items = blockLines.map(l => /^\s*-\s+(.*)$/.exec(l));
        if (items.every(Boolean)) tags = items.map(it => unquote(it![1])).map(s => s.trim()).filter(Boolean);
        else warn('Список tags не распознан (ожидались строки вида «- тег»): теги не взяты.', i);
      } else if (value && !blockLines.length) {
        tags = splitList(value);
      } else if (value || blockLines.length) warn('Поле tags не понято: теги не взяты.', i);
      if (tags) data.tags = [...new Set(tags)];
    } else {
      if (/^[|>][+-]?$/.test(value) || (!value && blockLines.length)) { warn(`Многострочное значение поля «${key}» не поддерживается: поле пропущено.`, i); continue; }
      if (blockLines.length) { warn(`Лишние строки после поля «${key}» пропущены.`, i); }
      const v = unquote(value);
      if (v) data[key as 'title' | 'summary'] = v;
    }
  }
  return { found: true, data, body: lines.slice(close + 1).join('\n'), ignoredFields: ignored, warnings };
}

// ---- markdown tree ----

type Pos = { offset?: number; line?: number };
export type MdNode = { type: string; depth?: number; value?: string; url?: string; alt?: string; identifier?: string; children?: MdNode[]; position?: { start: Pos; end: Pos } };
export const parseMarkdown = (body: string) => unified().use(remarkParse).use(remarkGfm).parse(body) as unknown as MdNode;
const plainText = (node: MdNode): string => node.value ?? (node.children || []).map(plainText).join('');
const walk = (node: MdNode, visit: (node: MdNode) => void) => { visit(node); node.children?.forEach(child => walk(child, visit)); };

export const fileBaseTitle = (fileName: string): string => fileName.replace(/^.*[\\/]/, '').replace(/\.(md|markdown)$/i, '').trim();

/** Front matter title, else the first H1 outside code, else the file name without extension. */
export function deriveTitle(body: string, fileName: string, frontTitle?: string): string {
  if (frontTitle?.trim()) return frontTitle.trim();
  let h1 = '';
  walk(parseMarkdown(body), node => { if (!h1 && node.type === 'heading' && node.depth === 1) h1 = plainText(node).replace(/\s+/g, ' ').trim(); });
  return h1 || fileBaseTitle(fileName);
}

// ---- analysis ----

export type ImageStatus = 'local' | 'missing' | 'external' | 'data' | 'ambiguous' | 'internal';
export type ImageRef = { url: string; alt: string; start: number; end: number; line: number; status: ImageStatus; fileIndex: number };
export type Analysis = { images: ImageRef[]; warnings: ImportWarning[] };

const safeDecode = (s: string) => { try { return decodeURIComponent(s); } catch { return s; } };
/** Relative path to canonical form: no "./", "/" separators, URL-decoded, no query/hash. */
export function normalizeRelPath(url: string): string {
  const clean = safeDecode(url.split('#')[0].split('?')[0]).replace(/\\/g, '/');
  const parts: string[] = [];
  for (const part of clean.split('/')) { if (!part || part === '.') continue; parts.push(part); }
  return parts.join('/');
}
const baseName = (path: string) => path.slice(path.lastIndexOf('/') + 1);

/** Finds the destination of `![alt](dest "title")` inside the node source; returns offsets relative to the node start. */
function imageDestination(source: string): [number, number] | null {
  if (!source.startsWith('![')) return null;
  let depth = 1; let i = 2;
  for (; i < source.length; i++) {
    const ch = source[i];
    if (ch === '\\') { i++; continue; }
    if (ch === '[') depth++;
    else if (ch === ']' && --depth === 0) break;
  }
  if (depth !== 0 || source[i + 1] !== '(') return null;
  let p = i + 2;
  while (p < source.length && /\s/.test(source[p])) p++;
  if (source[p] === '<') {
    const from = p + 1; let q = from;
    while (q < source.length && source[q] !== '>' ) { if (source[q] === '\\') q++; q++; }
    return q < source.length ? [from, q] : null;
  }
  const from = p; let nest = 0; let q = p;
  for (; q < source.length; q++) {
    const ch = source[q];
    if (ch === '\\') { q++; continue; }
    if (/\s/.test(ch)) break;
    if (ch === '(') nest++;
    else if (ch === ')') { if (nest === 0) break; nest--; }
  }
  return [from, q];
}

const lineOf = (node: MdNode) => node.position?.start.line ?? 0;
const SCHEME = /^[a-z][a-z0-9+.-]*:/i;

/** fileNames: names (or relative paths) of the chosen image files. */
export function analyzeMarkdown(body: string, fileNames: string[]): Analysis {
  const tree = parseMarkdown(body);
  const warnings: ImportWarning[] = []; const images: ImageRef[] = [];
  const normalized = fileNames.map(normalizeRelPath);
  const definitions = new Map<string, MdNode>();
  walk(tree, node => { if (node.type === 'definition' && node.identifier && !definitions.has(node.identifier)) definitions.set(node.identifier, node); });

  const classify = (rawUrl: string): { status: ImageStatus; fileIndex: number } => {
    const url = rawUrl.trim();
    if (/^data:/i.test(url)) return { status: 'data', fileIndex: -1 };
    if (/^(https?:)?\/\//i.test(url)) return { status: 'external', fileIndex: -1 };
    if (url.startsWith('/files/')) return { status: 'internal', fileIndex: -1 };
    if (SCHEME.test(url)) return { status: 'external', fileIndex: -1 };
    const path = normalizeRelPath(url);
    if (!path) return { status: 'missing', fileIndex: -1 };
    const exact = normalized.flatMap((n, i) => n === path ? [i] : []);
    if (exact.length === 1) return { status: 'local', fileIndex: exact[0] };
    const byName = normalized.flatMap((n, i) => baseName(n) === baseName(path) ? [i] : []);
    if (byName.length === 1) return { status: 'local', fileIndex: byName[0] };
    if (byName.length > 1) return { status: 'ambiguous', fileIndex: -1 };
    return { status: 'missing', fileIndex: -1 };
  };
  const addImage = (url: string, alt: string, start: number, end: number, line: number) => {
    const { status, fileIndex } = classify(url);
    images.push({ url, alt, start, end, line, status, fileIndex });
    const label = `«${url.length > 80 ? url.slice(0, 77) + '...' : url}»`;
    if (status === 'missing') warnings.push({ kind: 'image-missing', message: `Картинка ${label} не найдена среди выбранных файлов: ссылка останется как есть.`, line });
    else if (status === 'external') warnings.push({ kind: 'image-external', message: `Картинка ${label} останется внешней ссылкой.`, line });
    else if (status === 'data') warnings.push({ kind: 'image-data', message: 'Картинка data: встроена в текст и не будет загружена как вложение.', line });
    else if (status === 'ambiguous') warnings.push({ kind: 'image-ambiguous', message: `Картинка ${label}: среди выбранных файлов несколько с таким именем, выбрать нужный нельзя.`, line });
  };

  const seenDefinitionUrls = new Set<number>();
  const checkLink = (url: string, line: number) => {
    const u = url.trim();
    if (!u || u.startsWith('#') || u.startsWith('/') || /^(https?:|mailto:)/i.test(u)) return;
    if (SCHEME.test(u)) warnings.push({ kind: 'link-scheme', message: `Ссылка «${u}» использует схему, которая в wiki не поддерживается.`, line });
    else warnings.push({ kind: 'link-relative', message: `Относительная ссылка «${u}» не будет работать.`, line });
  };

  walk(tree, node => {
    const start = node.position?.start.offset, end = node.position?.end.offset;
    switch (node.type) {
      case 'image': {
        if (start === undefined || end === undefined) break;
        const dest = imageDestination(body.slice(start, end));
        addImage(node.url || '', node.alt || '', dest ? start + dest[0] : -1, dest ? start + dest[1] : -1, lineOf(node));
        break;
      }
      case 'imageReference': {
        const def = node.identifier ? definitions.get(node.identifier) : undefined;
        if (!def) break;
        const ds = def.position?.start.offset, de = def.position?.end.offset;
        let from = -1, to = -1;
        if (ds !== undefined && de !== undefined) {
          const src = body.slice(ds, de); const raw = def.url || '';
          const colon = src.indexOf(']:');
          const at = colon < 0 ? -1 : src.indexOf(raw, colon + 2);
          if (at >= 0 && raw) { from = ds + at; to = from + raw.length; }
        }
        if (from >= 0 && seenDefinitionUrls.has(from)) break;
        if (from >= 0) seenDefinitionUrls.add(from);
        addImage(def.url || '', node.alt || '', from, to, lineOf(node));
        break;
      }
      case 'link': checkLink(node.url || '', lineOf(node)); break;
      case 'linkReference': { const def = node.identifier ? definitions.get(node.identifier) : undefined; if (def) checkLink(def.url || '', lineOf(node)); break; }
      case 'html': {
        const v = (node.value || '').trim();
        if (v.startsWith('<!--')) break;
        warnings.push({ kind: 'html', message: `HTML не отображается в wiki: ${v.length > 60 ? v.slice(0, 57) + '...' : v}`, line: lineOf(node) });
        break;
      }
      case 'text': {
        const v = node.value || '';
        for (const m of v.matchAll(/\[\[([^\]\n]+)\]\]/g)) warnings.push({ kind: 'wikilink', message: `Ссылка Obsidian [[${m[1]}]] не будет работать как ссылка.`, line: lineOf(node) });
        break;
      }
    }
  });
  warnings.sort((a, b) => (a.line ?? 0) - (b.line ?? 0));
  return { images, warnings };
}

/** Replaces the given ranges (UTF-16 offsets into body) back to front so earlier offsets stay valid. */
export function rewriteImageUrls(body: string, replacements: { start: number; end: number; url: string }[]): string {
  const sorted = [...replacements].filter(r => r.start >= 0 && r.end >= r.start && r.end <= body.length).sort((a, b) => b.start - a.start);
  let out = body; let limit = Infinity;
  for (const r of sorted) {
    if (r.end > limit) continue; // overlapping range: skip
    out = out.slice(0, r.start) + r.url + out.slice(r.end); limit = r.start;
  }
  return out;
}

/** Decodes bytes as UTF-8, removes the BOM and converts CRLF/CR to LF. */
export const normalizeText = (text: string): string => text.replace(/^\uFEFF/, '').replace(/\r\n?/g, '\n');
export const IMAGE_EXT = /\.(png|jpe?g|gif|webp)$/i;
export const MARKDOWN_EXT = /\.(md|markdown)$/i;
export const MAX_MARKDOWN_BYTES = 2 * 1024 * 1024;
export const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;
