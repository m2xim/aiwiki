import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import type { Article } from './api';
import { Markdown, parseInternalLink } from './markdown';
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

export function printHtml(html: string) {
  const frame = document.createElement('iframe');
  frame.setAttribute('aria-hidden', 'true'); frame.tabIndex = -1;
  frame.style.cssText = 'position:fixed;right:0;bottom:0;width:0;height:0;border:0;visibility:hidden';
  let done = false; let timer = 0;
  const cleanup = () => { if (done) return; done = true; clearTimeout(timer); frame.remove(); };
  frame.addEventListener('load', () => {
    const win = frame.contentWindow; if (!win) { cleanup(); return; }
    win.addEventListener('afterprint', cleanup);
    timer = window.setTimeout(cleanup, 60000);
    try { win.focus(); win.print(); } catch { cleanup(); }
  });
  frame.srcdoc = html;
  document.body.append(frame);
}
