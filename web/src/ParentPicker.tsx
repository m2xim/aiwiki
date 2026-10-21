import React, { useEffect, useId, useMemo, useRef, useState } from 'react';
import { ChevronDown } from 'lucide-react';
import type { Article } from './api';
import './editor-wave2.css';

type Props = { options: Article[]; value: string; excludeId: string; onChange: (id: string) => void };
type Row = { id: string; title: string; depth: number };

const ROOT: Row = { id: '', title: 'Без родителя (корень)', depth: 0 };

/** Tree order with depth; the article itself and all of its descendants are left out so a cycle cannot be built. */
export function buildParentRows(options: Article[], excludeId: string): Row[] {
  const known = new Set(options.map(a => a.id));
  const children = new Map<string, Article[]>();
  options.forEach(a => { const parent = a.parent_id && a.parent_id !== a.id && known.has(a.parent_id) ? a.parent_id : ''; const list = children.get(parent); if (list) list.push(a); else children.set(parent, [a]); });
  const rows: Row[] = []; const seen = new Set<string>();
  const walk = (parent: string, depth: number) => (children.get(parent) || []).forEach(a => {
    if (a.id === excludeId || seen.has(a.id)) return;
    seen.add(a.id); rows.push({ id: a.id, title: a.title || 'Без названия', depth }); walk(a.id, depth + 1);
  });
  walk('', 0);
  return rows;
}

export function ParentPicker({ options, value, excludeId, onChange }: Props) {
  const uid = useId();
  const rows = useMemo(() => buildParentRows(options, excludeId), [options, excludeId]);
  const [open, setOpen] = useState(false); const [query, setQuery] = useState(''); const [active, setActive] = useState(0);
  const listRef = useRef<HTMLUListElement>(null);
  const selectedTitle = value ? rows.find(r => r.id === value)?.title ?? options.find(a => a.id === value)?.title ?? 'Статья не найдена' : ROOT.title;
  const needle = query.trim().toLocaleLowerCase('ru');
  const items = useMemo(() => {
    const found = needle ? rows.filter(r => r.title.toLocaleLowerCase('ru').includes(needle)) : rows;
    return !needle || ROOT.title.toLocaleLowerCase('ru').includes(needle) ? [ROOT, ...found] : found;
  }, [rows, needle]);
  useEffect(() => { setActive(0); }, [needle, open]);
  useEffect(() => { if (open) listRef.current?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' }); }, [active, open]);
  const close = () => { setOpen(false); setQuery(''); };
  const choose = (id: string) => { onChange(id); close(); };
  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); if (!open) { setOpen(true); return; } setActive(i => Math.min(items.length - 1, i + 1)); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); if (!open) { setOpen(true); return; } setActive(i => Math.max(0, i - 1)); }
    else if (event.key === 'Enter') { if (open) { event.preventDefault(); const item = items[active]; if (item) choose(item.id); } }
    else if (event.key === 'Escape') { if (open) { event.preventDefault(); event.stopPropagation(); close(); } }
    else if (event.key === 'Tab') close();
  };
  const listId = `${uid}-list`;
  return <div className="parent-picker" onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) close(); }}>
    <div className="parent-picker-box">
      <input role="combobox" aria-expanded={open} aria-controls={listId} aria-autocomplete="list" aria-activedescendant={open && items.length ? `${uid}-opt-${active}` : undefined} aria-label="Родительская статья"
        value={open ? query : selectedTitle} placeholder={open ? 'Поиск по заголовку…' : undefined}
        onFocus={() => setOpen(true)} onClick={() => setOpen(true)} onChange={event => { setQuery(event.target.value); setOpen(true); }} onKeyDown={onKeyDown}/>
      <ChevronDown size={14} aria-hidden="true"/>
    </div>
    {open && <ul id={listId} role="listbox" ref={listRef} className="parent-picker-list" onMouseDown={event => event.preventDefault()}>
      {items.map((item, index) => <li key={item.id || 'root'} id={`${uid}-opt-${index}`} role="option" aria-selected={item.id === value} data-active={index === active} className={`parent-picker-option${item.id === value ? ' selected' : ''}${!item.id ? ' root' : ''}`} style={{ paddingLeft: 10 + (needle ? 0 : item.depth) * 16 }} onMouseEnter={() => setActive(index)} onClick={() => choose(item.id)}>{item.title}</li>)}
      {!items.length && <li className="parent-picker-empty" role="presentation">Ничего не найдено</li>}
    </ul>}
  </div>;
}
