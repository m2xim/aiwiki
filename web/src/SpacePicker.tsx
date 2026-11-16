import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { ChevronDown, Search, UserRound } from 'lucide-react';
import type { Space } from './api';

type Props = { spaces: Space[]; current?: Space; onChange: (id: string) => void };

/** Case-insensitive substring match: index of the first hit or -1. */
export const matchAt = (name: string, query: string) => query ? name.toLocaleLowerCase('ru').indexOf(query.toLocaleLowerCase('ru')) : 0;

function Highlighted({ text, query }: { text: string; query: string }) {
  const at = matchAt(text, query);
  if (!query || at < 0) return <>{text}</>;
  return <>{text.slice(0, at)}<mark>{text.slice(at, at + query.length)}</mark>{text.slice(at + query.length)}</>;
}

/** Space switcher with type-ahead: filters by substring of the name and highlights the match. */
export function SpacePicker({ spaces, current, onChange }: Props) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const listId = useId();
  const q = query.trim();

  const options = useMemo(() => {
    const list = current?.archived_at && !spaces.some(s => s.id === current.id) ? [current, ...spaces] : spaces;
    if (!q) return list;
    // Matches at the start of the name first, then the rest; alphabetical inside each group is kept from the input order.
    return list.map(s => ({ s, at: matchAt(s.name, q) })).filter(x => x.at >= 0).sort((a, b) => Number(a.at > 0) - Number(b.at > 0)).map(x => x.s);
  }, [spaces, current, q]);

  useEffect(() => { setActive(0); }, [q]);
  useEffect(() => {
    if (!open) return;
    const close = (event: PointerEvent) => { if (!rootRef.current?.contains(event.target as Node)) { setOpen(false); setQuery(''); } };
    document.addEventListener('pointerdown', close);
    return () => document.removeEventListener('pointerdown', close);
  }, [open]);
  useEffect(() => { listRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.scrollIntoView({ block: 'nearest' }); }, [active, open]);

  const choose = (space: Space | undefined) => {
    if (!space) return;
    setOpen(false); setQuery(''); inputRef.current?.blur();
    if (space.id !== current?.id) onChange(space.id);
  };
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); if (!open) setOpen(true); else setActive(i => Math.min(i + 1, options.length - 1)); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setActive(i => Math.max(i - 1, 0)); }
    else if (event.key === 'Enter') { event.preventDefault(); if (open) choose(options[active]); else setOpen(true); }
    else if (event.key === 'Escape') { if (open) { event.stopPropagation(); setOpen(false); setQuery(''); } }
    else if (event.key === 'Tab') { setOpen(false); setQuery(''); }
  };

  const label = current ? `${current.name}${current.archived_at ? ' · В архиве' : ''}` : 'Нет активных пространств';
  return <div className={`space-picker space-suggest ${open ? 'open' : ''}`} ref={rootRef}>
    {open ? <Search size={14} className="space-suggest-icon"/> : null}
    <input ref={inputRef} role="combobox" aria-label="Пространство" aria-expanded={open} aria-controls={listId} aria-autocomplete="list"
      aria-activedescendant={open && options[active] ? `${listId}-${active}` : undefined}
      value={open ? query : label} placeholder={open ? 'Найти пространство…' : ''} readOnly={!open}
      onFocus={() => { setOpen(true); setQuery(''); }} onClick={() => { if (!open) setOpen(true); }}
      onChange={event => { setQuery(event.target.value); setOpen(true); }} onKeyDown={onKeyDown}/>
    <button type="button" className="space-suggest-chevron" tabIndex={-1} aria-hidden="true" onPointerDown={event => { event.preventDefault(); if (open) { setOpen(false); setQuery(''); inputRef.current?.blur(); } else inputRef.current?.focus(); }}><ChevronDown size={14}/></button>
    {open && <ul className="space-suggest-list" role="listbox" id={listId} ref={listRef}>
      {options.length === 0 ? <li className="space-suggest-empty">Ничего не найдено</li> : options.map((space, index) =>
        <li key={space.id} id={`${listId}-${index}`} data-index={index} role="option" aria-selected={index === active}
          className={`${index === active ? 'active' : ''} ${space.id === current?.id ? 'current' : ''}`}
          onPointerDown={event => event.preventDefault()} onPointerEnter={() => setActive(index)} onClick={() => choose(space)}>
          <span className="space-suggest-name"><Highlighted text={space.name} query={q}/></span>
          {space.archived_at ? <small>архив</small> : space.personal ? <small title="Личное пространство"><UserRound size={11}/></small> : null}
        </li>)}
    </ul>}
  </div>;
}
