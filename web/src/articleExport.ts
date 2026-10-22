import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import type { Article } from './api';
import { Markdown, parseInternalLink } from './markdown';
import type { PdfImage } from './articlePdf';
import { buildPdfDocument, classifyImageUrl, collectImageUrls, loadPdfMake, parseMarkdown } from './articlePdf';
import { EXPORT_CSS, MAX_EMBEDDED_BYTES, buildExportDocument, escapeHtml, exportFileName } from './articleExportCore';

export { escapeHtml, exportFileName, buildExportDocument } from './articleExportCore';

export type ExportResult = { html: string; warnings: string[] };
const IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);
const SAFE_SCHEMES = new Set(['http:', 'https:', 'mailto:']);
const BAD_TAGS = 'script,iframe,object,embed,frame,frameset,base,link,meta,form';

const fmtDate = (value: string) => { const d = new Date(value); return isNaN(d.getTime()) ? value : d.toLocaleString('ru-RU'); };

function placeholder(doc: Document, text: string): HTMLElement {
  const span = doc.createElement('span'); span.className = 'export-missing'; span.textContent = text; return span;
}

async function loadImage(url: string): Promise<{ dataUri: string; size: number } | string> {
  try {
    const response = await fetch(url, { credentials: 'same-origin' });
    if (!response.ok) return `ответ сервера ${response.status}`;
    const blob = await response.blob();
    const type = (blob.type || response.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
    if (!IMAGE_TYPES.has(type)) return 'неподдерживаемый тип файла';
    const dataUri = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader(); reader.onload = () => resolve(String(reader.result)); reader.onerror = () => reject(reader.error);
      reader.readAsDataURL(new Blob([blob], { type }));
    });
    return { dataUri, size: blob.size };
  } catch { return 'ошибка загрузки'; }
}

/** Post-processes rendered Markdown HTML: absolute links, embedded same-origin images, sanitisation. */
export async function processBody(rawHtml: string, warnings: string[]): Promise<string> {
  const doc = new DOMParser().parseFromString(`<body>${rawHtml}</body>`, 'text/html');
  doc.querySelectorAll(BAD_TAGS).forEach(el => el.remove());
  doc.querySelectorAll('*').forEach(el => { for (const attr of [...el.attributes]) if (/^on/i.test(attr.name)) el.removeAttribute(attr.name); });
  // Inline "comment on heading" buttons are interface only (heading ids stay).
  doc.querySelectorAll('button.heading-comment').forEach(el => el.remove());

  doc.querySelectorAll('a').forEach(a => {
    const href = a.getAttribute('href');
    if (href === null || href.startsWith('#')) { a.removeAttribute('target'); return; }
    if (!href.trim()) { a.removeAttribute('href'); return; } // react-markdown empties unsafe URLs
    const internal = parseInternalLink(href);
    if (internal) {
      a.setAttribute('href', `${location.origin}/?article=${encodeURIComponent(internal.id)}${internal.anchor ? '#' + encodeURIComponent(internal.anchor) : ''}`);
      a.removeAttribute('target'); return;
    }
    let url: URL;
    try { url = new URL(href, location.origin); } catch { a.removeAttribute('href'); return; }
    if (!SAFE_SCHEMES.has(url.protocol)) { a.removeAttribute('href'); return; }
    a.setAttribute('href', url.href);
  });

  let total = 0;
  for (const img of [...doc.querySelectorAll('img')]) {
    const src = img.getAttribute('src') || ''; const alt = img.getAttribute('alt') || '';
    const label = alt || src.slice(0, 80);
    const skip = (reason: string) => { img.replaceWith(placeholder(doc, `[изображение не включено: ${alt}]`)); warnings.push(`Изображение «${label}» не включено: ${reason}.`); };
    if (src.startsWith('data:')) {
      if (!/^data:image\/(png|jpeg|gif|webp)[;,]/i.test(src)) skip('неподдерживаемый тип');
      continue;
    }
    let url: URL | null = null;
    try { url = new URL(src, location.origin); } catch { /* invalid */ }
    if (url && url.origin === location.origin && /^\/files\/[^/]+$/.test(url.pathname)) {
      const result = await loadImage(url.href);
      if (typeof result === 'string') { skip(result); continue; }
      if (total + result.size > MAX_EMBEDDED_BYTES) { skip('превышен лимит 15 МБ'); continue; }
      total += result.size;
      img.setAttribute('src', result.dataUri); img.removeAttribute('loading'); continue;
    }
    if (url && SAFE_SCHEMES.has(url.protocol) && url.protocol !== 'mailto:') {
      const a = doc.createElement('a'); a.setAttribute('href', url.href); a.textContent = alt || url.href;
      const wrap = doc.createElement('span'); wrap.className = 'export-missing'; wrap.append('[внешнее изображение: ', a, ']');
      img.replaceWith(wrap); warnings.push(`Внешнее изображение «${label}» не встроено, оставлена ссылка.`); continue;
    }
    skip('недоступный адрес');
  }
  return doc.body.innerHTML;
}

export async function buildArticleExport(article: Article, spaceName?: string): Promise<ExportResult> {
  const warnings: string[] = [];
  const raw = renderToStaticMarkup(React.createElement(Markdown, { body: article.body || '', comment: false }));
  const bodyHtml = await processBody(raw, warnings);
  const link = `${location.origin}/?article=${encodeURIComponent(article.id)}`;
  const items = [`Ревизия ${article.revision}`, `Обновлено: ${fmtDate(article.updated_at)}`, article.author ? `Автор: ${article.author}` : '', spaceName ? `Пространство: ${spaceName}` : '']
    .filter(Boolean).map(x => `<span>${escapeHtml(x)}</span>`).join('');
  // Articles usually start with "# <title>"; do not print the title twice.
  const parsed = new DOMParser().parseFromString(bodyHtml, 'text/html').body;
  const first = (parsed.querySelector(':scope > .markdown') || parsed).firstElementChild;
  const titled = first?.tagName === 'H1' && first.textContent?.trim() === article.title.trim();
  const metaHtml = `<header class="doc-head">${titled ? '' : `<h1 class="doc-title">${escapeHtml(article.title)}</h1>`}<p class="doc-meta">${items}<span>Оригинал: <a href="${escapeHtml(link)}">${escapeHtml(link)}</a></span></p></header>`;
  const footerHtml = `<footer class="doc-foot">Экспортировано ${escapeHtml(new Date().toLocaleString('ru-RU'))}. Это независимый снимок ревизии ${article.revision}: он не обновляется при изменении статьи в вики.</footer>`;
  return { html: buildExportDocument({ title: article.title, metaHtml, bodyHtml, footerHtml, css: EXPORT_CSS }), warnings };
}

export function downloadHtml(article: Article, html: string) {
  const url = URL.createObjectURL(new Blob([html], { type: 'text/html;charset=utf-8' }));
  const a = document.createElement('a'); a.href = url; a.download = exportFileName(article.title, article.revision); a.style.display = 'none';
  document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 10000);
}

const CONVERT_TYPES = new Set(['image/gif', 'image/webp']);

async function blobToPdfImage(blob: Blob, type: string): Promise<PdfImage> {
  const bitmap = await createImageBitmap(blob);
  try {
    if (!CONVERT_TYPES.has(type)) {
      const dataUri = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader(); reader.onload = () => resolve(String(reader.result)); reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(new Blob([blob], { type }));
      });
      return { dataUri, width: bitmap.width, height: bitmap.height };
    }
    // pdfmake takes PNG/JPEG only: GIF (first frame) and WebP go through a canvas.
    const canvas = document.createElement('canvas'); canvas.width = bitmap.width; canvas.height = bitmap.height;
    const context = canvas.getContext('2d'); if (!context) return { error: 'ошибка преобразования' };
    context.drawImage(bitmap, 0, 0);
    return { dataUri: canvas.toDataURL('image/png'), width: bitmap.width, height: bitmap.height };
  } finally { bitmap.close(); }
}

/** Fetches/decodes every image the PDF can embed (same-origin /files/<id> within the 15 MB budget, data: URIs). */
export async function loadPdfImages(urls: string[]): Promise<Map<string, PdfImage>> {
  const images = new Map<string, PdfImage>();
  let total = 0;
  for (const src of urls) {
    const source = classifyImageUrl(src, location.origin);
    if (source.kind === 'external') { images.set(src, { external: true }); continue; }
    if (source.kind === 'invalid') { images.set(src, { error: source.url.startsWith('data:') ? 'неподдерживаемый тип' : 'недоступный адрес' }); continue; }
    try {
      const response = await fetch(source.url, { credentials: 'same-origin' });
      if (!response.ok) { images.set(src, { error: `ответ сервера ${response.status}` }); continue; }
      const blob = await response.blob();
      const type = (blob.type || response.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
      if (!IMAGE_TYPES.has(type)) { images.set(src, { error: 'неподдерживаемый тип файла' }); continue; }
      if (source.kind === 'file') {
        if (total + blob.size > MAX_EMBEDDED_BYTES) { images.set(src, { error: 'превышен лимит 15 МБ' }); continue; }
        total += blob.size;
      }
      images.set(src, await blobToPdfImage(blob, type));
    } catch { images.set(src, { error: 'ошибка загрузки' }); }
  }
  return images;
}

/** Builds the article PDF in the browser (pdfmake is imported lazily) and returns it as a Blob. */
export async function buildArticlePdf(article: Article, spaceName?: string): Promise<{ blob: Blob; warnings: string[] }> {
  const body = article.body || '';
  const images = await loadPdfImages(collectImageUrls(parseMarkdown(body)));
  const { docDefinition, warnings } = buildPdfDocument({ ...article, body }, { origin: location.origin, images, spaceName });
  try {
    const pdfMake = await loadPdfMake();
    return { blob: await pdfMake.createPdf(docDefinition).getBlob(), warnings };
  } catch (e) {
    throw new Error(`Не удалось сформировать PDF${e instanceof Error && e.message ? `: ${e.message}` : '.'}`);
  }
}

export function downloadPdf(article: Article, blob: Blob) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a'); a.href = url; a.download = exportFileName(article.title, article.revision, 'pdf'); a.style.display = 'none';
  document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 10000);
}
