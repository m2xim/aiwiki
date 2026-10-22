// Small DOM helpers. Everything is built with createElement/textContent: no innerHTML with data.

export type Target = string | Element;

export function resolve(target: Target, what = 'элемент'): HTMLElement {
  let el: Element | null = null;
  if (typeof target === 'string') {
    el = /^[A-Za-z][\w-]*$/.test(target) ? document.getElementById(target) ?? document.querySelector(target) : document.querySelector(target);
    if (!el) throw new Error(`AIWiki: не найден ${what} «${target}»`);
  } else if (target instanceof Element) el = target;
  else throw new Error(`AIWiki: ожидался селектор или DOM-элемент (${what})`);
  return el as HTMLElement;
}

type Attrs = Record<string, string | number | boolean | null | undefined | ((e: Event) => void)>;

export function h<K extends keyof HTMLElementTagNameMap>(tag: K, attrs?: Attrs | null, ...children: (Node | string | null | undefined)[]): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (typeof v === 'function') el.addEventListener(k.replace(/^on/, '').toLowerCase(), v);
    else if (k === 'class') el.className = String(v);
    else if (k === 'text') el.textContent = String(v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children) if (c !== null && c !== undefined) el.append(c);
  return el;
}

let uid = 0;
export const nextId = (prefix = 'aw') => `${prefix}-${++uid}`;

export function debounce<A extends unknown[]>(fn: (...a: A) => void, ms: number): (...a: A) => void {
  let t: ReturnType<typeof setTimeout> | undefined;
  return (...a: A) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

/** Modal overlay (div, not <dialog>) with Escape/backdrop close and focus restore. */
export function modal(title: string, body: Node, onClose?: () => void): { close(): void; root: HTMLElement } {
  const prev = document.activeElement as HTMLElement | null;
  const closeBtn = h('button', { type: 'button', class: 'aw-btn aw-icon', 'aria-label': 'Закрыть' }, '×');
  const box = h('div', { class: 'aw-modal-box', role: 'dialog', 'aria-modal': 'true', 'aria-label': title, tabindex: '-1' },
    h('div', { class: 'aw-modal-head' }, h('strong', null, title), closeBtn), body);
  const root = h('div', { class: 'aw-modal' }, box);
  const close = () => { root.remove(); document.removeEventListener('keydown', onKey, true); prev?.focus?.(); onClose?.(); };
  const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.stopPropagation(); close(); } };
  closeBtn.addEventListener('click', close);
  root.addEventListener('mousedown', e => { if (e.target === root) close(); });
  document.addEventListener('keydown', onKey, true);
  document.body.append(root);
  box.focus();
  return { close, root };
}
