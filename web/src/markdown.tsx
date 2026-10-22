import React from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import remarkParse from 'remark-parse';
import { unified } from 'unified';
import { replaceUrl } from './nav';
import { ArtifactEmbed } from './artifacts/ArtifactEmbed';
import { isArtifactFence } from './artifactsCore';

type Heading = { level: number; text: string; id: string };
type Node = { type: string; depth?: number; value?: string; children?: Node[]; data?: { hProperties?: Record<string, unknown> } };
const slugify = (text: string, used: Map<string, number>) => {
  const base = text.toLocaleLowerCase('ru').replace(/[^\p{L}\p{N}]+/gu, '-').replace(/^-+|-+$/g, '') || 'section';
  const count = (used.get(base) || 0) + 1; used.set(base, count);
  return count === 1 ? base : `${base}-${count}`;
};
const plainText = (node: Node): string => node.value ?? (node.children || []).map(plainText).join('');
const parse = (body: string) => unified().use(remarkParse).use(remarkGfm).parse(body) as unknown as Node;
function collectHeadings(tree: Node): Heading[] {
  const used = new Map<string, number>(); const result: Heading[] = [];
  const walk = (node: Node) => {
    if (node.type === 'heading' && node.depth) { const text = plainText(node).trim(); result.push({ level: node.depth, text, id: slugify(text, used) }); }
    node.children?.forEach(walk);
  };
  walk(tree); return result;
}
export function headings(body: string) { return collectHeadings(parse(body)); }
const headingIds = () => (tree: Node) => {
  const used = new Map<string, number>();
  const walk = (node: Node) => {
    if (node.type === 'heading') {
      const id = slugify(plainText(node).trim(), used); const old = node.data?.hProperties || {};
      node.data = { ...node.data, hProperties: { ...old, id } };
    }
    node.children?.forEach(walk);
  };
  walk(tree);
};

/** Recognises same-origin links of the form /?article=ID[#anchor]; returns null for everything else. */
export function parseInternalLink(href: string | undefined): { id: string; anchor: string } | null {
  if (!href) return null;
  try {
    const url = new URL(href, location.origin);
    if (url.origin !== location.origin || url.pathname !== '/') return null;
    const id = url.searchParams.get('article'); if (!id) return null;
    let anchor = url.hash.slice(1); try { anchor = decodeURIComponent(anchor); } catch { /* keep raw fragment */ }
    return { id, anchor };
  } catch { return null; }
}
const isExternal = (href: string | undefined) => {
  if (!href) return false;
  try { const url = new URL(href, location.origin); return /^https?:$/.test(url.protocol) && url.origin !== location.origin; } catch { return false; }
};

type HNode = { type: string; tagName?: string; value?: string; properties?: Record<string, unknown>; children?: HNode[] };
const SKIP_TAGS = new Set(['code', 'pre', 'script', 'style', 'textarea', 'button', 'svg']);
const normalizeTerm = (text: string) => text.toLocaleLowerCase('ru').replace(/ё/g, 'е');
/** Normalised, de-duplicated terms of length >= 2; callers memoise on the joined content, not on array identity. */
export function highlightTerms(highlight?: string[]): string[] {
  if (!highlight?.length) return [];
  return [...new Set(highlight.map(normalizeTerm).filter(term => [...term].length >= 2))];
}
const WORD_RE = /[\p{L}\p{N}]+/gu;
/** Splits a text value into text/mark nodes; null when nothing matches. Whole words that start with a term are marked. */
function markText(value: string, terms: string[]): HNode[] | null {
  let result: HNode[] | null = null; let last = 0;
  for (const match of value.matchAll(WORD_RE)) {
    const word = normalizeTerm(match[0]);
    if (!terms.some(term => word.startsWith(term))) continue;
    const start = match.index!; result ??= [];
    if (start > last) result.push({ type: 'text', value: value.slice(last, start) });
    result.push({ type: 'element', tagName: 'mark', properties: { className: ['search-hit-mark'] }, children: [{ type: 'text', value: match[0] }] });
    last = start + match[0].length;
  }
  if (!result) return null;
  if (last < value.length) result.push({ type: 'text', value: value.slice(last) });
  return result;
}
/** rehype plugin factory (hand-written hast walk, no extra dependencies). Heading ids are not touched. */
const rehypeHighlight = (terms: string[]) => () => (tree: HNode) => {
  const walk = (node: HNode) => {
    if (!node.children || (node.type === 'element' && node.tagName && SKIP_TAGS.has(node.tagName))) return;
    const next: HNode[] = []; let changed = false;
    for (const child of node.children) {
      if (child.type === 'text' && child.value) { const parts = markText(child.value, terms); if (parts) { next.push(...parts); changed = true; continue; } }
      else walk(child);
      next.push(child);
    }
    if (changed) node.children = next;
  };
  walk(tree);
};
/** Scrolls to the first search-hit mark inside root (default: document). Returns true when one was found. */
export const smoothBehavior = (): ScrollBehavior => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
export function scrollToFirstHighlight(root: ParentNode = document, behavior: ScrollBehavior = smoothBehavior()): boolean {
  const mark = root.querySelector<HTMLElement>('mark.search-hit-mark'); if (!mark) return false;
  mark.scrollIntoView({ behavior, block: 'center' }); return true;
}

type MarkdownProps = { highlight?: string[]; body: string; onHeading?: (id: string, text: string) => void; onNavigate?: (id: string, anchor: string) => void; comment?: boolean };
export const Markdown = React.memo(function Markdown({ body, onHeading, onNavigate, comment = false, highlight }: MarkdownProps) {
  const bodyRef = React.useRef(body); bodyRef.current = body;
  const onHeadingRef = React.useRef(onHeading); onHeadingRef.current = onHeading;
  const onNavigateRef = React.useRef(onNavigate); onNavigateRef.current = onNavigate;
  // Components are created once per mode so ReactMarkdown does not remount the tree on every render.
  const components = React.useMemo(() => {
    const heading = (level: 1|2|3|4|5|6) => {
      const Tag = `h${level}` as React.ElementType;
      if (comment) return ({ children, node: _node, ...props }: React.HTMLAttributes<HTMLHeadingElement> & { node?: unknown }) => <Tag {...props}>{children}</Tag>;
      return ({ id, children, node: _node, ...props }: React.HTMLAttributes<HTMLHeadingElement> & { id?: string; node?: unknown }) =>
        <Tag id={id} {...props}>{children}<button className="heading-comment" aria-label="Комментарий к заголовку" onClick={() => { if (!id) return; const text = headings(bodyRef.current).find(h => h.id === id)?.text; onHeadingRef.current?.(id, text || id); }}>＋</button></Tag>;
    };
    const link = ({ href, children, node: _node, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { node?: unknown }) => {
      const internal = parseInternalLink(href);
      if (internal) {
        return <a {...props} href={href} onClick={event => {
          if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
          const navigate = onNavigateRef.current; if (!navigate) return;
          event.preventDefault(); navigate(internal.id, internal.anchor);
        }}>{children}</a>;
      }
      if (href?.startsWith('#')) {
        return <a {...props} href={href} onClick={event => {
          if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
          let id = href.slice(1); try { id = decodeURIComponent(id); } catch { /* keep raw */ }
          event.preventDefault();
          const target = document.getElementById(id); if (!target) return;
          target.scrollIntoView({ behavior: smoothBehavior(), block: 'start' });
          replaceUrl(`${location.pathname}${location.search}#${encodeURIComponent(id)}`);
        }}>{children}</a>;
      }
      if (isExternal(href)) return <a {...props} href={href} target="_blank" rel="noopener noreferrer">{children}</a>;
      return <a {...props} href={href}>{children}</a>;
    };
    return { h1: heading(1), h2: heading(2), h3: heading(3), h4: heading(4), h5: heading(5), h6: heading(6), a: link, pre: ({ children, node: _node, ...props }: React.HTMLAttributes<HTMLPreElement> & { node?: unknown }) => {
      const child = React.Children.toArray(children)[0];
      if (!comment && React.isValidElement<{ className?: string; children?: React.ReactNode }>(child) && isArtifactFence(child.props.className)) {
        const id = String(child.props.children ?? '').trim();
        if (/^[A-Za-z0-9_-]{1,80}$/.test(id)) return <ArtifactEmbed id={id}/>;
      }
      return <pre {...props}>{children}</pre>;
    }, table: ({ children }: { children?: React.ReactNode }) => <div className="markdown-table-scroll" tabIndex={0} aria-label="Таблица"><table>{children}</table></div>, img: ({ src, alt }: { src?: string; alt?: string }) => <img src={src} alt={alt || ''} loading="lazy"/> };
  }, [comment]);
  const plugins = React.useMemo(() => comment ? [remarkGfm] : [remarkGfm, headingIds], [comment]);
  const termsKey = highlightTerms(highlight).join('\n');
  const rehypePlugins = React.useMemo(() => termsKey ? [rehypeHighlight(termsKey.split('\n'))] : [], [termsKey]);
  return <div className={`markdown${comment ? ' comment-markdown' : ''}`}><ReactMarkdown remarkPlugins={plugins} rehypePlugins={rehypePlugins} components={components}>{body}</ReactMarkdown></div>;
}, (a, b) => a.body === b.body && a.comment === b.comment && a.onHeading === b.onHeading && a.onNavigate === b.onNavigate && highlightTerms(a.highlight).join('\n') === highlightTerms(b.highlight).join('\n'));
