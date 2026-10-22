// Web Components: <aiwiki-slider>, <aiwiki-toggle>, <aiwiki-select>, <aiwiki-tabs>, <aiwiki-number>, <aiwiki-table>.
// Light DOM on purpose (no Shadow DOM): the kit stylesheet and the artifact author's CSS apply directly,
// forms/labels/focus work natively, and CSS custom properties (theme) need no piercing.
// Every control fires a bubbling `change` CustomEvent with detail.value (the inner native events are swallowed).
import { h, nextId, resolve } from './dom.ts';
import type { Target } from './dom.ts';
import { formatValue, toNumber } from './core/format.ts';
import type { FormatName } from './core/format.ts';
import { effect } from './core/state.ts';
import type { State } from './core/state.ts';
import { table } from './table.ts';
import type { TableHandle } from './table.ts';

type Opt = { value: unknown; label: string };

/** Run fn when the parser has (most likely) finished the element's children. */
function whenParsed(fn: () => void) {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', fn, { once: true });
  else queueMicrotask(fn);
}

function define() {
  abstract class Control extends HTMLElement {
    protected built = false;
    protected abstract build(): void;
    protected abstract sync(): void;
    abstract get value(): unknown;
    abstract set value(v: unknown);
    protected needsChildren = false;
    connectedCallback() {
      if (this.built) return;
      if (this.needsChildren && document.readyState === 'loading') whenParsed(() => this.ensure());
      else this.ensure();
    }
    protected ensure() { if (!this.built) { this.built = true; this.classList.add('aw-ctl'); this.build(); this.sync(); } }
    attributeChangedCallback() { if (this.built) this.sync(); }
    protected emit() { this.dispatchEvent(new CustomEvent('change', { detail: { value: this.value }, bubbles: true })); }
    protected num(name: string, def: number): number { const n = toNumber(this.getAttribute(name)); return n === null ? def : n; }
    protected get labelText() { return this.getAttribute('label') ?? ''; }
    get label() { return this.labelText; }
    set label(v: string) { this.setAttribute('label', v); }
  }
  const swallow = (e: Event) => e.stopPropagation();

  // ---- slider -------------------------------------------------------------------------------
  class Slider extends Control {
    static observedAttributes = ['min', 'max', 'step', 'label', 'unit', 'format', 'value'];
    private input!: HTMLInputElement; private out!: HTMLElement; private lab!: HTMLElement;
    private cur: number | null = null;
    protected build() {
      const id = nextId('aws');
      this.lab = h('span', { class: 'aw-ctl-label' });
      this.out = h('output', { class: 'aw-ctl-out', for: id });
      this.input = h('input', { type: 'range', id, class: 'aw-range-input' });
      this.replaceChildren(h('label', { class: 'aw-ctl-head', for: id }, this.lab, this.out), this.input);
      this.input.addEventListener('input', swallow); this.input.addEventListener('change', swallow);
      this.input.addEventListener('input', () => { this.cur = this.input.valueAsNumber; this.show(); this.emit(); });
      this.cur ??= toNumber(this.getAttribute('value'));
    }
    protected sync() {
      const min = this.num('min', 0), max = this.num('max', 100), step = this.getAttribute('step') ?? '1';
      this.input.min = String(min); this.input.max = String(max); this.input.step = step;
      this.lab.textContent = this.labelText;
      const v = this.cur ?? min;
      this.input.value = String(v);
      this.cur = this.input.valueAsNumber;
      this.show();
    }
    private show() {
      const f = (this.getAttribute('format') as FormatName | null) || 'number';
      this.out.textContent = formatValue(this.cur, f, { digits: 4 }) + (this.getAttribute('unit') ? ' ' + this.getAttribute('unit') : '');
    }
    get value(): number { this.ensure(); return this.cur ?? this.num('min', 0); }
    set value(v: unknown) { const n = toNumber(v); if (n === null) return; this.cur = n; if (this.built) this.sync(); }
  }

  // ---- number -------------------------------------------------------------------------------
  class NumberInput extends Control {
    static observedAttributes = ['min', 'max', 'step', 'label', 'value', 'unit'];
    private input!: HTMLInputElement; private lab!: HTMLElement; private dec!: HTMLButtonElement; private inc!: HTMLButtonElement;
    private cur: number | null = null;
    protected build() {
      const id = nextId('awn');
      this.lab = h('label', { class: 'aw-ctl-label', for: id });
      this.input = h('input', { type: 'text', inputmode: 'decimal', id, class: 'aw-input aw-num-input' });
      this.dec = h('button', { type: 'button', class: 'aw-btn aw-icon', 'aria-label': 'Уменьшить' }, '−');
      this.inc = h('button', { type: 'button', class: 'aw-btn aw-icon', 'aria-label': 'Увеличить' }, '+');
      this.replaceChildren(this.lab, h('div', { class: 'aw-num-row' }, this.dec, this.input, this.inc));
      this.input.addEventListener('input', swallow); this.input.addEventListener('change', swallow);
      this.input.addEventListener('input', () => { const n = toNumber(this.input.value); if (n !== null && this.inRange(n)) { this.cur = n; this.emit(); } });
      this.input.addEventListener('change', () => this.commit(toNumber(this.input.value)));
      this.input.addEventListener('keydown', e => { if (e.key === 'ArrowUp') { e.preventDefault(); this.stepBy(1); } else if (e.key === 'ArrowDown') { e.preventDefault(); this.stepBy(-1); } });
      this.dec.addEventListener('click', () => this.stepBy(-1));
      this.inc.addEventListener('click', () => this.stepBy(1));
      this.cur ??= toNumber(this.getAttribute('value'));
    }
    private inRange(n: number) { return n >= this.num('min', -Infinity) && n <= this.num('max', Infinity); }
    private clamp(n: number) { return Math.min(this.num('max', Infinity), Math.max(this.num('min', -Infinity), n)); }
    private stepBy(dir: number) {
      const step = this.num('step', 1);
      const base = this.cur ?? this.num('min', 0);
      this.commit(+(base + dir * step).toFixed(10));
    }
    private commit(n: number | null) {
      if (n === null) { this.show(); return; }
      const c = this.clamp(n);
      const changed = c !== this.cur;
      this.cur = c; this.show();
      if (changed) this.emit();
    }
    private show() { this.input.value = this.cur === null ? '' : formatValue(this.cur, 'number', { digits: 10 }).replace(/[\s  ]/g, ''); }
    protected sync() { this.lab.textContent = this.labelText; if (this.cur !== null) this.cur = this.clamp(this.cur); this.show(); }
    get value(): number | null { this.ensure(); return this.cur; }
    set value(v: unknown) { const n = toNumber(v); this.cur = n === null ? null : n; if (this.built) this.sync(); }
  }

  // ---- toggle -------------------------------------------------------------------------------
  class Toggle extends Control {
    static observedAttributes = ['label', 'checked'];
    private input!: HTMLInputElement; private lab!: HTMLElement;
    protected build() {
      this.input = h('input', { type: 'checkbox', role: 'switch', class: 'aw-toggle-input' });
      this.lab = h('span', { class: 'aw-ctl-label' });
      this.replaceChildren(h('label', { class: 'aw-toggle' }, this.input, h('span', { class: 'aw-track', 'aria-hidden': 'true' }), this.lab));
      this.input.addEventListener('input', swallow); this.input.addEventListener('change', swallow);
      this.input.addEventListener('change', () => { this.emit(); });
    }
    protected sync() { this.lab.textContent = this.labelText; if (this.getAttribute('checked') !== null && !this.dirty) this.input.checked = true; }
    private dirty = false;
    get checked() { return this.value; }
    set checked(v: boolean) { this.value = v; }
    get value(): boolean { this.ensure(); return this.input.checked; }
    set value(v: unknown) { this.ensure(); this.dirty = true; this.input.checked = v === true || v === 'true' || v === 1; }
  }

  // ---- select -------------------------------------------------------------------------------
  function parseOptions(raw: string | null): Opt[] | null {
    if (!raw) return null;
    const t = raw.trim();
    if (t.startsWith('[')) {
      try { return (JSON.parse(t) as unknown[]).map(o => (o && typeof o === 'object' ? { value: (o as Opt).value, label: String((o as Opt).label ?? (o as Opt).value) } : { value: o, label: String(o) })); } catch { /* fall through to the list form */ }
    }
    return t.split(',').map(s => s.trim()).filter(Boolean).map(s => { const i = s.indexOf(':'); return i > 0 ? { value: s.slice(0, i).trim(), label: s.slice(i + 1).trim() } : { value: s, label: s }; });
  }
  class Select extends Control {
    static observedAttributes = ['label', 'options', 'value'];
    protected needsChildren = true;
    private sel!: HTMLSelectElement; private lab!: HTMLElement;
    private opts: Opt[] = [];
    private pending: unknown = undefined;
    protected build() {
      const fromChildren = [...this.querySelectorAll('option')].map(o => ({ value: o.hasAttribute('value') ? o.value : o.textContent ?? '', label: o.textContent ?? '' }));
      this.opts = parseOptions(this.getAttribute('options')) ?? fromChildren;
      const id = nextId('awsel');
      this.lab = h('label', { class: 'aw-ctl-label', for: id });
      this.sel = h('select', { id, class: 'aw-input' });
      this.replaceChildren(this.lab, this.sel);
      this.sel.addEventListener('input', swallow); this.sel.addEventListener('change', swallow);
      this.sel.addEventListener('change', () => this.emit());
      this.pending = this.getAttribute('value') ?? undefined;
      this.fill();
    }
    private fill() {
      const keep = this.pending !== undefined ? this.pending : this.built && this.sel.selectedIndex >= 0 ? this.opts[this.sel.selectedIndex]?.value : undefined;
      this.sel.replaceChildren(...this.opts.map((o, i) => h('option', { value: String(i) }, o.label)));
      const idx = keep === undefined ? 0 : Math.max(0, this.opts.findIndex(o => String(o.value) === String(keep)));
      this.sel.selectedIndex = this.opts.length ? idx : -1;
      this.pending = undefined;
    }
    protected sync() {
      this.lab.textContent = this.labelText;
      const attr = parseOptions(this.getAttribute('options'));
      if (attr && JSON.stringify(attr) !== JSON.stringify(this.opts)) { this.opts = attr; this.fill(); }
    }
    get options(): Opt[] { this.ensure(); return this.opts; }
    set options(v: (string | number | Opt)[]) {
      this.ensure();
      this.opts = v.map(o => (typeof o === 'object' ? { value: o.value, label: String(o.label ?? o.value) } : { value: o, label: String(o) }));
      this.fill();
    }
    get value(): unknown { this.ensure(); return this.opts[this.sel.selectedIndex]?.value; }
    set value(v: unknown) { this.ensure(); const i = this.opts.findIndex(o => String(o.value) === String(v)); if (i >= 0) this.sel.selectedIndex = i; }
  }

  // ---- tabs ---------------------------------------------------------------------------------
  class Tabs extends Control {
    static observedAttributes = ['value'];
    protected needsChildren = true;
    private list!: HTMLElement; private panels: HTMLElement[] = []; private btns: HTMLButtonElement[] = [];
    private idx = 0;
    protected build() {
      this.panels = [...this.children].filter((c): c is HTMLElement => c instanceof HTMLElement && (c.hasAttribute('data-tab') || c.hasAttribute('label')));
      this.list = h('div', { class: 'aw-tablist', role: 'tablist' });
      const base = nextId('awtab');
      this.btns = this.panels.map((p, i) => {
        const b = h('button', { type: 'button', role: 'tab', class: 'aw-tab', id: `${base}-t${i}`, 'aria-controls': `${base}-p${i}` }, p.getAttribute('data-tab') ?? p.getAttribute('label') ?? `Вкладка ${i + 1}`);
        p.id ||= `${base}-p${i}`; b.setAttribute('aria-controls', p.id);
        p.setAttribute('role', 'tabpanel'); p.setAttribute('aria-labelledby', b.id); p.classList.add('aw-tabpanel');
        b.addEventListener('click', () => this.select(i, true));
        b.addEventListener('keydown', e => {
          const n = this.btns.length; let t = -1;
          if (e.key === 'ArrowRight') t = (this.idx + 1) % n; else if (e.key === 'ArrowLeft') t = (this.idx - 1 + n) % n; else if (e.key === 'Home') t = 0; else if (e.key === 'End') t = n - 1;
          if (t >= 0) { e.preventDefault(); this.select(t, true); this.btns[t].focus(); }
        });
        return b;
      });
      this.list.append(...this.btns);
      this.insertBefore(this.list, this.firstChild);
      const v = this.getAttribute('value');
      const i = v === null ? 0 : this.panels.findIndex((p, k) => this.valueOf(k) === v);
      this.idx = Math.max(0, i);
    }
    private valueOf(i: number) { const p = this.panels[i]; return p ? p.getAttribute('data-value') ?? p.getAttribute('data-tab') ?? p.getAttribute('label') ?? String(i) : ''; }
    private select(i: number, emit: boolean) { if (i === this.idx && !emit) return; const ch = i !== this.idx; this.idx = i; this.sync(); if (emit && ch) this.emit(); }
    protected sync() {
      this.panels.forEach((p, i) => { p.hidden = i !== this.idx; });
      this.btns.forEach((b, i) => { b.setAttribute('aria-selected', String(i === this.idx)); b.tabIndex = i === this.idx ? 0 : -1; });
    }
    attributeChangedCallback() { if (this.built) { const v = this.getAttribute('value'); const i = this.panels.findIndex((_, k) => this.valueOf(k) === v); if (i >= 0) { this.idx = i; this.sync(); } } }
    get value(): string { this.ensure(); return this.valueOf(this.idx); }
    set value(v: unknown) { this.ensure(); const i = this.panels.findIndex((_, k) => this.valueOf(k) === String(v)); if (i >= 0) { this.idx = i; this.sync(); } }
  }

  // ---- table element ------------------------------------------------------------------------
  class TableEl extends HTMLElement {
    handle: TableHandle | null = null;
    private opts: Record<string, unknown> = {};
    connectedCallback() { if (document.readyState === 'loading') whenParsed(() => this.mount()); else queueMicrotask(() => this.mount()); }
    /** Programmatic options (columns, highlight, rowFilter, …); call before the element renders or use .update(). */
    set config(o: Record<string, unknown>) { this.opts = { ...this.opts, ...o }; this.handle?.update(o); }
    update(o: Record<string, unknown>) { this.config = o; }
    get table(): TableHandle | null { return this.handle; }
    private mount() {
      if (this.handle) return;
      const script = this.querySelector('script[type="text/csv"], script[type="application/json"], script[type="text/plain"]');
      const o: Record<string, unknown> = { ...this.opts };
      if (script && o.rows === undefined) o.rows = script.textContent || '';
      const src = this.getAttribute('src');
      if (src && o.rows === undefined) o.source = src;
      const cols = this.getAttribute('columns');
      if (cols && !o.columns) { try { o.columns = JSON.parse(cols); } catch { o.columns = cols.split(',').map(s => s.trim()).filter(Boolean); } }
      for (const [attr, key] of [['group-by', 'groupBy'], ['title', 'title'], ['csv-name', 'csvName'], ['currency', 'currency'], ['locale', 'locale']] as const) if (this.hasAttribute(attr) && o[key] === undefined) o[key] = this.getAttribute(attr);
      if (this.hasAttribute('page-size') && o.pageSize === undefined) o.pageSize = Number(this.getAttribute('page-size'));
      if (this.hasAttribute('no-search')) o.searchable = false;
      if (this.hasAttribute('no-filters')) o.filters = false;
      if (this.hasAttribute('no-export')) o.exportable = false;
      if (this.hasAttribute('totals')) o.totals = true;
      script?.remove();
      this.removeAttribute('title');
      this.handle = table(this, o);
    }
  }

  const defs: [string, CustomElementConstructor][] = [
    ['aiwiki-slider', Slider], ['aiwiki-number', NumberInput], ['aiwiki-toggle', Toggle], ['aiwiki-select', Select], ['aiwiki-tabs', Tabs], ['aiwiki-table', TableEl],
  ];
  for (const [name, ctor] of defs) if (!customElements.get(name)) customElements.define(name, ctor);
}

if (typeof customElements !== 'undefined' && typeof HTMLElement !== 'undefined') define();

// ---- bind -----------------------------------------------------------------------------------
type Bindable = HTMLElement & { value?: unknown };

/** Two-way binding of a control (aiwiki-* element or native input/select) to state[key]. Returns unbind. */
export function bind<T extends object>(control: Target | Bindable, st: State<T>, key: keyof T & string): () => void {
  const el = (typeof control === 'string' ? resolve(control, 'элемент управления') : control) as Bindable;
  const native = el instanceof HTMLInputElement || el instanceof HTMLSelectElement || el instanceof HTMLTextAreaElement;
  const write = (v: unknown) => {
    if (el instanceof HTMLInputElement && el.type === 'checkbox') el.checked = !!v;
    else if (native) { const s = v === null || v === undefined ? '' : String(v); if ((el as HTMLInputElement).value !== s) (el as HTMLInputElement).value = s; }
    else el.value = v;
  };
  const read = (e: Event): unknown => {
    if (!native) return (e as CustomEvent).detail?.value;
    const i = el as HTMLInputElement;
    if (i.type === 'checkbox') return i.checked;
    if (i.type === 'number' || i.type === 'range') return Number.isNaN(i.valueAsNumber) ? undefined : i.valueAsNumber;
    return i.value;
  };
  const handler = (e: Event) => { const v = read(e); if (v !== undefined) (st as Record<string, unknown>)[key] = v; };
  const stop = effect(() => write((st as Record<string, unknown>)[key]));
  el.addEventListener('change', handler);
  if (native) el.addEventListener('input', handler);
  return () => { stop(); el.removeEventListener('change', handler); el.removeEventListener('input', handler); };
}
