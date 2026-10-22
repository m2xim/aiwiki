// PDF export: Markdown AST -> pdfmake document definition. Pure (no DOM/React), so scripts/qa-export.mjs imports it directly.
// pdfmake itself is loaded lazily by loadPdfMake() and only when a PDF is exported.
import { unified } from 'unified';
import remarkParse from 'remark-parse';
import remarkGfm from 'remark-gfm';
import type { Root, RootContent, PhrasingContent, Definition, TableRow } from 'mdast';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type PdfNode = Record<string, any>;
export type PdfImage = { dataUri: string; width: number; height: number } | { external: true } | { error: string };
export type PdfCtx = {
  origin: string;
  /** Keyed by the image URL exactly as written in Markdown. */
  images: Map<string, PdfImage>;
  warnings: string[];
  contentWidth?: number;
};
export type PdfArticle = { id: string; title: string; revision: number; updated_at: string; author?: string; body: string };

export const PDF_PAGE_MARGIN = 40;
export const PDF_CONTENT_WIDTH = 595.28 - PDF_PAGE_MARGIN * 2;
const MAX_IMAGE_HEIGHT = 640;
const SAFE_SCHEMES = new Set(['http:', 'https:', 'mailto:']);
const LINK_COLOR = '#1a56db';
const HEADING_SIZE = [0, 22, 18, 15, 13, 11.5, 10.5];

export function parseMarkdown(body: string): Root {
  return unified().use(remarkParse).use(remarkGfm).parse(body) as Root;
}

/** Plain text of a node (used to compare a leading H1 with the article title). */
export function plainText(node: { value?: string; children?: unknown[] }): string {
  if (typeof node.value === 'string') return node.value;
  return (node.children || []).map(child => plainText(child as { value?: string; children?: unknown[] })).join('');
}

/** `javascript:` and other schemes -> null (text without a link); `#anchor` has no meaning in a standalone PDF -> null. */
export function safeLinkUrl(href: string | undefined, origin: string): string | null {
  const raw = (href || '').trim();
  if (!raw || raw.startsWith('#')) return null;
  let url: URL;
  try { url = new URL(raw, origin); } catch { return null; }
  if (!SAFE_SCHEMES.has(url.protocol)) return null;
  const id = url.origin === origin && url.pathname === '/' ? url.searchParams.get('article') : null;
  if (id) {
    let anchor = url.hash.slice(1); try { anchor = decodeURIComponent(anchor); } catch { /* keep raw fragment */ }
    return `${origin}/?article=${encodeURIComponent(id)}${anchor ? '#' + encodeURIComponent(anchor) : ''}`;
  }
  return url.href;
}

export type ImageSource = { kind: 'data' | 'file' | 'external' | 'invalid'; url: string };
/** Which images can be embedded: data: URIs, same-origin /files/<id>; http(s) elsewhere is external, the rest is invalid. */
export function classifyImageUrl(src: string, origin: string): ImageSource {
  if (src.startsWith('data:')) return { kind: /^data:image\/(png|jpeg|gif|webp)[;,]/i.test(src) ? 'data' : 'invalid', url: src };
  let url: URL;
  try { url = new URL(src, origin); } catch { return { kind: 'invalid', url: src }; }
  if (url.origin === origin && /^\/files\/[^/]+$/.test(url.pathname)) return { kind: 'file', url: url.href };
  if (url.protocol === 'http:' || url.protocol === 'https:') return { kind: 'external', url: url.href };
  return { kind: 'invalid', url: src };
}

/** All image URLs (image and imageReference nodes) in document order, without duplicates. */
export function collectImageUrls(root: Root): string[] {
  const defs = collectDefinitions(root);
  const out = new Set<string>();
  const walk = (node: { type: string; url?: string; identifier?: string; children?: unknown[] }) => {
    if (node.type === 'image' && node.url) out.add(node.url);
    if (node.type === 'imageReference') { const def = defs.get(normId(node.identifier)); if (def?.url) out.add(def.url); }
    (node.children || []).forEach(child => walk(child as typeof node));
  };
  walk(root);
  return [...out];
}

const normId = (id: string | undefined) => (id || '').toLowerCase();
function collectDefinitions(root: Root): Map<string, Definition> {
  const defs = new Map<string, Definition>();
  const walk = (node: { type: string; children?: unknown[] }) => {
    if (node.type === 'definition') { const d = node as Definition; if (!defs.has(normId(d.identifier))) defs.set(normId(d.identifier), d); }
    (node.children || []).forEach(child => walk(child as typeof node));
  };
  walk(root);
  return defs;
}

type Style = { bold?: boolean; italics?: boolean; strike?: boolean; link?: string };
const LONG_TOKEN = /\S{32,}/;

function textRun(text: string, style: Style, extra: PdfNode = {}): PdfNode {
  const run: PdfNode = { text, ...extra };
  if (style.bold) run.bold = true;
  if (style.italics) run.italics = true;
  const decorations: string[] = [];
  if (style.strike) decorations.push('lineThrough');
  if (style.link) { run.link = style.link; run.color = LINK_COLOR; decorations.push('underline'); }
  if (decorations.length) run.decoration = decorations.length === 1 ? decorations[0] : decorations;
  if (LONG_TOKEN.test(text)) run.wordBreak = 'break-all'; // long URLs / identifiers must not leave the page
  return run;
}

type Converter = {
  ctx: PdfCtx;
  defs: Map<string, Definition>;
  width: number;
};

function imageBlocks(conv: Converter, src: string, alt: string, style: Style): PdfNode[] {
  const { ctx } = conv;
  const label = alt || src.slice(0, 80);
  const info = ctx.images.get(src);
  if (info && 'dataUri' in info) {
    const maxW = conv.width;
    const w = Math.max(1, Math.min(info.width || maxW, maxW));
    const h = Math.max(1, Math.min(info.height || MAX_IMAGE_HEIGHT, MAX_IMAGE_HEIGHT));
    return [{ image: info.dataUri, fit: [w, h], margin: [0, 2, 0, 6] }];
  }
  if (info && 'external' in info) {
    ctx.warnings.push(`Внешнее изображение «${label}» не встроено, оставлена ссылка.`);
    const url = safeLinkUrl(classifyImageUrl(src, ctx.origin).url, ctx.origin);
    const runs: PdfNode[] = [textRun('[внешнее изображение: ', { ...style, italics: true })];
    runs.push(textRun(alt || url || src, url ? { ...style, link: url } : style));
    runs.push(textRun(']', { ...style, italics: true }));
    return [{ text: runs, margin: [0, 0, 0, 6] }];
  }
  const reason = info && 'error' in info ? info.error : 'недоступный адрес';
  ctx.warnings.push(`Изображение «${label}» не включено: ${reason}.`);
  return [{ text: [textRun(`[изображение не включено: ${alt}]`, { ...style, italics: true }, { color: '#b42318' })], margin: [0, 0, 0, 6] }];
}

/** Phrasing content -> list of blocks: pdfmake text nodes (runs) and images (images are blocks in pdfmake). */
function inlineBlocks(conv: Converter, nodes: PhrasingContent[]): PdfNode[] {
  const blocks: PdfNode[] = [];
  let runs: PdfNode[] = [];
  const flush = () => {
    while (runs.length && runs[runs.length - 1].text === '\n') runs.pop();
    if (runs.length) blocks.push({ text: runs });
    runs = [];
  };
  const walk = (list: PhrasingContent[], style: Style) => {
    for (const node of list) {
      switch (node.type) {
        case 'text': {
          const text = node.value.replace(/\s*\n\s*/g, ' ');
          if (text) runs.push(textRun(text, style));
          break;
        }
        case 'strong': walk(node.children, { ...style, bold: true }); break;
        case 'emphasis': walk(node.children, { ...style, italics: true }); break;
        case 'delete': walk(node.children, { ...style, strike: true }); break;
        case 'inlineCode': runs.push(textRun(node.value.replace(/\s*\n\s*/g, ' '), style, { fontSize: 9, background: '#eceef2', color: '#1f2328' })); break;
        case 'break': runs.push({ text: '\n' }); break;
        case 'link': {
          const href = safeLinkUrl(node.url, conv.ctx.origin);
          walk(node.children, href ? { ...style, link: href } : style); break;
        }
        case 'linkReference': {
          const href = safeLinkUrl(conv.defs.get(normId(node.identifier))?.url, conv.ctx.origin);
          walk(node.children, href ? { ...style, link: href } : style); break;
        }
        case 'image': flush(); blocks.push(...imageBlocks(conv, node.url, node.alt || '', style)); break;
        case 'imageReference': {
          const def = conv.defs.get(normId(node.identifier));
          flush();
          if (def) blocks.push(...imageBlocks(conv, def.url, node.alt || '', style));
          else runs.push(textRun(`![${node.alt || ''}]`, style));
          break;
        }
        case 'footnoteReference': runs.push(textRun(`[${node.label || node.identifier}]`, style, { fontSize: 8 })); break;
        case 'html': { const text = node.value.replace(/\s*\n\s*/g, ' '); if (text) runs.push(textRun(text, style)); break; } // raw HTML is shown as text, never interpreted
        default: break;
      }
    }
  };
  walk(nodes, {});
  flush();
  return blocks;
}

const withMargin = (blocks: PdfNode[], margin: number[]): PdfNode[] => blocks.map(b => 'text' in b && !b.margin ? { ...b, margin } : b);
const single = (blocks: PdfNode[]): PdfNode => blocks.length === 1 ? blocks[0] : { stack: blocks };

function checkbox(checked: boolean): PdfNode {
  const canvas: PdfNode[] = [{ type: 'rect', x: 1, y: 1.5, w: 9, h: 9, r: 1.5, lineWidth: 1, lineColor: checked ? '#2f6f4f' : '#6b7280', ...(checked ? { color: '#2f6f4f' } : {}) }];
  if (checked) canvas.push({ type: 'polyline', lineWidth: 1.4, lineColor: '#ffffff', closePath: false, points: [{ x: 3, y: 6.2 }, { x: 5, y: 8.2 }, { x: 8.2, y: 3.8 }] });
  return { canvas, width: 14 };
}

function listBlock(conv: Converter, node: Extract<RootContent, { type: 'list' }>, innerWidth: number): PdfNode {
  const inner: Converter = { ...conv, width: innerWidth - 20 };
  const items = node.children.map(item => {
    const body = blocksOf(inner, item.children, true);
    return { item, body: body.length ? single(body) : { text: '' } };
  });
  const hasTasks = node.children.some(item => item.checked === true || item.checked === false);
  const margin = [0, 0, 0, 6];
  if (!hasTasks) {
    const key = node.ordered ? 'ol' : 'ul';
    return { [key]: items.map(i => i.body), ...(node.ordered && node.start != null && node.start !== 1 ? { start: node.start } : {}), margin };
  }
  // Roboto has no ballot-box glyphs: task lists draw their own markers.
  const start = node.start ?? 1;
  return {
    stack: items.map(({ item, body }, i) => ({
      columns: [
        item.checked === true || item.checked === false ? checkbox(item.checked)
          : { text: node.ordered ? `${start + i}.` : '•', width: 14, alignment: node.ordered ? 'left' : 'center' },
        { ...(body.stack ? body : { stack: [body] }), width: '*' },
      ],
      columnGap: 4, margin: [0, 0, 0, 2],
    })),
    margin,
  };
}

const sideBarLayout = {
  hLineWidth: () => 0, vLineWidth: (i: number) => i === 0 ? 3 : 0, vLineColor: () => '#c4c7d0',
  paddingLeft: () => 10, paddingRight: () => 4, paddingTop: () => 2, paddingBottom: () => 2,
};
const codeLayout = {
  hLineWidth: () => 0, vLineWidth: () => 0, fillColor: () => '#f3f4f6',
  paddingLeft: () => 8, paddingRight: () => 8, paddingTop: (i: number) => i === 0 ? 7 : 0,
  paddingBottom: (i: number, node: PdfNode) => i === node.table.body.length - 1 ? 7 : 0,
};
const tableLayout = {
  hLineWidth: () => 0.5, vLineWidth: () => 0.5, hLineColor: () => '#c9ced6', vLineColor: () => '#c9ced6',
  fillColor: (row: number) => row === 0 ? '#eef0f4' : null,
  paddingLeft: () => 5, paddingRight: () => 5, paddingTop: () => 3, paddingBottom: () => 3,
};

function codeBlock(value: string): PdfNode {
  const lines = value.replace(/\r\n?/g, '\n').replace(/\n$/, '').split('\n');
  const body = lines.map(line => [{ text: line.replace(/\t/g, '    ') || ' ', fontSize: 8.5, color: '#24292f', preserveLeadingSpaces: true, wordBreak: 'break-all', lineHeight: 1.2 }]);
  return { table: { widths: ['*'], body }, layout: codeLayout, margin: [0, 0, 0, 8] };
}

function tableBlock(conv: Converter, node: Extract<RootContent, { type: 'table' }>): PdfNode {
  const cols = Math.max(1, ...node.children.map((row: TableRow) => row.children.length));
  const body = node.children.map((row: TableRow, r) => {
    const cells: PdfNode[] = row.children.map((cell, c) => {
      const blocks = inlineBlocks(conv, cell.children);
      const content = blocks.length === 0 ? { text: '' } : single(blocks);
      const align = node.align?.[c];
      return { ...content, ...(r === 0 ? { bold: true } : {}), ...(align && align !== 'left' ? { alignment: align } : {}) };
    });
    while (cells.length < cols) cells.push({ text: '' });
    return cells;
  });
  return { table: { headerRows: 1, widths: Array(cols).fill('*'), body }, layout: tableLayout, fontSize: 9.5, margin: [0, 0, 0, 10] };
}

function blocksOf(conv: Converter, nodes: RootContent[], tight = false): PdfNode[] {
  const out: PdfNode[] = [];
  const gap = tight ? 2 : 6;
  for (const node of nodes) {
    switch (node.type) {
      case 'heading': {
        const blocks = inlineBlocks(conv, node.children);
        const size = HEADING_SIZE[node.depth] || 10.5;
        const text = blocks.flatMap(b => 'text' in b ? (Array.isArray(b.text) ? b.text : [b]) : []);
        out.push({ text: text.length ? text : '', fontSize: size, bold: true, headlineLevel: node.depth, margin: [0, node.depth <= 2 ? 14 : 10, 0, node.depth <= 2 ? 6 : 4], ...(node.depth === 1 ? { color: '#111827' } : {}) });
        break;
      }
      case 'paragraph': out.push(...withMargin(inlineBlocks(conv, node.children), [0, 0, 0, gap])); break;
      case 'list': out.push(listBlock(conv, node, conv.width)); break;
      case 'blockquote': {
        const inner = blocksOf({ ...conv, width: conv.width - 14 }, node.children, false);
        if (inner.length) out.push({ table: { widths: ['*'], body: [[{ stack: inner, color: '#4b5563' }]] }, layout: sideBarLayout, margin: [0, 0, 0, 8] });
        break;
      }
      case 'code': out.push(codeBlock(node.value)); break;
      case 'thematicBreak':
        out.push({ canvas: [{ type: 'line', x1: 0, y1: 0, x2: conv.width, y2: 0, lineWidth: 0.7, lineColor: '#c9ced6' }], margin: [0, 6, 0, 10] });
        break;
      case 'table': out.push(tableBlock(conv, node)); break;
      case 'html': out.push({ text: node.value, margin: [0, 0, 0, gap], wordBreak: 'break-all' }); break; // raw HTML is shown as text, never interpreted
      case 'footnoteDefinition': out.push({ stack: [{ text: `[${node.label || node.identifier}]`, bold: true, fontSize: 9 }, ...blocksOf(conv, node.children, true)], fontSize: 9, margin: [0, 0, 0, gap] }); break;
      default: break; // definition and others have no visual output
    }
  }
  return out;
}

/** Markdown AST -> array of pdfmake content nodes. Pure: images must already be loaded into ctx.images. */
export function markdownToPdfContent(root: Root, ctx: PdfCtx): PdfNode[] {
  return blocksOf({ ctx, defs: collectDefinitions(root), width: ctx.contentWidth ?? PDF_CONTENT_WIDTH }, root.children);
}

const fmtDate = (value: string) => { const d = new Date(value); return isNaN(d.getTime()) ? value : d.toLocaleString('ru-RU'); };

export type PdfBuildOptions = { origin: string; images: Map<string, PdfImage>; spaceName?: string; exportedAt?: Date };

/** Full pdfmake document definition for an article, plus export warnings. */
export function buildPdfDocument(article: PdfArticle, opts: PdfBuildOptions): { docDefinition: PdfNode; warnings: string[] } {
  const warnings: string[] = [];
  const root = parseMarkdown(article.body || '');
  const body = markdownToPdfContent(root, { origin: opts.origin, images: opts.images, warnings });
  const first = root.children[0];
  const titled = first?.type === 'heading' && first.depth === 1 && plainText(first).trim() === article.title.trim();
  const link = `${opts.origin}/?article=${encodeURIComponent(article.id)}`;
  const exportedAt = opts.exportedAt ?? new Date();

  const metaItems = [`Ревизия ${article.revision}`, `Обновлено: ${fmtDate(article.updated_at)}`, article.author ? `Автор: ${article.author}` : '', opts.spaceName ? `Пространство: ${opts.spaceName}` : ''].filter(Boolean);
  const head: PdfNode[] = [];
  if (!titled) head.push({ text: article.title, fontSize: 22, bold: true, color: '#111827', margin: [0, 0, 0, 6] });
  head.push({ text: [{ text: metaItems.join(' · ') + ' · Оригинал: ' }, { text: link, link, color: LINK_COLOR, decoration: 'underline', wordBreak: 'break-all' }], fontSize: 9, color: '#555555' });
  head.push({ canvas: [{ type: 'line', x1: 0, y1: 0, x2: PDF_CONTENT_WIDTH, y2: 0, lineWidth: 0.7, lineColor: '#c9ced6' }], margin: [0, 8, 0, 4] });

  const docDefinition: PdfNode = {
    info: { title: article.title, subject: 'AI Wiki', creator: 'AI Wiki', ...(article.author ? { author: article.author } : {}) },
    pageSize: 'A4',
    pageMargins: [PDF_PAGE_MARGIN, PDF_PAGE_MARGIN, PDF_PAGE_MARGIN, 52],
    defaultStyle: { font: 'Roboto', fontSize: 10.5, lineHeight: 1.25, color: '#1f2328' },
    content: [...head, ...body],
    footer: (page: number, pages: number) => ({
      columns: [
        { text: `стр. ${page} из ${pages}`, alignment: 'left' },
        { text: `Экспортировано ${exportedAt.toLocaleString('ru-RU')}`, alignment: 'right' },
      ],
      margin: [PDF_PAGE_MARGIN, 22, PDF_PAGE_MARGIN, 0], fontSize: 8, color: '#777777',
    }),
    // A heading must not stay alone at the bottom of a page.
    pageBreakBefore: (node: PdfNode, ctx: { getFollowingNodesOnPage: () => unknown[] }) => !!node.headlineLevel && ctx.getFollowingNodesOnPage().length === 0,
  };
  return { docDefinition, warnings };
}

/** Loads pdfmake with its bundled Roboto fonts (Cyrillic included). Called only when a PDF is exported, so the chunk stays out of the main bundle. */
export async function loadPdfMake(): Promise<{ createPdf: (doc: PdfNode) => { getBlob: () => Promise<Blob> } }> {
  // @ts-expect-error pdfmake 0.3 ships no type declarations
  const [pdfMod, vfsMod] = await Promise.all([import('pdfmake/build/pdfmake'), import('pdfmake/build/vfs_fonts')]);
  const pdfMake = pdfMod.default ?? pdfMod;
  const vfs = vfsMod.default ?? vfsMod;
  pdfMake.addVirtualFileSystem(vfs);
  return pdfMake;
}
